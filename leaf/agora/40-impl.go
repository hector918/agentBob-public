package agora

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"agentbob/contract"
)

// Impl implements contract.Agora over the in-process org mirror (OrgCache) + the
// InboxRouter. For an agora turn it SUPPLIES the authorization projection
// (MemberProjection / RoleProjection from the company's per-role permissions_yaml
// grants) — it does NOT judge; warrant is the single judge over the GrantSet
// (docs §14). An inbox with no live member/role projects empty (fail-closed).
type Impl struct {
	org    *OrgCache
	router *InboxRouter
	// learn is the role-guidance learner (80-learn.go); nil when no writable home.
	// TurnContext reads it to inject a member's accumulated role guidance.
	learn *memberLearn

	// paused is the in-memory set of paused companies (companyID → true). It is
	// NOT persisted — a bob restart clears every pause (acceptable per design).
	// Guarded by pausedMu. While a company is paused, the agora flow skips its
	// worker turns (the message link "stops") rather than rerouting to the normal
	// flow (which is what `disable` does).
	pausedMu sync.Mutex
	paused   map[string]bool
}

// newImpl builds the agora impl over the cache + router. agora no longer touches the
// tool/skill catalogs (warrant judges the projection — docs §14); the Module keeps its
// own catalog thunks for the slash/panel views.
func newImpl(org *OrgCache, router *InboxRouter, learn *memberLearn) *Impl {
	return &Impl{org: org, router: router, learn: learn, paused: map[string]bool{}}
}

// Pause flags a company as paused (in-memory; idempotent). While paused the agora
// flow skips that company's worker turns.
func (a *Impl) Pause(companyID string) {
	if a == nil || companyID == "" {
		return
	}
	a.pausedMu.Lock()
	defer a.pausedMu.Unlock()
	if a.paused == nil {
		a.paused = map[string]bool{}
	}
	a.paused[companyID] = true
}

// Resume clears a company's pause flag (in-memory; idempotent).
func (a *Impl) Resume(companyID string) {
	if a == nil || companyID == "" {
		return
	}
	a.pausedMu.Lock()
	defer a.pausedMu.Unlock()
	delete(a.paused, companyID)
}

// IsPausedCompany is the direct paused-set lookup keyed by companyID — used by the
// slash idempotency check + the webui panel toggle (not the scope→company mapping
// that the contract IsPaused does).
func (a *Impl) IsPausedCompany(companyID string) bool {
	if a == nil || companyID == "" {
		return false
	}
	a.pausedMu.Lock()
	defer a.pausedMu.Unlock()
	return a.paused[companyID]
}

// isAnyPaused reports whether any of the given company ids is paused (one lock).
func (a *Impl) isAnyPaused(companyIDs ...string) bool {
	if a == nil {
		return false
	}
	a.pausedMu.Lock()
	defer a.pausedMu.Unlock()
	for _, id := range companyIDs {
		if id != "" && a.paused[id] {
			return true
		}
	}
	return false
}

// IsPaused (contract.Agora) reports whether the company a scope belongs to is
// paused, so the agora flow skips the turn. It resolves the scope to an inbox and
// returns true if ANY of the inbox's company contexts is paused:
//   - a bridge inbox (OwnerCompanyID set) → that company.
//   - a member inbox → every active employment's company, PLUS the inbox's Name if
//     it is a known company id (a company inbox is named with the company's id).
//
// nil-safe throughout; a scope that does not resolve → false.
// PausedMemberScope (contract.Agora, F161) delegates to the router — true iff the scope
// maps to a paused member inbox whose owner company is still active.
func (a *Impl) PausedMemberScope(_ context.Context, scope string) bool {
	if a == nil || a.router == nil {
		return false
	}
	return a.router.PausedMemberScope(scope)
}

