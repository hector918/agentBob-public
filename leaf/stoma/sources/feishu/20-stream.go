package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/leaf/stoma/sources/streamsink"

	larkcard "github.com/larksuite/oapi-sdk-go/v3/card"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// NewSink returns a block-mode sink backed by the shared streamsink core.
// Feishu's message-edit endpoint is rate- and lifetime-capped, so this declares
// Caps{CanEdit:false} and renders one Message.Create at Finish (live edit-
// streaming is a later enhancement, validated on a real tenant first). The core
// owns Finish idempotency, over-cap splitting (a long reply → a chain of
// Create calls), and grace-ctx survival of a cancelled turn. sessionScope/sid
// are ignored — reply-routing via a message index is deferred.
func (s *Source) NewSink(ctx context.Context, t contract.Target, _ string, _ string, prefs contract.SinkPrefs) contract.Sink {
	w := &feishuWire{src: s, target: t}
	deliver := func(p, c string) error { return s.SendFile(ctx, t, p, c) }
	return streamsink.New(ctx, w, prefs, deliver, "chat_id", t.ChatID)
}

// feishuWire is the Feishu platform leaf for the streamsink core. Block-only
// (no edit, no typing); it renders trace as a separate message before content
// (a chat IM, like wechat). The embedded BlockWireDefaults supplies the no-op
// Edit / BenignEdit / EditGone / Typing (RateLimited is overridden below).
type feishuWire struct {
	streamsink.BlockWireDefaults
	src    *Source
	target contract.Target
}

var _ streamsink.Wire = (*feishuWire)(nil)

// Caps: block channel (no edit-stream, no typing) that does render trace.
//
// LineBreak is the GFM hard break because a short reply goes out as a markdown
// card (sendRich below) — the same soft-break collapse telegram was probed for
// applies to a markdown renderer. UNPROBED against Feishu's own dialect; it is
// the separator this channel has been sending since the trace convention landed
//, so declaring it keeps the bytes identical rather than betting on
// an untested change. On the long-reply plain-text path the trailing spaces are
// invisible either way.
func (w *feishuWire) Caps() streamsink.WireCaps {
	return streamsink.WireCaps{CanEdit: false, CanType: false, TraceRender: true, LineBreak: "  \n"}
}

// Send delivers one channel's text (trace or content) as a Feishu message.
// anchorReply (the core raises it only for the channel's first message) plus a
// Target.ReplyToID stamped by the gateway → the message goes out reply-anchored
// to the user's inbound message; later split-overflow messages post plain.
func (w *feishuWire) Send(ctx context.Context, text string, anchorReply bool) (string, error) {
	return w.src.sendText(ctx, w.target, text, replyAnchor(anchorReply, w.target.ReplyToID))
}

// replyAnchor decides the message id a send should be reply-anchored to: the
// gateway's Target.ReplyToID when the streamsink core asks for anchoring (the
// channel's first message), "" (plain create) otherwise. Pure decision — the
// wire executes the Target instruction, it doesn't make its own.
func replyAnchor(anchorReply bool, replyToID string) string {
	if anchorReply && replyToID != "" {
		return replyToID
	}
	return ""
}

func (w *feishuWire) WireLen(s string) int      { return len(s) } // UTF-8 bytes
func (w *feishuWire) MaxChars() int             { return feishuMaxBytes }
func (w *feishuWire) RedactErr(err error) error { return w.src.redactErr(err) }

// RateLimited overrides BlockWireDefaults' no-op so the streamsink core
// recognises a feishu 429 (createMessage/replyMessage surfaces *feishuRateLimit)
// as a sustained throttle and stops retrying immediately — pacing/cooldown
// belongs to the source's shared send gate (25-sendgate.go), which already
// relayed the call before the error surfaced. (L-FEISHU-D1)
func (w *feishuWire) RateLimited(err error) (time.Duration, bool) { return feishuRateLimited(err) }

