package turn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"agentbob/contract"
	"agentbob/i18n"
)

// scriptPool returns a scripted ChatResponse per round (tool calls, then a final
// reply) so the tool loop can be driven deterministically.
type scriptPool struct {
	fakePool
	resps []contract.ChatResponse
	i     int
}

func (p *scriptPool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	if p.i >= len(p.resps) {
		return contract.ChatResponse{Content: "(exhausted)"}, nil
	}
	r := p.resps[p.i]
	p.i++
	accum := &contract.StreamAccumulator{Text: r.Content, ToolCalls: r.ToolCalls}
	_ = w(contract.StreamEvent{Text: r.Content}, accum)
	_ = w(contract.StreamEvent{Done: true, Usage: r.Usage}, accum)
	return r, nil
}

// testTool is a configurable tool for dispatch tests.
type testTool struct {
	name       string
	sideEffect bool
	endsTurn   bool
	run        func(args json.RawMessage) contract.ToolResult
}

func (t testTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{Name: t.name, SideEffect: t.sideEffect, EndsTurn: t.endsTurn}
}
func (t testTool) Serialize() bool { return false }
func (t testTool) Run(_ context.Context, _ contract.ToolContext, args json.RawMessage) contract.ToolResult {
	return t.run(args)
}

type testSet struct {
	byName map[string]contract.Tool
	specs  []contract.ToolSpec
}

func newTestSet(ts ...contract.Tool) *testSet {
	s := &testSet{byName: map[string]contract.Tool{}}
	for _, t := range ts {
		s.byName[t.Spec().Name] = t
		s.specs = append(s.specs, t.Spec())
	}
	return s
}
func (s *testSet) Specs() []contract.ToolSpec { return s.specs }
func (s *testSet) Lookup(n string) (contract.Tool, bool) {
	t, ok := s.byName[n]
	return t, ok
}

// bloatPool aborts the first bloatN calls with an args-bloat error (wrapping the
// contract sentinel), then answers. It records req.Tools length per call so a test
// can see when the §6.8 ladder strips tools.
type bloatPool struct {
	fakePool
	bloatN   int
	calls    int
	toolsLen []int
}

func (p *bloatPool) ChatStreamWatch(_ context.Context, req contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	p.calls++
	p.toolsLen = append(p.toolsLen, len(req.Tools))
	if p.calls <= p.bloatN {
		return contract.ChatResponse{}, fmt.Errorf("args too big: %w", contract.ErrToolArgsBloat)
	}
	reply := "done"
	accum := &contract.StreamAccumulator{Text: reply}
	_ = w(contract.StreamEvent{Text: reply}, accum)
	return contract.ChatResponse{Content: reply}, nil
}

// TestRun_BloatLadderStripsToolsThenAnswers (§6.8): repeated args-bloat aborts are
// recovered (not surfaced as a model error) — first by nudging to chunk while tools
// are kept, then by stripping tools past the cap so the model answers in plain text.
// No DB (in-memory subStore).
func TestRun_BloatLadderStripsToolsThenAnswers(t *testing.T) {
	store := newSubStore()
	set := newTestSet(testTool{name: "echo", run: func(json.RawMessage) contract.ToolResult { return contract.OKResult("x") }})
	pool := &bloatPool{bloatN: maxBloatTrips + 1} // trip past the cap, then succeed
	c := &core{store: store, pool: pool}
	sink := &recSink{}
	pr := sysPrompt("sys")

	res := c.Run(context.Background(), contract.TurnSpec{Sid: "s1", Prompt: pr, UserText: "hi", Sink: sink, Tools: set})

	if res.Err != nil {
		t.Fatalf("turn errored instead of recovering bloat: %v", res.Err)
	}
	// BUG-1: the bloat nudge must be cleared once the model responds without bloating —
	// it should not linger in the prompt after recovery.
	if b := pr.Build(); strings.Contains(b, bloatChunkNudge) || strings.Contains(b, bloatStripNudge) {
		t.Fatalf("bloat_nudge layer not cleared after recovery; prompt still carries it:\n%s", b)
	}
	if !sink.done || sink.finished != "done" {
		t.Fatalf("finished=%q done=%v, want done/true (recovered)", sink.finished, sink.done)
	}
	if len(pool.toolsLen) != pool.bloatN+1 {
		t.Fatalf("model calls=%d, want %d", len(pool.toolsLen), pool.bloatN+1)
	}
	for i := 0; i < pool.bloatN; i++ {
		if pool.toolsLen[i] != 1 {
			t.Fatalf("call %d offered %d tools, want 1 (kept while nudging)", i, pool.toolsLen[i])
		}
	}
	if last := pool.toolsLen[pool.bloatN]; last != 0 {
		t.Fatalf("post-cap call offered %d tools, want 0 (stripped)", last)
	}
}

