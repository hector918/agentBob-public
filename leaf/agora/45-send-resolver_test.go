package agora

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/agora/store"
)

// seedMultiCompanyImpl extends the base seed so alice is ALSO employed at Globex
// (with her own Globex member inbox). alice is then a multi-company worker whose
// directory/reachability must UNION Acme + Globex; bob (Acme-only) and erin
// (Globex-only) stay single-company. Used to exercise the union behaviour.
func seedMultiCompanyImpl(t *testing.T) *Impl {
	t.Helper()
	org, loader := seedOrg(t)
	// Globex needs a bridge + playbook so the union surfaces a second bridge +
	// second playbook.
	org.UpsertCompany(store.Company{ID: "co_globex", Name: "Globex", Status: "active", ReportInboxID: "ib_gbridge", Playbook: "Globex 运作说明：快速迭代。"})
	org.UpsertInbox(store.Inbox{ID: "ib_gbridge", Kind: "bridge", OwnerCompanyID: "co_globex", Scope: "co:globex/bridge", Status: "active"})
	// Bind the Globex bridge to a real chat so it is DELIVERABLE — CheckSend now rejects
	// a bridge with no bound external chat (an un-wired bridge silently drops sends), so a
	// reachable bridge must also be wired to be sendable.
	loader.srcs = append(loader.srcs, store.InboxSource{ID: "s_gbridge", InboxID: "ib_gbridge", SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "910"})
	// alice also works at Globex as a contractor, with her own Globex inbox.
	org.UpsertEmployment(store.Employment{ID: "e_alice_gbx", MemberID: "m_alice", CompanyID: "co_globex", RoleName: "contractor", Status: "active"})
	org.UpsertInbox(store.Inbox{ID: "ib_alice_gbx", Kind: "member", MemberID: "m_alice", Scope: MemberOwnedInboxScope("alice", "co_globex"), Name: "co_globex", Status: "active"})

	router := NewInboxRouter(org, loader)
	if err := router.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return newImpl(org, router, nil)
}

// These reuse seedOrg / newTestImpl from 40-impl_test.go (same package). The org
// graph: Acme {alice/boss, bob/worker, bridge}, Globex {erin/agent}. Every
// company inbox is member-owned, scoped member:<name>/inbox:<companyID>. alice
// resolves via telegram:group:100; bob via telegram:dm:200; erin via
// telegram:group:300 + her own scope. There is no cross-company path.

func TestSendResolveTargetByName(t *testing.T) {
	a := newTestImpl(t)
	ctx := context.Background()

	// alice (caller telegram:group:100) → "bob" resolves to bob's same-company inbox.
	scope, _, err := a.ResolveTarget(ctx, "telegram:group:100", "bob")
	if err != nil {
		t.Fatalf("resolve bob: %v", err)
	}
	if want := MemberOwnedInboxScope("bob", "co_acme"); scope != want {
		t.Fatalf("resolve bob: got %q want %q", scope, want)
	}

	// An explicit scope passes through verbatim.
	erinScope := MemberOwnedInboxScope("erin", "co_globex")
	scope, _, err = a.ResolveTarget(ctx, "telegram:group:100", erinScope)
	if err != nil {
		t.Fatalf("resolve explicit scope: %v", err)
	}
	if scope != erinScope {
		t.Fatalf("explicit scope: got %q want %q", scope, erinScope)
	}

	// erin is cross-company (Globex) — not an implicit name target from Acme.
	if _, _, err := a.ResolveTarget(ctx, "telegram:group:100", "erin"); err == nil {
		t.Fatal("cross-company name should not implicitly resolve")
	}

	// no such member.
	if _, _, err := a.ResolveTarget(ctx, "telegram:group:100", "nobody"); err == nil {
		t.Fatal("unknown member should error")
	}
}

