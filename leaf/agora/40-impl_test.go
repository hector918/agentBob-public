package agora

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/agora/store"
)

// ---- fakes (no DB) ----

// fakeSourceLoader is the narrow SourceLoader the InboxRouter needs, seeded with
// in-memory inbox_sources.
type fakeSourceLoader struct{ srcs []store.InboxSource }

func (f *fakeSourceLoader) ListInboxSources(_ context.Context, inboxID string) ([]store.InboxSource, error) {
	if inboxID == "" {
		return f.srcs, nil
	}
	var out []store.InboxSource
	for _, s := range f.srcs {
		if s.InboxID == inboxID {
			out = append(out, s)
		}
	}
	return out, nil
}

// fakeTool is a minimal contract.Tool for the catalog fake.
type fakeTool struct{ name string }

func (t fakeTool) Spec() contract.ToolSpec { return contract.ToolSpec{Name: t.name} }
func (t fakeTool) Serialize() bool         { return false }
func (t fakeTool) Run(context.Context, contract.ToolContext, json.RawMessage) contract.ToolResult {
	return contract.OKResult("")
}

// fakeToolCatalog implements contract.ToolCatalog over a fixed name set.
type fakeToolCatalog struct{ tools map[string]contract.Tool }

func newFakeToolCatalog(names ...string) *fakeToolCatalog {
	c := &fakeToolCatalog{tools: map[string]contract.Tool{}}
	for _, n := range names {
		c.tools[n] = fakeTool{name: n}
	}
	return c
}
func (c *fakeToolCatalog) Specs() []contract.ToolSpec {
	out := make([]contract.ToolSpec, 0, len(c.tools))
	for n := range c.tools {
		out = append(out, contract.ToolSpec{Name: n})
	}
	return out
}
func (c *fakeToolCatalog) Lookup(name string) (contract.Tool, bool) {
	t, ok := c.tools[name]
	return t, ok
}

// permsYAML builds a matrix permissions_yaml with one role granting the given
// capability strings (e.g. "tool:use:read_file").
func permsYAML(role string, grants ...string) []byte {
	var b strings.Builder
	b.WriteString("roles:\n  - " + role + "\ngrants:\n")
	for _, g := range grants {
		b.WriteString("  " + g + ":\n    " + role + ": on\n")
	}
	return []byte(b.String())
}