// partialThenErrPool streams some content then drops with a generic error (not
// degenerate/cancel/bloat) — a post-content stream break.
type partialThenErrPool struct {
	fakePool
	partial string
}

func (p *partialThenErrPool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	accum := &contract.StreamAccumulator{Text: p.partial}
	_ = w(contract.StreamEvent{Text: p.partial}, accum) // shown live (when non-empty)
	return contract.ChatResponse{}, fmt.Errorf("stream dropped mid-flight")
}

// TestRun_StreamBreakPreservesPartial (§4.2): a stream break after visible content
// preserves the partial + an interrupt marker rather than clobbering it with the
// generic unavailable notice. No DB (in-memory subStore).
func TestRun_StreamBreakPreservesPartial(t *testing.T) {
	store := newSubStore()
	c := &core{store: store, pool: &partialThenErrPool{partial: "半截答案"}}
	sink := &recSink{}

	res := c.Run(context.Background(), contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	if res.Err == nil {
		t.Fatal("want the stream error surfaced")
	}
	// The marker is now i18n-keyed (rendered in the turn's detected lang — "hi" → default).
	want := "半截答案" + i18n.T(streamInterruptedMarkerKey, "default")
	if sink.finished != want {
		t.Fatalf("finished=%q, want %q (partial preserved, not clobbered)", sink.finished, want)
	}
	msgs, _ := store.GetMessages(context.Background(), "s1", 0)
	if len(msgs) < 2 || msgs[len(msgs)-1].Content != want {
		t.Fatalf("history last=%+v, want %q persisted", msgs, want)
	}
}

// TestRun_StreamBreakNoContentGetsNotice: a stream break with NOTHING shown still
// gets the generic notice (nothing to preserve).
func TestRun_StreamBreakNoContentGetsNotice(t *testing.T) {
	store := newSubStore()
	c := &core{store: store, pool: &partialThenErrPool{partial: ""}}
	sink := &recSink{}

	c.Run(context.Background(), contract.TurnSpec{Sid: "s2", Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	if sink.finished != i18n.T(modelUnavailableKey, "default") {
		t.Fatalf("finished=%q, want the unavailable notice (nothing shown to preserve)", sink.finished)
	}
}

// TestRun_SideEffectFailureFooter (§6.3.4): a SideEffect tool that fails, with the
// model then claiming success, gets an honest footer appended to the reply.
func TestRun_SideEffectFailureFooter(t *testing.T) {
	store := newSubStore()
	set := newTestSet(testTool{name: "deliver_file", sideEffect: true, run: func(json.RawMessage) contract.ToolResult {
		return contract.ErrResult("disk full")
	}})
	pool := &scriptPool{resps: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "deliver_file", Arguments: `{"path":"r.pdf"}`}}},
		{Content: "已把报告发给你了"}, // the model papers over the failure
	}}
	c := &core{store: store, pool: pool}
	sink := &recSink{}
	c.Run(context.Background(), contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "发报告", Sink: sink, Tools: set})

	if !strings.HasPrefix(sink.finished, "已把报告发给你了") {
		t.Fatalf("footer should append to the model reply, got %q", sink.finished)
	}
	if !strings.Contains(sink.finished, "deliver_file") || !strings.Contains(sink.finished, "未成功") {
		t.Fatalf("finished=%q, want a footer naming the failed deliver_file", sink.finished)
	}
}

// preamblePool models the real mid-stream shape of a tool round: the model narrates
// a visible preamble FIRST (text events while accum.ToolCalls is still empty — so the
// watcher streams it live), THEN the tool call emerges. Round 2 streams the final
// reply, with leading whitespace to exercise the head-trim half of F81.
type preamblePool struct {
	fakePool
	round int
}

func (p *preamblePool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	p.round++
	accum := &contract.StreamAccumulator{}
	if p.round == 1 {
		accum.Text = "我先查一下。"
		_ = w(contract.StreamEvent{Text: "我先查一下。"}, accum) // preamble: no tool call visible yet → streamed live
		calls := []contract.ToolCall{{ID: "c1", Name: "echo", Arguments: "{}"}}
		accum.ToolCalls = calls
		_ = w(contract.StreamEvent{Done: true}, accum)
		return contract.ChatResponse{Content: "我先查一下。", ToolCalls: calls}, nil
	}
	accum.Text = "  最终回复"
	_ = w(contract.StreamEvent{Text: "  最终回复"}, accum)
	_ = w(contract.StreamEvent{Done: true}, accum)
	return contract.ChatResponse{Content: "  最终回复"}, nil
}

