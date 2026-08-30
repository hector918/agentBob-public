package turn

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agentbob/contract"
)

// TestDriveLooping_QuietConverge: Mode=looping delivers the converged product in
// one piece — the model's streamed deltas never reach the wrapped rendering sink
// (quiet policy), Finish carries the bare reply, and the outcome is Final.
func TestDriveLooping_QuietConverge(t *testing.T) {
	pool := &fakePool{reply: "工作产品"}
	c := &core{store: newSubStore(), pool: pool}
	sink := &recSink{}

	res := c.Run(context.Background(), contract.TurnSpec{
		Sid: "loop-quiet", Prompt: sysPrompt("sys"), UserText: "干活",
		Sink: sink, Mode: contract.TurnModeLooping,
	})

	if res.Outcome != contract.OutcomeFinal || res.Reply != "工作产品" {
		t.Fatalf("outcome=%v reply=%q, want Final/工作产品", res.Outcome, res.Reply)
	}
	if len(sink.deltas) != 0 {
		t.Fatalf("quiet sink leaked %d ContentDeltas to the wire: %q", len(sink.deltas), sink.deltas)
	}
	if !sink.done || sink.finished != "工作产品" {
		t.Fatalf("finished=%q done=%v, want the bare product delivered in one piece", sink.finished, sink.done)
	}
}

// TestDriveLooping_FuseSalvage: the fuse is pathological — blowing it falls to
// the shared teardown (exitIterCap → salvage → OutcomeDegraded), never a fake
// Final; the sink is still finished exactly once (raw sink, whole from offset 0).
func TestDriveLooping_FuseSalvage(t *testing.T) {
	old := loopingFuseCap
	loopingFuseCap = 2
	defer func() { loopingFuseCap = old }()

	sink := &recSink{}
	c := &core{store: newSubStore(), pool: &emptyPool{}}
	res := c.Run(context.Background(), contract.TurnSpec{
		Sid: "loop-fuse", Prompt: sysPrompt("sys"), UserText: "干活",
		Sink: sink, Mode: contract.TurnModeLooping,
	})

	if res.Outcome != contract.OutcomeDegraded {
		t.Fatalf("outcome=%v, want Degraded (fuse → salvage)", res.Outcome)
	}
	if !sink.done {
		t.Fatal("sink not finished — a blown fuse must still release the sink exactly once")
	}
}

// TestDriveLooping_IterCapBoundsFuse: a flow-set IterCap bounds the looping turn
// tighter than the default fuse — counted in actual model rounds.
func TestDriveLooping_IterCapBoundsFuse(t *testing.T) {
	pool := &countingEmptyPool{}
	c := &core{store: newSubStore(), pool: pool}
	res := c.Run(context.Background(), contract.TurnSpec{
		Sid: "loop-itercap", Prompt: sysPrompt("sys"), UserText: "干活",
		Sink: &recSink{}, Mode: contract.TurnModeLooping, IterCap: 3,
	})

	if res.Outcome != contract.OutcomeDegraded {
		t.Fatalf("outcome=%v, want Degraded", res.Outcome)
	}
	// Salvage may issue its own pool call after the loop; the LOOP itself must
	// have run exactly IterCap rounds, so total calls are IterCap or IterCap+1.
	if pool.calls < 3 || pool.calls > 4 {
		t.Fatalf("model rounds = %d, want 3 (IterCap) [+1 salvage]", pool.calls)
	}
}

// countingEmptyPool counts MAIN-loop ChatStreamWatch rounds and always returns
// nothing — the loop makes no progress and runs to its cap. Rounds tagged
// Requires:["advisor"] are the harness's own consult sub-turn (docs/advisor.md),
// counted separately: they are not the driver's rounds and must not be read as the
// loop failing to exit.
type countingEmptyPool struct {
	fakePool
	calls   int
	advisor int
}

func (p *countingEmptyPool) ChatStreamWatch(_ context.Context, req contract.ModelRequest, _ []contract.Message, _ contract.StreamWatcher) (contract.ChatResponse, error) {
	for _, tag := range req.Requires {
		if tag == "advisor" {
			p.advisor++
			return contract.ChatResponse{}, nil
		}
	}
	p.calls++
	return contract.ChatResponse{}, nil
}

