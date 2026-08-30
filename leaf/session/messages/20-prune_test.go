package messages

import (
	"context"
	"strconv"
	"testing"
	"time"

	"agentbob/contract"
)

// TestPruneCompacted: only rows that are BOTH before the session's last summary
// marker AND past the age cutoff are pruned; the marker, the replay tail, and
// post-marker rows survive regardless of age, so GetReplay is unaffected.
// Env-gated live-DB test (see liveStore).
func TestPruneCompacted(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := "test_prune_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	human := contract.MsgAuthor{Kind: "human", ID: "u1", Name: "U"}

	// Pre-marker originals: one to be backdated (pruned), one recent (kept).
	if err := st.AppendMessage(ctx, sid, "user", "old-user", human); err != nil {
		t.Fatalf("append old-user: %v", err)
	}
	if err := st.AppendMessage(ctx, sid, "user", "fresh-user", human); err != nil {
		t.Fatalf("append fresh-user: %v", err)
	}
	// Compaction: summary marker + a replay-tail copy.
	if err := st.AppendCompactionBatch(ctx, sid, "the summary", []contract.Message{
		{Role: "user", Content: "fresh-user", Author: human},
	}); err != nil {
		t.Fatalf("compaction: %v", err)
	}
	// Post-marker row: inside the replay window, must survive even when old.
	if err := st.AppendMessage(ctx, sid, "assistant", "post-marker", human); err != nil {
		t.Fatalf("append post-marker: %v", err)
	}
	// Backdate old-user AND post-marker past the cutoff (same package → s.db is
	// reachable; production writes always stamp now()).
	oldTs := now() - 40*24*3600
	if _, err := st.db.Exec(ctx,
		`UPDATE bob_messages SET timestamp = $1 WHERE session_id = $2 AND content IN ('old-user', 'post-marker')`,
		oldTs, sid); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if _, err := st.PruneCompacted(ctx, 30*24*3600); err != nil {
		t.Fatalf("PruneCompacted: %v", err)
	}

	got, err := st.GetMessages(ctx, sid, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	byContent := map[string]contract.Message{}
	for _, m := range got {
		byContent[m.Content] = m
	}
	if _, ok := byContent["old-user"]; ok {
		t.Error("old-user (pre-marker + past cutoff) must be pruned")
	}
	if _, ok := byContent["fresh-user"]; !ok {
		t.Error("fresh-user 'msg' original (pre-marker but recent) must be kept")
	}
	if m, ok := byContent["the summary"]; !ok || m.RowKind != contract.RowKindSummary {
		t.Errorf("summary marker must be kept, got %+v", m)
	}
	if _, ok := byContent["post-marker"]; !ok {
		t.Error("post-marker row must survive the prune even when old (replay window)")
	}
	// The replay window still anchors on the marker and carries the tail whole.
	replay, err := st.GetReplay(ctx, sid, 10)
	if err != nil {
		t.Fatalf("GetReplay: %v", err)
	}
	if len(replay) != 3 || replay[0].RowKind != contract.RowKindSummary {
		t.Fatalf("replay = %d rows (want 3: marker + tail + post-marker), first kind %q", len(replay), replay[0].RowKind)
	}

	// A no-op cutoff prunes nothing.
	if n, err := st.PruneCompacted(ctx, 0); err != nil || n != 0 {
		t.Errorf("PruneCompacted(0) = (%d, %v), want (0, nil)", n, err)
	}
}

// TestImportRewritesReplayToMsg (D10): a cold-archived session exports its pre-compaction
// tail as RowKindReplay rows and deletes the 'msg' originals, so on restore the replay
// rows are the ONLY surviving copy. ImportMessages must rewrite them to RowKindMsg, else
// the webui chat browser (MessagesForScopes filters summary/replay) shows the conversation
// starting abruptly at the last compaction point. Env-gated live-DB test.
func TestImportRewritesReplayToMsg(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := "test_import_d10_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	human := contract.MsgAuthor{Kind: "human", ID: "u1", Name: "U"}
	msgs := []contract.Message{
		{Role: "user", Content: "replay-tail", RowKind: contract.RowKindReplay, Author: human},
		{Role: "assistant", Content: "normal-row", RowKind: contract.RowKindMsg, Author: human},
	}
	if err := st.ImportMessages(ctx, st.db, sid, msgs); err != nil {
		t.Fatalf("ImportMessages: %v", err)
	}
	got, err := st.GetMessages(ctx, sid, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	byContent := map[string]contract.Message{}
	for _, m := range got {
		if m.RowKind == contract.RowKindReplay {
			t.Errorf("a replay row survived import as %q — D10 must rewrite it to msg: %+v", m.RowKind, m)
		}
		byContent[m.Content] = m
	}
	if m, ok := byContent["replay-tail"]; !ok {
		t.Fatal("replay-tail row missing after import")
	} else if m.RowKind != contract.RowKindMsg {
		t.Errorf("replay-tail imported as %q, want msg (so the chat browser shows it)", m.RowKind)
	}
	if m := byContent["normal-row"]; m.RowKind != contract.RowKindMsg {
		t.Errorf("normal-row imported as %q, want msg", m.RowKind)
	}
}

// TestCompactionKeepTail_KeepsAttachments is the F92 twin regression: a bag-bearing
// user row that lands in a compaction's keep-tail is re-inserted as a replay row —
// that replay INSERT must carry the attachments bag, so that once PruneCompacted
// drops the pre-marker original 30d later, RecentAttachments (which scans
// attachments IS NOT NULL) still finds the file through the surviving replay row.
// Before the fix the replay copy had a NULL bag and the files were lost forever.
func TestCompactionKeepTail_KeepsAttachments(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := "test_compact_atts_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	human := contract.MsgAuthor{Kind: "human", ID: "u1", Name: "U"}
	atts := []contract.Attachment{{Kind: "image", MIME: "image/jpeg", FileName: "a.jpg", Path: "inbox/a.jpg", Size: 42}}

	// The original bag-bearing user row (pre-marker; will be pruned once old).
	if err := st.AppendUserMsgs(ctx, sid, []contract.UserMsg{{Author: human, Text: "look", Attachments: atts}}); err != nil {
		t.Fatalf("append user with atts: %v", err)
	}
	// A compaction whose keep-tail RE-INCLUDES that same user row (as GetReplay would
	// hand it to the turn's split — carrying the Atts bag on the contract.Message).
	tail, err := st.GetReplay(ctx, sid, 10)
	if err != nil {
		t.Fatalf("GetReplay: %v", err)
	}
	if len(tail) != 1 || len(tail[0].Atts) != 1 {
		t.Fatalf("GetReplay must carry the attachments bag on Atts, got %+v", tail)
	}
	if err := st.AppendCompactionBatch(ctx, sid, "the summary", tail); err != nil {
		t.Fatalf("compaction: %v", err)
	}

	// Backdate the pre-marker original past the cutoff and prune it — exactly what a
	// 30d sweep does. Only the replay copy (post-marker) then carries the bag.
	oldTs := now() - 40*24*3600
	if _, err := st.db.Exec(ctx,
		`UPDATE bob_messages SET timestamp = $1 WHERE session_id = $2 AND row_kind = 'msg'`,
		oldTs, sid); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := st.PruneCompacted(ctx, 30*24*3600); err != nil {
		t.Fatalf("PruneCompacted: %v", err)
	}

	// The bag must survive on the replay row.
	got, err := st.RecentAttachments(ctx, sid, 0)
	if err != nil {
		t.Fatalf("RecentAttachments: %v", err)
	}
	if len(got) != 1 || got[0].Path != "inbox/a.jpg" || got[0].FileName != "a.jpg" {
		t.Fatalf("post-compaction+prune attachments = %+v, want the bag preserved on the replay row", got)
	}
}