// TestRun_FinishExtendsStreamedPreamble (F81): Finish's full must EXTEND the
// ContentDeltas already streamed (contract/50-sink.go) — the tool-round preamble that
// streamed live before the tool call emerged is part of the finish text, and the
// streamed reply is not head-trimmed. A diverging full made an edit-stream sink that
// split mid-stream re-send the whole reply from offset 0 (frozen chunks duplicated).
// The PERSISTED assistant row keeps only the trimmed reply — the preamble is already
// persisted with its own round's assistant-calls row.
func TestRun_FinishExtendsStreamedPreamble(t *testing.T) {
	store := newSubStore()
	set := newTestSet(testTool{name: "echo", run: func(json.RawMessage) contract.ToolResult { return contract.OKResult("x") }})
	c := &core{store: store, pool: &preamblePool{}}
	sink := &recSink{}

	res := c.Run(context.Background(), contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "查一下", Sink: sink, Tools: set})

	if res.Err != nil {
		t.Fatalf("turn errored: %v", res.Err)
	}
	// The Sink contract, verbatim: full == concatenation of the ContentDeltas.
	want := strings.Join(sink.deltas, "")
	if want == "" || !strings.Contains(want, "我先查一下。") {
		t.Fatalf("test wiring: preamble was not streamed, deltas=%v", sink.deltas)
	}
	if sink.finished != want {
		t.Fatalf("Finish(full)=%q must equal the streamed deltas %q (Sink contract)", sink.finished, want)
	}
	// The caller-facing reply stays the bare trimmed reply (no preamble).
	if res.Reply != "最终回复" {
		t.Fatalf("res.Reply=%q, want the trimmed bare reply", res.Reply)
	}
	// The persisted final row stays the trimmed reply — no preamble duplicate
	// (round 1's assistant-calls row already carries it).
	msgs, _ := store.GetMessages(context.Background(), "s1", 0)
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" || last.Content != "最终回复" {
		t.Fatalf("persisted final row=%+v, want assistant(最终回复)", last)
	}
	preambleRows := 0
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.Content, "我先查一下。") {
			preambleRows++
		}
	}
	if preambleRows != 1 {
		t.Fatalf("preamble must be persisted exactly once (its own calls row), got %d rows", preambleRows)
	}
}

// preambleThenBreakPool: round 1 streams a preamble then a tool call; round 2 streams
// a partial and drops with a generic error — the §4.2 stream-break door with an
// earlier preamble in the sink.
type preambleThenBreakPool struct {
	fakePool
	round int
}

func (p *preambleThenBreakPool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	p.round++
	accum := &contract.StreamAccumulator{}
	if p.round == 1 {
		accum.Text = "先跑个工具。"
		_ = w(contract.StreamEvent{Text: "先跑个工具。"}, accum)
		calls := []contract.ToolCall{{ID: "c1", Name: "echo", Arguments: "{}"}}
		accum.ToolCalls = calls
		_ = w(contract.StreamEvent{Done: true}, accum)
		return contract.ChatResponse{Content: "先跑个工具。", ToolCalls: calls}, nil
	}
	accum.Text = "半截答案"
	_ = w(contract.StreamEvent{Text: "半截答案"}, accum)
	return contract.ChatResponse{}, fmt.Errorf("stream dropped mid-flight")
}

// TestRun_StreamBreakFinishExtendsPreamble (F81, the partial door): when a stream
// breaks after visible content AND an earlier round streamed a preamble, the finish
// text must extend ALL streamed bytes (preamble + partial) + the marker; the
// persisted row stays partial + marker only.
func TestRun_StreamBreakFinishExtendsPreamble(t *testing.T) {
	store := newSubStore()
	set := newTestSet(testTool{name: "echo", run: func(json.RawMessage) contract.ToolResult { return contract.OKResult("x") }})
	c := &core{store: store, pool: &preambleThenBreakPool{}}
	sink := &recSink{}

	res := c.Run(context.Background(), contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink, Tools: set})

	if res.Err == nil {
		t.Fatal("want the stream error surfaced")
	}
	marker := i18n.T(streamInterruptedMarkerKey, "default")
	if want := strings.Join(sink.deltas, "") + marker; sink.finished != want {
		t.Fatalf("finished=%q, want streamed deltas + marker %q (Sink contract)", sink.finished, want)
	}
	msgs, _ := store.GetMessages(context.Background(), "s1", 0)
	last := msgs[len(msgs)-1]
	if last.Content != "半截答案"+marker {
		t.Fatalf("persisted row=%q, want partial+marker only (preamble lives on its calls row)", last.Content)
	}
}

// bareSink is a recSink carrying the contract.BareProductSink marker — the shape
// of the agora bridge return sink / email coalesce sink / sub-turn capture sink
// (ContentDelta renders nothing; Finish's full is consumed verbatim as the
// product). It still RECORDS deltas so the tests can prove the core did stream.
type bareSink struct{ recSink }

func (s *bareSink) BareProductFinish() {}

var _ contract.BareProductSink = (*bareSink)(nil)

