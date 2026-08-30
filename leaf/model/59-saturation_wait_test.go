// B1: saturation is "busy", not "unavailable". When the only
// reason the primary required-tag set yields nothing in pick is that its
// entries are saturated, the pool must WAIT on those primary entries rather
// than silently degrade to a tag-fallback hop (e.g. smart → small). These
// tests pin that behaviour for both the streaming and non-streaming paths.
//
// White-box (package model).
package model

import (
	"context"
	"testing"
	"time"

	"agentbob/contract"
)

// taggedEntry builds an eligible LLM entry with the given tags and a bounded
// concurrency semaphore (capacity 1) so a test can saturate it.
func taggedEntry(name string, c contract.Chatter, tags ...string) *entryRow {
	return &entryRow{
		info: contract.ModelInfo{
			Name: name, Kind: contract.KindLLM, Model: name + "-model", Tags: tags,
		},
		chatter: c,
		sem:     make(chan struct{}, 1),
	}
}

// TestChatSaturatedPrimaryWaitsNotDegrade_NonStreaming: a request requiring
// `smart` with a smart→small fallback, where the single smart entry is
// saturated, must WAIT for the smart slot to free and serve smart — NOT
// silently degrade to the small entry.
func TestChatSaturatedPrimaryWaitsNotDegrade_NonStreaming(t *testing.T) {
	smartCh := &fakeChatter{text: "smart-answer"}
	smallCh := &fakeChatter{text: "small-answer"}
	smart := taggedEntry("smart", smartCh, "smart")
	small := taggedEntry("small", smallCh, "small")
	p := newTestPool(smart, small)
	defer p.Close()
	withFallback(p, FallbackRule{Tags: []string{"smart"}, To: []string{"small"}})

	// Saturate smart: take its only slot.
	smart.sem <- struct{}{}

	// Run Chat in the background — it must block waiting on the smart slot
	// rather than serve small.
	type result struct {
		resp contract.ChatResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := p.Chat(context.Background(), contract.ModelRequest{Requires: []string{"smart"}}, nil)
		done <- result{resp, err}
	}()

	// Give the call a moment to reach the wait. It must NOT have completed by
	// degrading to small.
	select {
	case r := <-done:
		t.Fatalf("Chat returned early (%v / %v) — it degraded to small instead of waiting", r.resp.Model, r.err)
	case <-time.After(100 * time.Millisecond):
	}

	// Free the smart slot — the waiter should now acquire it and serve smart.
	<-smart.sem

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Chat after slot freed: unexpected err %v", r.err)
		}
		if r.resp.Model != "smart" {
			t.Fatalf("served %q, want smart (waited, not degraded)", r.resp.Model)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Chat never completed after the smart slot was freed")
	}

	// The small entry must never have been called.
	if c, _ := smallCh.counts(); c != 0 {
		t.Errorf("small entry was called %d time(s) — degradation happened despite a saturated primary", c)
	}
}

// TestChatDegradesWhenPrimaryDead: degradation IS legitimate when the primary
// is genuinely unavailable (dead/paused), not merely saturated. A paused smart
// entry must fall back to small.
func TestChatDegradesWhenPrimaryDead(t *testing.T) {
	smartCh := &fakeChatter{text: "smart-answer"}
	smallCh := &fakeChatter{text: "small-answer"}
	smart := taggedEntry("smart", smartCh, "smart")
	small := taggedEntry("small", smallCh, "small")
	smart.paused = true // genuinely unavailable, not saturated
	p := newTestPool(smart, small)
	defer p.Close()
	withFallback(p, FallbackRule{Tags: []string{"smart"}, To: []string{"small"}})

	resp, err := p.Chat(context.Background(), contract.ModelRequest{Requires: []string{"smart"}}, nil)
	if err != nil {
		t.Fatalf("expected degradation to small, got err %v", err)
	}
	if resp.Model != "small" {
		t.Fatalf("served %q, want small (legitimate degradation — primary paused)", resp.Model)
	}
}

// TestChatStreamWatchSaturatedPrimaryWaitsNotDegrade: same contract for the
// third failover loop, ChatStreamWatch — the tool-round main path.
func TestChatStreamWatchSaturatedPrimaryWaitsNotDegrade(t *testing.T) {
	smartCh := &fakeChatter{text: "smart-answer"}
	smallCh := &fakeChatter{text: "small-answer"}
	smart := taggedEntry("smart", smartCh, "smart")
	small := taggedEntry("small", smallCh, "small")
	p := newTestPool(smart, small)
	defer p.Close()
	withFallback(p, FallbackRule{Tags: []string{"smart"}, To: []string{"small"}})

	smart.sem <- struct{}{} // saturate smart

	type result struct {
		resp contract.ChatResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := p.ChatStreamWatch(context.Background(), contract.ModelRequest{Requires: []string{"smart"}}, nil, nil)
		done <- result{resp, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("ChatStreamWatch returned early (%v / %v) — it degraded to small instead of waiting", r.resp.Model, r.err)
	case <-time.After(100 * time.Millisecond):
	}

	<-smart.sem // free the slot

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("ChatStreamWatch after slot freed: unexpected err %v", r.err)
		}
		if r.resp.Model != "smart" {
			t.Fatalf("served %q, want smart (waited, not degraded)", r.resp.Model)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ChatStreamWatch never completed after the smart slot was freed")
	}

	if _, s := smallCh.counts(); s != 0 {
		t.Errorf("small entry streamed %d time(s) — degradation despite a saturated primary", s)
	}
}
