package urllib

import (
	"context"
	"os"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/pgpool"
	"agentbob/trunk"
)

// liveLib builds a urllib library over a live pg (PGPOOL_TEST_DSN), migrated and
// emptied of bob_urls so each test starts clean. Skips without a DSN.
func liveLib(t *testing.T) (*library, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run the urllib prune test")
	}
	ctx := context.Background()
	reg := trunk.NewRegistry()
	pg := pgpool.New(dsn)
	if err := pg.Start(ctx, reg); err != nil {
		t.Fatalf("pgpool start: %v", err)
	}
	db := trunk.Require[contract.DB](reg)
	if err := migrateURLs(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM bob_urls`); err != nil {
		t.Fatalf("clean: %v", err)
	}
	return &library{db: db, lastBySid: map[string]string{}}, func() { _ = pg.Stop(ctx) }
}

// TestPrune_ValueAware: the retention sweep drops ONLY rows that are stale AND
// never useful AND not a search engine. Ever-satisfied, recently-seen, and engine
// rows survive regardless of age — the invariant that protects recall's long tail.
func TestPrune_ValueAware(t *testing.T) {
	lib, done := liveLib(t)
	defer done()
	ctx := context.Background()

	const old, recent = 1000.0, 2_000_000_000.0 // stale vs far-future last_seen
	ins := func(url string, satisfied int, lastSeen float64, engine bool) {
		t.Helper()
		if _, err := lib.db.Exec(ctx,
			`INSERT INTO bob_urls (url, satisfied_count, first_seen, last_seen, is_search_engine)
			 VALUES ($1, $2, $3, $3, $4)`, url, satisfied, lastSeen, engine); err != nil {
			t.Fatalf("insert %s: %v", url, err)
		}
	}
	ins("https://drop.example", 0, old, false)           // stale + unsatisfied + not engine → DROP
	ins("https://keep-sat.example", 3, old, false)       // ever useful → KEEP
	ins("https://keep-recent.example", 0, recent, false) // recently seen → KEEP
	ins("https://engine.example", 0, old, true)          // search engine → KEEP

	n, err := lib.Prune(ctx, 1_000_000.0) // cutoff between `old` and `recent`
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want exactly 1 (the stale-unsatisfied non-engine)", n)
	}

	var remaining int
	if err := lib.db.QueryRow(ctx, `SELECT COUNT(*) FROM bob_urls`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 3 {
		t.Fatalf("remaining %d rows, want 3 (sat / recent / engine kept)", remaining)
	}
	var gone int
	if err := lib.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM bob_urls WHERE url = 'https://drop.example'`).Scan(&gone); err != nil {
		t.Fatalf("check dropped: %v", err)
	}
	if gone != 0 {
		t.Fatal("the stale-unsatisfied row should have been pruned")
	}
}

// TestSeed_RefreshesLastSeen_SeedRowsSurvivePrune: re-seeding at Start refreshes
// last_seen, so the non-engine seed row (Wikipedia) stays ahead of Prune's
// stale-and-never-useful predicate. Regression for D19: the old UPSERT gated the
// whole DO UPDATE on is_search_engine=TRUE, leaving Wikipedia at its first-boot
// last_seen forever — pruned at retention age, resurrecting only on the next Start.
func TestSeed_RefreshesLastSeen_SeedRowsSurvivePrune(t *testing.T) {
	lib, done := liveLib(t)
	defer done()
	ctx := context.Background()

	const wiki = "https://en.wikipedia.org/wiki/"

	// First boot long ago: seed, then age every row past the retention cutoff.
	lib.seed(ctx, DefaultSeeds)
	if _, err := lib.db.Exec(ctx, `UPDATE bob_urls SET first_seen = 1, last_seen = 1`); err != nil {
		t.Fatalf("age rows: %v", err)
	}

	// Restart: re-seed must refresh last_seen on the already-present seed rows.
	lib.seed(ctx, DefaultSeeds)

	// A prune with a cutoff far past the aged timestamp keeps every seed row.
	n, err := lib.Prune(ctx, 1_000_000.0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 0 {
		t.Fatalf("prune removed %d seed rows after a re-seed, want 0", n)
	}
	var kept int
	if err := lib.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM bob_urls WHERE url = $1`, wiki).Scan(&kept); err != nil {
		t.Fatalf("check wikipedia: %v", err)
	}
	if kept != 1 {
		t.Fatal("non-engine Wikipedia seed row must survive prune after a re-seed")
	}
}