// seedOrg builds a two-company org graph used across the impl tests. Every inbox
// is MEMBER-OWNED (belongs to a member); a company inbox is named with the
// company's ID and scoped member:<name>/inbox:<companyID>:
//
//	Acme   (co_acme):   alice/boss employed → inbox ib_alice (scope member:alice/inbox:co_acme), source telegram chat 100
//	                     bob/worker  employed → inbox ib_bob   (scope member:bob/inbox:co_acme),   source telegram chat 200
//	                     bridge inbox ib_bridge (scope co:acme/bridge), ReportInboxID
//	Globex (co_globex): erin/agent  employed → inbox ib_erin  (scope member:erin/inbox:co_globex), source telegram chat 300
//
// No cross-company reachability (the channel mechanism was removed). Acme/boss
// grants tool:use:read_file + skill:use:research; Acme/worker grants none.
// Grants of a member-owned inbox = the intersection across the member's
// companies; alice/bob/erin each have a single company, so the intersection is
// just that company-role's grants.
func seedOrg(t *testing.T) (*OrgCache, *fakeSourceLoader) {
	t.Helper()
	org := NewOrgCache()

	org.UpsertCompany(store.Company{ID: "co_acme", Name: "Acme", Status: "active", ReportInboxID: "ib_bridge", Playbook: "Acme 运作说明：先问后做。"})
	org.UpsertCompany(store.Company{ID: "co_globex", Name: "Globex", Status: "active"})

	// Acme: boss grants read_file + research; worker declared but granted nothing.
	if err := org.SeedCompanyRolesFromYAML("co_acme", []byte(
		"roles:\n  - boss\n  - worker\ngrants:\n  tool:use:read_file:\n    boss: on\n  skill:use:research:\n    boss: on\n")); err != nil {
		t.Fatalf("seed acme roles: %v", err)
	}
	if err := org.SeedCompanyRolesFromYAML("co_globex", permsYAML("agent", "tool:use:web_search")); err != nil {
		t.Fatalf("seed globex roles: %v", err)
	}

	org.UpsertMember(store.Member{ID: "m_alice", Name: "alice"})
	org.UpsertMember(store.Member{ID: "m_bob", Name: "bob"})
	org.UpsertMember(store.Member{ID: "m_erin", Name: "erin"})

	org.UpsertEmployment(store.Employment{ID: "e_alice", MemberID: "m_alice", CompanyID: "co_acme", RoleName: "boss", Status: "active"})
	org.UpsertEmployment(store.Employment{ID: "e_bob", MemberID: "m_bob", CompanyID: "co_acme", RoleName: "worker", Status: "active"})
	org.UpsertEmployment(store.Employment{ID: "e_erin", MemberID: "m_erin", CompanyID: "co_globex", RoleName: "agent", Status: "active"})

	org.UpsertInbox(store.Inbox{ID: "ib_alice", Kind: "member", MemberID: "m_alice", Scope: MemberOwnedInboxScope("alice", "co_acme"), Name: "co_acme", Status: "active"})
	org.UpsertInbox(store.Inbox{ID: "ib_bob", Kind: "member", MemberID: "m_bob", Scope: MemberOwnedInboxScope("bob", "co_acme"), Name: "co_acme", Status: "active"})
	org.UpsertInbox(store.Inbox{ID: "ib_erin", Kind: "member", MemberID: "m_erin", Scope: MemberOwnedInboxScope("erin", "co_globex"), Name: "co_globex", Status: "active"})
	org.UpsertInbox(store.Inbox{ID: "ib_bridge", Kind: "bridge", OwnerCompanyID: "co_acme", Scope: "co:acme/bridge", Status: "active"})

	loader := &fakeSourceLoader{srcs: []store.InboxSource{
		// alice resolved via telegram:group:100 (TestInboxForScope / TurnContext).
		{ID: "s_alice", InboxID: "ib_alice", SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "100"},
		// bob resolved via BOTH telegram:dm:200 (TestInboxForScope) and
		// telegram:group:200 (TestTurnContextNoGrantsAndBridge) → two rows.
		{ID: "s_bob_dm", InboxID: "ib_bob", SourceID: "telegram", ChatType: contract.ChatDM, ChatID: "200"},
		{ID: "s_bob_grp", InboxID: "ib_bob", SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "200"},
		// erin only ever resolved via her own scope (co:globex/erin), not a chat.
		{ID: "s_erin", InboxID: "ib_erin", SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "300"},
	}}
	return org, loader
}

func newTestImpl(t *testing.T) *Impl {
	t.Helper()
	org, loader := seedOrg(t)
	router := NewInboxRouter(org, loader)
	if err := router.Reload(context.Background()); err != nil {
		t.Fatalf("router reload: %v", err)
	}
	return newImpl(org, router, nil)
}

func TestInboxForScope(t *testing.T) {
	a := newTestImpl(t)
	ctx := context.Background()

	// hit: telegram group chat 100 → ib_alice (group + dm forms both indexed).
	if id, ok := a.InboxForScope(ctx, "telegram:group:100"); !ok || id != "ib_alice" {
		t.Fatalf("group hit: got (%q,%v), want (ib_alice,true)", id, ok)
	}
	if id, ok := a.InboxForScope(ctx, "telegram:dm:200"); !ok || id != "ib_bob" {
		t.Fatalf("dm hit: got (%q,%v), want (ib_bob,true)", id, ok)
	}
	// internal/native path: the inbox's own scope resolves verbatim.
	if id, ok := a.InboxForScope(ctx, MemberOwnedInboxScope("erin", "co_globex")); !ok || id != "ib_erin" {
		t.Fatalf("own-scope hit: got (%q,%v), want (ib_erin,true)", id, ok)
	}
	// miss: no source / inbox.
	if id, ok := a.InboxForScope(ctx, "telegram:group:999"); ok {
		t.Fatalf("miss: got (%q,%v), want ok=false", id, ok)
	}
}

