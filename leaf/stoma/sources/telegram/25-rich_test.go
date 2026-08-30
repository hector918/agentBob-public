package telegram

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-telegram/bot"
)

// badRequest builds the error shape the library actually produces for a
// Telegram 400: fmt.Errorf("%w, %s", bot.ErrorBadRequest, description) —
// see vendor/github.com/go-telegram/bot/raw_request.go. Matching on the real
// shape is the point: richRejected uses errors.Is on the wrapped sentinel, so a
// hand-rolled errors.New would silently pass a test that proves nothing.
func badRequest(desc string) error {
	return fmt.Errorf("%w, %s", bot.ErrorBadRequest, desc)
}

// TestRichRejected pins the classification the rich-message fallback turns on.
//
// The asymmetry is the whole design: a MISSED rejection propagates out of Send,
// burns the core's retries, trips maybeDegrade and loses the WHOLE reply; a
// false positive only costs this sink its formatting (plus a WARN). So every
// 400 counts as a format failure EXCEPT the ones the core recovers from itself.
func TestRichRejected(t *testing.T) {
	w := &tgWire{src: &Source{}, chatID: 1}

	t.Run("400s classify as rich-rejected", func(t *testing.T) {
		for _, desc := range []string{
			// The live string, probed by editing with an empty rich_message.
			"RICH_MESSAGE_MARKDOWN_INVALID",
			"RICH_MESSAGE_HTML_INVALID",
			// The whole point of widening: an unsupported chat type that says so in
			// words we've never seen must cost formatting, not the reply.
			"rich messages are not available in this chat",
			"MEDIA_INVALID",
			// A dead chat lands here too — one wasted plain-text retry, then it
			// fails the same way. Cheaper than the alternative.
			"chat not found",
		} {
			if !w.richRejected(badRequest(desc)) {
				t.Errorf("richRejected(400 %q) = false, want true", desc)
			}
		}
	})

	t.Run("edit-state outcomes the core recovers itself are NOT rich-rejected", func(t *testing.T) {
		// BenignEdit swallows this one; EditGone drops the id and re-Sends. Both
		// must reach the core intact, or it loses its own recovery path.
		for _, desc := range []string{
			"message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message",
			"message to edit not found",
			"message can't be edited",
		} {
			if w.richRejected(badRequest(desc)) {
				t.Errorf("richRejected(400 %q) = true, want false — the core recovers this itself", desc)
			}
		}
	})

	t.Run("throttle and non-400 failures are NOT rich-rejected", func(t *testing.T) {
		// A 429 is transport pacing; degrading formatting on a throttle would
		// permanently strip every later reply on this sink after one blip.
		if w.richRejected(&bot.TooManyRequestsError{Message: "Too Many Requests", RetryAfter: 5}) {
			t.Error("a 429 must not classify as a format failure")
		}
		// Network / 403-after-kick / 5xx: not a 400, propagate untouched.
		for _, err := range []error{
			errors.New("dial tcp 149.154.167.220:443: connect: connection refused"),
			errors.New("context deadline exceeded"),
			fmt.Errorf("%w, bot was kicked from the group chat", bot.ErrorForbidden),
		} {
			if w.richRejected(err) {
				t.Errorf("richRejected(%v) = true, want false", err)
			}
		}
	})

	t.Run("nil is not a rejection", func(t *testing.T) {
		if w.richRejected(nil) {
			t.Error("richRejected(nil) = true, want false")
		}
	})
}

// TestDisableRichLatches pins the stickiness: once the server refuses our
// markdown, EVERY later frame on this sink goes plain text. Per-call retrying of
// rich would let one reply flip between rendered and literal markup mid-stream.
func TestDisableRichLatches(t *testing.T) {
	w := &tgWire{src: &Source{}, chatID: 1}
	if w.richDisabled {
		t.Fatal("a fresh wire must start rich-enabled")
	}
	w.disableRich(errors.New("Bad Request: RICH_MESSAGE_MARKDOWN_INVALID"))
	if !w.richDisabled {
		t.Fatal("disableRich must latch")
	}
	// Idempotent: a second refusal must not panic or unlatch.
	w.disableRich(errors.New("Bad Request: RICH_MESSAGE_MARKDOWN_INVALID"))
	if !w.richDisabled {
		t.Fatal("disableRich must stay latched")
	}
}

