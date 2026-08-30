package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/leaf/stoma/sources/streamsink"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// utf16Len returns the number of UTF-16 code units required to encode s —
// what Telegram counts against the 4096 message cap. BMP runes cost 1 unit;
// supplementary-plane runes (CJK Ext-B, many emoji) cost 2.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// tgMaxChars is our conservative outbound text budget. Telegram's hard cap is
// 4096 *UTF-16 code units* (NOT bytes, NOT runes — flag emoji + many BMP-pair
// runes cost 2 units). 4000 leaves 96 units of headroom which covers any
// plausible UTF-16 inflation on a 4000-rune string. The streamsink core splits
// a reply into a chain of messages at this budget (counted via WireLen).
//
// 4000 survives the move to rich messages (below) for a DIFFERENT reason than
// it was chosen for. Probed against the real client: sendRichMessage
// ACCEPTS at least 100000 characters, so the 4096 wire cap does not apply to
// the rich path at all — but the client only RENDERS the head of an over-long
// rich message and tails it with an ellipsis. A 4000-char body rendered whole;
// 6000 and up were visibly truncated. The binding constraint moved from the API
// to the client's render cap, which sits somewhere in (4000, 6000]. Same number,
// new justification — do not "relax it since the API allows 100k".
const tgMaxChars = 4000

// NewSink returns a streaming sink backed by the shared streamsink contract. The
// core owns the render state machine (two-channel trace/content, rate-limited
// edit cadence, over-cap splitting, 429 degrade-to-block, typing indicator,
// final-flush survival of a cancelled turn ctx); tgWire below supplies the
// Telegram wire calls and declares the channel's capabilities.
//
// Render behaviour depends on prefs (resolved by the core): Stream=true →
// live message-edit with content + trace in two separate messages (trace above,
// content below — Telegram orders by send timestamp); Stream=false → silent
// until Finish. Trace=false drops TraceDelta at the contract. The sessionScope /
// sid params are unused here — reply routing is the flow's job (it records the
// sink's OnAnchor/LastSent message ids into the MessageIndexer), so the source
// never consumes them.
func (s *Source) NewSink(ctx context.Context, t contract.Target, _, _ string, prefs contract.SinkPrefs) contract.Sink {
	// chatID MUST parse — without it SendMessage(ChatID:0) errors and the
	// underlying bot library may echo the bot token in the error string before
	// our redactTokenErr wrapper sees it. R4Y-1: validate up front and return
	// a no-op sink whose Finish surfaces the parse error.
	chatID, err := strconv.ParseInt(t.ChatID, 10, 64)
	if err != nil {
		slog.Warn("telegram sink: invalid chat_id; using no-op sink", "chat_id_raw", t.ChatID, "err", err)
		return &tgNoopSink{err: errors.New("telegram sink: invalid chat_id (parse failed); message dropped")}
	}
	// threadID / replyTo are cosmetic — parse failure logs a warn and continues
	// with 0 (no thread / no reply pointer).
	threadID := 0
	if t.ThreadID != "" {
		if v, perr := strconv.Atoi(t.ThreadID); perr == nil {
			threadID = v
		} else {
			slog.Warn("telegram sink: invalid thread_id; proceeding without thread", "thread_id_raw", t.ThreadID, "err", perr)
		}
	}
	replyTo := 0
	if t.ReplyToID != "" {
		if v, perr := strconv.Atoi(t.ReplyToID); perr == nil {
			replyTo = v
		} else {
			slog.Warn("telegram sink: invalid reply_to_id; proceeding without reply", "reply_to_id_raw", t.ReplyToID, "err", perr)
		}
	}
	w := &tgWire{src: s, chatID: chatID, threadID: threadID, replyTo: replyTo}
	deliver := func(p, c string) error { return s.SendFile(ctx, t, p, c) }
	// Telegram is one of the channels where "picture" and "attachment" are
	// genuinely different sends, so the sink gets both (contract.PhotoSender).
	photo := func(p, c string) error { return s.SendPhoto(ctx, t, p, c) }
	return streamsink.New(ctx, w, prefs, deliver, "chat_id", chatID).WithPhotoDeliverer(photo)
}