func (a *Impl) IsPaused(scope string) bool {
	if a == nil || a.router == nil {
		return false
	}
	ib, ok := a.router.ResolveByScope(scope)
	if !ok {
		return false
	}
	if ib.OwnerCompanyID != "" { // bridge inbox: a single owning company
		return a.isAnyPaused(ib.OwnerCompanyID)
	}
	// member inbox: union of the member's active companies + a company-inbox name.
	cos := make([]string, 0, 4)
	if a.org != nil && ib.MemberID != "" {
		for _, e := range a.org.ActiveEmploymentsByMember(ib.MemberID) {
			cos = append(cos, e.CompanyID)
		}
	}
	if a.org != nil && ib.Name != "" {
		if _, ok := a.org.CompanyByID(ib.Name); ok { // company inbox is named with the company id
			cos = append(cos, ib.Name)
		}
	}
	return a.isAnyPaused(cos...)
}

// InboxForScope reports the agora inbox a chat scope routes to, if any
// (ok=false for a non-agora scope). Pure in-process read via the router.
func (a *Impl) InboxForScope(_ context.Context, scope string) (string, bool) {
	if a == nil || a.router == nil {
		return "", false
	}
	ib, ok := a.router.ResolveByScope(scope)
	if !ok {
		return "", false
	}
	return ib.ID, true
}

// ScopeIsAgora reports whether scope is bound to ANY agora inbox — the onboarding
// "is this scope agora" predicate (contract.Agora). Union of two cases InboxForScope
// alone would miss: the 1:1 chat/native path (single inbox incl. a bridge) via
// ResolveByScope, AND a virtual-group fan (>1 member) via the group's member bindings
// — the fan is deliberately left unresolved by ResolveByScope, so a busy multi-member
// group would be a false negative without the groupMembers check. The member set is
// keyed by the BASE group scope, so strip any "#member" sub-scope first.
func (a *Impl) ScopeIsAgora(_ context.Context, scope string) bool {
	if a == nil || a.router == nil || scope == "" {
		return false
	}
	if _, ok := a.router.ResolveByScope(scope); ok {
		return true
	}
	base := scope
	if i := strings.IndexByte(scope, '#'); i >= 0 {
		base = scope[:i]
	}
	return len(a.router.groupMembersOf(base)) > 0
}

// CompanyBridgeScope returns the company's bridge-inbox scope (its talk-to-human line),
// or ("",false) when the company has no bridge / is unknown. Pure in-process read over
// the org mirror: company → ReportInboxID → that inbox's scope ("co:<name>/bridge").
func (a *Impl) CompanyBridgeScope(ctx context.Context, company string) (string, bool) {
	if a == nil || a.org == nil || company == "" {
		return "", false
	}
	coID := a.companyIDByIDOrName(ctx, company) // arrangement passes the company NAME, not id
	if coID == "" {
		return "", false
	}
	co, ok := a.org.CompanyByID(coID)
	if !ok || co.ReportInboxID == "" {
		return "", false
	}
	ib, ok := a.org.InboxByID(co.ReportInboxID)
	if !ok || ib.Scope == "" {
		return "", false
	}
	return ib.Scope, true
}

// RouteGroupScope picks the member sub-scope for a GROUP chat event bound to a
// virtual-group fan (>1 member inbox wired to the chat). It is the inbound member
// selector behind "@bot ^成员 起头 / 回复续聊"; reply-routing is handled upstream by the
// session message-index, so this covers the non-reply path. Returns:
//   - (subScope, members, true): a member was picked (a ^name token, else the group's
//     default member) → route the session to subScope "<group-scope>#<member>".
//   - ("", members, true): it IS a fanned group but no member was picked (no ^name match,
//     no default) → the caller asks the human "找谁" (members lists the choices; it may be
//     EMPTY when every wired member is currently unaddressable — the caller bounces then).
//   - ("", nil, false): NOT a fanned group (0/1 member, or not a group) → route normally.
func (a *Impl) RouteGroupScope(_ context.Context, ev contract.MessageEvent) (subScope string, members []string, ok bool) {
	if a == nil || a.router == nil || !ev.ChatType.IsGroupChat() {
		return "", nil, false
	}
	base := ev.RoutingScope()
	gms := a.router.groupMembersOf(base)
	if len(gms) <= 1 {
		return "", nil, false // single-member group routes 1:1 — no member addressing needed
	}
	// Selection + roster consider only ADDRESSABLE members (active inbox, owner company
	// not disabled — the exact gate ResolveByScope applies): picking a paused member
	// would mint a sub-scope the resolver then refuses, stranding every message in a
	// fresh ask-who whose roster still lists that member (a loop with no way out).
	// Fan-ness itself stays keyed on the WIRING (gms): an all-paused fan is still
	// agora's to claim (the flow bounces it), never the normal catch-all's to answer.
	live := make([]groupMember, 0, len(gms))
	for _, gm := range gms {
		if a.router.inboxAddressable(gm.InboxID) {
			live = append(live, gm)
		}
	}
	members = make([]string, 0, len(live))
	for _, gm := range live {
		members = append(members, gm.Name)
	}
	if want := parseMemberTag(ev.Text, members); want != "" {
		for _, gm := range live {
			if strings.EqualFold(gm.Name, want) {
				return base + "#" + gm.Name, members, true
			}
		}
		return "", members, true // ^unknown or unaddressable name → ask (list the live members)
	}
	for _, gm := range live {
		if gm.IsDefault {
			return base + "#" + gm.Name, members, true
		}
	}
	return "", members, true // no ^name, no live default → ask "找谁"
}

