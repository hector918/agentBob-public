package turn

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/heartwood/prompt"
)

func msg(role, content string) contract.Message {
	return contract.Message{Role: role, Content: content}
}

func TestSplitForCompaction_Basic(t *testing.T) {
	big := strings.Repeat("X", 400) // 100 tokens each (ASCII × 0.25)
	replay := []contract.Message{
		msg("user", big), msg("assistant", big), msg("user", big), msg("assistant", big),
	}
	older, recent, ok := splitForCompaction(replay, 150, prompt.EstTokensMsg)
	if !ok || len(older) != 2 || len(recent) != 2 {
		t.Fatalf("split = older %d / recent %d ok %v, want 2/2/true", len(older), len(recent), ok)
	}
}

func TestSplitForCompaction_KeepsPairIntact(t *testing.T) {
	big := strings.Repeat("X", 400) // 100 tok
	mid := strings.Repeat("R", 200) // 50 tok
	replay := []contract.Message{
		msg("user", big), // 0 — the only summarizable older content
		{Role: "assistant", Content: "calls", ToolCalls: []contract.ToolCall{{ID: "c1", Name: "t"}}}, // 1
		{Role: "tool", Content: mid, ToolCallID: "c1", ToolName: "t"},                                // 2
		msg("user", mid), // 3
	}
	older, recent, ok := splitForCompaction(replay, 60, prompt.EstTokensMsg)
	if !ok {
		t.Fatal("split should succeed")
	}
	// The cut moved back off the tool row so the assistant-calls + its tool stay
	// together in recent; only the leading user row is older.
	if len(older) != 1 || older[0].Role != "user" {
		t.Fatalf("older = %+v, want [user]", older)
	}
	if len(recent) == 0 || len(recent[0].ToolCalls) == 0 {
		t.Fatalf("recent must START with the assistant-calls row, got %+v", recent)
	}
}

// TestGetReplay_AnchorsBeyondWindow (dev-pg) is the BUG-1 regression: a summary
// marker followed by MORE recent rows than a fixed read window must still be the
// replay start (anchored in SQL, not dropped by a row cap).
func TestGetReplay_AnchorsBeyondWindow(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := uscope("turnreplayanchor")

	for i := 0; i < 20; i++ { // pre-summary rows
		_ = st.AppendMessage(ctx, sid, "user", "old", contract.MsgAuthor{})
	}
	_ = st.AppendCompactionBatch(ctx, sid, summaryPrefix+"\nSUMMARY", nil)
	for i := 0; i < 30; i++ { // recent tail AFTER the summary
		_ = st.AppendMessage(ctx, sid, "user", "recent", contract.MsgAuthor{})
	}

	// A tiny cap would, without anchoring, return a window that misses the summary.
	replay, err := st.GetReplay(ctx, sid, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) == 0 || replay[0].RowKind != contract.RowKindSummary {
		t.Fatalf("GetReplay must anchor on the summary (got first row %+v, len %d)", replay[0], len(replay))
	}
	if len(replay) != 31 { // summary + 30 recent
		t.Errorf("replay len = %d, want 31 (summary + recent tail)", len(replay))
	}
}

// TestSplitForCompaction_SingleUserRowSplits (实弹回归): a looping
// research turn — ONE user instruction at the head, then nothing but
// assistant-calls/tool rows — must split. The old "recent must hold ≥1 user
// row" rule pulled the cut back to row 0 on exactly this shape (the only user
// row is the head), yielding older=∅ → permanently uncompactable → the honest
// too-long notice on a perfectly compactable history. The rule is gone: the
// summary row itself is the user message the post-compaction window opens with.
func TestSplitForCompaction_SingleUserRowSplits(t *testing.T) {
	big := strings.Repeat("X", 400) // 100 est tokens each
	replay := []contract.Message{msg("user", "任务书")}
	for i := 0; i < 6; i++ {
		replay = append(replay,
			contract.Message{Role: "assistant", Content: "", ToolCalls: []contract.ToolCall{{ID: "c", Name: "t", Arguments: "{}"}}},
			contract.Message{Role: "tool", Content: big, ToolCallID: "c", ToolName: "t"},
		)
	}
	older, recent, ok := splitForCompaction(replay, 150, prompt.EstTokensMsg)
	if !ok || len(older) == 0 || len(recent) == 0 {
		t.Fatalf("single-user-row history must split: older %d recent %d ok %v", len(older), len(recent), ok)
	}
	// The pair rule still holds: recent never starts with an orphan tool row.
	if recent[0].Role == "tool" {
		t.Fatalf("cut landed inside a calls/result pair: recent starts with %+v", recent[0])
	}
}

