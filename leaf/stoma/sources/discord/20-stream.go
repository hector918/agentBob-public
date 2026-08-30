package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"agentbob/contract"
	"agentbob/leaf/stoma/sources/sendgate"
	"agentbob/leaf/stoma/sources/streamsink"

	"github.com/bwmarrin/discordgo"
)

// NewSink returns a streaming sink backed by the shared streamsink core. The
// core owns the render state machine (two-channel trace/content, rate-limited
// edit cadence, over-cap splitting at the 2000-char Discord limit, 429 degrade-
// to-block, typing indicator, final-flush survival of a cancelled turn ctx);
// discordWire below supplies the Discord wire calls and declares the channel's
// capabilities. Render behaviour depends on prefs (Stream / Trace), resolved by
// the core.
func (s *Source) NewSink(ctx context.Context, t contract.Target, _, _ string, prefs contract.SinkPrefs) contract.Sink {
	w := &discordWire{src: s, target: t}
	deliver := func(p, c string) error { return s.SendFile(ctx, t, p, c) }
	return streamsink.New(ctx, w, prefs, deliver, "chat_id", t.ChatID)
}

// discordWire is the Discord platform leaf for the streamsink core. Discord can
// edit-stream (ChannelMessageEdit), type (ChannelTyping), and render trace as a
// separate message, so all three capabilities are on. One per NewSink — it
// captures the target and exposes the Discord wire calls. (sessionScope/sid are
// unused — message-index reply-routing is deferred.)
type discordWire struct {
	src    *Source
	target contract.Target

	typingErrLogged bool // Typing runs on the core's loop goroutine only → no lock
}

var _ streamsink.Wire = (*discordWire)(nil)

func (w *discordWire) Caps() streamsink.WireCaps {
	// LineBreak: Discord renders bob's text as markdown (see Send below), so
	// line boundaries in bob's own line-structured text go out as GFM hard
	// breaks — the same separator this channel has sent since the trace
	// convention landed. Discord's renderer is not known to
	// collapse a bare newline the way Telegram's does, so this is byte-stability
	// rather than a fix; the trailing spaces are invisible either way.
	return streamsink.WireCaps{CanEdit: true, CanType: true, TraceRender: true, LineBreak: "  \n"}
}

// Send delivers one channel's text (trace or content) as a Discord message. On the
// anchor send (the channel's first), it threads the reply onto the triggering
// message (Caps.ReplyTo) via a MessageReference. 429s are relayed in-source
// (sendgate.Relay); the surviving RAW error goes to the core for its
// RateLimited / BenignEdit classification (the core redacts via RedactErr).
func (w *discordWire) Send(ctx context.Context, text string, anchorReply bool) (string, error) {
	replyTo := ""
	if anchorReply {
		replyTo = w.target.ReplyToID
	}
	var id string
	err := sendgate.Relay(ctx, discordRateLimited, func() error {
		var serr error
		id, serr = w.src.sendText(ctx, w.target, text, replyTo)
		return serr
	})
	return id, err
}

// Edit replaces a previously-sent message's text in place. 429s are relayed
// in-source (sendgate.Relay — the source owns throttle handling); any other
// error goes to the core raw for its transient retry + classification.
func (w *discordWire) Edit(ctx context.Context, msgID, text string) error {
	return sendgate.Relay(ctx, discordRateLimited, func() error {
		_, err := w.src.dg.ChannelMessageEdit(w.target.ChatID, msgID, text, discordgo.WithContext(ctx))
		return err
	})
}

// RateLimited reports a Discord 429 and its retry_after. With
// ShouldRetryOnRateLimit=false (set in New) discordgo surfaces a typed
// *RateLimitError instead of blocking; the wire relays it in-source, and a 429
// still standing afterwards is SUSTAINED — the core degrades the edit-stream to
// block mode instead of retrying further.
func (w *discordWire) RateLimited(err error) (time.Duration, bool) { return discordRateLimited(err) }