// TestRun_BareProductSinkGetsBareReply: a sink that declares BareProductSink
// must receive the bare TRIMMED reply in Finish even when a tool-round preamble
// (and the reply itself) went through the watcher — its Finish full IS the
// product (a bridge return event's Text / a coalesced mail segment), so gluing
// the sunk accumulation on would leak the preamble into it. The persisted row
// is the same trimmed reply (unchanged).
func TestRun_BareProductSinkGetsBareReply(t *testing.T) {
	store := newSubStore()
	set := newTestSet(testTool{name: "echo", run: func(json.RawMessage) contract.ToolResult { return contract.OKResult("x") }})
	c := &core{store: store, pool: &preamblePool{}}
	sink := &bareSink{}

	res := c.Run(context.Background(), contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "查一下", Sink: sink, Tools: set})

	if res.Err != nil {
		t.Fatalf("turn errored: %v", res.Err)
	}
	// Wiring check: the preamble DID go through ContentDelta (the core streamed).
	if joined := strings.Join(sink.deltas, ""); !strings.Contains(joined, "我先查一下。") {
		t.Fatalf("test wiring: preamble was not streamed, deltas=%v", sink.deltas)
	}
	if sink.finished != "最终回复" {
		t.Fatalf("Finish(full)=%q, want the bare trimmed reply (no preamble, no head whitespace)", sink.finished)
	}
	msgs, _ := store.GetMessages(context.Background(), "s1", 0)
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" || last.Content != "最终回复" {
		t.Fatalf("persisted final row=%+v, want assistant(最终回复)", last)
	}
}

// TestRun_BareProductSinkStreamBreakKeepsPartialBare (the finalizePartial door):
// when the stream breaks after visible content and an earlier round streamed a
// preamble, a BareProductSink still gets partial+marker only — never the sunk
// composition (preamble + partial + marker) rendering sinks need.
func TestRun_BareProductSinkStreamBreakKeepsPartialBare(t *testing.T) {
	store := newSubStore()
	set := newTestSet(testTool{name: "echo", run: func(json.RawMessage) contract.ToolResult { return contract.OKResult("x") }})
	c := &core{store: store, pool: &preambleThenBreakPool{}}
	sink := &bareSink{}

	res := c.Run(context.Background(), contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink, Tools: set})

	if res.Err == nil {
		t.Fatal("want the stream error surfaced")
	}
	marker := i18n.T(streamInterruptedMarkerKey, "default")
	if want := "半截答案" + marker; sink.finished != want {
		t.Fatalf("finished=%q, want partial+marker only %q (no preamble glued on)", sink.finished, want)
	}
	msgs, _ := store.GetMessages(context.Background(), "s1", 0)
	last := msgs[len(msgs)-1]
	if last.Content != "半截答案"+marker {
		t.Fatalf("persisted row=%q, want partial+marker only", last.Content)
	}
}

// ctxStore embeds subStore but makes AppendMessage honor ctx cancellation (like pgx /
// database-sql), so a test can prove a persist runs on a LIVE token, not the dead turn ctx.
type ctxStore struct{ *subStore }

func (s *ctxStore) AppendMessage(ctx context.Context, sid, role, content string, a contract.MsgAuthor) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return s.subStore.AppendMessage(ctx, sid, role, content, a)
}

// TestFinalizeTurn_DeadCtxPersistsOnFreshToken (B2): a reply-producing door whose turn
// ctx is already dead (e.g. /close-session during the rubric judge) must STILL persist
// the assistant row — finalizeTurn falls back to an independent short-timeout token, so
// the Finished reply and the stored history never diverge (no history hole).
func TestFinalizeTurn_DeadCtxPersistsOnFreshToken(t *testing.T) {
	store := &ctxStore{newSubStore()}
	c := &core{store: store}
	sink := &recSink{}
	dead, cancel := context.WithCancel(context.Background())
	cancel() // the turn ctx is dead before we finalize
	c.finalizeTurn(dead, contract.TurnSpec{Sid: "s1", Sink: sink}, "the answer", "the answer")
	if !sink.done || sink.finished != "the answer" {
		t.Fatalf("the reply must still ship, got done=%v finished=%q", sink.done, sink.finished)
	}
	got := 0
	for _, m := range store.rows {
		if m.Role == "assistant" && m.Content == "the answer" {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("assistant row must be persisted on a fresh token despite the dead turn ctx, got %d", got)
	}
}

// TestRun_CancelReleasesSink (B11): a turn cancelled before it produces a reply must
// still RELEASE the sink with exactly one Finish("") — nothing visible, but an email
// coalescer's per-turn counter is balanced (else its buffered replies wedge forever).
func TestRun_CancelReleasesSink(t *testing.T) {
	c := &core{store: newSubStore(), pool: &fakePool{reply: "hi"}}
	sink := &recSink{}
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	res := c.Run(dead, contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})
	if !sink.done || sink.finished != "" {
		t.Fatalf("a cancelled turn must release the sink with empty content, got done=%v finished=%q", sink.done, sink.finished)
	}
	if res.Err == nil {
		t.Fatal("a cancelled turn returns the cancel error")
	}
}

