package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/arrangement/store"
	"agentbob/leaf/pgpool"
	"agentbob/trunk"
)

// liveStore opens a migrated arrangement store against the test database. Env-gated:
// set PGPOOL_TEST_DSN to run (skipped otherwise) — same convention as accounts/store.
func liveStore(t *testing.T) (*store.PG, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run arrangement store integration tests")
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

// TestCloseStalledDefs: only a started def with NO runnable item and NO def/item
// activity since cutoff is closed (→ cancelled); a runnable item, recent item activity,
// or a recently-touched def each keep it started (inventory #41).
func TestCloseStalledDefs(t *testing.T) {
	s, done := liveStore(t)
	defer done()
	ctx := context.Background()

	now := time.Now().Unix()
	old := now - 40*24*3600    // well past the cutoff
	cutoff := now - 30*24*3600 // the 30d stall window

	mustDef := func(stamp int64) string {
		t.Helper()
		id, err := s.InsertDef(ctx, "test_co", "test_author", "started", "{}", stamp)
		if err != nil {
			t.Fatalf("InsertDef: %v", err)
		}
		t.Cleanup(func() { _ = s.DeleteDef(ctx, id) })
		return id
	}
	mustItem := func(def string, stamp int64) {
		t.Helper()
		if _, err := s.InsertItem(ctx, def, "r1", 0, "", stamp); err != nil {
			t.Fatalf("InsertItem: %v", err)
		}
	}
	park := func(def string, stamp int64) {
		t.Helper()
		if _, err := s.MarkUnmet(ctx, def, "r1", stamp); err != nil {
			t.Fatalf("MarkUnmet: %v", err)
		}
	}

	stalled := mustDef(old) // parked-only, all activity old → closed
	mustItem(stalled, old)
	park(stalled, old)

	runnable := mustDef(old) // still holds a queued item → untouched
	mustItem(runnable, old)

	recentItem := mustDef(old) // parked, but item touched recently → untouched
	mustItem(recentItem, old)
	park(recentItem, now)

	recentDef := mustDef(now) // def itself touched recently → untouched
	mustItem(recentDef, old)
	park(recentDef, old)

	if _, err := s.CloseStalledDefs(ctx, cutoff, now); err != nil {
		t.Fatalf("CloseStalledDefs: %v", err)
	}

	wantStatus := func(id, want string) {
		t.Helper()
		d, err := s.GetDef(ctx, id)
		if err != nil {
			t.Fatalf("GetDef %s: %v", id, err)
		}
		if d.Status != want {
			t.Errorf("def %s status = %q, want %q", id, d.Status, want)
		}
	}
	wantStatus(stalled, "cancelled")
	wantStatus(runnable, "started")
	wantStatus(recentItem, "started")
	wantStatus(recentDef, "started")

	// The close stamps updated_at=now so the terminal-defs prune grants its full grace.
	if d, err := s.GetDef(ctx, stalled); err != nil {
		t.Fatalf("GetDef: %v", err)
	} else if d.UpdatedAt != now {
		t.Errorf("closed def updated_at = %d, want %d", d.UpdatedAt, now)
	}
}