// tgNoopSink is returned when the target is unusable (e.g. invalid chat_id).
// All deltas are dropped silently; Finish surfaces the construction error so
// the agent turn completes cleanly with a delivery error logged upstream.
type tgNoopSink struct{ err error }

func (n *tgNoopSink) ContentDelta(string)           {}
func (n *tgNoopSink) TraceDelta(string)             {}
func (n *tgNoopSink) Finish(string) error           { return n.err }
func (n *tgNoopSink) SendFile(string, string) error { return n.err }
func (n *tgNoopSink) LastSent() string              { return "" }

// tgWire is the Telegram platform leaf for the streamsink contract. One per
// NewSink — it captures the resolved target ids and exposes the Telegram-
// specific wire calls. Telegram can edit-stream, type, and render trace, so all
// three capabilities are on.
type tgWire struct {
	src      *Source
	chatID   int64
	threadID int
	replyTo  int

	typingErrLogged bool // Typing runs on the core's loop goroutine only → no lock
	// richDisabled latches once the server rejects our markdown: every later
	// Send/Edit on this sink goes plain text. Sticky rather than per-call so a
	// reply can't flip between rendered and literal markup mid-stream. Written
	// only from Send/Edit, which the core serialises under its flush mutex
	// (see streamsink.Wire's contract) → no lock, same as typingErrLogged.
	richDisabled bool
}

var _ streamsink.Wire = (*tgWire)(nil)

// Caps: telegram can edit-stream, type, and render trace, so all three are on.
//
// LineBreak is the GFM hard break because this channel renders bob's text as
// markdown (richMD): probed against the live rich-message parser, a
// run of newline-joined lines comes back as a SINGLE paragraph with the
// newlines replaced by spaces, while two spaces before the newline preserves
// the boundary inside the paragraph.
func (w *tgWire) Caps() streamsink.WireCaps {
	return streamsink.WireCaps{CanEdit: true, CanType: true, TraceRender: true, LineBreak: "  \n"}
}

func (w *tgWire) WireLen(s string) int { return utf16Len(s) }
func (w *tgWire) MaxChars() int        { return tgMaxChars }

// richMD wraps bob's reply text as a Telegram rich message.
//
// bob's replies are already markdown, and Telegram's rich markdown is close to
// GitHub Flavored Markdown — crucially it needs NO MarkdownV2-style escaping.
// Probed against the live API with everything unescaped: `. - + = !
// | {} () [] # >`, version strings (v1.22.0), file:line refs, date ranges,
// `3 * 4 = 12`, `a_b_c` — all accepted and rendered literally. So the model's
// raw text goes on the wire as-is; there is no escaper here and there must not
// be one (an escaper would be visible as backslash litter in the rendering).
//
// Half-open markup is safe too: unclosed `**`, unclosed backtick, an unclosed
// fenced block, a half table and a half link were all accepted (they degrade to
// literal text rather than erroring), which is what makes it usable for an
// edit-stream whose window necessarily cuts mid-syntax.
func richMD(text string) models.InputRichMessage {
	return models.InputRichMessage{Markdown: text}
}

