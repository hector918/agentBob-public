package store_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/accounts/store"
	"agentbob/leaf/pgpool"
	"agentbob/trunk"
)

// liveStore opens a migrated accounts store against the test database. Env-gated:
// set PGPOOL_TEST_DSN to run (skipped otherwise).
func liveStore(t *testing.T) (*store.PG, contract.DB, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run accounts store integration tests")
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
	return store.NewPG(db), db, func() { _ = m.Stop(ctx) }
}

func uniqueUID(tag string) string {
	return "test_" + tag + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// TestResolveAndUsage: bind a handle and round-trip usage through AddTurnUsage →
// AccountUsageBreakdown.
func TestResolveAndUsage(t *testing.T) {
	s, _, done := liveStore(t)
	defer done()
	ctx := context.Background()
	now := time.Now().Unix()

	uid := uniqueUID("resolve")

	acc, err := s.CreateAccount(ctx, "Test User", "", now)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if acc.Flow != "normal" {
		t.Errorf("new account flow = %q, want normal", acc.Flow)
	}
	if _, err := s.BindHandle(ctx, store.SourceHandle{
		AccountID: acc.ID, Source: "telegram", PlatformUID: uid,
	}, now); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	// Usage round-trip: record two turns onto the handle, roll up by account.
	if err := s.AddTurnUsage(ctx, "telegram", uid, 1, 1, map[string]store.KindTokens{"llm": {In: 100, Out: 200}}, nil); err != nil {
		t.Fatalf("AddTurnUsage 1: %v", err)
	}
	if err := s.AddTurnUsage(ctx, "telegram", uid, 1, 0, map[string]store.KindTokens{"llm": {In: 10, Out: 5}, "ocr": {In: 7, Out: 0}}, nil); err != nil {
		t.Fatalf("AddTurnUsage 2: %v", err)
	}
	u, err := s.AccountUsageBreakdown(ctx, acc.ID)
	if err != nil {
		t.Fatalf("AccountUsageBreakdown: %v", err)
	}
	if u.Turns != 2 || u.Success != 1 {
		t.Errorf("rollup turns/success = %d/%d, want 2/1", u.Turns, u.Success)
	}
	if u.TokensByKind["llm"] != (store.KindTokens{In: 110, Out: 205}) {
		t.Errorf("rollup llm = %+v, want {110 205}", u.TokensByKind["llm"])
	}
	if u.TokensByKind["ocr"] != (store.KindTokens{In: 7, Out: 0}) {
		t.Errorf("rollup ocr = %+v, want {7 0}", u.TokensByKind["ocr"])
	}
}

// TestHandleLangSeedOnce exercises the per-sender reply-language seed on the passive
// handle-usage row: it creates without a binding, is seed-once (a later set never
// overwrites an established language → the seed stays stable), and coexists with
// AddTurnUsage on the same row (disjoint columns — neither upsert clobbers the other).
func TestHandleLangSeedOnce(t *testing.T) {
	s, _, done := liveStore(t)
	defer done()
	ctx := context.Background()
	uid := uniqueUID("lang")

	if l, err := s.GetHandleLang(ctx, "telegram", uid); err != nil || l != "" {
		t.Fatalf("fresh GetHandleLang = %q err %v, want \"\"/nil", l, err)
	}
	// Seed zh — creates the passive (source,uid) row, no account/binding needed.
	if err := s.SetHandleLangIfEmpty(ctx, "telegram", uid, "zh"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if l, _ := s.GetHandleLang(ctx, "telegram", uid); l != "zh" {
		t.Fatalf("after seed = %q, want zh", l)
	}
	// Seed-once: a second set must NOT overwrite the established language.
	if err := s.SetHandleLangIfEmpty(ctx, "telegram", uid, "default"); err != nil {
		t.Fatalf("second set: %v", err)
	}
	if l, _ := s.GetHandleLang(ctx, "telegram", uid); l != "zh" {
		t.Errorf("seed-once violated: lang = %q after second set, want zh (stable)", l)
	}
	// Usage upsert on the SAME row must not clobber the lang column (disjoint).
	if err := s.AddTurnUsage(ctx, "telegram", uid, 1, 1, map[string]store.KindTokens{"llm": {In: 5, Out: 5}}, nil); err != nil {
		t.Fatalf("AddTurnUsage after seed: %v", err)
	}
	if l, _ := s.GetHandleLang(ctx, "telegram", uid); l != "zh" {
		t.Errorf("usage upsert clobbered lang: %q, want zh", l)
	}
}

// TestAPIKeysCRUD: mint → verify-by-hash (active account) → list → revoke →
// verify fails; a paused account disables its keys.
func TestAPIKeysCRUD(t *testing.T) {
	s, _, done := liveStore(t)
	defer done()
	ctx := context.Background()

	a, err := s.CreateAccount(ctx, "apikey-owner", "", 1)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	hash := "hash_" + uniqueUID("k")
	k, err := s.CreateAPIKey(ctx, store.APIKey{
		AccountID: a.ID,
		Label:     "nas",
		Kinds:     []string{"llm", "translate"},
	}, hash, 2)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if k.ID == "" || k.Status != "active" {
		t.Fatalf("minted key malformed: %+v", k)
	}

	got, ok, err := s.APIKeyByHash(ctx, hash)
	if err != nil || !ok {
		t.Fatalf("APIKeyByHash: ok=%v err=%v", ok, err)
	}
	if got.AccountID != a.ID || len(got.Kinds) != 2 || got.Kinds[1] != "translate" {
		t.Errorf("resolved key mismatch: %+v", got)
	}
	// The three adjacent TEXT columns (label/kinds/models) are the one place a
	// Scan-order swap would go unnoticed — assert each lands where it belongs.
	if got.Label != "nas" || len(got.Models) != 0 {
		t.Errorf("policy columns crossed: %+v", got)
	}

	keys, err := s.ListAPIKeys(ctx, a.ID)
	if err != nil || len(keys) != 1 || keys[0].ID != k.ID {
		t.Fatalf("ListAPIKeys = %+v err %v, want the one key", keys, err)
	}

	// A paused account disables verification (JOIN gate).
	if err := s.SetAccountStatus(ctx, a.ID, "paused", 3); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, ok, _ := s.APIKeyByHash(ctx, hash); ok {
		t.Error("paused account's key must not verify")
	}
	if err := s.SetAccountStatus(ctx, a.ID, "active", 4); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// Revoke → verify fails; the row still lists (status revoked).
	if err := s.RevokeAPIKey(ctx, k.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if _, ok, _ := s.APIKeyByHash(ctx, hash); ok {
		t.Error("revoked key must not verify")
	}
	if err := s.RevokeAPIKey(ctx, "ak_absent"); err != store.ErrAPIKeyNotFound {
		t.Errorf("revoke absent: err = %v, want ErrAPIKeyNotFound", err)
	}
}