func TestTurnContextMemberPrincipalAndDirectory(t *testing.T) {
	a := newTestImpl(t)
	turn := a.TurnContext(context.Background(), "telegram:group:100") // alice/boss

	if turn.InboxID != "ib_alice" {
		t.Fatalf("inbox: got %q want ib_alice", turn.InboxID)
	}
	if turn.Principal != "member:alice" {
		t.Fatalf("principal: got %q want member:alice", turn.Principal)
	}
	// Directory: same-company colleague bob (cross-company erin is NOT reachable —
	// alice is employed only at Acme, so the union is just Acme).
	if !strings.Contains(turn.Directory, "你是 alice，在 Acme 任 boss") {
		t.Fatalf("directory self missing: %q", turn.Directory)
	}
	if !strings.Contains(turn.Directory, "bob（worker @ Acme）") {
		t.Fatalf("directory missing same-company bob: %q", turn.Directory)
	}
	if strings.Contains(turn.Directory, "erin") {
		t.Fatalf("directory should NOT contain cross-company erin: %q", turn.Directory)
	}
	if !strings.Contains(turn.Directory, "co:acme/bridge") {
		t.Fatalf("directory missing bridge scope: %q", turn.Directory)
	}
	// Playbook rendered from company.
	if turn.Playbook != "Acme 运作说明：先问后做。" {
		t.Fatalf("playbook: got %q", turn.Playbook)
	}
}

func TestTurnContextNoGrantsAndBridge(t *testing.T) {
	a := newTestImpl(t)
	ctx := context.Background()

	// worker (bob): principal is the MEMBER (identity no longer depends on grants —
	// grants are resolved separately as the member-company intersection, which is
	// empty for the worker role; see TestAuthorizeTools).
	bob := a.TurnContext(ctx, "telegram:group:200")
	if bob.Principal != "member:bob" {
		t.Fatalf("worker principal: got %q want member:bob", bob.Principal)
	}

	// bridge inbox (member-less) → no-grants sentinel; empty directory/playbook is
	// fine (bridge resolves via its own scope).
	bridge := a.TurnContext(ctx, "co:acme/bridge")
	if bridge.InboxID != "ib_bridge" {
		t.Fatalf("bridge inbox: got %q", bridge.InboxID)
	}
	if bridge.Principal != "agora:inbox:ib_bridge:no-grants" {
		t.Fatalf("bridge principal: got %q want no-grants sentinel", bridge.Principal)
	}

	// non-agora scope → empty turn.
	none := a.TurnContext(ctx, "telegram:group:999")
	if none.InboxID != "" || none.Principal != "" {
		t.Fatalf("non-agora turn not empty: %+v", none)
	}
}

// TestMemberProjection: agora SUPPLIES the projection (warrant judges it separately).
func TestMemberProjection(t *testing.T) {
	a := newTestImpl(t)
	ctx := context.Background()

	// alice/boss: grants tool:use:read_file only.
	p := a.MemberProjection(ctx, "ib_alice")
	if !p.Granted("tool:use:read_file") {
		t.Fatalf("granted read_file absent from projection")
	}
	if p.Granted("tool:use:web_search") {
		t.Fatalf("ungranted web_search present in projection")
	}

	// worker (bob): no grants → empty projection.
	if len(a.MemberProjection(ctx, "ib_bob")) != 0 {
		t.Fatalf("worker projection not empty")
	}
	// bridge inbox: no employment/role → empty (fail-closed).
	if len(a.MemberProjection(ctx, "ib_bridge")) != 0 {
		t.Fatalf("bridge projection not empty")
	}
	// unknown inbox → empty.
	if len(a.MemberProjection(ctx, "ib_none")) != 0 {
		t.Fatalf("unknown inbox projection not empty")
	}
}

