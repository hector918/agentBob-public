// Tests for context-overflow classification + the failover-stop it drives:
// when the prompt exceeds every same-window candidate's context window the pool
// must NOT churn the rest of the entries (they'd all 400 identically) — it
// stops on the first hit and surfaces ErrContextExceeded so the turn can
// compact + retry. White-box (package model) so a pool is built from fake
// chatters without the provider/probe machinery.
package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsContextExceededErr(t *testing.T) {
	matches := []string{
		// llama.cpp strict context guard
		`POST http://x/v1/chat/completions → 400 Bad Request: {"error":{"message":"exceed_context_size_error: the request exceeds the available context size"}}`,
		// openai / openrouter
		`POST http://x → 400: This model's maximum context length is 131072 tokens, however you requested 135773`,
		// in-stream error frame relayed as a normalized status arrow
		`provider error → 400: context_length_exceeded`,
	}
	for _, s := range matches {
		if !IsContextExceededErr(errors.New(s)) {
			t.Errorf("expected context-exceeded match for %q", s)
		}
	}
	nonMatches := []error{
		nil,
		errors.New(`POST http://x → 400: invalid 'temperature': must be <= 2`), // generic bad-params 400
		fmt.Errorf("failed to parse tool call arguments: unexpected end of JSON"),
		ErrNoModelAvailable,
	}
	for _, e := range nonMatches {
		if IsContextExceededErr(e) {
			t.Errorf("expected NO context-exceeded match for %v", e)
		}
	}
	// Idempotent: the sentinel's own text classifies as context-exceeded.
	if !IsContextExceededErr(ErrContextExceeded) {
		t.Error("ErrContextExceeded must classify as context-exceeded")
	}
	// The tool-args-truncated guard must stay a distinct classification.
	if IsToolArgsTruncatedErr(errors.New("exceeds the available context size")) {
		t.Error("a context-overflow error must not match the tool-args-truncated guard")
	}
}

// TestChatFailoverStopsOnContextExceeded: the FIRST candidate 400s on context
// size; failover must stop (surface ErrContextExceeded) instead of trying the
// second entry — every same-window candidate would 400 the same way.
func TestChatFailoverStopsOnContextExceeded(t *testing.T) {
	ctxErr := errors.New(`POST http://a → 400: exceed_context_size_error: prompt too big`)
	a := &fakeChatter{chatErr: ctxErr}
	b := &fakeChatter{text: "ok"}
	p := newTestPool(entry("a", 3, a), entry("b", 0, b)) // a is higher priority → picked first

	_, err := p.Chat(context.Background(), smartReq, nil)
	if !errors.Is(err, ErrContextExceeded) {
		t.Fatalf("expected ErrContextExceeded, got %v", err)
	}
	ca, _ := a.counts()
	cb, _ := b.counts()
	if ca != 1 || cb != 0 {
		t.Fatalf("failover must STOP on context overflow — got a=%d b=%d, want a=1 b=0", ca, cb)
	}
}

// TestStreamFailoverStopsOnContextExceeded: same guarantee on the streaming
// entrypoint (both go through the shared failover driver). The overflow arrives
// as a pre-content stream error (rejected pre-generation, nothing committed).
func TestStreamFailoverStopsOnContextExceeded(t *testing.T) {
	ctxErr := errors.New(`provider error → 400: This model's maximum context length is 131072 tokens`)
	a := &fakeChatter{preErr: ctxErr}
	b := &fakeChatter{text: "ok"}
	p := newTestPool(entry("a", 3, a), entry("b", 0, b))

	_, err := p.ChatStreamWatch(context.Background(), smartReq, nil, nil)
	if !errors.Is(err, ErrContextExceeded) {
		t.Fatalf("stream: expected ErrContextExceeded, got %v", err)
	}
	if _, sb := b.counts(); sb != 0 {
		t.Fatalf("stream failover must STOP on context overflow — b streamed %d times, want 0", sb)
	}
}
