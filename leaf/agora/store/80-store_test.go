package store_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/agora/store"
	"agentbob/leaf/pgpool"
	"agentbob/trunk"
)

// liveStore opens a migrated agora store against the test database. Env-gated:
// set PGPOOL_TEST_DSN to run (skipped otherwise).
func liveStore(t *testing.T) (*store.PG, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run agora store integration tests")
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

// uniq isolates a test's rows from prior runs (tables persist).
func uniq(tag string) string {
	return "test-" + tag + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func TestAgora_RoundTrip(t *testing.T) {
	s, done := liveStore(t)
	defer done()
	ctx := context.Background()
	now := time.Now().Unix()

	// Company + Member + Employment + member Inbox + inbox source.
	co, err := s.CreateCompany(ctx, store.Company{
		Name:            uniq("co"),
		PermissionsYAML: "roles:\n  - boss\ngrants:\n  tool:use:now:\n    boss: on\n",
		Playbook:        "be excellent",
	}, now)
	if err != nil {
		t.Fatalf("CreateCompany: %v", err)
	}
	m, err := s.CreateMember(ctx, store.Member{Name: uniq("m")}, now)
	if err != nil {
		t.Fatalf("CreateMember: %v", err)
	}
	emp, err := s.Hire(ctx, m.ID, co.ID, "boss", now)
	if err != nil {
		t.Fatalf("Hire: %v", err)
	}
	ib, err := s.CreateInbox(ctx, store.Inbox{
		Kind:     "member",
		MemberID: m.ID,
		Scope:    uniq("scope"),
		Name:     "main",
	}, now)
	if err != nil {
		t.Fatalf("CreateInbox: %v", err)
	}
	if _, err := s.AddInboxSource(ctx, store.InboxSource{
		InboxID: ib.ID, SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "-100",
	}, now); err != nil {
		t.Fatalf("AddInboxSource: %v", err)
	}

	// Read back through each accessor.
	if got, err := s.GetCompany(ctx, co.ID); err != nil || got.Name != co.Name || got.Playbook != "be excellent" {
		t.Fatalf("GetCompany = %+v, %v", got, err)
	}
	if got, err := s.GetMember(ctx, m.ID); err != nil || got.Name != m.Name {
		t.Fatalf("GetMember = %+v, %v", got, err)
	}
	if got, err := s.GetEmployment(ctx, emp.ID); err != nil || got.RoleName != "boss" || got.Status != "active" {
		t.Fatalf("GetEmployment = %+v, %v", got, err)
	}
	if got, err := s.GetInboxByScope(ctx, ib.Scope); err != nil || got.ID != ib.ID || got.Kind != "member" {
		t.Fatalf("GetInboxByScope = %+v, %v", got, err)
	}
	srcs, err := s.ListInboxSources(ctx, ib.ID)
	if err != nil || len(srcs) != 1 || srcs[0].ChatType != contract.ChatGroup || srcs[0].ChatID != "-100" {
		t.Fatalf("ListInboxSources = %+v, %v", srcs, err)
	}

	// Focused accessors + visibility write-through to pg.
	if err := s.SetCompanyPlaybook(ctx, co.ID, "v2", now); err != nil {
		t.Fatalf("SetCompanyPlaybook: %v", err)
	}
	if pb, err := s.GetCompanyPlaybook(ctx, co.ID); err != nil || pb != "v2" {
		t.Fatalf("GetCompanyPlaybook = %q, %v", pb, err)
	}
	if err := s.SetInboxVisibility(ctx, ib.ID, &store.InboxVisibility{HiddenTools: []string{"now"}}); err != nil {
		t.Fatalf("SetInboxVisibility: %v", err)
	}
	if got, err := s.GetInbox(ctx, ib.ID); err != nil || got.Visibility == nil || len(got.Visibility.HiddenTools) != 1 {
		t.Fatalf("GetInbox visibility = %+v, %v", got.Visibility, err)
	}

	// not-found maps to contract.ErrNoRows.
	if _, err := s.GetCompany(ctx, "co_nope"); err == nil {
		t.Fatal("GetCompany(unknown) should error")
	}
}
