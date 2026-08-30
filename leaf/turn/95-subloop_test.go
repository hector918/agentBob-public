package turn

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentbob/contract"
)

func TestNarrowToSuite(t *testing.T) {
	parent := newTestSet(
		testTool{name: "file_storage"}, testTool{name: "terminal"}, testTool{name: "now"},
	)
	out := narrowToSuite(parent, []string{"file_storage", "terminal", "missing"})
	if len(out.Specs()) != 2 {
		t.Fatalf("suite size = %d, want 2 (intersection with authorized)", len(out.Specs()))
	}
	if _, ok := out.Lookup("now"); ok {
		t.Error("'now' is outside the suite — must not be reachable")
	}
	if _, ok := out.Lookup("missing"); ok {
		t.Error("a suite name the parent isn't authorized for must be absent")
	}
}

// A turn-ending PROCESS tool (EndsTurn) is dropped from a sub's bag even when the
// suite names it: a channel-less sub can't ask/hand off and must return a product
// (L-D2 turn). The gate reads the self-declared flag, not a hard-coded name.
func TestNarrowToSuite_DropsEndsTurnTools(t *testing.T) {
	parent := newTestSet(
		testTool{name: "file_storage"},
		testTool{name: "clarify", endsTurn: true},
		testTool{name: "stay_silent", endsTurn: true},
	)
	out := narrowToSuite(parent, []string{"file_storage", "clarify", "stay_silent"})
	if len(out.Specs()) != 1 {
		t.Fatalf("suite size = %d, want 1 (EndsTurn tools dropped)", len(out.Specs()))
	}
	if _, ok := out.Lookup("clarify"); ok {
		t.Error("clarify (EndsTurn) must be excluded from a sub-turn")
	}
	if _, ok := out.Lookup("stay_silent"); ok {
		t.Error("stay_silent (EndsTurn) must be excluded from a sub-turn")
	}
	if _, ok := out.Lookup("file_storage"); !ok {
		t.Error("an ordinary tool in the suite must remain")
	}
}

func TestSubRunner_Depth1Refuses(t *testing.T) {
	r := subRunner{depth1: true}
	res := r.RunSub(context.Background(), contract.SubSpec{Task: "x"})
	if res.Err == nil {
		t.Fatal("a sub-turn's runner must refuse to delegate further (depth 1)")
	}
}

func TestCaptureSink(t *testing.T) {
	s := &captureSink{}
	s.ContentDelta("partial")
	s.TraceDelta("trace")
	_ = s.Finish("the product")
	if s.final != "the product" {
		t.Errorf("captureSink.final = %q, want the product", s.final)
	}
}

// spawnTool calls the sub-runner so the integration test can drive a real sub-turn
// through dispatchOne (which wires ToolContext.Sub).
type spawnTool struct{ got *contract.SubResult }

func (spawnTool) Spec() contract.ToolSpec { return contract.ToolSpec{Name: "spawn"} }
func (spawnTool) Serialize() bool         { return true }
func (s spawnTool) Run(ctx context.Context, tc contract.ToolContext, _ json.RawMessage) contract.ToolResult {
	res := tc.Sub.RunSub(ctx, contract.SubSpec{Task: "do the sub work", Suite: nil})
	*s.got = res
	if res.Err != nil {
		return contract.ErrResult(res.Err.Error())
	}
	return contract.OKResult(res.Product)
}

// TestSubLoop_Integration (dev-pg): a parent turn delegates; the sub-turn runs on a
// CHILD sid, produces a product, and the parent gets it back as the tool result.
func TestSubLoop_Integration(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := uscope("turnsub")

	var got contract.SubResult
	set := newTestSet(spawnTool{got: &got})
	// Sequence served to the SHARED pool: parent calls spawn → sub gives the product
	// → parent gives its final reply.
	pool := &scriptPool{resps: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "d1", Name: "spawn", Arguments: "{}"}}},
		{Content: "子产出：完成"}, // the sub-turn's final
		{Content: "父答复"},    // the parent's final
	}}
	c := &core{store: st, pool: pool}
	sink := &recSink{}
	res := c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink, Tools: set})

	if got.Product != "子产出：完成" || got.Partial {
		t.Fatalf("sub product = %q partial=%v, want clean 子产出", got.Product, got.Partial)
	}
	if res.Reply != "父答复" {
		t.Fatalf("parent reply = %q, want 父答复", res.Reply)
	}
	// The sub ran on a child sid and persisted its own history — never delivered.
	subRows, _ := st.GetMessages(ctx, sid+":sub:1", 0)
	if len(subRows) == 0 {
		t.Error("sub-turn should have persisted history under the child sid")
	}
	if strings.Contains(sink.finished, "子产出") {
		t.Error("the sub product must not be delivered to the parent sink directly")
	}
}
