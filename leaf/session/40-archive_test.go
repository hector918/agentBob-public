package session_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/pgpool"
	"agentbob/leaf/session"
	"agentbob/leaf/session/messages"
	"agentbob/leaf/session/store"
	"agentbob/trunk"
)

// liveArchiver opens migrated session + messages stores and an Archiver over them.
// Env-gated: set PGPOOL_TEST_DSN to run (skipped otherwise).
func liveArchiver(t *testing.T) (*session.Archiver, *store.PG, *messages.PG, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run session archive integration tests")
	}
	ctx := context.Background()
	reg := trunk.NewRegistry()
	pm := pgpool.New(dsn)
	if err := pm.Start(ctx, reg); err != nil {
		t.Fatalf("pgpool start: %v", err)
	}
	db := trunk.Require[contract.DB](reg)
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate session: %v", err)
	}
	if err := messages.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate messages: %v", err)
	}
	st := store.NewPG(db)
	ms := messages.NewPG(db)
	return session.NewArchiver(db, st, ms), st, ms, func() { _ = pm.Stop(ctx) }
}

func ascope(tag string) string {
	return "test:archive:" + tag + ":" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// TestArchiveCold_RoundTrip: a cold (was-alive) session is archived — its message
// rows leave bob_messages, an archive row appears, and the hot session row is gone.
func TestArchiveCold_RoundTrip(t *testing.T) {
	a, st, ms, done := liveArchiver(t)
	defer done()
	ctx := context.Background()

	scope := ascope("cold")
	sid, err := st.OpenSession(ctx, scope, "telegram", "u1", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.AppendMessage(ctx, sid, "user", "hello", contract.MsgAuthor{}); err != nil {
		t.Fatal(err)
	}
	if err := ms.AppendMessage(ctx, sid, "assistant", "hi there", contract.MsgAuthor{}); err != nil {
		t.Fatal(err)
	}

	// cutoff in the far future → everything is cold.
	cutoff := clock.UnixSeconds() + 3600
	n, err := a.ArchiveCold(ctx, cutoff)
	if err != nil {
		t.Fatalf("ArchiveCold: %v", err)
	}
	if n < 1 {
		t.Fatalf("ArchiveCold archived %d, want >= 1", n)
	}

	// hot session row gone.
	if _, err := st.SessionByID(ctx, sid); err == nil {
		t.Fatalf("session %s still live after archive", sid)
	}
	// message rows gone.
	if msgs, err := ms.GetMessages(ctx, sid, 0); err != nil {
		t.Fatal(err)
	} else if len(msgs) != 0 {
		t.Fatalf("messages remain after archive: %d", len(msgs))
	}
	// archive row present + revivable (was alive).
	if _, wasAlive, ok, err := st.ArchivedSession(ctx, sid); err != nil {
		t.Fatal(err)
	} else if !ok || !wasAlive {
		t.Fatalf("ArchivedSession ok=%v wasAlive=%v, want true/true", ok, wasAlive)
	}
}

// F92: the cold-archive → restore round-trip must carry the per-row attachments
// bag — a revived session's RecentAttachments used to come back empty because
// the export (contract.Message) never carried the column and the import wrote NULL.
func TestArchiveRestore_KeepsAttachments(t *testing.T) {
	a, st, ms, done := liveArchiver(t)
	defer done()
	ctx := context.Background()

	scope := ascope("atts")
	sid, err := st.OpenSession(ctx, scope, "telegram", "u1", "m")
	if err != nil {
		t.Fatal(err)
	}
	atts := []contract.Attachment{{Kind: "image", MIME: "image/jpeg", FileName: "a.jpg", Path: "inbox/a.jpg", Size: 42}}
	if err := ms.AppendUserMsgs(ctx, sid, []contract.UserMsg{{Text: "look", Attachments: atts}}); err != nil {
		t.Fatal(err)
	}
	if err := ms.AppendMessage(ctx, sid, "assistant", "nice", contract.MsgAuthor{}); err != nil {
		t.Fatal(err)
	}

	cutoff := clock.UnixSeconds() + 3600
	if _, err := a.ArchiveCold(ctx, cutoff); err != nil {
		t.Fatalf("ArchiveCold: %v", err)
	}
	if !a.Restore(ctx, sid) {
		t.Fatal("Restore = false, want true")
	}
	got, err := ms.RecentAttachments(ctx, sid, 0)
	if err != nil {
		t.Fatalf("RecentAttachments: %v", err)
	}
	if len(got) != 1 || got[0].Path != "inbox/a.jpg" || got[0].FileName != "a.jpg" {
		t.Fatalf("restored attachments = %+v, want the archived bag", got)
	}
}

// TestArchiveCold_SkipsInFlight (D18): a sid the Manager reports in-flight (busy or
// closing) is NOT archived — the sweep leaves it so it can't delete a live (or
// just-revived) turn's row + transcript out from under it.
func TestArchiveCold_SkipsInFlight(t *testing.T) {
	a, st, ms, done := liveArchiver(t)
	defer done()
	ctx := context.Background()

	scope := ascope("inflight")
	sid, err := st.OpenSession(ctx, scope, "telegram", "u1", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.AppendMessage(ctx, sid, "user", "busy", contract.MsgAuthor{}); err != nil {
		t.Fatal(err)
	}

	a.SetInFlight(func(s string) bool { return s == sid }) // this sid has a live turn

	cutoff := clock.UnixSeconds() + 3600 // cold by time, but in flight
	if _, err := a.ArchiveCold(ctx, cutoff); err != nil {
		t.Fatalf("ArchiveCold: %v", err)
	}

	// the in-flight sid must still be live (not archived) and have no archive row.
	if _, err := st.SessionByID(ctx, sid); err != nil {
		t.Fatalf("in-flight session %s was archived despite being in flight: %v", sid, err)
	}
	if _, _, ok, err := st.ArchivedSession(ctx, sid); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("in-flight sid %s should NOT have an archive row", sid)
	}
}

// TestRestore_WasAliveVsWasEnded: a was-alive archive restores session+messages;
// a was-ended one does NOT restore.
func TestRestore_WasAliveVsWasEnded(t *testing.T) {
	a, st, ms, done := liveArchiver(t)
	defer done()
	ctx := context.Background()
	cutoff := clock.UnixSeconds() + 3600

	// --- was-alive: archive then restore brings session + messages back. ---
	scopeA := ascope("alive")
	aliveSid, err := st.OpenSession(ctx, scopeA, "telegram", "u1", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.AppendMessage(ctx, aliveSid, "user", "remember this", contract.MsgAuthor{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ArchiveCold(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	if !a.Restore(ctx, aliveSid) {
		t.Fatalf("Restore(was-alive) = false, want true")
	}
	if si, err := st.SessionByID(ctx, aliveSid); err != nil {
		t.Fatalf("restored session missing: %v", err)
	} else if si.EndedAt != 0 {
		t.Fatalf("restored session ended_at = %v, want alive", si.EndedAt)
	}
	if msgs, err := ms.GetMessages(ctx, aliveSid, 0); err != nil {
		t.Fatal(err)
	} else if len(msgs) != 1 || msgs[0].Content != "remember this" {
		t.Fatalf("restored messages = %+v, want the 1 archived row", msgs)
	}
	// archive row consumed by restore.
	if _, _, ok, err := st.ArchivedSession(ctx, aliveSid); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("archive row still present after restore")
	}

	// --- was-ended: ended before archival → not revivable. ---
	scopeE := ascope("ended")
	endedSid, err := st.OpenSession(ctx, scopeE, "telegram", "u2", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.AppendMessage(ctx, endedSid, "user", "old", contract.MsgAuthor{}); err != nil {
		t.Fatal(err)
	}
	if err := st.EndSession(ctx, endedSid, "closed"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ArchiveCold(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	if a.Restore(ctx, endedSid) {
		t.Fatalf("Restore(was-ended) = true, want false (not revivable)")
	}
	// no live session was recreated.
	if _, err := st.SessionByID(ctx, endedSid); err == nil {
		t.Fatalf("was-ended session %s should not be live after declined restore", endedSid)
	}
}
