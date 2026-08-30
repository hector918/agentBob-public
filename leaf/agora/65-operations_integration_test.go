package agora

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/agora/store"
	"agentbob/leaf/pgpool"
	"agentbob/trunk"
)

// liveDeps opens a migrated agora store + a fresh OrgCache + router against the
// test database, returning the OpDeps the operations use. Env-gated: set
// PGPOOL_TEST_DSN to run (skipped otherwise).
func liveDeps(t *testing.T) (OpDeps, *InboxRouter, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run agora operations integration tests")
	}
	ctx := context.Background()
	reg := trunk.NewRegistry()
	pm := pgpool.New(dsn)
	if err := pm.Start(ctx, reg); err != nil {
		t.Fatalf("pgpool start: %v", err)
	}
	db := trunk.Require[contract.DB](reg)
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := store.NewPG(db)
	org := NewOrgCache()
	if err := org.Load(ctx, s); err != nil {
		t.Fatalf("orgcache load: %v", err)
	}
	router := NewInboxRouter(org, s)
	if err := router.Reload(ctx); err != nil {
		t.Fatalf("router reload: %v", err)
	}
	deps := OpDeps{Store: s, Org: org, RouterReload: router.Reload}
	return deps, router, func() { _ = pm.Stop(ctx) }
}

