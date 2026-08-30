package agora

import (
	"context"
	"errors"
	"sync"
	"testing"

	"agentbob/leaf/agora/store"
)

// fakeOrgStore stubs the narrow OrgLoader + the PermissionsWriter method
// OrgCache touches. No database — these tests must run without one.
type fakeOrgStore struct {
	cos []store.Company
	mem []store.Member
	emp []store.Employment
	ibs []store.Inbox

	mu           sync.Mutex
	setPermCalls []setYAMLCall

	// non-nil → SetPermissionsYAML returns this error and the in-memory cache
	// entry must NOT be updated (write-through order check).
	setPermErr error
}

type setYAMLCall struct {
	companyID string
	yaml      string
}

// OrgLoader methods.

func (f *fakeOrgStore) ListCompanies(context.Context) ([]store.Company, error) { return f.cos, nil }
func (f *fakeOrgStore) ListMembers(context.Context) ([]store.Member, error)    { return f.mem, nil }
func (f *fakeOrgStore) ListEmployments(context.Context, bool) ([]store.Employment, error) {
	return f.emp, nil
}
func (f *fakeOrgStore) ListInboxes(context.Context) ([]store.Inbox, error) { return f.ibs, nil }

// PermissionsWriter method OrgCache uses for SetCompanyRolesFromYAML.
func (f *fakeOrgStore) SetPermissionsYAML(_ context.Context, companyID, yaml string, _ int64) error {
	if f.setPermErr != nil {
		return f.setPermErr
	}
	f.mu.Lock()
	f.setPermCalls = append(f.setPermCalls, setYAMLCall{companyID, yaml})
	f.mu.Unlock()
	for i := range f.cos {
		if f.cos[i].ID == companyID {
			f.cos[i].PermissionsYAML = yaml
		}
	}
	return nil
}

// VisibilityWriter method OrgCache uses for SetInboxVisibility.
func (f *fakeOrgStore) SetInboxVisibility(_ context.Context, inboxID string, vis *store.InboxVisibility) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.ibs {
		if f.ibs[i].ID == inboxID {
			f.ibs[i].Visibility = vis
		}
	}
	return nil
}

// ---- Load + lookup ----------------------------------------------------------

func TestOrgCache_LoadEmpty(t *testing.T) {
	c := NewOrgCache()
	if err := c.Load(context.Background(), &fakeOrgStore{}); err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if got := c.RolesFor("co_anything"); got != nil {
		t.Fatalf("missing companyID should yield nil entry, got %+v", got)
	}
	if len(c.companies) != 0 {
		t.Fatalf("company count on empty store = %d, want 0", len(c.companies))
	}
}

// Empty permissions_yaml → non-nil empty-Roles entry (company-exists ↔ entry-exists).
func TestOrgCache_RolesEmptyYAMLNonNilEntry(t *testing.T) {
	s := &fakeOrgStore{cos: []store.Company{{ID: "co_empty", PermissionsYAML: ""}}}
	c := NewOrgCache()
	if err := c.Load(context.Background(), s); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.RolesFor("co_empty")
	if got == nil || got.Roles == nil || len(got.Roles) != 0 {
		t.Fatalf("co_empty should be non-nil empty-Roles entry, got %+v", got)
	}
}

// Bad permissions_yaml → non-nil empty-Roles entry (fail-closed), not nil.
func TestOrgCache_RolesBadYAMLEmptyEntry(t *testing.T) {
	s := &fakeOrgStore{cos: []store.Company{{ID: "co_bad", Name: "Acme", PermissionsYAML: "roles: : :"}}}
	c := NewOrgCache()
	if err := c.Load(context.Background(), s); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.RolesFor("co_bad")
	if got == nil || got.Roles == nil || len(got.Roles) != 0 {
		t.Fatalf("co_bad should be non-nil empty-Roles entry, got %+v", got)
	}
}