func TestMemberProjectionVisibilityNarrows(t *testing.T) {
	org, loader := seedOrg(t)
	// Grant alice/boss read_file + terminal, then a visibility DENYLIST hiding read_file.
	if err := org.SeedCompanyRolesFromYAML("co_acme", []byte(
		"roles:\n  - boss\n  - worker\ngrants:\n  tool:use:read_file:\n    boss: on\n  tool:use:terminal:\n    boss: on\n")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	org.UpsertInbox(store.Inbox{
		ID: "ib_alice", Kind: "member", MemberID: "m_alice",
		Scope: MemberOwnedInboxScope("alice", "co_acme"), Name: "co_acme", Status: "active",
		Visibility: &store.InboxVisibility{HiddenTools: []string{"read_file"}},
	})
	router := NewInboxRouter(org, loader)
	if err := router.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	a := newImpl(org, router, nil)

	p := a.MemberProjection(context.Background(), "ib_alice")
	if !p.Granted("tool:use:terminal") {
		t.Fatalf("non-hidden terminal absent from projection")
	}
	if p.Granted("tool:use:read_file") {
		t.Fatalf("denylist-hidden read_file present in projection")
	}
}

func TestMemberProjectionSkills(t *testing.T) {
	a := newTestImpl(t)
	ctx := context.Background()
	p := a.MemberProjection(ctx, "ib_alice") // boss grants skill:use:research
	if !p.Granted("skill:use:research") {
		t.Fatalf("granted research absent from projection")
	}
	if p.Granted("skill:use:summarize") {
		t.Fatalf("ungranted summarize present in projection")
	}
}

// TestMemberProjectionPausedInboxFailsClosed is a regression for the
// review: a paused inbox must project NOTHING. grantsAndVisibility's active-inbox
// gate keeps the projection consistent with the router's ResolveByScope active-only
// addressability (no split-brain where a decommissioned inbox still grants).
func TestMemberProjectionPausedInboxFailsClosed(t *testing.T) {
	org := NewOrgCache()
	org.UpsertCompany(store.Company{ID: "co", Name: "Co", Status: "active"})
	if err := org.SeedCompanyRolesFromYAML("co", permsYAML("boss", "tool:use:read_file")); err != nil {
		t.Fatal(err)
	}
	org.UpsertMember(store.Member{ID: "m", Name: "m"})
	org.UpsertEmployment(store.Employment{ID: "e", MemberID: "m", CompanyID: "co", RoleName: "boss", Status: "active"})
	org.UpsertInbox(store.Inbox{ID: "ib", Kind: "member", MemberID: "m", Scope: "co/x", Name: "co", Status: "paused"})
	a := newImpl(org, NewInboxRouter(org, nil), nil)

	if p := a.MemberProjection(context.Background(), "ib"); len(p) != 0 {
		t.Fatalf("paused inbox projected %d grants, want 0 (fail-closed)", len(p))
	}
	if p := a.principalForInbox("ib"); !strings.Contains(p, "no-grants") {
		t.Fatalf("paused inbox principal = %q, want a no-grants sentinel", p)
	}
}

// TestResolveByScopeEmailForwardedAlias is a regression for the review:
// an email forwarded-alias DM carries a "#<alias>" sub-scope suffix (the session
// resolver appends ev.ThreadID); the inbox_source matches the base sender scope,
// so ResolveByScope must fall back past the suffix.
func TestResolveByScopeEmailForwardedAlias(t *testing.T) {
	org := NewOrgCache()
	org.UpsertInbox(store.Inbox{ID: "ib_mail", Kind: "member", MemberID: "m", Scope: "co/mail", Name: "co", Status: "active"})
	router := NewInboxRouter(org, &fakeSourceLoader{srcs: []store.InboxSource{
		{ID: "s", InboxID: "ib_mail", SourceID: "email", ChatType: contract.ChatDM, ChatID: "sender@x.com"},
	}})
	if err := router.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ib, ok := router.ResolveByScope("email:dm:sender@x.com"); !ok || ib.ID != "ib_mail" {
		t.Fatalf("base sender scope: got (%q,%v), want ib_mail", ib.ID, ok)
	}
	if ib, ok := router.ResolveByScope("email:dm:sender@x.com#team@x.com"); !ok || ib.ID != "ib_mail" {
		t.Fatalf("forwarded-alias scope: got (%q,%v), want ib_mail (suffix fallback)", ib.ID, ok)
	}
}

// TestPauseResumeCompany covers the in-memory pause set: Pause/Resume are
// idempotent and IsPausedCompany is the direct lookup.
func TestPauseResumeCompany(t *testing.T) {
	a := newTestImpl(t)

	if a.IsPausedCompany("co_acme") {
		t.Fatal("co_acme should start un-paused")
	}
	a.Pause("co_acme")
	a.Pause("co_acme") // idempotent
	if !a.IsPausedCompany("co_acme") {
		t.Fatal("co_acme should be paused after Pause")
	}
	if a.IsPausedCompany("co_globex") {
		t.Fatal("only co_acme paused; co_globex must be unaffected")
	}
	a.Resume("co_acme")
	a.Resume("co_acme") // idempotent
	if a.IsPausedCompany("co_acme") {
		t.Fatal("co_acme should be un-paused after Resume")
	}
	// nil-safe + empty-id no-ops.
	a.Pause("")
	if a.IsPausedCompany("") {
		t.Fatal("empty company id must never be considered paused")
	}
	var nilImpl *Impl
	nilImpl.Pause("x")  // must not panic
	nilImpl.Resume("x") // must not panic
	if nilImpl.IsPausedCompany("x") || nilImpl.IsPaused("any") {
		t.Fatal("nil impl must report not-paused")
	}
}

// TestIsPausedScopeMapping covers the contract IsPaused scope→company(ies)
// mapping: a member-inbox scope is paused iff one of the member's active companies
// (or the company-inbox name) is paused; a bridge scope maps to its owner company.
func TestIsPausedScopeMapping(t *testing.T) {
	a := newTestImpl(t)

	const aliceScope = "telegram:group:100" // member inbox ib_alice (member:alice @ co_acme)
	const bridgeScope = "co:acme/bridge"    // bridge inbox owned by co_acme

	if a.IsPaused(aliceScope) {
		t.Fatal("alice scope should not be paused initially")
	}
	// Pausing alice's company pauses her member-inbox scope (resolved via her
	// active employment AND the company-inbox name co_acme).
	a.Pause("co_acme")
	if !a.IsPaused(aliceScope) {
		t.Fatal("paused co_acme should make alice's member-inbox scope paused")
	}
	if !a.IsPaused(bridgeScope) {
		t.Fatal("paused co_acme should make its bridge scope paused")
	}
	// A different company's scope stays live.
	if a.IsPaused(MemberOwnedInboxScope("erin", "co_globex")) {
		t.Fatal("co_globex worker scope must stay live while only co_acme is paused")
	}
	a.Resume("co_acme")
	if a.IsPaused(aliceScope) || a.IsPaused(bridgeScope) {
		t.Fatal("resume must clear pause for both member + bridge scopes")
	}
	// Unknown / non-agora scope → false.
	if a.IsPaused("telegram:group:999") {
		t.Fatal("non-agora scope must never be paused")
	}
}

// TestRouteGroupScopeAndSubScope covers the virtual-group inbound picker (Phase 2):
// a group fanned to >1 member is addressed by #member / default; the picked member's
// sub-scope "<group>#<member>" resolves to that inbox, and the plain fanned scope does
// NOT resolve 1:1 (it needs member addressing).
func TestRouteGroupScopeAndSubScope(t *testing.T) {
	org := NewOrgCache()
	org.UpsertCompany(store.Company{ID: "co_acme", Name: "Acme", Status: "active"})
	org.UpsertMember(store.Member{ID: "m_alice", Name: "alice"})
	org.UpsertMember(store.Member{ID: "m_bob", Name: "bob"})
	org.UpsertEmployment(store.Employment{ID: "e_alice", MemberID: "m_alice", CompanyID: "co_acme", RoleName: "boss", Status: "active"})
	org.UpsertEmployment(store.Employment{ID: "e_bob", MemberID: "m_bob", CompanyID: "co_acme", RoleName: "worker", Status: "active"})
	org.UpsertInbox(store.Inbox{ID: "ib_alice", Kind: "member", MemberID: "m_alice", Scope: MemberOwnedInboxScope("alice", "co_acme"), Name: "co_acme", Status: "active"})
	org.UpsertInbox(store.Inbox{ID: "ib_bob", Kind: "member", MemberID: "m_bob", Scope: MemberOwnedInboxScope("bob", "co_acme"), Name: "co_acme", Status: "active"})
	loader := &fakeSourceLoader{srcs: []store.InboxSource{
		{ID: "s1", InboxID: "ib_alice", SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "777"},
		{ID: "s2", InboxID: "ib_bob", SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "777", IsDefault: true},
	}}
	router := NewInboxRouter(org, loader)
	if err := router.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	a := newImpl(org, router, nil)
	ctx := context.Background()
	grp := "telegram:group:777"
	ev := func(text string) contract.MessageEvent {
		return contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, ChatID: "777", Text: text}
	}

	if sub, _, ok := a.RouteGroupScope(ctx, ev("^alice 看下购物车")); !ok || sub != grp+"#alice" {
		t.Fatalf("^alice: got (%q,%v) want (%s#alice,true)", sub, ok, grp)
	}
	if sub, _, ok := a.RouteGroupScope(ctx, ev("随便问问")); !ok || sub != grp+"#bob" {
		t.Fatalf("default member: got (%q,%v) want (%s#bob,true)", sub, ok, grp)
	}
	if sub, members, ok := a.RouteGroupScope(ctx, ev("^charlie hi")); !ok || sub != "" || len(members) != 2 {
		t.Fatalf("^unknown → ask: got (%q,%v,%v) want ('',2 members,true)", sub, members, ok)
	}
	if id, ok := a.InboxForScope(ctx, grp); ok {
		t.Fatalf("fanned plain scope must NOT resolve 1:1: got (%q,%v)", id, ok)
	}
	if id, ok := a.InboxForScope(ctx, grp+"#alice"); !ok || id != "ib_alice" {
		t.Fatalf("sub-scope alice: got (%q,%v) want (ib_alice,true)", id, ok)
	}
	if id, ok := a.InboxForScope(ctx, grp+"#bob"); !ok || id != "ib_bob" {
		t.Fatalf("sub-scope bob: got (%q,%v) want (ib_bob,true)", id, ok)
	}
	if _, ok := a.InboxForScope(ctx, grp+"#charlie"); ok {
		t.Fatalf("sub-scope unknown member must be false")
	}
}

