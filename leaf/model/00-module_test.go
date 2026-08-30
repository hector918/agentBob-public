package model_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/model"
	"agentbob/leaf/pgpool"
	"agentbob/trunk"
)

func liveDB(t *testing.T) (*trunk.Registry, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run the model module integration test")
	}
	ctx := context.Background()
	reg := trunk.NewRegistry()
	pg := pgpool.New(dsn)
	if err := pg.Start(ctx, reg); err != nil {
		t.Fatalf("pgpool start: %v", err)
	}
	return reg, func() { _ = pg.Stop(ctx) }
}

// TestModule_NoConfig: an empty home (no models.yaml) → empty pool floor (mtime watch alive).
func TestModule_NoConfig(t *testing.T) {
	reg, done := liveDB(t)
	defer done()
	ctx := context.Background()

	m := model.New(t.TempDir()) // no models.yaml here
	if m.Optional() {
		t.Fatal("model must be critical")
	}
	if err := m.Start(ctx, reg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.Health() != trunk.StateReady {
		t.Fatalf("Health = %v, want Ready", m.Health())
	}
	mp := trunk.Require[contract.ModelPool](reg)
	if _, err := mp.Chat(ctx, contract.ModelRequest{}, nil); err == nil {
		t.Fatal("empty pool floor should error on use (no models configured)")
	}
	if len(mp.Snapshot().Entries) != 0 {
		t.Fatal("empty pool should have no entries")
	}
	if err := m.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestModule_WithConfig: a models.yaml entry builds a MultiPool (the entry shows
// up in the snapshot even though no backend is reachable in the test).
func TestModule_WithConfig(t *testing.T) {
	reg, done := liveDB(t)
	defer done()
	ctx := context.Background()

	home := t.TempDir()
	yaml := "entries:\n  - name: test-llm\n    provider: openai\n    model: gpt-4\n    api_key_env: NOPE\n    tags: [smart]\n"
	if err := os.WriteFile(filepath.Join(home, "models.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	m := model.New(home)
	if err := m.Start(ctx, reg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(ctx)

	mp := trunk.Require[contract.ModelPool](reg)
	snap := mp.Snapshot()
	if len(snap.Entries) != 1 || snap.Entries[0].Name != "test-llm" {
		t.Fatalf("snapshot entries = %+v, want one test-llm", snap.Entries)
	}
	if !snap.Usage.HasStore {
		t.Fatal("usage store should be wired (HasStore)")
	}
}