func TestOrgCache_LoadAndRolesFor(t *testing.T) {
	s := &fakeOrgStore{cos: []store.Company{
		{ID: "co_a", PermissionsYAML: "roles:\n  - boss\ngrants:\n  tool:use:read_file:\n    boss: on\n"},
		{ID: "co_b", PermissionsYAML: "roles:\n  - intern\ngrants:\n  tool:use:todo:\n    intern: on\n"},
	}}
	c := NewOrgCache()
	if err := c.Load(context.Background(), s); err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := c.RolesFor("co_a")
	if a == nil || len(a.Roles) != 1 {
		t.Fatalf("co_a entry malformed: %+v", a)
	}
	if boss, ok := a.Roles["boss"]; !ok || len(boss.Grants) != 1 || boss.Grants[0] != "tool:use:read_file" {
		t.Fatalf("co_a 'boss' grants malformed: %+v", a.Roles)
	}
	b := c.RolesFor("co_b")
	if b == nil || len(b.Roles) != 1 {
		t.Fatalf("co_b entry malformed: %+v", b)
	}
	if _, ok := b.Roles["intern"]; !ok {
		t.Fatalf("co_b missing 'intern': %+v", b.Roles)
	}
	if got := c.RolesFor("co_missing"); got != nil {
		t.Fatalf("missing companyID should be nil, got %+v", got)
	}
}

// ---- Entity accessors + Snapshot --------------------------------------------

func TestOrgCache_AccessorsAndSnapshot(t *testing.T) {
	s := &fakeOrgStore{
		cos: []store.Company{{ID: "co_1", Name: "Acme", Playbook: "be nice"}},
		mem: []store.Member{{ID: "m_1", Name: "Ada"}},
		emp: []store.Employment{
			{ID: "e_1", MemberID: "m_1", CompanyID: "co_1", RoleName: "boss", Status: "active"},
			{ID: "e_2", MemberID: "m_1", CompanyID: "co_1", RoleName: "old", Status: "terminated"},
		},
		ibs: []store.Inbox{
			{ID: "ib_1", Scope: "company:Acme/member:Ada/inbox:main", Status: "active"},
			{ID: "ib_2", Scope: "company:Acme/member:Ada/inbox:gone", Status: "archived"},
		},
	}
	c := NewOrgCache()
	if err := c.Load(context.Background(), s); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if co, ok := c.CompanyByID("co_1"); !ok || co.Name != "Acme" || co.Playbook != "be nice" {
		t.Fatalf("CompanyByID = %+v,%v", co, ok)
	}
	if m, ok := c.MemberByID("m_1"); !ok || m.Name != "Ada" {
		t.Fatalf("MemberByID = %+v,%v", m, ok)
	}
	if ib, ok := c.InboxByID("ib_2"); !ok || ib.Status != "archived" {
		t.Fatalf("InboxByID should return archived rows too: %+v,%v", ib, ok)
	}

	snap := c.Snapshot(context.Background(), nil)
	if len(snap.Companies) != 1 || len(snap.Members) != 1 {
		t.Fatalf("snapshot scalar counts off: %+v", snap)
	}
	if len(snap.Employments) != 1 || snap.Employments[0].ID != "e_1" {
		t.Fatalf("snapshot should drop terminated employments: %+v", snap.Employments)
	}
	if len(snap.Inboxes) != 1 || snap.Inboxes[0].ID != "ib_1" {
		t.Fatalf("snapshot should drop archived inboxes: %+v", snap.Inboxes)
	}
}

// ---- Write-through ----------------------------------------------------------

func TestOrgCache_SetCompanyRolesFromYAML(t *testing.T) {
	s := &fakeOrgStore{cos: []store.Company{{ID: "co_1"}}}
	c := NewOrgCache()
	if err := c.Load(context.Background(), s); err != nil {
		t.Fatalf("Load: %v", err)
	}

	body := []byte("roles:\n  - boss\ngrants:\n  tool:use:todo:\n    boss: on\n")
	if err := c.SetCompanyRolesFromYAML(context.Background(), s, "co_1", body, 100); err != nil {
		t.Fatalf("valid yaml: %v", err)
	}
	if got := c.RolesFor("co_1"); got == nil || len(got.Roles["boss"].Grants) != 1 || got.Roles["boss"].Grants[0] != "tool:use:todo" {
		t.Fatalf("co_1 entry malformed: %+v", got)
	}
	if n := len(s.setPermCalls); n != 1 {
		t.Fatalf("expected 1 SetPermissionsYAML call, got %d", n)
	}

	// empty yaml → entry stays non-nil empty-Roles.
	if err := c.SetCompanyRolesFromYAML(context.Background(), s, "co_1", nil, 101); err != nil {
		t.Fatalf("empty yaml: %v", err)
	}
	if got := c.RolesFor("co_1"); got == nil || len(got.Roles) != 0 {
		t.Fatalf("empty yaml should yield empty-Roles entry, got %+v", got)
	}

	// invalid yaml → store write succeeded, parse error surfaced, cache empty.
	if err := c.SetCompanyRolesFromYAML(context.Background(), s, "co_1", []byte("roles: : :"), 102); err == nil {
		t.Fatal("invalid yaml: expected parse error")
	}
	if got := c.RolesFor("co_1"); got == nil || len(got.Roles) != 0 {
		t.Fatalf("invalid yaml should store empty-Roles entry, got %+v", got)
	}
}