// TestRun_SilentToolExit: a tool exit with an EMPTY reply (the stay_silent tool) ends
// the turn delivering NOTHING VISIBLE — but it still RELEASES the sink with exactly one
// Finish(""): streamsink skips the empty send, the agora return sink no-ops on empty
// (so a would-be loop still ends here), and an email coalescer balances its per-turn
// counter (else its buffered replies wedge). No persisted line, no salvage, no error.
func TestRun_SilentToolExit(t *testing.T) {
	store := newSubStore()
	set := newTestSet(testTool{name: "stay_silent", run: func(json.RawMessage) contract.ToolResult {
		return contract.ToolResult{OK: true, Data: "silent", ExitRequest: &contract.ToolExit{}}
	}})
	pool := &scriptPool{resps: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "stay_silent", Arguments: "{}"}}},
	}}
	c := &core{store: store, pool: pool}
	sink := &recSink{}
	res := c.Run(context.Background(), contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "谢谢", Sink: sink, Tools: set})

	if !sink.done {
		t.Fatal("silent exit must RELEASE the sink (one terminal Finish) so the coalescer counter is balanced")
	}
	if sink.finished != "" {
		t.Fatalf("silent exit must release with EMPTY content (deliver nothing visible), got %q", sink.finished)
	}
	if res.Reply != "" {
		t.Fatalf("silent exit must return an empty reply, got %q", res.Reply)
	}
	if res.Err != nil {
		t.Fatalf("silent exit is not an error, got %v", res.Err)
	}
}

// TestRun_SideEffectSuccessSupersedes (§6.3.4): a retry of the SAME failed call that
// succeeds clears the failure — no footer.
func TestRun_SideEffectSuccessSupersedes(t *testing.T) {
	store := newSubStore()
	n := 0
	set := newTestSet(testTool{name: "send_message", sideEffect: true, run: func(json.RawMessage) contract.ToolResult {
		n++
		if n == 1 {
			return contract.ErrResult("通道忙")
		}
		return contract.OKResult("已投递")
	}})
	args := `{"to":"X","body":"hi"}`
	pool := &scriptPool{resps: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "send_message", Arguments: args}}}, // fail
		{ToolCalls: []contract.ToolCall{{ID: "c2", Name: "send_message", Arguments: args}}}, // retry SAME → ok
		{Content: "发好了"},
	}}
	c := &core{store: store, pool: pool}
	sink := &recSink{}
	c.Run(context.Background(), contract.TurnSpec{Sid: "s2", Prompt: sysPrompt("sys"), UserText: "发", Sink: sink, Tools: set})

	if strings.Contains(sink.finished, "未成功") {
		t.Fatalf("a same-args retry succeeded → failure superseded, want no footer, got %q", sink.finished)
	}
}

// TestRun_SideEffectFooterOnToolExit (§6.3.4 / review D1): a tool-authored exit reply
// still carries the side-effect failure footer.
func TestRun_SideEffectFooterOnToolExit(t *testing.T) {
	store := newSubStore()
	set := newTestSet(
		testTool{name: "deliver_file", sideEffect: true, run: func(json.RawMessage) contract.ToolResult {
			return contract.ErrResult("disk full")
		}},
		testTool{name: "finish", run: func(json.RawMessage) contract.ToolResult {
			return contract.ToolResult{OK: true, ExitRequest: &contract.ToolExit{Reply: "搞定了"}}
		}},
	)
	pool := &scriptPool{resps: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{
			{ID: "c1", Name: "deliver_file", Arguments: `{"path":"r.pdf"}`},
			{ID: "c2", Name: "finish", Arguments: "{}"},
		}},
	}}
	c := &core{store: store, pool: pool}
	sink := &recSink{}
	c.Run(context.Background(), contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "发", Sink: sink, Tools: set})

	if !strings.HasPrefix(sink.finished, "搞定了") {
		t.Fatalf("exit reply should lead, got %q", sink.finished)
	}
	if !strings.Contains(sink.finished, "deliver_file") || !strings.Contains(sink.finished, "未成功") {
		t.Fatalf("tool-exit reply should carry the side-effect footer, got %q", sink.finished)
	}
}

