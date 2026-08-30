package learn

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
)

// fakePool is a contract.ModelPool stub: Chat returns a canned reply and records
// how many times it was called and the last request's Requires tags.
type fakePool struct {
	reply   string
	calls   int
	lastReq contract.ModelRequest
}

func (p *fakePool) Chat(_ context.Context, req contract.ModelRequest, _ []contract.Message) (contract.ChatResponse, error) {
	p.calls++
	p.lastReq = req
	return contract.ChatResponse{Content: p.reply}, nil
}
func (p *fakePool) ChatStream(context.Context, contract.ModelRequest, []contract.Message) (<-chan contract.StreamEvent, error) {
	return nil, nil
}
func (p *fakePool) ChatStreamWatch(context.Context, contract.ModelRequest, []contract.Message, contract.StreamWatcher) (contract.ChatResponse, error) {
	return contract.ChatResponse{}, nil
}
func (p *fakePool) ContextBudget() int              { return 0 }
func (p *fakePool) Snapshot() contract.PoolSnapshot { return contract.PoolSnapshot{} }
func (p *fakePool) FlushUsage(context.Context)      {}
func (p *fakePool) FlushUsageFinal(context.Context) {}
func (p *fakePool) Close()                          {}

// fakeTarget is a contract.Learnable with an in-memory note + a fixed trace set.
type fakeTarget struct {
	note    string
	traces  []contract.Rollout
	applied int
}

