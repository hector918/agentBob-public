package agora

import (
	"testing"

	"agentbob/leaf/agora/store"
)

// orphanMemberIDs is the pure delete-policy: a member is orphaned by deleting
// companyID only when ALL their active employments were at companyID (none
// elsewhere). Multi-company members survive; the result is sorted.
func TestOrphanMemberIDs(t *testing.T) {
	// alice: only at co1 → orphan. bob: co1 + co2 → survives. carol: only co2 →
	// untouched. (terminated rows are never passed in — caller uses activeOnly.)
	active := []store.Employment{
		{MemberID: "alice", CompanyID: "co1", Status: "active"},
		{MemberID: "bob", CompanyID: "co1", Status: "active"},
		{MemberID: "bob", CompanyID: "co2", Status: "active"},
		{MemberID: "carol", CompanyID: "co2", Status: "active"},
	}
	got := orphanMemberIDs(active, "co1")
	if len(got) != 1 || got[0] != "alice" {
		t.Fatalf("orphanMemberIDs(co1) = %v, want [alice]", got)
	}
	// Deleting co2: bob still at co1 → survives; carol only at co2 → orphan.
	if got := orphanMemberIDs(active, "co2"); len(got) != 1 || got[0] != "carol" {
		t.Fatalf("orphanMemberIDs(co2) = %v, want [carol]", got)
	}
	// A company nobody is employed at → no orphans.
	if got := orphanMemberIDs(active, "co3"); len(got) != 0 {
		t.Fatalf("orphanMemberIDs(co3) = %v, want []", got)
	}
}

// ValidateRoleInCompany via the seeded OrgCache (no DB): a known role passes, an
// unknown role is rejected.
func TestValidateRoleInCompany(t *testing.T) {
	org := NewOrgCache()
	co := store.Company{ID: "co1", Name: "Acme"}
	org.UpsertCompany(co)
	if err := org.SeedCompanyRolesFromYAML("co1", permsYAML("boss", "tool:use:now")); err != nil {
		t.Fatalf("seed roles: %v", err)
	}

	if err := ValidateRoleInCompany(org, co, "boss"); err != nil {
		t.Fatalf("ValidateRoleInCompany(boss) = %v, want nil", err)
	}
	if err := ValidateRoleInCompany(org, co, "nope"); err == nil {
		t.Fatal("ValidateRoleInCompany(nope) = nil, want unknown-role error")
	}
}

// ValidateRoleInCompany falls back to parsing the company's PermissionsYAML when
// the OrgCache is absent (cold/cli path).
func TestValidateRoleInCompany_NoCacheFallback(t *testing.T) {
	co := store.Company{ID: "co1", Name: "Acme", PermissionsYAML: string(permsYAML("boss", "tool:use:now"))}
	if err := ValidateRoleInCompany(nil, co, "boss"); err != nil {
		t.Fatalf("fallback ValidateRoleInCompany(boss) = %v, want nil", err)
	}
	if err := ValidateRoleInCompany(nil, co, "ghost"); err == nil {
		t.Fatal("fallback ValidateRoleInCompany(ghost) = nil, want error")
	}
}

// Scope-string construction is deterministic and matches the bridge/member
// grammar Bootstrap + Hire rely on (UNIQUE(scope) collision avoidance). A
// company inbox is member-owned and named with the company's ID.
func TestScopeStrings(t *testing.T) {
	if got := bridgeScope("Acme"); got != "company:Acme/bridge:main" {
		t.Fatalf("bridgeScope = %q", got)
	}
	if got := MemberOwnedInboxScope("alice", "co_acme"); got != "member:alice/inbox:co_acme" {
		t.Fatalf("MemberOwnedInboxScope = %q", got)
	}
}

// FirstRoleNameInYAML picks the first role in source order (matrix sequence
// form), used by Bootstrap to choose the founder's position.
func TestFirstRoleNameInYAML(t *testing.T) {
	body := []byte("roles:\n  - boss\n  - member\ngrants: {}\n")
	got, err := FirstRoleNameInYAML(body)
	if err != nil || got != "boss" {
		t.Fatalf("FirstRoleNameInYAML = %q, %v; want boss,nil", got, err)
	}
	if got, _ := FirstRoleNameInYAML(nil); got != "" {
		t.Fatalf("FirstRoleNameInYAML(empty) = %q, want \"\"", got)
	}
}

// The default seed template parses cleanly + declares the founder role used by
// Bootstrap, so a freshly-created company is never roleless.
func TestSeededPermissionsParse(t *testing.T) {
	cfg, err := parseRolesYAML([]byte(SeededPermissionsYAML))
	if err != nil {
		t.Fatalf("parse SeededPermissionsYAML: %v", err)
	}
	if len(cfg.Roles) == 0 {
		t.Fatal("SeededPermissionsYAML has no roles")
	}
	first, err := FirstRoleNameInYAML([]byte(SeededPermissionsYAML))
	if err != nil || first == "" {
		t.Fatalf("FirstRoleNameInYAML(seed) = %q, %v", first, err)
	}
	if _, ok := cfg.Roles[first]; !ok {
		t.Fatalf("first role %q not in parsed roles", first)
	}
}