// sendText posts one reply to t.ChatID and returns the new message id. Empty
// text is a no-op.
//
// Rendering: bob's reply text is markdown (**bold** / `code` / fenced blocks /
// [links] / lists). A plain "text" message would show that markup literally, so
// a SHORT reply is sent as an interactive card whose single `markdown` element
// lets Feishu render the markdown (reply rendering only — no buttons). But a
// markdown card silently truncates long content (feishuCardMaxBytes), so a long
// reply skips the card and goes straight to plain text — complete beats pretty
// (OCR reports, the long case, are plain text anyway). If a short reply's card
// send is rejected for ANY reason, the SAME text is retried as plain text so the
// user still gets it — degraded to literal markup but never dropped.
//
// replyTo, when non-empty, anchors the message as a reply to that message id —
// both the card and the plain path (createMessage handles the reply endpoint
// and its own anchor-less fallback).
func (s *Source) sendText(ctx context.Context, t contract.Target, text, replyTo string) (string, error) {
	if s.closed.Load() {
		return "", fmt.Errorf("feishu: source closed")
	}
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t.ChatID == "" {
		return "", fmt.Errorf("feishu: send needs a chat_id")
	}

	// Long content truncates inside a markdown card (feishu-side, no error), so
	// route it to a plain text message — complete, just markup shown literally.
	if len(text) > feishuCardMaxBytes {
		return s.sendPlain(ctx, t, text, replyTo)
	}

	// Short content: rich markdown card. On any failure, fall back to plain text.
	cardContent, cardErr := markdownCardContent(text)
	if cardErr == nil {
		msgID, err := s.createMessage(ctx, t.ChatID, "interactive", cardContent, replyTo)
		if err == nil {
			return msgID, nil
		}
		if _, ok := feishuRateLimited(err); ok {
			return "", err // 429:上抛给 streamsink 终态处理,别再降级重拨(对齐 D15/D16)
		}
		// Already redacted by createMessage; log and fall through to plain text.
		slog.Warn("feishu: markdown card send failed — retrying as plain text", "err", err)
	} else {
		slog.Warn("feishu: markdown card build failed — sending plain text", "err", s.redactErr(cardErr))
	}
	return s.sendPlain(ctx, t, text, replyTo)
}

// sendPlain posts text as a plain "text" message (no markdown rendering). The
// content JSON is built with json.Marshal so arbitrary agent text (quotes,
// backslashes, newlines) is escaped correctly — the SDK's NewTextMsgBuilder does
// raw string concatenation and would produce invalid JSON on such input. Shared
// by the long-content path and the card failure fallback in sendText. replyTo
// as in sendText.
func (s *Source) sendPlain(ctx context.Context, t contract.Target, text, replyTo string) (string, error) {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", fmt.Errorf("feishu: marshal text content: %w", err)
	}
	return s.createMessage(ctx, t.ChatID, "text", string(content), replyTo)
}

// markdownCardContent renders text as the JSON content of an interactive card
// holding a single `markdown` element. Feishu renders the markdown dialect
// (bold / italic / inline code / links / lists; fenced blocks degrade to inline
// on the platform side) so bob's reply shows formatted, not raw markup.
func markdownCardContent(text string) (string, error) {
	card := larkcard.NewMessageCard().
		Config(larkcard.NewMessageCardConfig().
			WideScreenMode(true).
			EnableForward(true).
			Build()).
		Elements([]larkcard.MessageCardElement{
			larkcard.NewMessageCardMarkdown().
				Content(text).
				Build(),
		}).
		Build()
	return card.String()
}

// createMessage posts one message of msgType with the given content JSON to
// chatID and returns the new message id. A non-empty replyTo routes through the
// reply endpoint, anchoring the message to that id; a reply failure falls back
// to a plain create — an unanchored message beats a dropped one (same spirit as
// the card→plain fallback in sendText).
func (s *Source) createMessage(ctx context.Context, chatID, msgType, content, replyTo string) (string, error) {
	if replyTo != "" {
		msgID, err := s.replyMessage(ctx, replyTo, msgType, content)
		if err == nil {
			return msgID, nil
		}
		if _, ok := feishuRateLimited(err); ok {
			return "", err // 429:上抛给 streamsink 终态处理,别再降级重拨(对齐 D15/D16)
		}
		// Already redacted by replyMessage; log and fall through to plain create.
		slog.Warn("feishu: reply-anchored send failed — retrying as plain create",
			"reply_to", replyTo, "err", err)
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(msgType).
			Content(content).
			Build()).
		Build()

	// Through the bot's single outbound serialiser: serialises across every sink and
	// shares the 429 cooldown (the core's retry re-enters here too). (L-FEISHU-D1)
	var msgID string
	err := s.gate.Do(ctx, func() error {
		resp, err := s.api.Im.V1.Message.Create(ctx, req)
		if err != nil {
			return s.redactErr(fmt.Errorf("feishu: send: %w", err))
		}
		if d, ok := feishuRespRateLimited(resp.ApiResp); ok {
			return &feishuRateLimit{retryAfter: d} // classified by the gate → shared cooldown + relay
		}
		if resp.Code != 0 {
			// Code/Msg come from the embedded larkcore.CodeError; they carry no
			// secret, but route through redactErr for uniformity.
			return s.redactErr(fmt.Errorf("feishu: send failed: code=%d msg=%s", resp.Code, resp.Msg))
		}
		if resp.Data != nil && resp.Data.MessageId != nil {
			msgID = *resp.Data.MessageId
		}
		return nil
	})
	return msgID, err
}

