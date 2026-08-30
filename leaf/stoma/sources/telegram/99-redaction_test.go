// Tests for redactTokenErr (defined in 30-attachments.go), the package's
// last-line defense against the bot token landing in slog output. The
// go-telegram bot library wraps net/http errors verbatim, which carry
// "https://api.telegram.org/bot<TOKEN>/..." in the URL — anywhere we
// log the bot client's raw err, that token can leak. redactTokenErr is
// wrapped around every such site in 10-telegram.go, 20-stream.go,
// 30-attachments.go, 50-send.go.
//
// Invariants pinned here:
//
//   - When the token IS in the err msg, it must be replaced before
//     reaching the slog backend.
//   - When the token is NOT in the err msg, the original err is
//     returned unchanged (no allocation, no wrap) — keeps the
//     errors.Is/As chain identical for safe paths.
//   - The wrap MUST preserve errors.Is so sentinel-checking callers
//     (e.g. context.DeadlineExceeded discriminator) still work after
//     redaction.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeToken mimics the shape of a real Telegram bot token (digits :
// alphanum) without being one. Hard-coded so the test never touches
// the real env-loaded token.
const fakeToken = "1234567890:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi"

// TestRedactToken_FakeError covers the primary case: an error whose
// Error() string embeds the bot token (as net/http produces when the
// API URL contains it) must get scrubbed before reaching slog.
func TestRedactToken_FakeError(t *testing.T) {
	raw := fmt.Errorf("Get \"https://api.telegram.org/bot%s/getMe\": dial tcp: lookup api.telegram.org: no such host", fakeToken)
	wrapped := redactTokenErr(raw, fakeToken)
	if wrapped == nil {
		t.Fatalf("redactTokenErr returned nil for non-nil input")
	}
	msg := wrapped.Error()
	if strings.Contains(msg, fakeToken) {
		t.Fatalf("redactTokenErr left token in error message: %q", msg)
	}
	if !strings.Contains(msg, "<redacted>") {
		t.Errorf("expected <redacted> marker in scrubbed message; got: %q", msg)
	}
	// Unwrap should still reach the original error (so errors.Is/As
	// keep working). The original Error() string of course still
	// contains the token — that's fine, the wrapper's Error() is
	// what slog uses.
	inner := errors.Unwrap(wrapped)
	if inner == nil {
		t.Fatalf("redactTokenErr did not preserve Unwrap chain")
	}
}

// TestRedactToken_NoToken_PassThrough covers the safe-error path:
// when an err doesn't contain the token, the original err is
// returned unchanged. This is the no-op guarantee that lets us
// sprinkle the wrapper defensively everywhere without paying any
// behavioral cost on the 99.9% of err values that have nothing
// to do with the API URL.
func TestRedactToken_NoToken_PassThrough(t *testing.T) {
	raw := errors.New("some unrelated error: connection refused")
	wrapped := redactTokenErr(raw, fakeToken)
	// Identity check: the wrapper MUST return the exact same error
	// value, not a freshly-allocated wrapper that happens to
	// stringify the same.
	if wrapped != raw {
		t.Fatalf("expected pass-through (same err pointer); got a new wrapper: %v", wrapped)
	}
	if wrapped.Error() != raw.Error() {
		t.Errorf("error message changed despite no-token: orig=%q got=%q", raw.Error(), wrapped.Error())
	}
}

// TestRedactToken_PreservesIsAs covers the errors.Is contract: a
// sentinel deep in the chain (here context.DeadlineExceeded, which
// every Telegram-side timeout will produce) must still be
// discoverable after redactTokenErr wraps it. If this regresses,
// every caller doing `errors.Is(err, context.DeadlineExceeded)` to
// classify timeouts vs hard failures silently breaks.
func TestRedactToken_PreservesIsAs(t *testing.T) {
	// Build a chain: sentinel <- fmt wrap (carrying token) <- redactTokenErr
	inner := fmt.Errorf("Post \"https://api.telegram.org/bot%s/sendMessage\": %w", fakeToken, context.DeadlineExceeded)
	wrapped := redactTokenErr(inner, fakeToken)
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(wrapped, context.DeadlineExceeded) returned false; wrap broke sentinel chain")
	}
	// Sanity: also confirm the token is gone from the visible
	// string AND that the wrapper actually wrapped (not pass-through).
	if strings.Contains(wrapped.Error(), fakeToken) {
		t.Errorf("token still present in scrubbed message: %q", wrapped.Error())
	}
}

// TestRedactToken_WrappedReturn covers the SendButtons return path
// (50-send.go::SendButtons): the raw bot-client wire error is redacted
// BEFORE being wrapped with fmt.Errorf and returned to a caller that has
// no token to scrub it itself. Asserts the token doesn't survive the
// outer %w wrap.
func TestRedactToken_WrappedReturn(t *testing.T) {
	raw := fmt.Errorf("Post \"https://api.telegram.org/bot%s/sendMessage\": dial tcp: connection refused", fakeToken)
	returned := fmt.Errorf("telegram: send buttons: %w", redactTokenErr(raw, fakeToken))
	if strings.Contains(returned.Error(), fakeToken) {
		t.Fatalf("token leaked through wrapped return: %q", returned.Error())
	}
	if !strings.Contains(returned.Error(), "<redacted>") {
		t.Errorf("expected <redacted> marker in wrapped return; got: %q", returned.Error())
	}
}

// TestRedactToken_NilErr covers the nil-input contract: redactTokenErr
// must return nil on a nil err so callers can wrap unconditionally
// without an extra `if err != nil` guard at every site.
func TestRedactToken_NilErr(t *testing.T) {
	if got := redactTokenErr(nil, fakeToken); got != nil {
		t.Fatalf("expected nil for nil input, got: %v", got)
	}
}

// TestRedactToken_EmptyToken covers the empty-token contract: when
// the source's token field is unset (test fixtures with a zero-value
// Source struct), redactTokenErr is a pass-through. Without this
// guard, strings.Contains(msg, "") returns true for every err and
// we'd replace empty strings (no-op visually but extra allocation).
func TestRedactToken_EmptyToken(t *testing.T) {
	raw := errors.New("some error")
	if got := redactTokenErr(raw, ""); got != raw {
		t.Fatalf("expected pass-through for empty token; got: %v", got)
	}
}