// richRejected reports whether err means the server won't take this rich
// PAYLOAD, i.e. whether dropping to plain text is the right answer.
//
// Judged by consequence rather than by pattern-matching one error string. The
// two ways to be wrong are not symmetric:
//
//   - miss a rejection → Send propagates → the core burns its retries →
//     maybeDegrade → the WHOLE reply is never delivered;
//   - false-positive → this one sink loses its formatting, with a WARN naming
//     the error.
//
// So every Bad Request that isn't a known non-format failure counts as a format
// failure. "RICH_MESSAGE_MARKDOWN_INVALID" (probed) is only the case
// we've SEEN; a chat type that doesn't support rich messages and says so in some
// other 400 must not cost the user their reply.
//
// Excluded, in order: a 429 (transport pacing — a sustained throttle is the
// core's degrade-to-block, not a format problem), and the two deterministic
// edit-state outcomes the core already recovers from (BenignEdit swallows a
// no-op edit; EditGone drops the dead id and re-Sends). Everything that isn't a
// 400 at all — network, 403 after being kicked, 5xx — propagates untouched.
//
// A dead chat ("Bad Request: chat not found") does land here and buys one
// pointless plain-text retry before failing the same way; that costs a single
// request on a path that shouldn't happen in a healthy deployment, and
// richDisabled latches so it's not repeated per frame.
func (w *tgWire) richRejected(err error) bool {
	if err == nil {
		return false
	}
	if _, throttled := rateLimited429(err); throttled {
		return false
	}
	if w.BenignEdit(err) || w.EditGone(err) {
		return false
	}
	return errors.Is(err, bot.ErrorBadRequest)
}

// disableRich latches the plain-text fallback for the rest of this sink's life
// and records WHY once — a reply whose markdown the server won't take is a
// content bug worth seeing, not a routine event.
func (w *tgWire) disableRich(err error) {
	if w.richDisabled {
		return
	}
	w.richDisabled = true
	slog.Warn("telegram: rich message rejected — this sink falls back to plain text",
		"chat_id", w.chatID, "err", redactTokenErr(err, w.src.token))
}

// replyParams builds the reply-to-user pointer, or nil when this message must
// not anchor. AllowSendingWithoutReply: if the anchor message was deleted before
// the first flush, send unanchored instead of failing — a hard 400 here would
// repeat on every flush + Finish and silently lose the whole reply (mirrors
// discord's FailIfNotExists=false / feishu's plain-create fallback).
func (w *tgWire) replyParams(anchorReply bool) *models.ReplyParameters {
	if w.replyTo == 0 || !anchorReply {
		return nil
	}
	return &models.ReplyParameters{MessageID: w.replyTo, AllowSendingWithoutReply: true}
}

// Send posts a new Telegram message. anchorReply gates the reply-to-user pointer
// to the FIRST message on the channel (cleaner thread — later split-overflow
// messages don't reply). The core wraps this with 429 retry/backoff, so Send is
// a single attempt that returns the raw error.
//
// Rich first, plain text as the fallback: sendRichMessage renders bob's markdown
// natively (bold, tables, code blocks, lists, LaTeX), which the plain path shows
// as literal `**` / `|---|` litter.
func (w *tgWire) Send(ctx context.Context, text string, anchorReply bool) (string, error) {
	if w.src.b == nil {
		return "", nil // no bot client (tests / startup race) — drop silently
	}
	reply := w.replyParams(anchorReply)

	if !w.richDisabled {
		p := &bot.SendRichMessageParams{ChatID: w.chatID, RichMessage: richMD(text), ReplyParameters: reply}
		if w.threadID != 0 {
			p.MessageThreadID = w.threadID
		}
		var m *models.Message
		// Through the bot's single outbound serialiser: serialises this send against
		// every other sink and shares the 429 cooldown (the core's retry re-enters here
		// and is paced too).
		err := w.src.gate.Do(ctx, func() error {
			var e error
			m, e = w.src.b.SendRichMessage(ctx, p)
			return e
		})
		if err == nil {
			if m == nil {
				return "", nil
			}
			return strconv.Itoa(m.ID), nil
		}
		if !w.richRejected(err) {
			return "", err // 429 / network / dead chat — the core's problem, not a format problem
		}
		w.disableRich(err)
	}

	params := &bot.SendMessageParams{ChatID: w.chatID, Text: text, ReplyParameters: reply}
	if w.threadID != 0 {
		params.MessageThreadID = w.threadID
	}
	var m *models.Message
	err := w.src.gate.Do(ctx, func() error {
		var e error
		m, e = w.src.b.SendMessage(ctx, params)
		return e
	})
	if err != nil {
		return "", err
	}
	if m == nil {
		return "", nil
	}
	return strconv.Itoa(m.ID), nil
}