// memberTagPrefix is bob's dedicated member-ROUTING marker in group text. "^" (not "#"):
// it is reserved for routing and unique to bob, so it never collides with a real hashtag
// or a forum topic the way "#" would (a stray "#促销" must not be read as a member).
const memberTagPrefix = "^"

// memberTagTrailingPunct is the trailing punctuation trimmed off a "^name" addressing
// token. KEEP IN SYNC — BYTE-IDENTICAL — with flow/agora's copy (stripLeadingMemberTag,
// flow/agora/10-flow.go): routing (here) and stripping (there) must trim the same set,
// or a token that routes still leaks into the member's prompt (or the reverse: a token
// strips but never routed). arch forbids the cross import (TestModuleImportBoundary),
// so the literal is mirrored; flow/agora's TestMemberTagPunctInSync locks the two equal.
const memberTagTrailingPunct = ",.;:!?()，。；：！？、）"

// parseMemberTag extracts the first "^<name>" routing token from group text (the member to
// route to), trimming trailing punctuation. "" when none. Names are matched
// case-insensitively against the group's wired members by the caller.
// parseMemberTag finds a "^name" member-routing token in text and resolves it against the
// live roster by PREFIX match (F159): CJK users glue the tag to the content with no space
// ("^alice帮我查下"), so a whole-token equality never fires. The LONGEST roster name that
// prefixes the token (after "^"), with a name-BOUNDARY right after it (a non-[A-Za-z0-9_-]
// byte or end — CJK content, a space, punctuation), wins. Returns that member name; or the
// raw token when a "^" appears but matches no member (the caller then asks who). Member
// names are ASCII (entityNameRe), so the boundary check is byte-wise.
func parseMemberTag(text string, roster []string) string {
	for _, f := range strings.Fields(text) {
		if !strings.HasPrefix(f, memberTagPrefix) {
			continue
		}
		tok := strings.TrimRight(strings.TrimPrefix(f, memberTagPrefix), memberTagTrailingPunct)
		if tok == "" {
			continue
		}
		best := ""
		for _, name := range roster {
			if len(name) <= len(best) || len(tok) < len(name) {
				continue // already have a longer match, or the name can't fit
			}
			if !strings.EqualFold(tok[:len(name)], name) {
				continue
			}
			if len(tok) > len(name) && isNameByte(tok[len(name)]) {
				continue // the next byte extends an ASCII name — not a clean boundary
			}
			best = name
		}
		if best != "" {
			return best
		}
		return tok // a "^" token that matched no member → caller lists the roster and asks
	}
	return ""
}

// isNameByte reports whether b is a valid member-name byte (entityNameRe = [A-Za-z0-9_-]).
// Used as the "^name" boundary test so "^alice帮我查" matches "alice" but "^alicexyz"
// (no roster member "alicexyz") does not silently match "alice".
func isNameByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
}

// InboxScopes returns every chat scope that routes to inboxID (its own scope + wired
// source scopes) — the inverse of InboxForScope, for the webui chat-log reader. Pure
// in-process read via the router.
func (a *Impl) InboxScopes(_ context.Context, inboxID string) []string {
	if a == nil || a.router == nil {
		return nil
	}
	return a.router.ScopesForInbox(inboxID)
}