func uniq(tag string) string {
	return "test-" + tag + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// CreateCompany + Hire round-trip; Hire rejects a duplicate active employment;
// AddInboxSource → the router resolves the new scope.
func TestOps_CreateCompany_Hire_AddSource(t *testing.T) {
	deps, router, done := liveDeps(t)
	defer done()
	ctx := context.Background()

	coName := uniq("co")
	co, err := CreateCompany(ctx, deps, coName)
	if err != nil {
		t.Fatalf("CreateCompany: %v", err)
	}
	if co.ReportInboxID == "" {
		t.Fatal("CreateCompany: bridge inbox not wired (empty ReportInboxID)")
	}

	// Hire the first seeded role (the default template's first role).
	role, _ := FirstRoleNameInYAML([]byte(SeededPermissionsYAML))
	memberName := uniq("m")
	_, _, inbox, err := Hire(ctx, deps, coName, memberName, role)
	if err != nil {
		t.Fatalf("Hire: %v", err)
	}
	if want := MemberOwnedInboxScope(memberName, co.ID); inbox.Scope != want {
		t.Fatalf("Hire inbox scope = %q, want %q", inbox.Scope, want)
	}

	// Hire again (same member, same company) → rejected (duplicate active employment).
	if _, _, _, err := Hire(ctx, deps, coName, memberName, role); err == nil {
		t.Fatal("Hire(duplicate) = nil, want rejection")
	}

	// Hire with an unknown role → rejected by ValidateRoleInCompany.
	if _, _, _, err := Hire(ctx, deps, coName, uniq("m2"), "no-such-role"); err == nil {
		t.Fatal("Hire(unknown role) = nil, want rejection")
	}

	// AddInboxSource → the router resolves the new chat scope to the inbox.
	chatID := uniq("chat")
	if _, err := AddInboxSource(ctx, deps, inbox.ID, "telegram", contract.ChatGroup, chatID, ""); err != nil {
		t.Fatalf("AddInboxSource: %v", err)
	}
	scope := contract.ScopeFor("telegram", contract.ChatGroup, chatID, "")
	if got, ok := router.ResolveByScope(scope); !ok || got.ID != inbox.ID {
		t.Fatalf("router.ResolveByScope(%q) = (%q,%v), want (%q,true)", scope, got.ID, ok, inbox.ID)
	}

	// Re-binding the SAME chat must NOT fail on the unique constraint — a binding
	// re-points (the new bumps the old), it never errors with a raw 23505.
	if _, err := AddInboxSource(ctx, deps, inbox.ID, "telegram", contract.ChatGroup, chatID, ""); err != nil {
		t.Fatalf("AddInboxSource(re-bind same chat) must re-point, got error: %v", err)
	}
	// Wiring the same chat to a DIFFERENT inbox moves it there (old binding bumped).
	ib2, err := CreateMemberOwnedInbox(ctx, deps, memberName, uniq("ib2"))
	if err != nil {
		t.Fatalf("CreateMemberOwnedInbox: %v", err)
	}
	if _, err := AddInboxSource(ctx, deps, ib2.ID, "telegram", contract.ChatGroup, chatID, ""); err != nil {
		t.Fatalf("AddInboxSource(re-point to ib2): %v", err)
	}
	if got, ok := router.ResolveByScope(scope); !ok || got.ID != ib2.ID {
		t.Fatalf("after re-point, ResolveByScope(%q) = (%q,%v), want (%q,true)", scope, got.ID, ok, ib2.ID)
	}
}

// TestOps_RemoveInboxSource: unwire-by-scope removes the binding (router stops
// resolving it) and is idempotent-loud — a second remove reports contract.ErrNoRows.
func TestOps_RemoveInboxSource(t *testing.T) {
	deps, router, done := liveDeps(t)
	defer done()
	ctx := context.Background()

	coName := uniq("co")
	if _, err := CreateCompany(ctx, deps, coName); err != nil {
		t.Fatalf("CreateCompany: %v", err)
	}
	role, _ := FirstRoleNameInYAML([]byte(SeededPermissionsYAML))
	memberName := uniq("m")
	_, _, inbox, err := Hire(ctx, deps, coName, memberName, role)
	if err != nil {
		t.Fatalf("Hire: %v", err)
	}

	chatID := uniq("chat")
	if _, err := AddInboxSource(ctx, deps, inbox.ID, "telegram", contract.ChatGroup, chatID, ""); err != nil {
		t.Fatalf("AddInboxSource: %v", err)
	}
	scope := contract.ScopeFor("telegram", contract.ChatGroup, chatID, "")
	if _, ok := router.ResolveByScope(scope); !ok {
		t.Fatalf("precondition: router must resolve %q before unwire", scope)
	}

	// unwire by scope → router stops resolving it.
	if err := RemoveInboxSource(ctx, deps, inbox.ID, scope); err != nil {
		t.Fatalf("RemoveInboxSource: %v", err)
	}
	if got, ok := router.ResolveByScope(scope); ok {
		t.Fatalf("after unwire, ResolveByScope(%q) still resolves to %q", scope, got.ID)
	}

	// second remove → no such binding (loud, not silent success).
	if err := RemoveInboxSource(ctx, deps, inbox.ID, scope); !errors.Is(err, contract.ErrNoRows) {
		t.Fatalf("RemoveInboxSource(already gone) = %v, want ErrNoRows", err)
	}
	// an unknown scope on a valid inbox → also ErrNoRows.
	if err := RemoveInboxSource(ctx, deps, inbox.ID, "telegram:group:nonesuch"); !errors.Is(err, contract.ErrNoRows) {
		t.Fatalf("RemoveInboxSource(unknown scope) = %v, want ErrNoRows", err)
	}

	// Multi-topic group: two rows (same chat, different thread) flatten to ONE group
	// scope; unwire-by-scope must remove ALL of them (they route identically).
	gchat := uniq("gchat")
	if _, err := AddInboxSource(ctx, deps, inbox.ID, "telegram", contract.ChatGroup, gchat, "topic-1"); err != nil {
		t.Fatalf("AddInboxSource topic-1: %v", err)
	}
	if _, err := AddInboxSource(ctx, deps, inbox.ID, "telegram", contract.ChatGroup, gchat, "topic-2"); err != nil {
		t.Fatalf("AddInboxSource topic-2: %v", err)
	}
	gscope := contract.ScopeFor("telegram", contract.ChatGroup, gchat, "topic-1") // flattens (== topic-2's)
	if err := RemoveInboxSource(ctx, deps, inbox.ID, gscope); err != nil {
		t.Fatalf("RemoveInboxSource(multi-topic): %v", err)
	}
	if _, ok := router.ResolveByScope(gscope); ok {
		t.Fatalf("after multi-topic unwire, %q still resolves", gscope)
	}
	if err := RemoveInboxSource(ctx, deps, inbox.ID, gscope); !errors.Is(err, contract.ErrNoRows) {
		t.Fatalf("RemoveInboxSource(multi-topic, again) = %v, want ErrNoRows — all rows must be gone", err)
	}
}

// TestOps_DeleteCompany exercises the company hard-delete cascade end to end:
// the disable precondition, the in-flight refusal, the FK cascade (employments +
// bridge inbox), orphan-member delete (member only here + their inbox), and the
// survival of a member employed elsewhere.
func TestOps_DeleteCompany(t *testing.T) {
	deps, _, done := liveDeps(t)
	defer done()
	ctx := context.Background()
	role, _ := FirstRoleNameInYAML([]byte(SeededPermissionsYAML))

	// co1 with two members: alice (only here → orphan) and bob (also at co2).
	co1Name := uniq("co1")
	co1, err := CreateCompany(ctx, deps, co1Name)
	if err != nil {
		t.Fatalf("CreateCompany co1: %v", err)
	}
	alice := uniq("alice")
	_, _, aliceInbox, err := Hire(ctx, deps, co1Name, alice, role)
	if err != nil {
		t.Fatalf("Hire alice: %v", err)
	}
	bob := uniq("bob")
	if _, _, _, err := Hire(ctx, deps, co1Name, bob, role); err != nil {
		t.Fatalf("Hire bob@co1: %v", err)
	}
	co2Name := uniq("co2")
	if _, err := CreateCompany(ctx, deps, co2Name); err != nil {
		t.Fatalf("CreateCompany co2: %v", err)
	}
	if _, _, bobCo2Inbox, err := Hire(ctx, deps, co2Name, bob, role); err != nil {
		t.Fatalf("Hire bob@co2: %v", err)
	} else {
		defer func() { // bobCo2Inbox must still exist after the delete
			if _, gerr := deps.Store.GetInbox(ctx, bobCo2Inbox.ID); gerr != nil {
				t.Errorf("bob's co2 inbox must survive co1 delete, got: %v", gerr)
			}
		}()
	}

	// Precondition 1: refuse while the company is still active (not disabled).
	deps.RunningAtScope = func(string) int { return 0 }
	if err := DeleteCompany(ctx, deps, co1.ID); err == nil {
		t.Fatal("DeleteCompany on an ACTIVE company = nil, want refuse (must disable first)")
	}
	if err := DisableCompany(ctx, deps, co1.ID); err != nil {
		t.Fatalf("DisableCompany: %v", err)
	}

	// Precondition 2: refuse while a to-be-deleted inbox has an in-flight turn.
	deps.RunningAtScope = func(scope string) int {
		if scope == aliceInbox.Scope {
			return 1
		}
		return 0
	}
	if err := DeleteCompany(ctx, deps, co1.ID); err == nil {
		t.Fatal("DeleteCompany with in-flight turn = nil, want refuse")
	}

	// fail-closed: no SessionManager probe → refuse (can't verify in-flight).
	deps.RunningAtScope = nil
	if err := DeleteCompany(ctx, deps, co1.ID); err == nil {
		t.Fatal("DeleteCompany with nil RunningAtScope = nil, want fail-closed refuse")
	}

	// Happy path: disabled + idle → the cut lands.
	deps.RunningAtScope = func(string) int { return 0 }
	if err := DeleteCompany(ctx, deps, co1.ID); err != nil {
		t.Fatalf("DeleteCompany: %v", err)
	}

	// co1 gone; its bridge inbox gone (cascade).
	if _, gerr := deps.Store.GetCompany(ctx, co1.ID); !errors.Is(gerr, contract.ErrNoRows) {
		t.Errorf("GetCompany(co1) after delete = %v, want ErrNoRows", gerr)
	}
	if _, gerr := deps.Store.GetInbox(ctx, co1.ReportInboxID); !errors.Is(gerr, contract.ErrNoRows) {
		t.Errorf("bridge inbox after delete = %v, want ErrNoRows (cascade)", gerr)
	}
	// alice (orphan) + her inbox gone.
	if _, gerr := deps.Store.GetMemberByName(ctx, alice); !errors.Is(gerr, contract.ErrNoRows) {
		t.Errorf("orphan member alice after delete = %v, want ErrNoRows", gerr)
	}
	if _, gerr := deps.Store.GetInboxByScope(ctx, aliceInbox.Scope); !errors.Is(gerr, contract.ErrNoRows) {
		t.Errorf("orphan member inbox after delete = %v, want ErrNoRows (cascade)", gerr)
	}
	// bob survives (employed at co2).
	if _, gerr := deps.Store.GetMemberByName(ctx, bob); gerr != nil {
		t.Errorf("multi-company member bob must survive, got: %v", gerr)
	}
}