func (t *fakeTarget) Key() string                             { return "tool:fake" }
func (t *fakeTarget) Current(context.Context) (string, error) { return t.note, nil }
func (t *fakeTarget) Apply(_ context.Context, revised string) error {
	t.note = revised
	t.applied++
	return nil
}
func (t *fakeTarget) Traces(_ context.Context, since time.Time) ([]contract.Rollout, error) {
	var out []contract.Rollout
	for _, r := range t.traces {
		if r.At.After(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

type fakeSource struct{ targets []contract.Learnable }

func (s fakeSource) Targets(context.Context) []contract.Learnable { return s.targets }

func TestDistillRewriteAndWatermark(t *testing.T) {
	pool := &fakePool{reply: "  使用经验：先检查参数。  "}
	tgt := &fakeTarget{
		note:   "旧经验",
		traces: []contract.Rollout{{Args: `{"a":1}`, OK: false, Error: "bad", At: time.Now()}},
	}
	e := &engine{model: func() contract.ModelPool { return pool }, lastRun: map[string]time.Time{}, minFail: 1}
	e.AddSource(fakeSource{targets: []contract.Learnable{tgt}})

	// First cycle: a fresh FAILURE (minFail=1) → distill, trimmed reply applied, "learn" tag routed.
	if err := e.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if tgt.applied != 1 || tgt.note != "使用经验：先检查参数。" {
		t.Fatalf("after cycle 1: applied=%d note=%q", tgt.applied, tgt.note)
	}
	if pool.calls != 1 {
		t.Fatalf("model calls = %d, want 1", pool.calls)
	}
	if len(pool.lastReq.Requires) != 1 || pool.lastReq.Requires[0] != "learn" {
		t.Errorf("distill request Requires = %v, want [learn]", pool.lastReq.Requires)
	}

	// Second cycle: the watermark advanced past the only trace → no new traces → no
	// model call, no apply.
	if err := e.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle 2: %v", err)
	}
	if pool.calls != 1 || tgt.applied != 1 {
		t.Fatalf("cycle 2 should be a no-op: calls=%d applied=%d", pool.calls, tgt.applied)
	}
}

// TestWatermarkIsConsumedMaxNotNow is the regression for the trace-loss window:
// the watermark must advance to the newest CONSUMED trace's At, NOT wall-clock at
// completion. If it were now(), a row recorded DURING the (slow) model call — At ≤
// now() but not in this batch — would be filtered out next cycle and lost. Asserted
// deterministically: the traces are timestamped in the past, so a now()-watermark
// is plainly distinguishable from the consumed max.
func TestWatermarkIsConsumedMaxNotNow(t *testing.T) {
	pool := &fakePool{reply: "经验"}
	newest := time.Now().Add(-time.Hour) // clearly in the past, distinct from now()
	tgt := &fakeTarget{traces: []contract.Rollout{
		{At: newest.Add(-time.Minute), OK: true},
		{At: newest, OK: false, Error: "x"}, // newest consumed
	}}
	e := &engine{model: func() contract.ModelPool { return pool }, lastRun: map[string]time.Time{}, minFail: 1}
	e.AddSource(fakeSource{targets: []contract.Learnable{tgt}})

	if err := e.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	got := e.lastRun["tool:fake"]
	if !got.Equal(newest) {
		t.Fatalf("watermark = %v, want the newest consumed trace's At (%v), not wall-clock now — a now() watermark loses in-flight traces", got, newest)
	}
}

// TestFailureGate covers the failure-driven trigger (docs/learn.md §4/§5):
// successes alone never distill (and the watermark advances past them); a
// sub-threshold batch of fresh failures waits; reaching minFail distills.
func TestFailureGate(t *testing.T) {
	now := time.Now()
	mk := func(ok bool, age time.Duration) contract.Rollout {
		return contract.Rollout{At: now.Add(-age), OK: ok, Error: "x"}
	}

	// (1) successes only → no distill; watermark advances past them (don't rescan).
	pool := &fakePool{reply: "经验"}
	tgt := &fakeTarget{traces: []contract.Rollout{mk(true, 2*time.Minute), mk(true, time.Minute)}}
	e := &engine{model: func() contract.ModelPool { return pool }, lastRun: map[string]time.Time{}, minFail: 3}
	e.AddSource(fakeSource{targets: []contract.Learnable{tgt}})
	if err := e.runCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pool.calls != 0 || tgt.applied != 0 {
		t.Fatalf("successes-only must not distill: calls=%d applied=%d", pool.calls, tgt.applied)
	}
	if e.lastRun["tool:fake"].IsZero() {
		t.Fatal("watermark should advance past consumed successes")
	}

	// (2) two fresh failures, minFail=3, well under the 24h backstop → wait, no advance.
	pool2 := &fakePool{reply: "经验"}
	tgt2 := &fakeTarget{traces: []contract.Rollout{mk(false, 2*time.Minute), mk(false, time.Minute)}}
	e2 := &engine{model: func() contract.ModelPool { return pool2 }, lastRun: map[string]time.Time{}, minFail: 3}
	e2.AddSource(fakeSource{targets: []contract.Learnable{tgt2}})
	if err := e2.runCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pool2.calls != 0 {
		t.Fatalf("sub-threshold fresh failures must wait: calls=%d", pool2.calls)
	}
	if !e2.lastRun["tool:fake"].IsZero() {
		t.Fatal("watermark must be HELD while failures accumulate")
	}

	// (3) reaching minFail → distill.
	tgt2.traces = append(tgt2.traces, mk(false, 30*time.Second))
	if err := e2.runCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pool2.calls != 1 || tgt2.applied != 1 {
		t.Fatalf("minFail reached must distill: calls=%d applied=%d", pool2.calls, tgt2.applied)
	}
}

func TestDistillSkippedWithoutPool(t *testing.T) {
	tgt := &fakeTarget{traces: []contract.Rollout{{At: time.Now()}}}
	e := &engine{model: func() contract.ModelPool { return nil }, lastRun: map[string]time.Time{}}
	e.AddSource(fakeSource{targets: []contract.Learnable{tgt}})
	if err := e.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if tgt.applied != 0 {
		t.Fatalf("no distiller pool → nothing applied, got applied=%d", tgt.applied)
	}
}

func TestCapText(t *testing.T) {
	if got := capText("héllo", 3); got != "hél" {
		t.Errorf("capText rune boundary = %q, want hél", got)
	}
	if got := capText("hi", 10); got != "hi" {
		t.Errorf("capText under cap = %q, want hi", got)
	}
}

func TestFormatTracesCap(t *testing.T) {
	var rs []contract.Rollout
	for i := 0; i < maxTracesPerDistill+10; i++ {
		rs = append(rs, contract.Rollout{Args: "a", OK: false, Error: "boom"})
	}
	got := formatTraces(rs)
	if lines := strings.Count(got, "\n"); lines != maxTracesPerDistill {
		t.Errorf("formatTraces emitted %d lines, want capped at %d", lines, maxTracesPerDistill)
	}
}