// Write-through ordering: a failed store write must leave the cache unchanged.
func TestOrgCache_SetCompanyRoles_WritesStoreThenCache(t *testing.T) {
	s := &fakeOrgStore{cos: []store.Company{{
		ID:              "co_1",
		PermissionsYAML: "roles:\n  - base\ngrants:\n  tool:use:todo:\n    base: on\n",
	}}}
	c := NewOrgCache()
	if err := c.Load(context.Background(), s); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := c.RolesFor("co_1").Roles["base"]; !ok {
		t.Fatalf("baseline: expected 'base' role")
	}

	s.setPermErr = errors.New("store down")
	body := []byte("roles:\n  - newrole\ngrants:\n  tool:use:todo:\n    newrole: on\n")
	if err := c.SetCompanyRolesFromYAML(context.Background(), s, "co_1", body, 1); err == nil {
		t.Fatal("expected store-write error to surface")
	}
	after := c.RolesFor("co_1")
	if _, ok := after.Roles["base"]; !ok {
		t.Fatalf("cache mutated on failed store write — got %+v", after)
	}
	if _, leaked := after.Roles["newrole"]; leaked {
		t.Fatalf("cache mutated on failed store write — got %+v", after)
	}
}

func TestOrgCache_SetInboxVisibilityWriteThrough(t *testing.T) {
	s := &fakeOrgStore{ibs: []store.Inbox{{ID: "ib_1", Scope: "x", Status: "active"}}}
	c := NewOrgCache()
	if err := c.Load(context.Background(), s); err != nil {
		t.Fatalf("Load: %v", err)
	}
	vis := &store.InboxVisibility{HiddenTools: []string{"read_file"}}
	if err := c.SetInboxVisibility(context.Background(), s, "ib_1", vis); err != nil {
		t.Fatalf("SetInboxVisibility: %v", err)
	}
	ib, ok := c.InboxByID("ib_1")
	if !ok || ib.Visibility == nil || len(ib.Visibility.HiddenTools) != 1 || ib.Visibility.HiddenTools[0] != "read_file" {
		t.Fatalf("cache not updated through SetInboxVisibility: %+v", ib.Visibility)
	}
}

// ---- Atomic swap ------------------------------------------------------------

func TestOrgCache_LoadAtomicSwap(t *testing.T) {
	c := NewOrgCache()
	storeA := &fakeOrgStore{cos: []store.Company{
		{ID: "co_1", PermissionsYAML: "roles:\n  - base\ngrants:\n  tool:use:todo:\n    base: on\n"},
	}}
	storeB := &fakeOrgStore{cos: []store.Company{
		{ID: "co_2", PermissionsYAML: "roles:\n  - intern\ngrants:\n  tool:use:todo:\n    intern: on\n"},
	}}
	for i := 0; i < 10; i++ {
		s := storeA
		want, other := "co_1", "co_2"
		if i%2 == 0 {
			s, want, other = storeB, "co_2", "co_1"
		}
		if err := c.Load(context.Background(), s); err != nil {
			t.Fatalf("Load iter %d: %v", i, err)
		}
		if c.RolesFor(want) == nil {
			t.Fatalf("iter %d: RolesFor(%s) nil after Load", i, want)
		}
		if c.RolesFor(other) != nil {
			t.Fatalf("iter %d: stale RolesFor(%s) — previous Load not fully cleared", i, other)
		}
	}
}