// TurnContext assembles the per-turn agora context for a scope: the resolved
// inbox, the minted principal, and the pre-rendered directory + playbook prompt
// text. It collapses skeleton's three resolvers (40/45/95) into one in-memory
// read. A scope that does not resolve to a live member inbox yields a no-grants
// principal so a hub identity can never leak into an agora turn.
func (a *Impl) TurnContext(ctx context.Context, scope string) contract.AgoraTurn {
	if a == nil || a.router == nil {
		return contract.AgoraTurn{}
	}
	ib, ok := a.router.ResolveByScope(scope)
	if !ok {
		return contract.AgoraTurn{} // non-agora scope: empty turn (flow keeps hub identity)
	}
	turn := contract.AgoraTurn{InboxID: ib.ID, MemberID: ib.MemberID}
	turn.Principal = a.principalForInbox(ib.ID)
	turn.Identity = a.renderIdentity(ib.MemberID)
	turn.Directory, turn.Playbook = a.renderDirectory(ctx, ib.ID)
	// Inject the member's accumulated role guidance (learned from past failures,
	// docs/wip-member-learn.md) alongside the company playbook — same platform layer.
	// Uses ib.MemberID directly (already resolved above) — no redundant scope re-resolve.
	if a.learn != nil {
		if g := a.learn.guidanceForMember(ib.MemberID); g != "" {
			turn.Playbook = strings.TrimSpace(turn.Playbook + "\n\n【岗位经验（自动积累）】\n" + g)
		}
	}
	for _, s := range a.memberSpaces(ib.ID) {
		turn.Spaces = append(turn.Spaces, s.Name)
	}
	return turn
}

// renderIdentity builds the member IDENTITY card the agora flow injects into the identity
// prompt layer (replacing the default bob persona): who the agent IS this turn — member name,
// each active employment's company name + role, then the member's PromptStyle persona (when
// set). NO internal IDs (member/company): they are opaque keys of zero value to a user and the
// model could echo them — names + role convey the identity fully (prompt hygiene, docs/prompt.md
// §5). "" when there is no live member (the flow keeps the default identity).
func (a *Impl) renderIdentity(memberID string) string {
	if a == nil || a.org == nil || memberID == "" {
		return ""
	}
	mb, ok := a.org.MemberByID(memberID)
	if !ok || mb.Name == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "你是 %s。", mb.Name)
	if emps := a.org.ActiveEmploymentsByMember(memberID); len(emps) > 0 {
		b.WriteString("\n你的任职：")
		for _, e := range emps {
			// company name only — never the opaque company ID, even on a cache miss.
			if co, ok := a.org.CompanyByID(e.CompanyID); ok && co.Name != "" {
				fmt.Fprintf(&b, "\n- %s · 角色：%s", co.Name, e.RoleName)
			} else {
				fmt.Fprintf(&b, "\n- 角色：%s", e.RoleName)
			}
		}
	}
	if s := strings.TrimSpace(mb.PromptStyle); s != "" {
		b.WriteString("\n\n" + s)
	}
	return b.String()
}

// spaceOption is one reachable file space for an agora turn: the raw scope Name the
// tool passes to warrant + a human Label (the company name) shown to the model.
type spaceOption struct {
	Name  string
	Label string
}

// memberSpaces returns the company file spaces the inbox's member may reach — one
// per company it is actively employed by (deduped: two roles at one company → one
// space). Empty when the inbox has no live member or no companies. This is the
// "member in N companies → N spaces" set, the single source for both the hard gate
// (flow → BoundChannels.Allowed) and the model-facing tool hint.
func (a *Impl) memberSpaces(inboxID string) []spaceOption {
	if a == nil || a.org == nil {
		return nil
	}
	ib, ok := a.org.InboxByID(inboxID)
	if !ok || ib.Status != "active" || ib.MemberID == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []spaceOption
	for _, e := range a.org.ActiveEmploymentsByMember(ib.MemberID) {
		if seen[e.CompanyID] {
			continue
		}
		seen[e.CompanyID] = true
		if co, ok := a.org.CompanyByID(e.CompanyID); ok {
			out = append(out, spaceOption{Name: "company:" + co.ID, Label: co.Name})
		}
	}
	return out
}