// TestRun_ToolRound: the model calls a tool, the result is persisted, the next
// round sees it and produces the final reply. History = user / assistant-calls /
// tool-result / assistant-final.
func TestRun_ToolRound(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := uscope("toolround")

	called := false
	set := newTestSet(testTool{name: "echo", run: func(json.RawMessage) contract.ToolResult {
		called = true
		return contract.OKResult("echoed")
	}})
	pool := &scriptPool{resps: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "echo", Arguments: "{}"}}},
		{Content: "done"},
	}}
	c := &core{store: st, pool: pool}
	sink := &recSink{}
	c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink, Tools: set})

	if !called {
		t.Fatal("echo tool was not dispatched")
	}
	if sink.finished != "done" {
		t.Fatalf("finished = %q, want done", sink.finished)
	}
	msgs, err := st.GetMessages(ctx, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("history len = %d, want 4 (user, assistant-calls, tool, assistant): %+v", len(msgs), msgs)
	}
	if msgs[1].Role != "assistant" || len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "echo" {
		t.Errorf("msg[1] not assistant-with-calls: %+v", msgs[1])
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "c1" || msgs[2].ToolName != "echo" || !strings.Contains(msgs[2].Content, "echoed") {
		t.Errorf("msg[2] not the tool result: %+v", msgs[2])
	}
	if msgs[3].Role != "assistant" || msgs[3].Content != "done" {
		t.Errorf("msg[3] not the final reply: %+v", msgs[3])
	}
}

// TestRun_ToolExit: a tool's ExitRequest ends the turn with its reply.
func TestRun_ToolExit(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := uscope("toolexit")

	set := newTestSet(testTool{name: "ask", run: func(json.RawMessage) contract.ToolResult {
		return contract.ToolResult{OK: true, Data: "asked", ExitRequest: &contract.ToolExit{Reply: "what color?"}}
	}})
	pool := &scriptPool{resps: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "ask", Arguments: "{}"}}},
	}}
	c := &core{store: st, pool: pool}
	sink := &recSink{}
	res := c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink, Tools: set})

	if sink.finished != "what color?" || res.Reply != "what color?" {
		t.Fatalf("exit reply finished=%q res=%q, want 'what color?'", sink.finished, res.Reply)
	}
}

// TestRun_ToolPanic: a panicking tool becomes an error result; the loop survives
// and reaches the next round's final reply.
func TestRun_ToolPanic(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := uscope("toolpanic")

	set := newTestSet(testTool{name: "boom", run: func(json.RawMessage) contract.ToolResult {
		panic("kaboom")
	}})
	pool := &scriptPool{resps: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "boom", Arguments: "{}"}}},
		{Content: "recovered"},
	}}
	c := &core{store: st, pool: pool}
	sink := &recSink{}
	c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink, Tools: set})

	if sink.finished != "recovered" {
		t.Fatalf("finished = %q, want recovered (loop survived the panic)", sink.finished)
	}
	msgs, _ := st.GetMessages(ctx, sid, 0)
	if len(msgs) < 3 || msgs[2].Role != "tool" || !strings.Contains(msgs[2].Content, "panic") {
		t.Errorf("panic should yield an error tool result: %+v", msgs)
	}
}

// TestRun_UnknownTool: a call to a tool not in the set yields an error result,
// not a crash; the loop continues.
func TestRun_UnknownTool(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := uscope("toolunknown")

	pool := &scriptPool{resps: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "ghost", Arguments: "{}"}}},
		{Content: "ok"},
	}}
	c := &core{store: st, pool: pool}
	sink := &recSink{}
	c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink, Tools: newTestSet()})

	if sink.finished != "ok" {
		t.Fatalf("finished = %q, want ok", sink.finished)
	}
	msgs, _ := st.GetMessages(ctx, sid, 0)
	if len(msgs) < 3 || msgs[2].Role != "tool" || !strings.Contains(msgs[2].Content, "unknown tool") {
		t.Errorf("unknown tool should yield an error result: %+v", msgs)
	}
}

// splitMarkerPool streams a plain <tool_call> marker SPLIT across two deltas
// ("hello <tool" then "_call>…") with accum.ToolCalls still empty (the pool only
// surfaces the extracted call on the Done event, as the real pool does post-stream).
// Round 2 streams a normal final reply. It exercises B20: the straddling partial-marker
// prefix ("<tool") must be held back, never streamed raw to the sink.
type splitMarkerPool struct {
	fakePool
	round int
}

func (p *splitMarkerPool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	p.round++
	accum := &contract.StreamAccumulator{}
	if p.round == 1 {
		accum.Text = "hello <tool" // trailing "<tool" is a proper prefix of "<tool_call>" → held back
		_ = w(contract.StreamEvent{Text: "hello <tool"}, accum)
		accum.Text = "hello <tool_call>\n{\"name\":\"echo\"}\n</tool_call>" // marker completes
		_ = w(contract.StreamEvent{Text: "_call>\n{\"name\":\"echo\"}\n</tool_call>"}, accum)
		calls := []contract.ToolCall{{ID: "c1", Name: "echo", Arguments: "{}"}} // extracted post-stream
		accum.ToolCalls = calls
		_ = w(contract.StreamEvent{Done: true}, accum)
		return contract.ChatResponse{Content: "hello ", ToolCalls: calls}, nil
	}
	accum.Text = "done"
	_ = w(contract.StreamEvent{Text: "done"}, accum)
	_ = w(contract.StreamEvent{Done: true}, accum)
	return contract.ChatResponse{Content: "done"}, nil
}

