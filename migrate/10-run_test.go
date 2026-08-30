package migrate_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/pgpool"
	"agentbob/migrate"
	"agentbob/trunk"
)

func TestRun_DuplicateVersionErrors(t *testing.T) {
	// Validation happens before any DB call, so a nil db is never touched.
	steps := []migrate.Step{
		{Version: 1, SQL: "x"},
		{Version: 1, SQL: "y"},
	}
	err := migrate.Run(context.Background(), nil, "dupmod", steps)
	if err == nil || !strings.Contains(err.Error(), "duplicate step version") {
		t.Fatalf("expected duplicate-version error, got %v", err)
	}
}

// liveDB opens a contract.DB against the test database. Env-gated: set
// PGPOOL_TEST_DSN to run (skipped otherwise — pg is no longer bundled locally).
func liveDB(t *testing.T) (contract.DB, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run migrate integration tests")
	}
	reg := trunk.NewRegistry()
	m := pgpool.New(dsn)
	if err := m.Start(context.Background(), reg); err != nil {
		t.Fatalf("pgpool start: %v", err)
	}
	return trunk.Require[contract.DB](reg), func() { _ = m.Stop(context.Background()) }
}

func TestRun_AppliesThenIdempotent(t *testing.T) {
	db, done := liveDB(t)
	defer done()
	ctx := context.Background()

	const mod = "migrate_selftest"
	// clean slate for a repeatable run.
	_, _ = db.Exec(ctx, `DROP TABLE IF EXISTS mt_widget`)
	_, _ = db.Exec(ctx, `DELETE FROM schema_version WHERE module = $1`, mod)

	steps := []migrate.Step{
		{Version: 1, SQL: `CREATE TABLE mt_widget (id BIGINT PRIMARY KEY)`},
		{Version: 2, SQL: `ALTER TABLE mt_widget ADD COLUMN name TEXT`},
	}

	if err := migrate.Run(ctx, db, mod, steps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v := version(t, db, mod); v != 2 {
		t.Fatalf("recorded version = %d, want 2", v)
	}
	// Both steps applied → inserting with the v2 column works.
	if _, err := db.Exec(ctx, `INSERT INTO mt_widget (id, name) VALUES (1, 'a')`); err != nil {
		t.Fatalf("insert after migrate: %v", err)
	}

	// Idempotent: re-running applies nothing, version unchanged, no error.
	if err := migrate.Run(ctx, db, mod, steps); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if v := version(t, db, mod); v != 2 {
		t.Fatalf("after re-run version = %d, want 2", v)
	}

	// Adding a higher step advances only the new one.
	steps = append(steps, migrate.Step{Version: 3, SQL: `ALTER TABLE mt_widget ADD COLUMN qty INTEGER`})
	if err := migrate.Run(ctx, db, mod, steps); err != nil {
		t.Fatalf("Run v3: %v", err)
	}
	if v := version(t, db, mod); v != 3 {
		t.Fatalf("after v3 version = %d, want 3", v)
	}

	_, _ = db.Exec(ctx, `DROP TABLE mt_widget`)
	_, _ = db.Exec(ctx, `DELETE FROM schema_version WHERE module = $1`, mod)
}

func TestRun_BadStepRollsBack(t *testing.T) {
	db, done := liveDB(t)
	defer done()
	ctx := context.Background()

	const mod = "migrate_rollback_test"
	_, _ = db.Exec(ctx, `DROP TABLE IF EXISTS mr_widget`)
	_, _ = db.Exec(ctx, `DELETE FROM schema_version WHERE module = $1`, mod)

	steps := []migrate.Step{
		{Version: 1, SQL: `CREATE TABLE mr_widget (id BIGINT PRIMARY KEY)`},
		{Version: 2, SQL: `THIS IS NOT VALID SQL`},
	}
	if err := migrate.Run(ctx, db, mod, steps); err == nil {
		t.Fatal("a step with invalid SQL should error")
	}
	// v1 committed (version 1); v2's bad DDL rolled back atomically with its
	// version bump → version stays 1.
	if v := version(t, db, mod); v != 1 {
		t.Fatalf("version after failed v2 = %d, want 1 (v2 must roll back)", v)
	}
	// v1's table is present and usable.
	if _, err := db.Exec(ctx, `INSERT INTO mr_widget (id) VALUES (1)`); err != nil {
		t.Fatalf("v1 table should exist: %v", err)
	}

	_, _ = db.Exec(ctx, `DROP TABLE mr_widget`)
	_, _ = db.Exec(ctx, `DELETE FROM schema_version WHERE module = $1`, mod)
}

func version(t *testing.T, db contract.DB, mod string) int {
	t.Helper()
	var v int
	if err := db.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT version FROM schema_version WHERE module = $1), 0)`, mod,
	).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	return v
}
