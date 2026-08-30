package turn

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/i18n"
	"agentbob/leaf/model"
)

// oversizedPool rejects the FIRST ChatStreamWatch as a context overflow (the
// real leaf/model.ErrContextExceeded wrapping the turn recognizes by string),
// then succeeds on the retry. Chat returns a canned compaction summary; it
// reports a real context budget so the proactive check can be reasoned about.
//
// Importing leaf/model here is intentional and allowed: the arch import
// boundary excludes _test.go files, and pinning the REAL wrapped sentinel makes
// this a genuine end-to-end check of the cross-module coupling (not a hand-typed
// string that could drift from the sentinel).
type oversizedPool struct {
	*fakePool
	budget    int
	summary   string
	reply     string
	calls     int
	overOnce  error // returned on the first ChatStreamWatch call (an overflow reject)
	overEvery error // if set, returned on EVERY ChatStreamWatch call (never fits)
}

// WindowFor is the sizing seam budgetFor asserts (windowSizer).
func (p *oversizedPool) WindowFor(contract.ModelRequest) int { return p.budget }

func (p *oversizedPool) Chat(context.Context, contract.ModelRequest, []contract.Message) (contract.ChatResponse, error) {
	return contract.ChatResponse{Content: p.summary}, nil // compressPass's canned summary
}

func (p *oversizedPool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	p.calls++
	if p.overEvery != nil {
		return contract.ChatResponse{}, p.overEvery
	}
	if p.calls == 1 {
		return contract.ChatResponse{}, p.overOnce
	}
	usage := contract.Usage{InputTokens: 10, OutputTokens: 5}
	accum := &contract.StreamAccumulator{Text: p.reply}
	_ = w(contract.StreamEvent{Text: p.reply}, accum)
	_ = w(contract.StreamEvent{Done: true, Usage: usage}, accum)
	return contract.ChatResponse{Content: p.reply, Usage: usage}, nil
}

// TestRun_ContextOverflowCompactsAndRetries: the pool 400s the prompt as too
// large (EstTokens under-counted, so proactive compaction never fired) → the
// turn takes the roundOversized path, force-compacts, and the retry completes.
func TestRun_ContextOverflowCompactsAndRetries(t *testing.T) {
	ctx := context.Background()
	store := newSubStore()
	// Seed history whose ESTIMATE is under the proactive 80% line (budget 400 →
	// trigger 320) but that still has an older part to summarize — so the overflow
	// reject (not the proactive check) is what drives compaction.
	row := strings.Repeat("X", 100) // ~25 est tokens each; 8 rows ≈ 200 est
	for i := 0; i < 4; i++ {
		if err := store.AppendMessage(ctx, "s1", "user", row, contract.MsgAuthor{}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(ctx, "s1", "assistant", row, contract.MsgAuthor{}); err != nil {
			t.Fatal(err)
		}
	}
	pool := &oversizedPool{
		fakePool: &fakePool{},
		budget:   400,
		summary:  "早期对话摘要",
		reply:    "done",
		overOnce: fmt.Errorf("model pool: %w", model.ErrContextExceeded),
	}
	c := &core{store: store, pool: pool}
	sink := &recSink{}

	res := c.Run(ctx, contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	if res.Outcome != contract.OutcomeFinal || res.Reply != "done" {
		t.Fatalf("want OutcomeFinal reply=done, got outcome=%v reply=%q err=%v", res.Outcome, res.Reply, res.Err)
	}
	if sink.finished != "done" {
		t.Fatalf("sink finished=%q, want done", sink.finished)
	}
	if pool.calls < 2 {
		t.Fatalf("model must have been retried after compaction — calls=%d, want >=2", pool.calls)
	}
	// A compaction summary marker must have been written by the force path.
	all, _ := store.GetMessages(ctx, "s1", 0)
	foundSummary := false
	for _, m := range all {
		if m.RowKind == contract.RowKindSummary {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Fatal("force-compact must have written a summary marker")
	}
}

// TestRun_ContextOverflowPersistentDegradesNoThrash: the prompt overflows on
// EVERY attempt and the oversized bulk lives in the un-compactable recent tail.
// The first force-compact makes real progress (summarizes the older rows); a
// later one re-compresses the prior summary marker and MEASURES no shrink —
// the weighed progress check (real outcome, not a futility forecast) stops the
// retry loop, so the turn ends with the honest context_too_long notice +
// OutcomeDegraded in a BOUNDED number of model calls, rather than thrashing to
// loopCap and falling through to salvage. (A still-oversized summary keeps
// measuring real shrink and proceeds — that is the multi-pass convergence.)
func TestRun_ContextOverflowPersistentDegradesNoThrash(t *testing.T) {
	ctx := context.Background()
	store := newSubStore()
	row := strings.Repeat("X", 100)
	for i := 0; i < 4; i++ {
		if err := store.AppendMessage(ctx, "s1", "user", row, contract.MsgAuthor{}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(ctx, "s1", "assistant", row, contract.MsgAuthor{}); err != nil {
			t.Fatal(err)
		}
	}
	pool := &oversizedPool{
		fakePool:  &fakePool{},
		budget:    400,
		summary:   "早期对话摘要",
		reply:     "unused",
		overEvery: fmt.Errorf("model pool: %w", model.ErrContextExceeded),
	}
	c := &core{store: store, pool: pool}
	sink := &recSink{}

	res := c.Run(ctx, contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	if res.Outcome != contract.OutcomeDegraded {
		t.Fatalf("want OutcomeDegraded, got %v (err=%v)", res.Outcome, res.Err)
	}
	want := i18n.T(contextTooLongKey, "default")
	if !sink.done || sink.finished != want {
		t.Fatalf("sink finished=%q done=%v, want %q/true", sink.finished, sink.done, want)
	}
	// The anti-thrash property: a handful of attempts, NOT loopCap. Without the
	// all-summary no-progress guard this would run ~loopCap (40) times.
	if pool.calls > 4 {
		t.Fatalf("persistent overflow thrashed: %d model calls, want a bounded few (≤4)", pool.calls)
	}
}

// TestRun_ContextOverflowNoProgressDegrades: the prompt overflows AND there is
// nothing to compact (only the current user row) — the turn can't shrink, so it
// ends with the honest context_too_long notice + OutcomeDegraded, never a bogus
// model-unavailable line.
func TestRun_ContextOverflowNoProgressDegrades(t *testing.T) {
	ctx := context.Background()
	store := newSubStore()
	pool := &oversizedPool{
		fakePool: &fakePool{},
		budget:   400,
		summary:  "unused",
		reply:    "unused",
		overOnce: fmt.Errorf("model pool: %w", model.ErrContextExceeded),
	}
	c := &core{store: store, pool: pool}
	sink := &recSink{}

	res := c.Run(ctx, contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	if res.Outcome != contract.OutcomeDegraded {
		t.Fatalf("want OutcomeDegraded, got %v (err=%v)", res.Outcome, res.Err)
	}
	want := i18n.T(contextTooLongKey, "default")
	if !sink.done || sink.finished != want {
		t.Fatalf("sink finished=%q done=%v, want %q/true", sink.finished, sink.done, want)
	}
	// The notice must ALSO be persisted as an assistant row (not just Finished to the
	// sink) so history matches what the user saw and the tool sequence is closed.
	all, _ := store.GetMessages(ctx, "s1", 0)
	persisted := false
	for _, m := range all {
		if m.Role == "assistant" && m.Content == want {
			persisted = true
		}
	}
	if !persisted {
		t.Fatal("context-too-long notice must be persisted as an assistant row, not just Finished")
	}
}