// TestRouteGroupScopeFiltersPausedInbox (F157): a paused member inbox is neither
// pickable (default / ^name) nor listed in the fan roster — selection applies the
// same addressability gate as ResolveByScope, so routing can never mint a
// sub-scope the resolver then refuses (which stranded every message in a fresh
// ask-who that STILL listed the paused member: a loop with no exit). Fan-ness
// stays keyed on the wiring: an ALL-paused fan still reports ok=true with an
// empty roster, so the flow bounces it instead of the normal catch-all answering.
func TestRouteGroupScopeFiltersPausedInbox(t *testing.T) {
	org := NewOrgCache()
	org.UpsertCompany(store.Company{ID: "co_acme", Name: "Acme", Status: "active"})
	org.UpsertMember(store.Member{ID: "m_alice", Name: "alice"})
	org.UpsertMember(store.Member{ID: "m_bob", Name: "bob"})
	org.UpsertInbox(store.Inbox{ID: "ib_alice", Kind: "member", MemberID: "m_alice", Scope: MemberOwnedInboxScope("alice", "co_acme"), Name: "co_acme", Status: "paused"})
	bobInbox := store.Inbox{ID: "ib_bob", Kind: "member", MemberID: "m_bob", Scope: MemberOwnedInboxScope("bob", "co_acme"), Name: "co_acme", Status: "active"}
	org.UpsertInbox(bobInbox)
	loader := &fakeSourceLoader{srcs: []store.InboxSource{
		// The PAUSED member is the group default — the sharpest trap pre-fix (every
		// unaddressed message routed to a sub-scope the resolver refuses).
		{ID: "s1", InboxID: "ib_alice", SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "777", IsDefault: true},
		{ID: "s2", InboxID: "ib_bob", SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "777"},
	}}
	router := NewInboxRouter(org, loader)
	if err := router.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	a := newImpl(org, router, nil)
	ctx := context.Background()
	grp := "telegram:group:777"
	ev := func(text string) contract.MessageEvent {
		return contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, ChatID: "777", Text: text}
	}

	// The paused DEFAULT is not picked: unaddressed → ask, roster lists only bob.
	if sub, members, ok := a.RouteGroupScope(ctx, ev("随便问问")); !ok || sub != "" || len(members) != 1 || members[0] != "bob" {
		t.Fatalf("paused default: got (%q,%v,%v) want ('',[bob],true)", sub, members, ok)
	}
	// ^alice (paused) does not route to the dead sub-scope → ask with the live roster.
	if sub, members, ok := a.RouteGroupScope(ctx, ev("^alice 看下")); !ok || sub != "" || len(members) != 1 || members[0] != "bob" {
		t.Fatalf("^paused member: got (%q,%v,%v) want ('',[bob],true)", sub, members, ok)
	}
	// ^bob (active) still routes.
	if sub, _, ok := a.RouteGroupScope(ctx, ev("^bob 看下")); !ok || sub != grp+"#bob" {
		t.Fatalf("^active member: got (%q,%v) want (%s#bob,true)", sub, ok, grp)
	}

	// ALL paused: fan-ness preserved (agora still claims — the flow bounces), empty roster.
	bobInbox.Status = "paused"
	org.UpsertInbox(bobInbox)
	if err := router.Reload(context.Background()); err != nil {
		t.Fatalf("reload after pause: %v", err)
	}
	if sub, members, ok := a.RouteGroupScope(ctx, ev("随便问问")); !ok || sub != "" || len(members) != 0 {
		t.Fatalf("all paused: got (%q,%v,%v) want ('',[],true)", sub, members, ok)
	}
}