// discordRateLimited classifies a discord error as a 429 + its retry_after. Shared by
// the streamsink wire (streaming sends) and the one-off send retry (50-send.go), so both
// recognise the typed *RateLimitError that ShouldRetryOnRateLimit=false surfaces.
func discordRateLimited(err error) (time.Duration, bool) {
	var rle *discordgo.RateLimitError
	if errors.As(err, &rle) && rle.RateLimit != nil && rle.TooManyRequests != nil {
		return rle.RetryAfter, true
	}
	return 0, false
}

// BenignEdit: discord has no "content unchanged" edit error (an identical edit
// simply succeeds), so no edit error is a benign no-op.
func (w *discordWire) BenignEdit(error) bool { return false }

// EditGone reports that the edit target (the message, or its whole channel) is
// gone — the core drops the dead id and re-delivers the window as a new message
// instead of re-editing it on every flush.
func (w *discordWire) EditGone(err error) bool {
	var re *discordgo.RESTError
	if errors.As(err, &re) && re.Message != nil {
		switch re.Message.Code {
		case discordgo.ErrCodeUnknownMessage, discordgo.ErrCodeUnknownChannel:
			return true
		}
	}
	return false
}

// Typing shows the "is typing…" indicator on the channel. The core gates this to
// "no content shown yet"; here we only send it. Best-effort — a persistently-
// failing action is logged once (redacted).
func (w *discordWire) Typing(ctx context.Context) {
	if err := w.src.dg.ChannelTyping(w.target.ChatID, discordgo.WithContext(ctx)); err != nil && !w.typingErrLogged {
		w.typingErrLogged = true
		slog.Warn("discord: typing indicator failed", "chat_id", w.target.ChatID, "err", w.src.redactErr(err))
	}
}

// WireLen counts Unicode code points — Discord caps a message at 2000 of them.
func (w *discordWire) WireLen(s string) int      { return utf8.RuneCountInString(s) }
func (w *discordWire) MaxChars() int             { return discordMaxChars }
func (w *discordWire) RedactErr(err error) error { return w.src.redactErr(err) }

// sendText posts one reply to t.ChatID (the channel/thread the turn belongs to).
// Empty text is a no-op.
//
// Rendering: bob's reply text is markdown, which Discord renders natively
// (**bold** / `code` / fenced blocks / [links] / lists), so it is sent as-is —
// no card wrapper (unlike feishu).
func (s *Source) sendText(ctx context.Context, t contract.Target, text, replyToID string) (string, error) {
	if s.closed.Load() {
		return "", fmt.Errorf("discord: source closed")
	}
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t.ChatID == "" {
		return "", fmt.Errorf("discord: send needs a chat_id")
	}
	return s.createMessage(ctx, t.ChatID, text, replyToID)
}

// createMessage posts one text message to channelID and returns its id. Returns
// the RAW error (wrapped with %w so the discordgo type is still reachable) — the
// streamsink core classifies it (RateLimited / BenignEdit) and redacts via
// RedactErr at log time; non-streamsink callers redact at their own log site.
func (s *Source) createMessage(ctx context.Context, channelID, text, replyToID string) (string, error) {
	if channelID == "" {
		return "", fmt.Errorf("discord: send needs a channel id")
	}
	var (
		msg *discordgo.Message
		err error
	)
	if replyToID != "" {
		// Reply-anchor (Caps.ReplyTo): thread the reply onto the triggering message.
		// fail_if_not_exists=false so a since-deleted parent degrades to a plain
		// message instead of erroring the whole send.
		failSoft := false
		msg, err = s.dg.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
			Content:   text,
			Reference: &discordgo.MessageReference{MessageID: replyToID, ChannelID: channelID, FailIfNotExists: &failSoft},
		}, discordgo.WithContext(ctx))
	} else {
		msg, err = s.dg.ChannelMessageSend(channelID, text, discordgo.WithContext(ctx))
	}
	if err != nil {
		return "", fmt.Errorf("discord: send: %w", err)
	}
	if msg == nil {
		// err==nil but no message: discordgo handled this as a no-op (e.g. a
		// persistent throttle past its retries). Return ("",nil) — the core treats
		// the transient case — but surface it: a silently-swallowed reply is worth a
		// warn so it's not invisible on a target=file deploy.
		slog.Warn("discord: send returned a nil message with no error — reply may have been dropped", "channel_id", channelID)
		return "", nil
	}
	return msg.ID, nil
}