// ---- Upsert / Delete cache paths --------------------------------------------

func TestOrgCache_UpsertAndDelete(t *testing.T) {
	c := NewOrgCache()
	c.UpsertCompany(store.Company{ID: "co_1", Name: "Acme"})
	c.UpsertMember(store.Member{ID: "m_1", Name: "Ada"})
	if _, ok := c.CompanyByID("co_1"); !ok {
		t.Fatal("UpsertCompany not reflected")
	}
	if _, ok := c.MemberByID("m_1"); !ok {
		t.Fatal("UpsertMember not reflected")
	}
}

// setRoles helper (test-only path) round-trips.
func TestOrgCache_SetRolesHelper(t *testing.T) {
	c := NewOrgCache()
	c.setRoles("co_x", &RolesConfig{Roles: map[string]RoleDef{"r": {Grants: []string{"tool:use:now"}}}})
	if got := c.RolesFor("co_x"); got == nil || got.Roles["r"].Grants[0] != "tool:use:now" {
		t.Fatalf("setRoles round-trip failed: %+v", got)
	}
	c.setRoles("co_nil", nil)
	if got := c.RolesFor("co_nil"); got == nil || len(got.Roles) != 0 {
		t.Fatalf("setRoles(nil) should store empty-Roles: %+v", got)
	}
}

// TestArrangementBundleExpand: the coarse arrangement:role:creator / collaborator grants
// expand to the underlying tool grants (additive sugar), so one bundle line replaces the
// four separate tool grants. collaborator-only does NOT leak the creator grants.
func TestArrangementBundleExpand(t *testing.T) {
	s := &fakeOrgStore{cos: []store.Company{{ID: "co_x", PermissionsYAML: "" +
		"roles:\n  - master\n  - worker\n" +
		"grants:\n" +
		"  arrangement:role:creator:\n    master: on\n" +
		"  arrangement:role:collaborator:\n    master: on\n    worker: on\n"}}}
	c := NewOrgCache()
	if err := c.Load(context.Background(), s); err != nil {
		t.Fatalf("Load: %v", err)
	}
	rc := c.RolesFor("co_x")
	if rc == nil {
		t.Fatal("co_x missing")
	}
	has := func(role, grant string) bool {
		for _, g := range rc.Roles[role].Grants {
			if g == grant {
				return true
			}
		}
		return false
	}
	// master = creator + collaborator → all four underlying tool grants present.
	for _, g := range []string{"tool:use:arrangement_define", "tool:use:arrangement_inject", "tool:use:arrangement_pull", "tool:use:arrangement_submit"} {
		if !has("master", g) {
			t.Errorf("master missing expanded %s: %v", g, rc.Roles["master"].Grants)
		}
	}
	// worker = collaborator only → pull+submit, but NOT define/inject.
	if !has("worker", "tool:use:arrangement_pull") || !has("worker", "tool:use:arrangement_submit") {
		t.Errorf("worker missing collaborator grants: %v", rc.Roles["worker"].Grants)
	}
	if has("worker", "tool:use:arrangement_define") || has("worker", "tool:use:arrangement_inject") {
		t.Errorf("worker (collaborator-only) leaked creator grants: %v", rc.Roles["worker"].Grants)
	}
}

// TestRawArrangementGrantsRejected: the four raw arrangement tool grants are refused in
// permissions_yaml (use the bundles); the bundle itself parses fine.
func TestRawArrangementGrantsRejected(t *testing.T) {
	for _, raw := range []string{"tool:use:arrangement_define", "tool:use:arrangement_inject", "tool:use:arrangement_pull", "tool:use:arrangement_submit"} {
		y := "roles:\n  - r\ngrants:\n  " + raw + ":\n    r: on\n"
		if _, err := parseRolesYAML([]byte(y)); err == nil {
			t.Errorf("raw grant %q should be rejected (use the bundle)", raw)
		}
	}
	if _, err := parseRolesYAML([]byte("roles:\n  - r\ngrants:\n  arrangement:role:collaborator:\n    r: on\n")); err != nil {
		t.Errorf("bundle should be accepted: %v", err)
	}
}