// TestQuietSink_PassThrough pins the wrapper's per-method policy: content is
// dropped, everything else forwards to the wrapped rendering sink.
func TestQuietSink_PassThrough(t *testing.T) {
	inner := &recSink{last: "msg-42"}
	q := &quietSink{inner: inner}

	q.ContentDelta("preamble") // dropped — never rendered live
	q.TraceDelta("working…")   // forwarded — trace pref governs at the inner sink
	if err := q.SendFile("/tmp/x.pdf", "报告"); err != nil {
		t.Fatal(err)
	}
	if err := q.Finish("产品"); err != nil {
		t.Fatal(err)
	}

	if len(inner.deltas) != 0 {
		t.Fatalf("ContentDelta leaked through: %q", inner.deltas)
	}
	if len(inner.trace) != 1 || inner.trace[0] != "working…" {
		t.Fatalf("trace = %q, want [working…]", inner.trace)
	}
	if len(inner.files) != 1 || inner.files[0] != "/tmp/x.pdf" {
		t.Fatalf("files = %q, want the forwarded SendFile", inner.files)
	}
	if inner.finished != "产品" {
		t.Fatalf("finished = %q, want 产品", inner.finished)
	}
	if q.LastSent() != "msg-42" {
		t.Fatalf("LastSent = %q, want the inner sink's id (reply-routing depends on it)", q.LastSent())
	}
}

// TestDriveLooping_StallGuardFires: the pace-invariant stuck-loop guard kills a
// stalled looping turn at stale 14 — long before the 500-round fuse. This is the
// driver doc's "a stalled looping turn still dies at stale 14 however large the
// fuse" claim, pinned (a regression that skips/reorders loopTopNudge would burn
// the whole fuse in model calls before any other test noticed).
func TestDriveLooping_StallGuardFires(t *testing.T) {
	pool := &countingEmptyPool{}
	c := &core{store: newSubStore(), pool: pool}
	res := c.Run(context.Background(), contract.TurnSpec{
		Sid: "loop-stall", Prompt: sysPrompt("sys"), UserText: "干活",
		Sink: &recSink{}, Mode: contract.TurnModeLooping,
	})

	if res.Outcome != contract.OutcomeDegraded {
		t.Fatalf("outcome=%v, want Degraded (stuck-loop → salvage)", res.Outcome)
	}
	// Never-progressed watermarks: stale = iter+1 → STUCK_LOOP exit at iter 13
	// (stale 14). Allow slack for salvage's own pool call(s), nothing near the fuse.
	if pool.calls > 16 {
		t.Fatalf("stalled looping turn ran %d model rounds — the stuck-loop guard did not fire", pool.calls)
	}
	// The same stall crosses advisorStale exactly once, so the harness buys exactly
	// one consult before the ladder takes over (docs/advisor.md §2).
	if pool.advisor == 0 {
		t.Fatal("a 14-round stall never consulted the advisor — the stale-6 trigger is dead")
	}
}

// TestDriveLooping_CancelMidStreamStaysSilent (§4.3 × quiet policy): a cancel
// landing mid-stream on a looping turn must NOT deliver never-rendered preamble
// fragments — nothing was shown (quietSink drops deltas), so the turn exits
// fully silent (Finish("") releases the sink) and persists no assistant partial.
func TestDriveLooping_CancelMidStreamStaysSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := newSubStore()
	c := &core{store: store, pool: &cancelPool{fakePool: &fakePool{}, partial: "先查一下库存…", cancel: cancel}}
	sink := &recSink{}

	res := c.Run(ctx, contract.TurnSpec{
		Sid: "loop-cancel", Prompt: sysPrompt("sys"), UserText: "干活",
		Sink: sink, Mode: contract.TurnModeLooping,
	})

	if res.Outcome != contract.OutcomeCancelled {
		t.Fatalf("outcome=%v, want Cancelled", res.Outcome)
	}
	if sink.finished != "" || len(sink.deltas) != 0 {
		t.Fatalf("cancelled quiet turn leaked output: finished=%q deltas=%q — the user never saw this content", sink.finished, sink.deltas)
	}
	msgs, _ := store.GetMessages(ctx, "loop-cancel", 0)
	for _, m := range msgs {
		if m.Role == "assistant" {
			t.Fatalf("cancelled quiet turn persisted an assistant partial %q — history must match what the user saw (nothing)", m.Content)
		}
	}
}

// TestQuietSink_HoldsPicturesUntilFinish: the reason the wrapper holds at all. A
// looping turn may draw the same picture several times — an acceptance gate rejects
// the delivery, the model redoes the work — and every attempt used to reach the chat
// the instant it existed. Nothing goes out until the turn ends; then all of them do,
// in the order they were produced.
func TestQuietSink_HoldsPicturesUntilFinish(t *testing.T) {
	inner := &recSink{}
	q := &quietSink{inner: inner}

	var sent []string
	q.HoldPicture("/tmp/try-1.png", "", func(error) { sent = append(sent, "try-1") })
	q.HoldPicture("/tmp/try-2.png", "", func(error) { sent = append(sent, "try-2") })

	if len(inner.files) != 0 {
		t.Fatalf("a held picture reached the chat before the turn ended: %q", inner.files)
	}
	if len(sent) != 0 {
		t.Fatalf("producers were told %q before anything was sent", sent)
	}

	if err := q.Finish("画好了"); err != nil {
		t.Fatal(err)
	}
	if len(inner.files) != 2 || inner.files[0] != "/tmp/try-1.png" || inner.files[1] != "/tmp/try-2.png" {
		t.Fatalf("files = %q, want both attempts in production order", inner.files)
	}
	if len(sent) != 2 || sent[0] != "try-1" || sent[1] != "try-2" {
		t.Fatalf("callbacks = %q, want one per picture in order", sent)
	}
	if inner.finished != "画好了" {
		t.Fatalf("finished = %q — the pictures must go out BEFORE the closing text", inner.finished)
	}
}