// TestRichMDNoEscaping pins the deliberate absence of an escaper. Telegram's
// rich markdown takes MarkdownV2's "dangerous" characters literally — probed
//, every string below round-tripped unescaped and rendered as typed.
// If someone later adds an escaper here, users see backslash litter in the chat.
func TestRichMDNoEscaping(t *testing.T) {
	for _, raw := range []string{
		"句号. 减号- 加号+ 等号= 感叹号! 竖线| 花括号{} 圆括号() 方括号[] 井号# 大于号>",
		"版本号 v1.22.0",
		"leaf/stoma/sources/telegram/20-stream.go:127",
		"3 * 4 = 12, a_b_c",
		"**未闭合的粗体",
	} {
		got := richMD(raw)
		if got.Markdown != raw {
			t.Errorf("richMD(%q).Markdown = %q — text must go on the wire verbatim", raw, got.Markdown)
		}
		if got.HTML != "" {
			t.Errorf("richMD(%q) set HTML=%q — exactly one of markdown/html may be used", raw, got.HTML)
		}
	}
}

// TestWireNoBotClientIsSilent pins the tests/startup-race guard on the rich
// path: with no bot client, Send drops silently rather than nil-dereferencing
// its way into a panic that would take down the process.
func TestWireNoBotClientIsSilent(t *testing.T) {
	w := &tgWire{src: &Source{}, chatID: 1}
	id, err := w.Send(context.Background(), "hi", true)
	if err != nil || id != "" {
		t.Fatalf("Send with no bot client = (%q, %v), want (\"\", nil)", id, err)
	}
	if err := w.Edit(context.Background(), "1", "hi"); err != nil {
		t.Fatalf("Edit with no bot client = %v, want nil", err)
	}
}

// TestReplyParamsAnchorsOnlyFirst pins the anchoring rule the core relies on:
// only the FIRST message of a reply carries the reply-to pointer, so a split
// overflow chain doesn't stack N reply quotes in the chat.
func TestReplyParamsAnchorsOnlyFirst(t *testing.T) {
	w := &tgWire{src: &Source{}, chatID: 1, replyTo: 42}
	if p := w.replyParams(true); p == nil || p.MessageID != 42 || !p.AllowSendingWithoutReply {
		t.Fatalf("replyParams(true) = %+v, want anchored at 42 with AllowSendingWithoutReply", p)
	}
	if p := w.replyParams(false); p != nil {
		t.Fatalf("replyParams(false) = %+v, want nil (overflow messages don't re-anchor)", p)
	}
	noAnchor := &tgWire{src: &Source{}, chatID: 1}
	if p := noAnchor.replyParams(true); p != nil {
		t.Fatalf("replyParams with replyTo=0 = %+v, want nil", p)
	}
}

// TestRateLimited429StillClassifies guards the shared 429 classifier through the
// library upgrade — the streaming wire and the one-off sends both pace off it.
func TestRateLimited429StillClassifies(t *testing.T) {
	d, ok := rateLimited429(&bot.TooManyRequestsError{RetryAfter: 7})
	if !ok || d.Seconds() != 7 {
		t.Fatalf("rateLimited429(429 retry_after=7) = (%v, %v), want (7s, true)", d, ok)
	}
	if _, ok := rateLimited429(errors.New("Bad Request: chat not found")); ok {
		t.Fatal("a 400 must not classify as rate-limited")
	}
}

// TestCaps_LineBreakIsHardBreak pins telegram's line-boundary declaration.
//
// This channel renders bob's text as markdown (richMD), where a bare "\n" is a
// SOFT break: probed, newline-joined lines come back as one
// paragraph with the newlines turned into spaces. Two spaces before the newline
// is the GFM hard break that survives. The streaming core reads this for its
// trace channel and the slash dispatcher reads it (via contract.LineBreakSink)
// for command replies — drop it and both collapse into one running paragraph.
func TestCaps_LineBreakIsHardBreak(t *testing.T) {
	if got := (&tgWire{}).Caps().LineBreak; got != "  \n" {
		t.Fatalf("telegram LineBreak = %q, want %q (GFM hard break)", got, "  \n")
	}
}