// compactPool reports a context budget and returns a canned summary on Chat.
type compactPool struct {
	*fakePool
	budget  int
	summary string
}

// WindowFor is the sizing seam budgetFor asserts (windowSizer) — the fake
// reports one flat window for every request.
func (p compactPool) WindowFor(contract.ModelRequest) int { return p.budget }
func (p compactPool) Chat(context.Context, contract.ModelRequest, []contract.Message) (contract.ChatResponse, error) {
	return contract.ChatResponse{Content: p.summary}, nil
}

// TestCompaction_Roundtrip (dev-pg): a session whose replay exceeds the budget gets
// compacted in place — a summary marker row is written, the recent tail re-appended,
// and the replay (from the marker) carries the summary instead of the old rows.
func TestCompaction_Roundtrip(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := uscope("turncompact")

	// Seed a history that estimates well over a tiny budget.
	big := strings.Repeat("X", 400) // 100 tok each
	for i := 0; i < 6; i++ {
		if err := st.AppendMessage(ctx, sid, "user", big, contract.MsgAuthor{}); err != nil {
			t.Fatal(err)
		}
		if err := st.AppendMessage(ctx, sid, "assistant", big, contract.MsgAuthor{}); err != nil {
			t.Fatal(err)
		}
	}

	c := &core{store: st, pool: compactPool{fakePool: &fakePool{}, budget: 200, summary: "早期对话摘要"}}
	c.maybeCompressInPlace(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys")})

	all, err := st.GetMessages(ctx, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	// GetReplay anchors on the summary marker — it must lead and be shorter than all.
	replay, err := st.GetReplay(ctx, sid, historyReplayCap)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) == 0 || replay[0].RowKind != contract.RowKindSummary {
		t.Fatalf("replay must start at a summary marker, got %+v", replay)
	}
	if replay[0].Role != "user" {
		t.Errorf("summary row role = %q, want user (the post-compaction window must open with a user message)", replay[0].Role)
	}
	if !strings.Contains(replay[0].Content, "早期对话摘要") || !strings.Contains(replay[0].Content, summaryPrefix) {
		t.Errorf("summary row content = %q, want the prefix + summary", replay[0].Content)
	}
	if len(replay) >= len(all) {
		t.Error("compacted replay should be shorter than the full history")
	}
}

// manifestStore is a MessageStore whose RecentAttachments returns a fixed set, for
// testing attachmentManifest (everything else delegates to the in-memory subStore).
type manifestStore struct {
	*subStore
	atts []contract.Attachment
}

func (m manifestStore) RecentAttachments(context.Context, string, int) ([]contract.Attachment, error) {
	return m.atts, nil
}

func TestAttachmentManifest(t *testing.T) {
	ctx := context.Background()
	// no files → empty (the summary is just the LLM text)
	if got := attachmentManifest(ctx, manifestStore{subStore: newSubStore()}, "s"); got != "" {
		t.Errorf("no files → %q, want empty", got)
	}
	// files → a [本会话文件] block listing the space paths; empty Path skipped
	st := manifestStore{subStore: newSubStore(), atts: []contract.Attachment{
		{Path: "inbox/a.jpg"}, {Path: "inbox/b.pdf"}, {Path: ""},
	}}
	got := attachmentManifest(ctx, st, "s")
	for _, want := range []string{"[本会话文件]", "inbox/a.jpg", "inbox/b.pdf"} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "- \n") || strings.HasSuffix(got, "- ") {
		t.Errorf("empty path must be skipped: %q", got)
	}
}