// TestQuietSink_FlushIsOnceOnly: the flush drains under the lock, so a second Finish
// (a doubled release, a teardown racing a door) cannot re-send a picture the user
// already has — nor run a producer's release callback twice.
func TestQuietSink_FlushIsOnceOnly(t *testing.T) {
	inner := &recSink{}
	q := &quietSink{inner: inner}

	calls := 0
	q.HoldPicture("/tmp/one.png", "", func(error) { calls++ })

	_ = q.Finish("a")
	_ = q.Finish("b")

	if len(inner.files) != 1 {
		t.Fatalf("files = %q, want the picture sent exactly once", inner.files)
	}
	if calls != 1 {
		t.Fatalf("release callback ran %d times, want 1 (a second run drops a record twice)", calls)
	}
}

// TestQuietSink_FailedSendTellsTheProducer: a picture that cannot go out must not be
// silently forgotten — the producer's callback is what routes it to a recovery path,
// and one failure must not abort the rest of the flush.
func TestQuietSink_FailedSendTellsTheProducer(t *testing.T) {
	inner := &explodingFileSink{}
	q := &quietSink{inner: inner}

	var got []error
	q.HoldPicture("/tmp/a.png", "", func(err error) { got = append(got, err) })
	q.HoldPicture("/tmp/b.png", "", func(err error) { got = append(got, err) })

	_ = q.Finish("done")

	if len(got) != 2 {
		t.Fatalf("callbacks ran %d times, want 2 — a failed send must not abort the flush", len(got))
	}
	for i, err := range got {
		if err == nil {
			t.Errorf("picture %d reported success though the send failed", i)
		}
	}
}

// explodingFileSink fails every attachment send; Finish still succeeds.
type explodingFileSink struct{ recSink }

func (s *explodingFileSink) SendFile(string, string) error {
	return errors.New("chat is gone")
}

// TestDriveLooping_FuseStillDeliversHeldPictures: the teardown path the wrapper
// cannot see. driveLooping wraps the sink on its OWN spec copy, so a turn that ends
// through the fuse (or a guard, or a cancel) finishes through the RAW sink and never
// runs quietSink.Finish. Without the state-parked drain the pictures would be
// stranded — never sent, and their WAL records never released for recovery.
func TestDriveLooping_FuseStillDeliversHeldPictures(t *testing.T) {
	old := loopingFuseCap
	loopingFuseCap = 2
	defer func() { loopingFuseCap = old }()

	released := 0
	sink := &recSink{}
	set := newTestSet(holdPictureTool{released: &released})
	c := &core{store: newSubStore(), pool: &neverConvergesPool{}}

	res := c.Run(context.Background(), contract.TurnSpec{
		Sid: "loop-fuse-pic", Prompt: sysPrompt("sys"), UserText: "画一张",
		Sink: sink, Tools: set, Mode: contract.TurnModeLooping,
	})

	if res.Outcome != contract.OutcomeDegraded {
		t.Fatalf("outcome=%v, want Degraded (fuse → salvage)", res.Outcome)
	}
	if len(sink.files) == 0 {
		t.Fatal("a blown fuse stranded the held picture — the user paid for it and never got it")
	}
	if released == 0 {
		t.Fatal("the producer was never told the picture went out — its WAL record stays claimed forever")
	}
}

// neverConvergesPool asks for the same picture every round and never answers, so the
// turn can only end through the fuse — the teardown path that keeps the RAW sink.
type neverConvergesPool struct{ fakePool }

func (p *neverConvergesPool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	calls := []contract.ToolCall{{ID: "c1", Name: "draw", Arguments: "{}"}}
	accum := &contract.StreamAccumulator{ToolCalls: calls}
	_ = w(contract.StreamEvent{Done: true}, accum)
	return contract.ChatResponse{ToolCalls: calls}, nil
}

// holdPictureTool produces a picture the way image_create does — handing it to the
// sink to send whenever the turn decides, and counting the release callbacks.
type holdPictureTool struct{ released *int }

func (holdPictureTool) Spec() contract.ToolSpec { return contract.ToolSpec{Name: "draw", Delivers: true} }
func (holdPictureTool) Serialize() bool         { return false }
func (t holdPictureTool) Run(_ context.Context, tc contract.ToolContext, _ json.RawMessage) contract.ToolResult {
	contract.DeliverPictureWhenReady(tc.Sink, "/tmp/held.png", "", func(error) { *t.released++ })
	return contract.OKResult("已生成")
}
