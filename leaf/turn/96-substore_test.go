package turn

import (
	"context"
	"testing"

	"agentbob/contract"
)

// TestSubStore_ReplayAnchorsOnSummary: the in-memory sub-turn store must reproduce
// PG's §6.9 replay semantics — GetReplay returns from the LAST summary marker when
// one exists, else the last cap rows. This is the invariant that lets the shared
// round loop run identically on a sub-turn.
func TestSubStore_ReplayAnchorsOnSummary(t *testing.T) {
	ctx := context.Background()
	s := newSubStore()

	// No summary yet → GetReplay falls back to the last `cap` rows.
	for _, txt := range []string{"u1", "a1", "u2", "a2"} {
		if err := s.AppendMessage(ctx, "sid", "user", txt, contract.MsgAuthor{}); err != nil {
			t.Fatal(err)
		}
	}
	rep, _ := s.GetReplay(ctx, "sid", 2)
	if len(rep) != 2 || rep[0].Content != "u2" || rep[1].Content != "a2" {
		t.Fatalf("pre-summary replay = %v, want last 2 [u2 a2]", rep)
	}

	// After a compaction, GetReplay anchors on the marker and returns the whole tail
	// regardless of cap.
	if err := s.AppendCompactionBatch(ctx, "sid", "SUMMARY", []contract.Message{
		{Role: "user", Content: "recent1"},
		{Role: "assistant", Content: "recent2"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(ctx, "sid", "user", "after", contract.MsgAuthor{}); err != nil {
		t.Fatal(err)
	}
	rep, _ = s.GetReplay(ctx, "sid", 1) // cap=1 must be ignored once a summary exists
	if len(rep) != 4 {
		t.Fatalf("post-summary replay len = %d, want 4 (marker + 2 tail + 1 after)", len(rep))
	}
	if rep[0].RowKind != contract.RowKindSummary || rep[0].Content != "SUMMARY" {
		t.Fatalf("replay[0] should be the summary marker, got %+v", rep[0])
	}
	if rep[3].Content != "after" {
		t.Fatalf("replay tail should include the post-compaction row, got %+v", rep[3])
	}
}

// TestSubStore_ToolShapeRoundTrip: assistant-with-calls and tool-result rows keep
// their pairing fields through append → read.
func TestSubStore_ToolShapeRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newSubStore()
	calls := []contract.ToolCall{{ID: "c1", Name: "ocr"}}
	if err := s.AppendAssistantCalls(ctx, "sid", "", calls, contract.MsgAuthor{}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendToolResult(ctx, "sid", "c1", "ocr", "text"); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.GetMessages(ctx, "sid", 0)
	if len(msgs) != 2 {
		t.Fatalf("got %d msgs, want 2", len(msgs))
	}
	if len(msgs[0].ToolCalls) != 1 || msgs[0].ToolCalls[0].ID != "c1" {
		t.Fatalf("assistant calls not preserved: %+v", msgs[0])
	}
	if msgs[1].ToolCallID != "c1" || msgs[1].ToolName != "ocr" {
		t.Fatalf("tool result pairing not preserved: %+v", msgs[1])
	}
}

// TestSubStore_Delete clears all rows.
func TestSubStore_Delete(t *testing.T) {
	ctx := context.Background()
	s := newSubStore()
	_ = s.AppendMessage(ctx, "sid", "user", "x", contract.MsgAuthor{})
	if err := s.DeleteSessionMessages(ctx, "sid"); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.GetMessages(ctx, "sid", 0)
	if len(msgs) != 0 {
		t.Fatalf("after delete got %d msgs, want 0", len(msgs))
	}
}