// Edit replaces a previously-sent message's text. Single attempt — the core
// handles retry/backoff.
//
// editMessageText carries rich_message, and it genuinely re-renders the target:
// probed, editing a plain-text message with rich_message set comes
// back with text=null and a parsed rich_message on the Message — so a message
// that started plain can be upgraded in place. That is what lets the whole
// edit-stream stay rich without send-new-and-delete-the-old churn.
//
// Text is still filled with the same content: the API wants it, and it is what
// the reader sees if a client (or a future server) ignores rich_message.
func (w *tgWire) Edit(ctx context.Context, msgID, text string) error {
	if w.src.b == nil {
		return nil
	}
	id, err := strconv.Atoi(msgID)
	if err != nil {
		return err
	}
	p := &bot.EditMessageTextParams{ChatID: w.chatID, MessageID: id, Text: text}
	if !w.richDisabled {
		rm := richMD(text)
		p.RichMessage = &rm
	}
	// Through the bot's single outbound serialiser (same serialisation + 429 cooldown
	// as Send).
	err = w.src.gate.Do(ctx, func() error {
		_, e := w.src.b.EditMessageText(ctx, p)
		return e
	})
	if err == nil || w.richDisabled || !w.richRejected(err) {
		return err
	}
	// Payload refusal: latch plain text and retry this same window immediately,
	// so the frame isn't lost waiting for the next flush.
	w.disableRich(err)
	p.RichMessage = nil
	return w.src.gate.Do(ctx, func() error {
		_, e := w.src.b.EditMessageText(ctx, p)
		return e
	})
}

// RateLimited reports a Telegram 429 (TooManyRequestsError) and its
// retry_after. telegram's own client surfaces this typed; the streamsink core
// treats it as terminal (429 pacing already happened in the send gate this call
// went through) and degrades the edit-stream to block mode.
func (w *tgWire) RateLimited(err error) (time.Duration, bool) { return rateLimited429(err) }

// rateLimited429 classifies a telegram error as a 429 (TooManyRequestsError) and
// returns its retry_after. Shared by the streaming wire AND the one-off sends so
// both pace through the same gatedSend cooldown floor.
func rateLimited429(err error) (time.Duration, bool) {
	var tmr *bot.TooManyRequestsError
	if errors.As(err, &tmr) {
		return time.Duration(tmr.RetryAfter) * time.Second, true
	}
	return 0, false
}

// BenignEdit: "message is not modified" — the message already shows the pushed
// text, a no-op the core counts as success.
func (w *tgWire) BenignEdit(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "not modified")
}

// EditGone: the edit target no longer exists or is locked — the core drops the
// dead id and re-delivers the window as a new message.
func (w *tgWire) EditGone(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "message to edit not found") ||
		strings.Contains(m, "message can't be edited")
}

// Typing shows the "is typing…" chat action. The core gates this to "no
// content shown yet"; here we only send it. Typing is a per-CHAT indicator —
// sent at chat scope, NEVER thread-scoped (a thread-scoped action renders only
// in the thread view; a user watching the main timeline never sees it,
// confirmed). A persistently-failing action is logged once.
func (w *tgWire) Typing(ctx context.Context) {
	if w.src.b == nil {
		return
	}
	p := &bot.SendChatActionParams{ChatID: w.chatID, Action: models.ChatActionTyping}
	if _, err := w.src.b.SendChatAction(ctx, p); err != nil && !w.typingErrLogged {
		w.typingErrLogged = true
		slog.Warn("telegram: typing indicator (SendChatAction) failed", "chat_id", w.chatID, "err", redactTokenErr(err, w.src.token))
	}
}

func (w *tgWire) RedactErr(err error) error { return redactTokenErr(err, w.src.token) }