// replyMessage posts one message of msgType with the given content JSON as a
// reply anchored to parentID (POST /im/v1/messages/:message_id/reply — body is
// create-shaped: msg_type + content) and returns the new message id.
func (s *Source) replyMessage(ctx context.Context, parentID, msgType, content string) (string, error) {
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(parentID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(msgType).
			Content(content).
			Build()).
		Build()

	// Through the same per-bot serialiser + 429 cooldown as createMessage. (L-FEISHU-D1)
	var msgID string
	err := s.gate.Do(ctx, func() error {
		resp, err := s.api.Im.V1.Message.Reply(ctx, req)
		if err != nil {
			return s.redactErr(fmt.Errorf("feishu: reply send: %w", err))
		}
		if d, ok := feishuRespRateLimited(resp.ApiResp); ok {
			return &feishuRateLimit{retryAfter: d}
		}
		if resp.Code != 0 {
			return s.redactErr(fmt.Errorf("feishu: reply send failed: code=%d msg=%s", resp.Code, resp.Msg))
		}
		if resp.Data != nil && resp.Data.MessageId != nil {
			msgID = *resp.Data.MessageId
		}
		return nil
	})
	return msgID, err
}

// Send posts a one-off text message (e.g. an async callback-feedback or admin
// notice). Synchronous — the returned error is REJECTION/failure, per the
// contract.Source.Send contract. It executes the Target instruction as-is: a
// non-empty t.ReplyToID anchors the message.
func (s *Source) Send(ctx context.Context, t contract.Target, text string) error {
	_, err := s.sendText(ctx, t, text, t.ReplyToID)
	return err
}

// SendFile uploads path to Feishu and posts it as a "file" message to t.ChatID.
// SYNCHRONOUS: the bytes are read + uploaded before this returns, so the caller
// may delete the source file immediately. caption, if non-empty, follows as a
// short plain-text note (Feishu file messages carry no caption field).
func (s *Source) SendFile(ctx context.Context, t contract.Target, path, caption string) error {
	if s.closed.Load() {
		return fmt.Errorf("feishu: source closed")
	}
	if t.ChatID == "" {
		return fmt.Errorf("feishu: SendFile needs a chat_id")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("feishu: SendFile open %s: %w", path, err)
	}
	defer f.Close()

	upReq := larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().
			FileType("stream"). // generic; Feishu sniffs the rest
			FileName(filepath.Base(path)).
			File(f).
			Build()).
		Build()
	// The upload counts against the per-bot REST limit like any other send, so it
	// routes through the same gate (it used to bypass it — the one hole in the
	// "every REST send is gated" claim). A 429 is classified so the shared floor
	// pushes and the gate relays; the file reader must rewind before each dial or
	// a relayed attempt would upload an empty body.
	var upResp *larkim.CreateFileResp
	err = s.gate.Do(ctx, func() error {
		if _, serr := f.Seek(0, io.SeekStart); serr != nil {
			return fmt.Errorf("feishu: SendFile rewind: %w", serr)
		}
		resp, cerr := s.api.Im.V1.File.Create(ctx, upReq)
		if cerr != nil {
			return s.redactErr(fmt.Errorf("feishu: SendFile upload: %w", cerr))
		}
		if d, ok := feishuRespRateLimited(resp.ApiResp); ok {
			return &feishuRateLimit{retryAfter: d}
		}
		upResp = resp
		return nil
	})
	if err != nil {
		return err
	}
	if !upResp.Success() {
		return s.redactErr(fmt.Errorf("feishu: SendFile upload failed: code=%d msg=%s", upResp.Code, upResp.Msg))
	}
	if upResp.Data == nil || upResp.Data.FileKey == nil || *upResp.Data.FileKey == "" {
		return fmt.Errorf("feishu: SendFile upload returned empty file_key")
	}

	content, err := json.Marshal(map[string]string{"file_key": *upResp.Data.FileKey})
	if err != nil {
		return fmt.Errorf("feishu: SendFile marshal: %w", err)
	}
	// Executes the Target instruction: a non-empty ReplyToID anchors the file
	// message; the caption rides unanchored (the file already marks the thread).
	if _, err := s.createMessage(ctx, t.ChatID, "file", string(content), t.ReplyToID); err != nil {
		return err
	}
	if strings.TrimSpace(caption) != "" {
		_, _ = s.sendPlain(ctx, t, caption, "") // best-effort label
	}
	return nil
}