func TestParseMemberTag(t *testing.T) {
	roster := []string{"alice", "bob", "carol"}
	cases := []struct {
		in, want string
		roster   []string
	}{
		{"^alice 看下", "alice", roster},
		{"hi ^bob, 你好", "bob", roster},
		{"no token here", "", roster},
		{"trailing ^carol.", "carol", roster},
		{"#promo 不是成员", "", roster}, // a real hashtag must NOT be read as a member
		// F159: CJK content glued to the tag with NO space must still resolve by prefix.
		{"^alice帮我查下库存", "alice", roster},
		{"^bob，来活了", "bob", roster},
		// an unknown "^" token returns the raw token → the caller lists the roster + asks.
		{"^zack 来", "zack", roster},
		// longest prefix wins when one name prefixes another.
		{"^alicia说", "alicia", []string{"ali", "alicia"}},
		{"^ali说", "ali", []string{"ali", "alicia"}},
		// an ASCII-glued token with no clean boundary + no exact member → raw (asks).
		{"^alicexyz", "alicexyz", roster},
	}
	for _, c := range cases {
		if got := parseMemberTag(c.in, c.roster); got != c.want {
			t.Errorf("parseMemberTag(%q, %v) = %q, want %q", c.in, c.roster, got, c.want)
		}
	}
}