// heldTailPool streams a plain reply with LEADING whitespace that ends in a viable
// <tool_call> marker prefix ("<tool") which never completes — so the holdback keeps
// that tail out of the live stream, but at stream end it is genuine content.
type heldTailPool struct {
	fakePool
}

func (p *heldTailPool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	accum := &contract.StreamAccumulator{}
	accum.Text = " answer"
	_ = w(contract.StreamEvent{Text: " answer"}, accum)
	accum.Text = " answer <tool" // trailing "<tool" held back; the marker never completes
	_ = w(contract.StreamEvent{Text: " <tool"}, accum)
	_ = w(contract.StreamEvent{Done: true}, accum)
	return contract.ChatResponse{Content: " answer <tool"}, nil // no tool call materialized
}

// TestRun_HeldTailFlushedAtStreamEnd (B20 follow-up): when a tool round's plain reply
// ends in a never-completing marker prefix, the held tail must be flushed at stream
// end so st.sunk == what the sink received and the byte-exact st.sunk finish path
// stays armed (finish must equal the streamed deltas, not the whitespace-trimmed
// full — otherwise a split-message edit-stream sink duplicates the frozen prefix).
func TestRun_HeldTailFlushedAtStreamEnd(t *testing.T) {
	store := newSubStore()
	set := newTestSet(testTool{name: "echo", run: func(json.RawMessage) contract.ToolResult { return contract.OKResult("x") }})
	c := &core{store: store, pool: &heldTailPool{}}
	sink := &recSink{}

	res := c.Run(context.Background(), contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink, Tools: set})
	if res.Err != nil {
		t.Fatalf("turn errored: %v", res.Err)
	}
	joined := strings.Join(sink.deltas, "")
	// The held tail is genuine content (the marker never completed) — it must reach
	// the sink, and st.sunk (→ Finish's full) must be byte-exact with the deltas.
	if joined != " answer <tool" {
		t.Fatalf("sink content=%q, want %q (leading space + flushed tail preserved)", joined, " answer <tool")
	}
	if sink.finished != joined {
		t.Fatalf("Finish full=%q must equal streamed deltas %q (st.sunk byte-exact; not the trimmed full)", sink.finished, joined)
	}
}

// TestRun_SplitToolMarkerHoldback (B20): when the plain <tool_call> marker straddles
// two stream deltas, the partial-marker prefix must NEVER reach the sink — the F76
// live guard now holds back the longest trailing suffix that is a viable prefix of the
// marker until the next delta disambiguates it. The sink sees only the real content
// before the marker ("hello ") plus the next round's reply, and st.sunk stays byte-
// exact with what the sink received (proven via Finish's full == joined deltas).
func TestRun_SplitToolMarkerHoldback(t *testing.T) {
	store := newSubStore()
	set := newTestSet(testTool{name: "echo", run: func(json.RawMessage) contract.ToolResult { return contract.OKResult("x") }})
	c := &core{store: store, pool: &splitMarkerPool{}}
	sink := &recSink{}

	res := c.Run(context.Background(), contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink, Tools: set})

	if res.Err != nil {
		t.Fatalf("turn errored: %v", res.Err)
	}
	joined := strings.Join(sink.deltas, "")
	// No partial-marker bytes ever streamed: not the full marker, and not the split
	// prefix "<tool" that the first delta carried.
	if strings.Contains(joined, plainToolCallMarker) || strings.Contains(joined, "<tool") {
		t.Fatalf("partial/full tool-call marker leaked to the sink: deltas=%v", sink.deltas)
	}
	// The sink saw exactly the pre-marker content plus round 2's reply.
	if joined != "hello done" {
		t.Fatalf("sink content=%q, want %q (pre-marker content + final reply)", joined, "hello done")
	}
	// Finish's full is built from st.sunk — asserting it equals the joined deltas proves
	// st.sunk stayed byte-exact with what actually reached the sink (Sink contract).
	if sink.finished != joined {
		t.Fatalf("Finish full=%q must equal streamed deltas %q (st.sunk == sink output)", sink.finished, joined)
	}
}

// TestToolMarkerHoldback exercises the B20 holdback boundary directly: the length of
// the trailing suffix of text that is a proper prefix of the marker (0 when none).
func TestToolMarkerHoldback(t *testing.T) {
	const m = plainToolCallMarker // "<tool_call>"
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"hello", 0},
		{"hello <", 1},          // "<" is a prefix of the marker
		{"hello <tool", 5},      // "<tool"
		{"a<tool_call", 10},     // proper prefix, one byte short of complete
		{"<tool_call>", 0},      // COMPLETE marker → not a proper prefix (caller's strings.Index handles it)
		{"<tool_call>extra", 0}, // complete marker in the middle → no viable trailing prefix
		{"foo<t", 2},            // "<t"
		{"foo<tool bar<", 1},    // only the final "<" is a viable prefix (earlier "<tool " was broken by a space)
	}
	for _, tc := range cases {
		if got := toolMarkerHoldback(tc.text, m); got != tc.want {
			t.Errorf("toolMarkerHoldback(%q)=%d, want %d", tc.text, got, tc.want)
		}
	}
}

