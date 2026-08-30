// Generic cooling regression tests retained after the model-pool ssh-wake /
// lifecycle subsystem was removed (task #550). The wake-breaker-specific tests
// that lived here were dropped; what remains pins the GENERIC behaviour all
// entries share: recordError's cancel/deadline filter and the tool-args-truncated
// pool-cascade guard.
//
// White-box (package model).
package model

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"
)

// expiredCtx returns a ctx whose deadline has already elapsed — the
// "caller's own deadline fired" shape recordError's hard-timeout
// classification keys off (ctx.Err() != nil ⇒ a DeadlineExceeded error
// is the caller's doing, not the provider's hard cap).
func expiredCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
	t.Cleanup(cancel)
	return ctx
}

// --- recordError: cancel / deadline filter --------------------------------

// TestRecordError_BareDeadlineExceeded_Skipped: a bare
// context.DeadlineExceeded recorded with a DEAD caller ctx (the caller's
// own request-timeout fired) is filtered out — it must not count toward
// the cooling fail counter.
func TestRecordError_BareDeadlineExceeded_Skipped(t *testing.T) {
	row := entry("plain", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	ctx := expiredCtx(t)
	for i := 0; i < 5; i++ {
		p.recordError(ctx, row, context.DeadlineExceeded)
	}
	row.mu.Lock()
	defer row.mu.Unlock()
	if row.consecutiveFails != 0 {
		t.Errorf("bare DeadlineExceeded must not count toward consecutiveFails, got %d", row.consecutiveFails)
	}
}

// TestRecordError_URLWrappedCancel_Skipped pins B1:
// http.Client.Do wraps a mid-roundtrip caller cancellation in *url.Error,
// so the cancel filter must run unconditionally on context.Canceled — a
// user /stop while the request is in flight must never cool the entry.
func TestRecordError_URLWrappedCancel_Skipped(t *testing.T) {
	row := entry("plain", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	wrapped := fmt.Errorf("POST http://host/v1/chat/completions: %w",
		&url.Error{Op: "Post", URL: "http://host/v1/chat/completions", Err: context.Canceled})
	for i := 0; i < 3; i++ {
		p.recordError(context.Background(), row, wrapped)
	}
	row.mu.Lock()
	defer row.mu.Unlock()
	if row.consecutiveFails != 0 {
		t.Errorf("url.Error-wrapped Canceled must not count toward consecutiveFails, got %d", row.consecutiveFails)
	}
	if !row.deadUntil.IsZero() {
		t.Error("url.Error-wrapped Canceled must not mark the entry dead")
	}
}

// urlWrappedDeadlineErr is the shape http.Client.Do surfaces when ANY
// deadline elapses mid-roundtrip — the caller's ctx and the provider's
// own hard cap (streamHardTimeout) produce the identical error chain, so
// only the caller ctx's state can tell them apart.
func urlWrappedDeadlineErr() error {
	return fmt.Errorf("POST http://host/v1/chat/completions: %w",
		&url.Error{Op: "Post", URL: "http://host/v1/chat/completions", Err: context.DeadlineExceeded})
}

// TestRecordError_URLWrappedDeadline_CallerDead_Skipped: the B1 shape for
// a caller deadline elapsing mid-roundtrip (turn budget firing while the
// HTTP request is in flight). The caller's ctx is dead, so the deadline
// was the caller's — not the entry's fault, must not count.
func TestRecordError_URLWrappedDeadline_CallerDead_Skipped(t *testing.T) {
	row := entry("plain", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	ctx := expiredCtx(t)
	for i := 0; i < 3; i++ {
		p.recordError(ctx, row, urlWrappedDeadlineErr())
	}
	row.mu.Lock()
	defer row.mu.Unlock()
	if row.consecutiveFails != 0 {
		t.Errorf("caller-deadline url.Error-wrapped DeadlineExceeded must not count toward consecutiveFails, got %d", row.consecutiveFails)
	}
}

// TestRecordError_URLWrappedDeadline_CallerAlive_Counts: the SAME error
// shape with the caller's ctx still alive means the deadline that fired
// was the provider's own hard cap — a backend that accepted the request
// and never responded. recordError stamps it errHardTimeout, the filter
// lets it through, and the wedged entry accrues fails / cools / becomes
// heartbeat-eligible instead of staying first in pick forever.
func TestRecordError_URLWrappedDeadline_CallerAlive_Counts(t *testing.T) {
	row := entry("plain", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	for i := 0; i < consecutiveFailThreshold; i++ {
		p.recordError(context.Background(), row, urlWrappedDeadlineErr())
	}
	row.mu.Lock()
	defer row.mu.Unlock()
	if row.consecutiveFails != consecutiveFailThreshold {
		t.Errorf("provider hard-timeout DeadlineExceeded must count toward consecutiveFails, got %d, want %d",
			row.consecutiveFails, consecutiveFailThreshold)
	}
	if row.deadUntil.IsZero() {
		t.Error("a wedged backend at the consecutive-fail threshold must enter cooldown")
	}
}

// TestRecordError_StreamStallDeadline_CountsAsTransport: an err that is
// BOTH context.DeadlineExceeded AND wraps errStreamStall counts as a real
// backend failure — the errStreamStall sentinel alone overrides the
// DeadlineExceeded filter. Recorded with a dead caller ctx so the
// errHardTimeout classification stays out of the picture and the count
// is attributable to errStreamStall.
func TestRecordError_StreamStallDeadline_CountsAsTransport(t *testing.T) {
	row := entry("plain", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	// Synthetic: wraps both errStreamStall AND context.DeadlineExceeded.
	hybrid := fmt.Errorf("entry stalled: %w: %w", errStreamStall, context.DeadlineExceeded)
	p.recordError(expiredCtx(t), row, hybrid)
	row.mu.Lock()
	defer row.mu.Unlock()
	if row.consecutiveFails != 1 {
		t.Errorf("stream-stall+deadline must count as a real fail, got consecutiveFails=%d", row.consecutiveFails)
	}
}

// --- recordError: tool-args-truncated pool-cascade guard ------

// TestRecordError_ToolArgsTruncated_Skipped: a "Failed to parse tool call
// arguments as JSON" error is a PROMPT-level failure (this conversation's
// content blew max_tokens mid-tool-call), not a candidate-health failure.
// recordError must NOT bump consecutiveFails — otherwise one bad turn
// cascades through all pool entries (each sees the same content +
// fine-tune behaviour) and marks the whole pool dead.
func TestRecordError_ToolArgsTruncated_Skipped(t *testing.T) {
	row := entry("smart", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	truncated := errors.New("POST http://192.168.1.100:11434/v1/chat/completions → 500 Internal Server Error: " +
		`{"error":{"code":500,"message":"Failed to parse tool call arguments as JSON: ` +
		`parse error at line 1, column 12917: syntax error while parsing value - invalid string"}}`)
	for i := 0; i < 5; i++ {
		p.recordError(context.Background(), row, truncated)
	}
	row.mu.Lock()
	defer row.mu.Unlock()
	if row.consecutiveFails != 0 {
		t.Errorf("tool-args-truncated must not count toward consecutiveFails, got %d", row.consecutiveFails)
	}
	if row.totalErrors != 0 {
		t.Errorf("tool-args-truncated must not bump totalErrors, got %d", row.totalErrors)
	}
}

// TestRecordError_ToolArgsTruncated_AnthropicVariant: same guard, but the
// error wording is from Anthropic native API instead of llama.cpp.
func TestRecordError_ToolArgsTruncated_AnthropicVariant(t *testing.T) {
	row := entry("anth", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	p.recordError(context.Background(), row, errors.New("anthropic: 400 — Invalid JSON in tool_use input"))
	row.mu.Lock()
	defer row.mu.Unlock()
	if row.consecutiveFails != 0 {
		t.Errorf("anthropic tool_use truncation must not count, got %d", row.consecutiveFails)
	}
}

// TestRecordError_ToolArgsTruncated_GenericJSONError_StillCounts: a
// NON-tool-call JSON parse error elsewhere (e.g. config unmarshal) must
// NOT trip the filter — that's a real bug we want recorded.
func TestRecordError_ToolArgsTruncated_GenericJSONError_StillCounts(t *testing.T) {
	row := entry("smart", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	p.recordError(context.Background(), row, errors.New("unmarshal user config: invalid JSON at line 3"))
	row.mu.Lock()
	defer row.mu.Unlock()
	if row.consecutiveFails != 1 {
		t.Errorf("generic JSON parse error MUST count toward fails, got %d", row.consecutiveFails)
	}
}

// --- B3: request-level 4xx not counted toward entry health ---

// TestRecordError_Context4xx_Skipped: a backend HTTP 4xx (context overflow
// → 400) is a REQUEST-level rejection, not a backend-health failure. Two
// such errors on a big conversation must NOT cool a healthy small-window
// entry. Mirrors the format both providers emit: "POST <url> → <status>".
func TestRecordError_Context4xx_Skipped(t *testing.T) {
	row := entry("small", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	overflow := errors.New("POST http://192.168.1.100:11434/v1/chat/completions → 400 Bad Request: " +
		`{"error":{"message":"the request exceeds the available context size, try increasing it"}}`)
	for i := 0; i < 3; i++ {
		p.recordError(context.Background(), row, overflow)
	}
	row.mu.Lock()
	defer row.mu.Unlock()
	if row.consecutiveFails != 0 {
		t.Errorf("request-level 4xx must not count toward consecutiveFails, got %d", row.consecutiveFails)
	}
	if !row.deadUntil.IsZero() {
		t.Error("request-level 4xx must not mark the entry dead")
	}
}

// TestRecordError_413And422_Skipped: payload-too-large (413) and
// unprocessable-entity (422) are also request-level — same skip.
func TestRecordError_413And422_Skipped(t *testing.T) {
	row := entry("small", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	p.recordError(context.Background(), row, errors.New("POST http://host/v1/chat/completions → 413 Request Entity Too Large: too big"))
	p.recordError(context.Background(), row, errors.New("POST http://host/v1/chat/completions → 422 Unprocessable Entity: bad input"))
	row.mu.Lock()
	defer row.mu.Unlock()
	if row.consecutiveFails != 0 {
		t.Errorf("413/422 must not count toward consecutiveFails, got %d", row.consecutiveFails)
	}
}

// TestRecordError_5xx_StillCounts: a 5xx is a genuine backend-health
// signal — the server errored, not the request — so it MUST still count
// toward cooling. Guards against isPromptLevel4xx over-matching.
func TestRecordError_5xx_StillCounts(t *testing.T) {
	row := entry("smart", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	p.recordError(context.Background(), row, errors.New("POST http://host/v1/chat/completions → 500 Internal Server Error: boom"))
	row.mu.Lock()
	defer row.mu.Unlock()
	if row.consecutiveFails != 1 {
		t.Errorf("5xx must count toward consecutiveFails, got %d", row.consecutiveFails)
	}
}

// TestRecordError_EntryHealth4xx_NowCounts pins the B3 narrowing:
// 401/403/429/404 are this entry's OWN health faults (key revoked, forbidden,
// rate-limited, endpoint misconfig), NOT prompt-level — they must cool the
// entry so a dead-key backend stops burning a guaranteed-failing attempt on
// every request. Previously the blanket "any 4xx" let them escape.
func TestRecordError_EntryHealth4xx_NowCounts(t *testing.T) {
	for _, status := range []string{"401 Unauthorized", "403 Forbidden", "429 Too Many Requests", "404 Not Found"} {
		row := entry("smart", 0, &fakeChatter{})
		p := newTestPool(row)
		p.recordError(context.Background(), row, errors.New("POST http://host/v1/chat/completions → "+status+": nope"))
		row.mu.Lock()
		got := row.consecutiveFails
		row.mu.Unlock()
		p.Close()
		if got != 1 {
			t.Errorf("%s must count toward consecutiveFails, got %d", status, got)
		}
	}
}

// TestIsPromptLevel4xx_InStreamErrorFrame pins D3: an
// in-stream error frame normalised by the providers carries the same
// " → <code>" arrow as the status-line path, so a gateway-relayed 400
// classifies as prompt-level while a 429 stays entry health.
func TestIsPromptLevel4xx_InStreamErrorFrame(t *testing.T) {
	if !isPromptLevel4xx(errors.New("provider error → 400: maximum context length exceeded")) {
		t.Error("normalised in-stream 400 frame must classify as prompt-level")
	}
	if !isPromptLevel4xx(errors.New("anthropic error → 413: Prompt is too long")) {
		t.Error("normalised anthropic request_too_large frame must classify as prompt-level")
	}
	if isPromptLevel4xx(errors.New("provider error → 429: slow down")) {
		t.Error("in-stream 429 is entry health — must NOT classify as prompt-level")
	}
	if isPromptLevel4xx(errors.New("provider error: something broke")) {
		t.Error("a frame with no recoverable status must not classify as 4xx")
	}
}

// TestIsPromptLevel4xx_NoArrow: a transport-style error with no
// " → <status>" arrow must not be misclassified as a 4xx.
func TestIsPromptLevel4xx_NoArrow(t *testing.T) {
	if isPromptLevel4xx(errors.New("dial tcp 10.0.0.1:443: connect: connection refused")) {
		t.Error("a transport error without a status arrow must not classify as 4xx")
	}
	if isPromptLevel4xx(errors.New("POST http://host: context deadline exceeded")) {
		t.Error("a bare POST error without status arrow must not classify as 4xx")
	}
}