// TestSendResolveTargetAmbiguousName: a member named the same in TWO of the caller's
// companies must NOT silently resolve to the first-by-company-name — ResolveTarget
// returns an ambiguity error listing the candidate inbox scopes so the worker
// disambiguates with to=<inbox-scope> (L-AG-D2).
func TestSendResolveTargetAmbiguousName(t *testing.T) {
	a := seedMultiCompanyImpl(t) // alice employed at Acme + Globex
	ctx := context.Background()

	// dave works at BOTH Acme and Globex, each with his own member inbox — so a bare
	// "dave" from alice (who reaches both companies) is ambiguous.
	a.org.UpsertMember(store.Member{ID: "m_dave", Name: "dave"})
	a.org.UpsertEmployment(store.Employment{ID: "e_dave_acme", MemberID: "m_dave", CompanyID: "co_acme", RoleName: "worker", Status: "active"})
	a.org.UpsertInbox(store.Inbox{ID: "ib_dave_acme", Kind: "member", MemberID: "m_dave", Scope: MemberOwnedInboxScope("dave", "co_acme"), Name: "co_acme", Status: "active"})
	a.org.UpsertEmployment(store.Employment{ID: "e_dave_gbx", MemberID: "m_dave", CompanyID: "co_globex", RoleName: "contractor", Status: "active"})
	a.org.UpsertInbox(store.Inbox{ID: "ib_dave_gbx", Kind: "member", MemberID: "m_dave", Scope: MemberOwnedInboxScope("dave", "co_globex"), Name: "co_globex", Status: "active"})

	_, _, err := a.ResolveTarget(ctx, "telegram:group:100", "dave")
	if err == nil {
		t.Fatal("ambiguous same-name colleague in two companies must NOT silently resolve")
	}
	// the error must name BOTH candidate scopes and steer to an explicit inbox scope.
	for _, want := range []string{MemberOwnedInboxScope("dave", "co_acme"), MemberOwnedInboxScope("dave", "co_globex"), "to=<inbox-scope>"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ambiguity error %q missing %q", err.Error(), want)
		}
	}

	// Control: a name present in only ONE of the caller's companies still resolves cleanly.
	scope, _, err := a.ResolveTarget(ctx, "telegram:group:100", "bob") // bob is Acme-only
	if err != nil {
		t.Fatalf("unambiguous resolve should still work: %v", err)
	}
	if want := MemberOwnedInboxScope("bob", "co_acme"); scope != want {
		t.Fatalf("resolve bob: got %q want %q", scope, want)
	}
}

func TestCheckSend(t *testing.T) {
	a := newTestImpl(t)
	ctx := context.Background()

	bobScope := MemberOwnedInboxScope("bob", "co_acme")
	erinScope := MemberOwnedInboxScope("erin", "co_globex")
	aliceScope := MemberOwnedInboxScope("alice", "co_acme")

	// same-company: alice → bob's scope allowed; deliver = the resolved inbox's own scope (F87).
	if deliver, deny, err := a.CheckSend(ctx, "telegram:group:100", bobScope); err != nil || deny != "" || deliver != bobScope {
		t.Fatalf("same-company allow: deliver=%q deny=%q err=%v (want deliver=%s)", deliver, deny, err, bobScope)
	}

	// cross-company is never reachable: alice → erin denied.
	if _, deny, _ := a.CheckSend(ctx, "telegram:group:100", erinScope); deny == "" {
		t.Fatal("cross-company should be denied")
	}
	// likewise bob (worker) → erin denied.
	if _, deny, _ := a.CheckSend(ctx, "telegram:dm:200", erinScope); deny == "" {
		t.Fatal("cross-company should be denied")
	}

	// self-send: alice → alice's own scope denied.
	if _, deny, _ := a.CheckSend(ctx, "telegram:group:100", aliceScope); deny == "" {
		t.Fatal("self-send should be denied")
	}
}

func TestSendResolveCredentialName(t *testing.T) {
	a := newTestImpl(t)
	ctx := context.Background()

	// Empty asName → passthrough.
	if name, err := a.ResolveCredentialName(ctx, "telegram:group:100", ""); err != nil || name != "" {
		t.Fatalf("empty passthrough: name=%q err=%v", name, err)
	}

	// alice/boss in the default seed grants tool:use:read_file + skill:use:research —
	// no credential grant → an `as=` request is denied.
	if _, err := a.ResolveCredentialName(ctx, "telegram:group:100", "botoken"); err == nil {
		t.Fatal("ungranted credential should be denied")
	}

	// Re-seed Acme/boss WITH credential:use:botoken → now it resolves.
	org, loader := seedOrg(t)
	if err := org.SeedCompanyRolesFromYAML("co_acme", permsYAML("boss", "credential:use:botoken")); err != nil {
		t.Fatalf("seed cred grant: %v", err)
	}
	router := NewInboxRouter(org, loader)
	if err := router.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	a2 := newImpl(org, router, nil)
	if name, err := a2.ResolveCredentialName(ctx, "telegram:group:100", "botoken"); err != nil || name != "botoken" {
		t.Fatalf("granted credential: name=%q err=%v", name, err)
	}
}