// TestSplitForCompaction_KeepCapRegression (实弹): keepTokens is a
// real-token target, but on the estimator-fallback path the walk measures est
// units — a dense (mojibake) history can measure entirely under the target, so
// the walk consumes every row and abandons (cut==0) even though the model just
// 400'd this very replay. compactHistory caps keep at total/3 (scale-free) so
// the split always lands strictly inside; this pins both halves of that story.
func TestSplitForCompaction_KeepCapRegression(t *testing.T) {
	big := strings.Repeat("X", 400) // 100 est tokens each
	replay := []contract.Message{
		msg("user", big), msg("assistant", big), msg("user", big), msg("assistant", big),
	}
	// keep ≥ total(400) → the old bail: everything is "recent", nothing to summarize.
	if _, _, ok := splitForCompaction(replay, 500, prompt.EstTokensMsg); ok {
		t.Fatal("keep ≥ total must abandon — the trap the total/3 cap exists for")
	}
	// The capped keep (total/3 ≈ 133) lands inside: real progress.
	older, recent, ok := splitForCompaction(replay, 133, prompt.EstTokensMsg)
	if !ok || len(older) == 0 || len(recent) == 0 {
		t.Fatalf("capped keep must split: older %d recent %d ok %v", len(older), len(recent), ok)
	}
}

// TestCompactHistory_NoProgressAbandons — the parroting net: a compress model
// that echoes its input (output no smaller than input) must abandon the round
// instead of landing a summary row that the next loop-top would re-compress
// forever, one wasted call per loop-top.
func TestCompactHistory_NoProgressAbandons(t *testing.T) {
	ctx := context.Background()
	store := newSubStore()
	big := strings.Repeat("X", 400)
	for i := 0; i < 6; i++ {
		_ = store.AppendMessage(ctx, "s", "user", big, contract.MsgAuthor{})
		_ = store.AppendMessage(ctx, "s", "assistant", big, contract.MsgAuthor{})
	}
	// The "summary" is far larger than any older slice this history can produce.
	parrot := compactPool{fakePool: &fakePool{}, budget: 200, summary: strings.Repeat("Y", 8000)}
	c := &core{store: store, pool: parrot}
	replay, _ := store.GetReplay(ctx, "s", historyReplayCap)
	if c.compactHistory(ctx, contract.TurnSpec{Sid: "s"}, replay, 200/3) {
		t.Fatal("a no-shrink pass must abandon (return false)")
	}
	all, _ := store.GetMessages(ctx, "s", 0)
	for _, m := range all {
		if m.RowKind == contract.RowKindSummary {
			t.Fatal("no summary row may land on a no-progress pass")
		}
	}
}

// TestContextGauge pins the /session gauge lifecycle: floor and budget are
// independent halves — compaction zeroes the MEAL reading (stale window) but
// keeps the MOUTH (budget describes the winner, not the history).
func TestContextGauge(t *testing.T) {
	c := &core{}
	if toks, budget, rows := c.ContextGauge("s"); toks != 0 || budget != 0 || rows != 0 {
		t.Fatalf("fresh gauge = %d/%d/%d, want 0/0/0 (unknown)", toks, budget, rows)
	}
	c.recordPromptFloor("s", 90616)
	c.recordCtxBudget("s", 131072)
	c.recordCtxRows("s", 37)
	if toks, budget, rows := c.ContextGauge("s"); toks != 90616 || budget != 131072 || rows != 37 {
		t.Fatalf("gauge = %d/%d/%d, want 90616/131072/37", toks, budget, rows)
	}
	// Compaction clears the meal, keeps the mouth. rows is refreshed by the next
	// buildMessages, not cleared here.
	c.clearPromptFloor("s")
	if toks, budget, _ := c.ContextGauge("s"); toks != 0 || budget != 131072 {
		t.Fatalf("post-compaction gauge = %d/%d, want 0/131072", toks, budget)
	}
	// lastPromptFloor (the trigger's floor) reads the same tokens half.
	c.recordPromptFloor("s", 42)
	if c.lastPromptFloor("s") != 42 {
		t.Fatal("lastPromptFloor must read the gauge's tokens half")
	}
}