// TestRouteGroupScope_TopicChatType pins the fix for the bb→bob flow-flip: a forum-TOPIC
// message — now a ChatGroup event carrying a ThreadID (topics fold to ChatGroup at the
// source boundary; ScopeFor flattens the thread out of "<src>:group:<id>") — must route
// through the same agora member path as a plain group message, not bypass it.
func TestRouteGroupScope_TopicChatType(t *testing.T) {
	org := NewOrgCache()
	org.UpsertCompany(store.Company{ID: "co_acme", Name: "Acme", Status: "active"})
	org.UpsertMember(store.Member{ID: "m_alice", Name: "alice"})
	org.UpsertMember(store.Member{ID: "m_bob", Name: "bob"})
	org.UpsertInbox(store.Inbox{ID: "ib_alice", Kind: "member", MemberID: "m_alice", Scope: MemberOwnedInboxScope("alice", "co_acme"), Name: "co_acme", Status: "active"})
	org.UpsertInbox(store.Inbox{ID: "ib_bob", Kind: "member", MemberID: "m_bob", Scope: MemberOwnedInboxScope("bob", "co_acme"), Name: "co_acme", Status: "active"})
	loader := &fakeSourceLoader{srcs: []store.InboxSource{
		{ID: "s1", InboxID: "ib_alice", SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "777"},
		{ID: "s2", InboxID: "ib_bob", SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "777", ThreadID: "15"},
	}}
	router := NewInboxRouter(org, loader)
	if err := router.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	a := newImpl(org, router, nil)

	// a forum-topic event (ChatGroup + ThreadID, #alice) must still resolve to the member sub-scope.
	topicEv := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, ChatID: "777", ThreadID: "15", Text: "^alice 看下"}
	if sub, _, ok := a.RouteGroupScope(context.Background(), topicEv); !ok || sub != "telegram:group:777#alice" {
		t.Fatalf("topic ^alice: got (%q,%v) want (telegram:group:777#alice,true)", sub, ok)
	}
	if !contract.ChatGroup.IsGroupChat() || contract.ChatDM.IsGroupChat() {
		t.Fatalf("IsGroupChat: group must be true, dm false")
	}
}

// TestLiveCapsArrangementBundles: the permissions matrix hides the four raw arrangement
// tool caps and exposes the two role bundles instead (granted via bundle, not individually).
func TestLiveCapsArrangementBundles(t *testing.T) {
	tc := newFakeToolCatalog("read_file", "arrangement_define", "arrangement_inject", "arrangement_pull", "arrangement_submit")
	m := &Module{toolCat: func() contract.ToolCatalog { return tc }}
	caps, _ := m.liveCaps()
	if !caps["tool:use:read_file"] {
		t.Error("normal tool cap missing from matrix")
	}
	for _, raw := range []string{"tool:use:arrangement_define", "tool:use:arrangement_inject", "tool:use:arrangement_pull", "tool:use:arrangement_submit"} {
		if caps[raw] {
			t.Errorf("raw arrangement cap %q should be hidden from the matrix", raw)
		}
	}
	if !caps["arrangement:role:creator"] || !caps["arrangement:role:collaborator"] {
		t.Errorf("bundle caps missing from matrix: %v", caps)
	}
}