// TestUnionDirectory: a multi-company member (alice in Acme + Globex) sees
// colleagues, bridges and playbooks from BOTH companies in one directory.
func TestUnionDirectory(t *testing.T) {
	a := seedMultiCompanyImpl(t)
	dir, ok := a.buildDirectory(context.Background(), "ib_alice")
	if !ok {
		t.Fatal("buildDirectory declined for multi-company member")
	}

	// Self spans both companies (name-sorted: Acme before Globex).
	if dir.Self.Name != "alice" || dir.Self.Bridge {
		t.Fatalf("self: %+v", dir.Self)
	}
	if len(dir.Self.Memberships) != 2 ||
		dir.Self.Memberships[0].Company != "Acme" || dir.Self.Memberships[0].Role != "boss" ||
		dir.Self.Memberships[1].Company != "Globex" || dir.Self.Memberships[1].Role != "contractor" {
		t.Fatalf("memberships: %+v", dir.Self.Memberships)
	}

	// Contacts UNION: bob (Acme) + erin (Globex); alice (self) excluded everywhere.
	gotContacts := map[string]string{} // name -> company
	for _, c := range dir.Contacts {
		gotContacts[c.Name] = c.Company
	}
	if gotContacts["bob"] != "Acme" {
		t.Fatalf("missing bob @ Acme: %+v", dir.Contacts)
	}
	if gotContacts["erin"] != "Globex" {
		t.Fatalf("missing erin @ Globex (union failed): %+v", dir.Contacts)
	}
	if _, self := gotContacts["alice"]; self {
		t.Fatalf("self alice must not be a contact: %+v", dir.Contacts)
	}
	// Deterministic sort by (Company, Name): Acme/bob before Globex/erin.
	if dir.Contacts[0].Name != "bob" || dir.Contacts[len(dir.Contacts)-1].Name != "erin" {
		t.Fatalf("contacts not (company,name)-sorted: %+v", dir.Contacts)
	}

	// Both bridges reachable.
	bridges := strings.Join(dir.BridgeScopes, ",")
	if !strings.Contains(bridges, "co:acme/bridge") || !strings.Contains(bridges, "co:globex/bridge") {
		t.Fatalf("union bridges missing: %v", dir.BridgeScopes)
	}

	// Both playbooks present, combined with per-company headers.
	combined := combinePlaybooks(dir.Playbooks)
	if !strings.Contains(combined, "## Acme 运作说明") || !strings.Contains(combined, "## Globex 运作说明") {
		t.Fatalf("combined playbook missing a company section: %q", combined)
	}
}

// TestUnionCheckSendAndResolve: alice (multi-company) can now send to erin (Globex)
// AND to bob (Acme), and resolve either by name across her companies.
func TestUnionCheckSendAndResolve(t *testing.T) {
	a := seedMultiCompanyImpl(t)
	ctx := context.Background()

	bobScope := MemberOwnedInboxScope("bob", "co_acme")
	erinScope := MemberOwnedInboxScope("erin", "co_globex")

	// Both companies' colleagues reachable now (erin was cross-company before).
	if _, deny, err := a.CheckSend(ctx, "telegram:group:100", bobScope); err != nil || deny != "" {
		t.Fatalf("send to bob (Acme): deny=%q err=%v", deny, err)
	}
	if _, deny, err := a.CheckSend(ctx, "telegram:group:100", erinScope); err != nil || deny != "" {
		t.Fatalf("send to erin (Globex union): deny=%q err=%v", deny, err)
	}

	// Both bridges reachable.
	if _, deny, _ := a.CheckSend(ctx, "telegram:group:100", "co:globex/bridge"); deny != "" {
		t.Fatalf("send to Globex bridge denied: %q", deny)
	}

	// Resolve by name across companies.
	if scope, _, err := a.ResolveTarget(ctx, "telegram:group:100", "erin"); err != nil || scope != erinScope {
		t.Fatalf("resolve erin across companies: scope=%q err=%v", scope, err)
	}
	if scope, _, err := a.ResolveTarget(ctx, "telegram:group:100", "bob"); err != nil || scope != bobScope {
		t.Fatalf("resolve bob: scope=%q err=%v", scope, err)
	}
	// Truly unknown name still errors.
	if _, _, err := a.ResolveTarget(ctx, "telegram:group:100", "nobody"); err == nil {
		t.Fatal("unknown member should still error")
	}
}