// principalForInbox mints the warrant principal for an inbox turn (trunk's
// single-string principal model, docs §2.1). Every inbox belongs to a MEMBER,
// so the identity is the member itself; its grants are the
// intersection of the member's companies (resolved in grantsAndVisibility), so
// the principal and the authorized grants read the same OrgCache rows and can't
// disagree between router Reloads ("one read per turn"):
//   - live active inbox WITH a known member → "member:<memberName>".
//   - otherwise a no-grants sentinel ("agora:inbox:<id>:no-grants"), so a hub
//     admin/user identity can NEVER leak into an agora turn (a bridge inbox, with
//     no member, also falls here).
func (a *Impl) principalForInbox(inboxID string) string {
	sentinel := "agora:inbox:" + inboxID + ":no-grants"
	if a == nil || a.org == nil {
		return sentinel
	}
	ib, ok := a.org.InboxByID(inboxID)
	if !ok || ib.Status != "active" || ib.MemberID == "" {
		return sentinel
	}
	mb, ok := a.org.MemberByID(ib.MemberID)
	if !ok || mb.Name == "" {
		return sentinel
	}
	return "member:" + mb.Name
}

// memberOwnedGrants is the grant set of a member-owned inbox: the INTERSECTION of
// every active employment's company-role grants (net-subtractive — a capability
// survives only if EVERY company the member is in grants it; no companies → empty).
func (a *Impl) memberOwnedGrants(memberID string) map[string]struct{} {
	emps := a.org.ActiveEmploymentsByMember(memberID)
	acc := map[string]struct{}{}
	for i, e := range emps {
		cur := map[string]struct{}{}
		if cfg := a.org.RolesFor(e.CompanyID); cfg != nil {
			if def, ok := cfg.Roles[e.RoleName]; ok {
				for _, g := range def.Grants {
					cur[g] = struct{}{}
				}
			}
		}
		if i == 0 {
			acc = cur
			continue
		}
		for g := range acc { // keep only grants present in this company too
			if _, ok := cur[g]; !ok {
				delete(acc, g)
			}
		}
	}
	return acc
}

// MemberProjection collapses the inbox member's authorization into ONE GrantSet —
// the cross-company INTERSECTION of its (company, role) grants (memberOwnedGrants),
// handed to warrant (the single JUDGE). agora SUPPLIES, it does not judge. Bundles
// already expanded at config load. No live member/role → empty (fail-closed).
// Visibility is a DENYLIST (per-inbox) that narrows the projection: a hidden
// tool:use/skill:use cap is dropped; credentials are never hidden. Empty denylist →
// no narrowing (the common case). docs §14.
func (a *Impl) MemberProjection(_ context.Context, inboxID string) contract.GrantSet {
	if a == nil {
		return contract.GrantSet{}
	}
	grants, vis, ok := a.grantsAndVisibility(inboxID)
	if !ok {
		return contract.GrantSet{} // no live employment/role → fail-closed
	}
	if len(vis.HiddenTools) == 0 && len(vis.HiddenSkills) == 0 {
		return contract.GrantSet(grants) // nothing hidden → no narrowing
	}
	out := contract.GrantSet{}
	for capStr := range grants {
		if visiblePass(capStr, vis) {
			out[capStr] = struct{}{}
		}
	}
	return out
}

// visiblePass reports whether a capability survives the inbox DENYLIST: a tool:use: /
// skill:use: name listed in HiddenTools / HiddenSkills is hidden (dropped); everything
// else (incl. credentials) passes. So visibility only ever NARROWS, never expands.
func visiblePass(capStr string, vis visInfo) bool {
	if name, ok := strings.CutPrefix(capStr, "tool:use:"); ok {
		return !slices.Contains(vis.HiddenTools, name)
	}
	if name, ok := strings.CutPrefix(capStr, "skill:use:"); ok {
		return !slices.Contains(vis.HiddenSkills, name)
	}
	return true
}

