package store_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/pgpool"
	"agentbob/leaf/session/store"
	"agentbob/trunk"
)

// liveStore opens a migrated session store against the test database. Env-gated:
// set PGPOOL_TEST_DSN to run (skipped otherwise).
func liveStore(t *testing.T) (*store.PG, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run session store integration tests")
	}
	ctx := context.Background()
	reg := trunk.NewRegistry()
	m := pgpool.New(dsn)
	if err := m.Start(ctx, reg); err != nil {
		t.Fatalf("pgpool start: %v", err)
	}
	db := trunk.Require[contract.DB](reg)
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store.NewPG(db), func() { _ = m.Stop(ctx) }
}

// uniqueScope isolates a test's rows from prior runs (tables persist).
func uniqueScope(tag string) string {
	return "test:" + tag + ":" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func TestSessions_Lifecycle(t *testing.T) {
	s, done := liveStore(t)
	defer done()
	ctx := context.Background()
	scope := uniqueScope("lifecycle")

	// Ensure creates; Ensure again returns the same active sid.
	sid1, isNew, err := s.EnsureSession(ctx, scope, "telegram", "u1", "m")
	if err != nil || !isNew || sid1 == "" {
		t.Fatalf("Ensure(1) = %q,%v,%v", sid1, isNew, err)
	}
	if got, isNew2, _ := s.EnsureSession(ctx, scope, "telegram", "u1", "m"); got != sid1 || isNew2 {
		t.Fatalf("Ensure(2) = %q,%v, want %q,false", got, isNew2, sid1)
	}
	if cur, ok, _ := s.CurrentSession(ctx, scope); !ok || cur != sid1 {
		t.Fatalf("CurrentSession = %q,%v, want %q", cur, ok, sid1)
	}

	// SessionByID + title/model mutations.
	if err := s.SetTitle(ctx, sid1, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionModel(ctx, sid1, "m2"); err != nil {
		t.Fatal(err)
	}
	si, err := s.SessionByID(ctx, sid1)
	if err != nil || si.Title != "hello" || si.Model != "m2" || si.Key != scope {
		t.Fatalf("SessionByID = %+v, %v", si, err)
	}

	// Open a second session; SetCursor flips the active election.
	sid2, err := s.OpenSession(ctx, scope, "telegram", "u1", "m")
	if err != nil || sid2 == sid1 {
		t.Fatalf("Open = %q,%v", sid2, err)
	}
	if err := s.SetCursor(ctx, sid2); err != nil {
		t.Fatal(err)
	}
	if cur, _, _ := s.CurrentSession(ctx, scope); cur != sid2 {
		t.Fatalf("after SetCursor: active = %q, want %q", cur, sid2)
	}
	if alive, _ := s.AliveSessionsByKey(ctx, scope); len(alive) != 2 {
		t.Fatalf("AliveSessionsByKey = %d, want 2", len(alive))
	}

	// End sid2 → active falls back to sid1.
	if err := s.EndSession(ctx, sid2, "test"); err != nil {
		t.Fatal(err)
	}
	if cur, _, _ := s.CurrentSession(ctx, scope); cur != sid1 {
		t.Fatalf("after End(sid2): active = %q, want %q", cur, sid1)
	}

	// SessionByID on an unknown id → contract.ErrNoRows.
	if _, err := s.SessionByID(ctx, "nope"); err == nil {
		t.Fatal("SessionByID(unknown) should error")
	}
}

// MarkSessionAdmin is promote-only + idempotent, and SessionByID reports the bit.
func TestMarkSessionAdmin(t *testing.T) {
	s, done := liveStore(t)
	defer done()
	ctx := context.Background()
	scope := uniqueScope("adminowned")

	sid, err := s.OpenSession(ctx, scope, "telegram", "u1", "")
	if err != nil {
		t.Fatal(err)
	}
	if si, _ := s.SessionByID(ctx, sid); si.AdminOwned {
		t.Fatal("fresh session must default AdminOwned=false")
	}
	if err := s.MarkSessionAdmin(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if si, _ := s.SessionByID(ctx, sid); !si.AdminOwned {
		t.Fatal("after MarkSessionAdmin, AdminOwned must be true")
	}
	// Idempotent: a second mark is a no-op (still true, no error).
	if err := s.MarkSessionAdmin(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if si, _ := s.SessionByID(ctx, sid); !si.AdminOwned {
		t.Fatal("AdminOwned must stay true after a repeat mark")
	}
}

func TestSessions_NewGenerationAndCaller(t *testing.T) {
	s, done := liveStore(t)
	defer done()
	ctx := context.Background()

	// NewGeneration ends the active session and opens a fresh one.
	scope := uniqueScope("newgen")
	old, _, _ := s.EnsureSession(ctx, scope, "telegram", "u1", "m")
	fresh, err := s.NewGeneration(ctx, scope, "", "", "m")
	if err != nil || fresh == old {
		t.Fatalf("NewGeneration = %q,%v (old %q)", fresh, err, old)
	}
	oldInfo, _ := s.SessionByID(ctx, old)
	if oldInfo.EndedAt == 0 {
		t.Fatal("NewGeneration must end the old session")
	}
	if cur, _, _ := s.CurrentSession(ctx, scope); cur != fresh {
		t.Fatalf("active after NewGeneration = %q, want %q", cur, fresh)
	}

	// OpenSessionWithCaller stamps caller_session_id (the auto-return lineage edge).
	wscope := uniqueScope("worker")
	worker, err := s.OpenSessionWithCaller(ctx, wscope, "internal", "", "m", fresh)
	if err != nil {
		t.Fatal(err)
	}
	winfo, err := s.SessionByID(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if winfo.CallerID != fresh {
		t.Fatalf("worker CallerID = %q, want %q", winfo.CallerID, fresh)
	}
}

func TestPending_Queue(t *testing.T) {
	s, done := liveStore(t)
	defer done()
	ctx := context.Background()
	scope := uniqueScope("pending")

	if err := s.AddPending(ctx, scope, "telegram", "c1", "", "m1", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPending(ctx, scope, "telegram", "c1", "", "m2", map[string]any{"media_group_id": "g"}); err != nil {
		t.Fatal(err)
	}
	chats, err := s.ListPendingChats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mine *store.PendingChat
	for i := range chats {
		if chats[i].ChatID == "c1" && chats[i].Source == "telegram" {
			mine = &chats[i]
		}
	}
	if mine == nil || len(mine.MsgIDs) != 2 {
		t.Fatalf("ListPendingChats: chat not found or wrong msg count: %+v", chats)
	}

	if err := s.RemovePending(ctx, scope, []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePendingForChat(ctx, "telegram", "c1", "", []string{"m2"}); err != nil {
		t.Fatal(err)
	}
	// both gone now — a stale-clear of everything removes 0 (already empty for this scope).
	chats, _ = s.ListPendingChats(ctx)
	for _, c := range chats {
		if c.ChatID == "c1" && c.Source == "telegram" {
			t.Fatalf("pending rows for c1 should be gone: %+v", c)
		}
	}
}

func TestMessageIndex_ReplyRouting(t *testing.T) {
	s, done := liveStore(t)
	defer done()
	ctx := context.Background()
	scope := uniqueScope("msgidx")
	sid, _, _ := s.EnsureSession(ctx, scope, "telegram", "u1", "m")

	if err := s.RecordSent(ctx, scope, "botmsg1", sid); err != nil {
		t.Fatal(err)
	}
	gotSid, _, ok, err := s.SessionForMessage(ctx, scope, "botmsg1")
	if err != nil || !ok || gotSid != sid {
		t.Fatalf("SessionForMessage = %q,%v,%v, want %q", gotSid, ok, err, sid)
	}
	// unknown message → ok=false, no error.
	if _, _, ok, err := s.SessionForMessage(ctx, scope, "nope"); ok || err != nil {
		t.Fatalf("SessionForMessage(unknown) = ok=%v err=%v, want false/nil", ok, err)
	}
}
