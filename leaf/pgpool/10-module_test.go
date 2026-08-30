package pgpool_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/pgpool"
	"agentbob/trunk"
)

// TestModule_QueryRowNoRows verifies the adapter translates the driver's
// no-rows sentinel into contract.ErrNoRows (so store impls never import
// database/sql). Env-gated like the other integration tests.
func TestModule_QueryRowNoRows(t *testing.T) {
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run the pgpool integration test")
	}
	reg := trunk.NewRegistry()
	m := pgpool.New(dsn)
	if err := m.Start(context.Background(), reg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(context.Background())

	db := trunk.Require[contract.DB](reg)
	var x int
	err := db.QueryRow(context.Background(), "SELECT 1 WHERE false").Scan(&x)
	if !errors.Is(err, contract.ErrNoRows) {
		t.Fatalf("no-rows Scan = %v, want contract.ErrNoRows", err)
	}
}

func TestModule_Contract(t *testing.T) {
	m := pgpool.New("dummy")
	if m.Name() != "pgpool" {
		t.Fatalf("Name = %q", m.Name())
	}
	prov := m.Provides()
	if len(prov) != 1 || prov[0] != trunk.TypeOf[contract.DB]() {
		t.Fatalf("Provides = %v, want [contract.DB]", prov)
	}
	if len(m.Needs()) != 0 {
		t.Fatalf("Needs = %v, want none", m.Needs())
	}
	if m.Optional() {
		t.Fatal("pgpool must be critical (Optional=false)")
	}
	if m.Health() != trunk.StateNew {
		t.Fatalf("fresh Health = %v, want StateNew", m.Health())
	}
}

func TestModule_EmptyDSNFails(t *testing.T) {
	m := pgpool.New("")
	if err := m.Start(context.Background(), trunk.NewRegistry()); err == nil {
		t.Fatal("empty DSN should fail Start")
	}
	if m.Health() != trunk.StateFailed {
		t.Fatalf("Health after failed Start = %v, want StateFailed", m.Health())
	}
}

func TestModule_UnreachableFails(t *testing.T) {
	// Parses fine but refuses to connect → ping fails fast.
	m := pgpool.New("postgres://u:p@127.0.0.1:1/none?sslmode=disable")
	if err := m.Start(context.Background(), trunk.NewRegistry()); err == nil {
		t.Fatal("unreachable server should fail Start on ping")
	}
}

// TestModule_Integration runs the real pool through the trunk against a live
// database. Env-gated: set PGPOOL_TEST_DSN to run it (skipped otherwise, since
// pg is no longer bundled locally).
func TestModule_Integration(t *testing.T) {
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run the pgpool integration test")
	}
	reg := trunk.NewRegistry()
	m := pgpool.New(dsn)
	if err := m.Start(context.Background(), reg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(context.Background())

	// A consumer gets contract.DB through the registry and runs its own SQL.
	db := trunk.Require[contract.DB](reg)
	var got int
	if err := db.QueryRow(context.Background(), "SELECT 1").Scan(&got); err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if got != 1 {
		t.Fatalf("SELECT 1 = %d, want 1", got)
	}

	// Transaction round-trip.
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.QueryRow(context.Background(), "SELECT 2").Scan(&got); err != nil {
		t.Fatalf("tx QueryRow: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got != 2 {
		t.Fatalf("tx SELECT 2 = %d, want 2", got)
	}
}