// TestRepairOrphanToolCalls: an assistant-with-calls missing a result row gets a
// synthetic one spliced in (pure — no pg).
func TestRepairOrphanToolCalls(t *testing.T) {
	in := []contract.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "c1", Name: "x"}, {ID: "c2", Name: "y"}}},
		{Role: "tool", ToolCallID: "c1", ToolName: "x", Content: "ok"},
	}
	out := repairOrphanToolCalls(in)
	// c2 had no result → one synthetic tool message added.
	if len(out) != 4 {
		t.Fatalf("repaired len = %d, want 4: %+v", len(out), out)
	}
	var foundC2 bool
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c2" {
			foundC2 = true
			if !strings.Contains(m.Content, "interrupted") {
				t.Errorf("synthetic c2 result = %q, want an interrupted marker", m.Content)
			}
		}
	}
	if !foundC2 {
		t.Errorf("no synthetic result for orphan call c2: %+v", out)
	}
}

// sentenceBoundary paces the thinking channel. Reasoning arrives one token at a
// time and often carries no newline until it ends, so the flush horizon has to be
// the last SENTENCE end — waiting for a line break would dump the whole thought in
// one burst at stream close, and flushing every delta would print one word per line
// (TraceDelta terminates each call with a line break).
func TestSentenceBoundary(t *testing.T) {
	cases := []struct {
		in   string
		want int
		why  string
	}{
		{"", 0, "empty"},
		{"We need to check", 0, "no complete sentence yet — hold everything back"},
		{"We need to check. And", 17, "cut after the period, keep the trailing fragment"},
		{"one. two! three?", 16, "last boundary wins, not the first"},
		{"line one\nline two", 9, "a newline is a boundary too"},
		{"看一下第三行。然后", 21, "multi-byte terminator: cut AFTER the full rune"},
	}
	for _, c := range cases {
		if got := sentenceBoundary(c.in); got != c.want {
			t.Errorf("sentenceBoundary(%q) = %d, want %d (%s)", c.in, got, c.want, c.why)
		}
		// Whatever it returns must be a valid slice point that never splits a rune.
		if got := sentenceBoundary(c.in); got > 0 && !utf8.ValidString(c.in[:got]) {
			t.Errorf("sentenceBoundary(%q) = %d cuts mid-rune", c.in, got)
		}
	}
}

// The thinking flush horizon must reset on IDENTITY, not length. The pool declares
// a fresh StreamAccumulator per attempt (leaf/model/45-chat-stream-watch.go) while
// runRound's watcher closure is reused across a failover — and ErrThinkingOnly makes
// failover-after-reasoning the EXPECTED path, not an edge. A length test misses the
// restart whenever the replacement thought is already longer than the old horizon,
// and then slices a different string at a stale offset: it eats the new thought's
// head, or cuts mid-rune on CJK.
//
// This exercises the same cursor discipline flushThinking implements, over the shape
// that broke it: attempt 1 flushes a horizon, attempt 2 opens with a LONGER thought.
func TestThinkingCursorResetsOnNewThought(t *testing.T) {
	var out []string
	sent := 0
	prev := ""
	flush := func(all string) {
		if !strings.HasPrefix(all, prev) {
			sent = 0
		}
		prev = all
		b := sentenceBoundary(all[sent:])
		if b == 0 {
			return
		}
		end := sent + b
		chunk := strings.TrimSpace(all[sent:end])
		sent = end
		if chunk != "" {
			out = append(out, chunk)
		}
	}

	// Attempt 1: a leading newline advances the horizon while printing nothing —
	// vLLM's reasoning parser emits exactly this after <think>.
	flush("\n")
	if len(out) != 0 {
		t.Fatalf("leading newline must print nothing, got %v", out)
	}
	// Attempt 1 fails over. Attempt 2's first delta is longer than that horizon and
	// starts with a multi-byte rune — the case the length test waved through.
	flush("我们需要检查第三行。")
	if len(out) != 1 || out[0] != "我们需要检查第三行。" {
		t.Fatalf("out = %q, want the whole second thought intact", out)
	}
	if !utf8.ValidString(strings.Join(out, "")) {
		t.Fatalf("emitted invalid UTF-8: %q", out)
	}
	// A genuine continuation of the SAME thought must not reset — only the new tail
	// is emitted, never a repeat of what was already shown.
	flush("我们需要检查第三行。然后核对第四行。")
	if len(out) != 2 || out[1] != "然后核对第四行。" {
		t.Fatalf("out = %q, want only the new tail appended", out)
	}
}