// DecorateToolSpecs returns specs with each space-taking tool's description appended
// with the member's reachable company spaces — the model can't pick a space it isn't
// shown. This is DECORATION, not judgment (the old AuthorizeTools bundled it): agora
// enriches the specs, warrant judges the result against MemberProjection. The hard
// space gate stays BoundChannels.Allowed, fed from the same memberSpaces set. specs
// are COPIED so the shared catalog isn't mutated.
func (a *Impl) DecorateToolSpecs(_ context.Context, inboxID string, specs []contract.ToolSpec) []contract.ToolSpec {
	if a == nil {
		return specs
	}
	hint := formatSpaceHint(a.memberSpaces(inboxID))
	if hint == "" {
		return specs
	}
	out := make([]contract.ToolSpec, len(specs))
	copy(out, specs)
	for i := range out {
		if specHasSpaceParam(out[i]) {
			out[i].Description += hint
		}
	}
	return out
}

// specHasSpaceParam reports whether a tool spec exposes a top-level "space" param.
func specHasSpaceParam(spec contract.ToolSpec) bool {
	var p struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(spec.Parameters, &p) != nil {
		return false
	}
	_, ok := p.Properties["space"]
	return ok
}

// formatSpaceHint renders the model-facing list of reachable company spaces for a
// space-taking tool's description. "" when the member has no companies (then the
// tool keeps its default-space-only behavior).
func formatSpaceHint(opts []spaceOption) string {
	if len(opts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(opts))
	for _, o := range opts {
		parts = append(parts, o.Name+"（"+o.Label+"）")
	}
	return "\n可用 space（留空=你的个人空间；一次只填一个；括号内是公司名，仅供识别、不要填入）：" + strings.Join(parts, "、")
}

// visInfo holds the resolved inbox visibility DENYLIST in a kind-agnostic shape: the
// names to HIDE per kind. Empty/nil → nothing hidden (no narrowing).
type visInfo struct {
	HiddenTools  []string
	HiddenSkills []string
}

// grantsAndVisibility resolves an inbox → its grant set (a lookup set of literal
// "kind:use:name" strings) + the inbox's visibility filter, all from the
// OrgCache. Every inbox belongs to a MEMBER; its grants are
// the INTERSECTION of the member's current companies' role-grants
// (memberOwnedGrants). ok=false (authorize nothing) when the OrgCache is absent,
// the inbox is unknown / NOT active, or it has no member (a bridge inbox →
// fail-closed). The active-inbox gate matches the router's ResolveByScope
// active-only addressability (no split-brain where a paused/archived inbox can't
// mint a turn yet still authorizes tools).
func (a *Impl) grantsAndVisibility(inboxID string) (map[string]struct{}, visInfo, bool) {
	if a == nil || a.org == nil {
		return nil, visInfo{}, false
	}
	ib, ok := a.org.InboxByID(inboxID)
	if !ok || ib.Status != "active" || ib.MemberID == "" {
		return nil, visInfo{}, false // unknown / inactive / bridge (no member) — fail-closed
	}
	grants := a.memberOwnedGrants(ib.MemberID)
	vi := visInfo{}
	if ib.Visibility != nil {
		vi.HiddenTools = ib.Visibility.HiddenTools
		vi.HiddenSkills = ib.Visibility.HiddenSkills
	}
	return grants, vi, true
}

// companyForInbox resolves the owner Company id of an inbox via the OrgCache: a
// bridge inbox uses OwnerCompanyID; a company inbox (member-owned, named with the
// company's ID — see hireAndProvision) resolves by that name. "" for a
// member-created inbox (Name is not a company ID) or an unknown inbox.
func companyForInbox(org *OrgCache, inboxID string) string {
	if org == nil || inboxID == "" {
		return ""
	}
	ib, ok := org.InboxByID(inboxID)
	if !ok {
		return ""
	}
	if ib.OwnerCompanyID != "" {
		return ib.OwnerCompanyID
	}
	// company inbox is named with the company's ID (survives renames).
	if _, ok := org.CompanyByID(ib.Name); ok {
		return ib.Name
	}
	return ""
}

var _ contract.Agora = (*Impl)(nil)
