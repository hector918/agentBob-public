// Package agora — operation layer: the data-creation command surface shared by
// the /agora slash adapter (70-slash.go) and any future cli. Ported from
// skeleton's operations_*.go + 86/87/88/89, adapted to the trunk store
// (leaf/agora/store, *PG methods take nowUnix; not-found = contract.ErrNoRows).
//
// Each operation does: validate → store-write → OrgCache write-through →
// router.Reload (only for changes that affect inbox routing). Error policy
// (skeleton's):
//   - validation errors: plain errors (fmt.Errorf)
//   - store errors: propagate as-is (callers can errors.Is them)
//   - cache / router-refresh errors: logged at WARN, never returned — the side
//     effect already committed; a stale cache resyncs on the next /agora reload.
//
// Idempotency: the bootstrap upserts get-by-natural-key first (resolving
// contract.ErrNoRows → create). Trunk has no unique-violation sentinel, so a
// genuine concurrent create races to a raw pg error — acceptable: bootstrap is
// admin-driven single-flight.
package agora

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/agora/store"
)

// entityNameRe constrains an agora entity name (company / member / inbox /
// founder) to ASCII letters, digits, '-' and '_'. This is load-bearing for the
// send-target resolver: a name can NEVER contain a scope delimiter (':' '/' '#'),
// so looksLikeSessionScope (45-send-resolver.go) can distinguish a member name
// from a raw session scope unambiguously — a name like "x:inbox:y" is rejected at
// creation rather than mis-resolved at send time. (audit decision: root-
// cause the name-vs-scope ambiguity here instead of an ordering heuristic.)
var entityNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// nameOK validates an entity name: required, single-token (no whitespace, so the
// flag form --company=<name> is unambiguous) and free of scope delimiters — see
// entityNameRe.
func nameOK(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("agora: name required")
	}
	if !entityNameRe.MatchString(name) {
		return fmt.Errorf("agora: name 只能用 ASCII 字母/数字/'-'/'_'（无空格、无 : / # 等特殊字符）")
	}
	return nil
}

// OpDeps bundles the collaborators every agora operation needs.
//
//   - Store is the agora pg store (the single backing *store.PG).
//   - Org is the in-memory mirror; write-through keeps it fresh without a Load.
//   - RouterReload refreshes the inbox router after a routing-affecting change
//     (inbox_source add/remove, inbox status flip). nil → skipped.
//   - RunningAtScope reports how many turns are in flight at a session scope (the
//     SessionManager probe DeleteCompany consults to refuse a delete that would
//     yank a live turn's inbox). nil → the in-flight guard can't verify, so
//     DeleteCompany fails closed (refuses). Other ops don't use it.
//   - Home is the $BOB_HOME root (reserved for disk-side callers; unused by the
//     in-process trunk path which seeds from the SeededPermissionsYAML constant).
type OpDeps struct {
	Store          *store.PG
	Org            *OrgCache
	RouterReload   func(ctx context.Context) error
	RunningAtScope func(scope string) int
	Home           string
}

// reloadRouter fires the optional router reload best-effort, logging WARN on
// failure (stale routing for one inbox event until the next reload).
func (d OpDeps) reloadRouter(ctx context.Context) {
	if d.RouterReload == nil {
		return
	}
	if err := d.RouterReload(ctx); err != nil {
		slog.Warn("agora: router reload failed (stale routing until next /agora reload)", "err", err)
	}
}

// ---------- Company ----------

// CreateCompany seeds a new Company (permissions_yaml + playbook from the
// default templates) and provisions its bridge inbox + ReportInboxID wiring.
// Cache write-through (company row + parsed roles + bridge inbox). No router
// reload — a bridge inbox has no inbox_source until an operator binds one.
//
// Returns the created Company (with ReportInboxID wired).
func CreateCompany(ctx context.Context, deps OpDeps, name string) (store.Company, error) {
	if err := nameOK(name); err != nil {
		return store.Company{}, err
	}
	if _, err := deps.Store.GetCompanyByName(ctx, name); err == nil {
		return store.Company{}, fmt.Errorf("agora: company %q already exists", name)
	} else if !errors.Is(err, contract.ErrNoRows) {
		return store.Company{}, err
	}
	now := clock.UnixEpoch()
	co, err := deps.Store.CreateCompany(ctx, store.Company{
		Name:            name,
		PermissionsYAML: SeededPermissionsYAML,
		Playbook:        SeededCompanyPlaybook,
	}, now)
	if err != nil {
		return store.Company{}, err
	}
	if deps.Org != nil {
		deps.Org.UpsertCompany(co)
		// "company exists" ↔ "cache roles entry exists" invariant (empty/bad →
		// fail-closed empty entry, never nil).
		_ = deps.Org.SeedCompanyRolesFromYAML(co.ID, []byte(SeededPermissionsYAML))
	}
	if updated, _, berr := provisionBridge(ctx, deps, co, now); berr != nil {
		// Best-effort like the cache step: the company row is usable; admin can
		// re-create (idempotent) to converge the bridge.
		slog.Warn("agora CreateCompany: bridge provisioning failed — company has no human-facing bus until re-create",
			"company", co.ID, "err", berr)
	} else {
		co = updated
	}
	return co, nil
}

// provisionBridge upserts the Company's bridge inbox (by scope) and wires
// Company.ReportInboxID to it. Idempotent: a company that already has its bridge
// (founder Bootstrap) finds the existing inbox and only re-asserts the wiring.
// Returns the updated company + the bridge inbox (Bootstrap reports both).
func provisionBridge(ctx context.Context, deps OpDeps, co store.Company, now int64) (store.Company, store.Inbox, error) {
	bridge, _, err := upsertInbox(ctx, deps.Store, store.Inbox{
		Kind: "bridge", OwnerCompanyID: co.ID, Scope: bridgeScope(co.Name), Name: "bridge",
	}, now)
	if err != nil {
		return co, store.Inbox{}, fmt.Errorf("upsert bridge inbox: %w", err)
	}
	if deps.Org != nil {
		deps.Org.UpsertInbox(bridge)
	}
	if err := deps.Store.SetReportInbox(ctx, co.ID, bridge.ID, now); err != nil {
		return co, bridge, fmt.Errorf("wire report_inbox_id: %w", err)
	}
	co.ReportInboxID = bridge.ID
	if deps.Org != nil {
		deps.Org.UpsertCompany(co)
	}
	return co, bridge, nil
}

// ListCompanies is a pure read-through.
func ListCompanies(ctx context.Context, deps OpDeps) ([]store.Company, error) {
	return deps.Store.ListCompanies(ctx)
}

// DisableCompany flips a Company's status to "disabled" (drops new inbound; lets
// outbound + in-flight continue). Cache write-through + router reload.
func DisableCompany(ctx context.Context, deps OpDeps, companyID string) error {
	return setCompanyStatus(ctx, deps, companyID, "disabled")
}

// EnableCompany is the inverse of DisableCompany.
func EnableCompany(ctx context.Context, deps OpDeps, companyID string) error {
	return setCompanyStatus(ctx, deps, companyID, "active")
}

// DeleteCompany HARD-deletes a Company and cleanly cuts its org structure. Unlike
// DisableCompany (a reversible kill switch), this is irreversible — it is gated by
// three preconditions and a transactional cascade:
//
//  1. The company MUST already be disabled. Disable drops new inbound, so in-flight
//     work can't grow between the in-flight check below and the delete (closes the
//     TOCTOU window) — the admin's deliberate two-step.
//  2. No in-flight turn on any inbox that will be deleted (the company's bridge
//     inboxes + the orphan members' inboxes). A running turn → refuse, so a delete
//     never yanks an inbox out from under a live turn. RunningAtScope nil (no
//     SessionManager) → can't verify → fail closed.
//  3. The cut itself (store.DeleteCompanyCascade, one tx): the company row cascades
//     its employments + bridge inboxes + their sources + permissions_yaml; ORPHAN
//     members (active here and at NO other company — terminated history elsewhere
//     does not count) are deleted too, cascading their member inboxes + sources.
//     Members still employed elsewhere — and their inboxes — survive (durable
//     cross-company identities). This is the org-structure boundary: sessions /
//     messages live outside agora (no FK) and roll out via session retention once
//     their inbox scope no longer resolves; DeleteCompany does not touch them.
//
// Full OrgCache reload + router reload after (many entities removed at once — a
// targeted write-through would be more code than a reload for a rare admin op).
func DeleteCompany(ctx context.Context, deps OpDeps, companyID string) error {
	co, err := deps.Store.GetCompany(ctx, companyID)
	if err != nil {
		if errors.Is(err, contract.ErrNoRows) {
			return fmt.Errorf("company %s not found", companyID)
		}
		return err
	}
	if co.Status != "disabled" {
		return fmt.Errorf("company %s 必须先 disable 才能删除（先跑 /agora company disable %s）", co.Name, companyID)
	}
	// Fail closed before any work: no SessionManager → can't verify in-flight.
	if deps.RunningAtScope == nil {
		return fmt.Errorf("agora: 无法校验 in-flight（session 管理器不可用），拒绝删除 %s", co.Name)
	}

	// Orphan members: active here, no active employment at any OTHER company.
	active, err := deps.Store.ListEmployments(ctx, true)
	if err != nil {
		return err
	}
	orphans := orphanMemberIDs(active, companyID)

	// In-flight guard over exactly the inboxes that will be deleted.
	scopes, err := deletionScopes(ctx, deps.Store, companyID, orphans)
	if err != nil {
		return err
	}
	busy := 0
	for _, sc := range scopes {
		busy += deps.RunningAtScope(sc)
	}
	if busy > 0 {
		return fmt.Errorf("company %s 有 %d 个在跑的 turn，删不了 —— 等它们跑完再删", co.Name, busy)
	}

	if err := deps.Store.DeleteCompanyCascade(ctx, companyID, orphans); err != nil {
		return err
	}

	// Company + employments + bridge/orphan inboxes + sources + orphan members are
	// all gone — full reload is the cheapest correct cache fix here. Load swaps the
	// entity maps but never touches writeMu, so drop this company's per-company
	// write-through mutex explicitly or its slot leaks until restart.
	if deps.Org != nil {
		deps.Org.DropCompanyWriteMu(companyID)
		if rerr := deps.Org.Load(ctx, deps.Store); rerr != nil {
			slog.Warn("agora DeleteCompany: org cache reload failed (snapshot stale until /agora reload)", "company", companyID, "err", rerr)
		}
	}
	deps.reloadRouter(ctx)
	return nil
}

// orphanMemberIDs returns the members whose ONLY active employment is at companyID
// (no active employment at any other company) — exactly the members hard-deleted
// when companyID is deleted. Pure policy over the active-employment set; the caller
// passes ListEmployments(activeOnly=true), so terminated history never counts. The
// result is sorted for determinism.
func orphanMemberIDs(activeEmployments []store.Employment, companyID string) []string {
	here := map[string]bool{}
	elsewhere := map[string]bool{}
	for _, e := range activeEmployments {
		if e.CompanyID == companyID {
			here[e.MemberID] = true
		} else {
			elsewhere[e.MemberID] = true
		}
	}
	out := make([]string, 0, len(here))
	for mid := range here {
		if !elsewhere[mid] {
			out = append(out, mid)
		}
	}
	sort.Strings(out)
	return out
}

// deletionScopes lists the session scopes a company-delete could yank a live turn
// from — for every inbox that will be removed (the company's bridge inboxes + each
// orphan member's inboxes):
//   - the inbox's own native scope (internal send_message dispatch runs there), and
//   - every wired chat scope (inbox_sources → contract.ScopeFor — the same grammar
//     the router keys on; a chat-driven turn runs at the CHAT scope, not the inbox's
//     native scope), and
//   - for a member inbox wired into a group, the fan sub-scope "<scope>#<member>"
//     a virtual-group session runs at.
//
// The in-flight guard probes exactly these (not surviving members' inboxes).
// Deduped so the busy count doesn't double-count (forum topics flatten to one scope).
func deletionScopes(ctx context.Context, s *store.PG, companyID string, orphanIDs []string) ([]string, error) {
	var scopes []string
	seen := map[string]bool{}
	add := func(sc string) {
		if sc != "" && !seen[sc] {
			seen[sc] = true
			scopes = append(scopes, sc)
		}
	}
	addInbox := func(ib store.Inbox, memberName string) error {
		add(ib.Scope)
		srcs, err := s.ListInboxSources(ctx, ib.ID)
		if err != nil {
			return err
		}
		for _, src := range srcs {
			sc := contract.ScopeFor(src.SourceID, src.ChatType, src.ChatID, src.ThreadID)
			add(sc)
			if memberName != "" && src.ChatType.IsGroupChat() {
				add(sc + "#" + memberName) // virtual-group fan sub-scope
			}
		}
		return nil
	}

	bridges, err := s.ListInboxesByCompany(ctx, companyID)
	if err != nil {
		return nil, err
	}
	for _, ib := range bridges {
		if err := addInbox(ib, ""); err != nil {
			return nil, err
		}
	}
	for _, mid := range orphanIDs {
		mb, err := s.GetMember(ctx, mid)
		if err != nil {
			return nil, err
		}
		ibs, err := s.ListInboxesByMember(ctx, mid)
		if err != nil {
			return nil, err
		}
		for _, ib := range ibs {
			if err := addInbox(ib, mb.Name); err != nil {
				return nil, err
			}
		}
	}
	return scopes, nil
}

func setCompanyStatus(ctx context.Context, deps OpDeps, companyID, status string) error {
	if err := deps.Store.UpdateCompanyStatus(ctx, companyID, status, clock.UnixEpoch()); err != nil {
		return err
	}
	if deps.Org != nil {
		if updated, gerr := deps.Store.GetCompany(ctx, companyID); gerr == nil {
			deps.Org.UpsertCompany(updated)
		} else {
			slog.Warn("agora setCompanyStatus: cache read-back failed (snapshot stale until /agora reload)", "company", companyID, "err", gerr)
		}
	}
	deps.reloadRouter(ctx)
	return nil
}

// ---------- Hire ----------

// Hire validates the role exists in the company's roles + rejects an existing
// active employment, then upserts the member, hires the employment, and
// provisions the member's main inbox. Returns (member, employment, inbox).
// Mirrors skeleton 86-hire.go HireAndProvision (rehire reuses the inbox row).
func Hire(ctx context.Context, deps OpDeps, companyName, memberName, role string) (store.Member, store.Employment, store.Inbox, error) {
	now := clock.UnixEpoch()
	co, err := deps.Store.GetCompanyByName(ctx, companyName)
	if err != nil {
		if errors.Is(err, contract.ErrNoRows) {
			return store.Member{}, store.Employment{}, store.Inbox{}, fmt.Errorf("company %q not found", companyName)
		}
		return store.Member{}, store.Employment{}, store.Inbox{}, err
	}
	if err := ValidateRoleInCompany(deps.Org, co, role); err != nil {
		return store.Member{}, store.Employment{}, store.Inbox{}, err
	}
	if co.ReportInboxID == "" {
		return store.Member{}, store.Employment{}, store.Inbox{}, fmt.Errorf(
			"company %s has no bridge inbox — create the company first so the admin escalation root exists", co.Name)
	}

	member, _, err := upsertMember(ctx, deps.Store, memberName, now)
	if err != nil {
		return store.Member{}, store.Employment{}, store.Inbox{}, err
	}
	if deps.Org != nil {
		deps.Org.UpsertMember(member)
	}

	// Reject a duplicate active employment (single inbox scope per (Co, Member);
	// a second active employment would orphan the first on the rehire rebind).
	if _, has, eerr := ExistingActiveEmployment(ctx, deps.Store, member.ID, co.ID); eerr != nil {
		return member, store.Employment{}, store.Inbox{}, eerr
	} else if has {
		return member, store.Employment{}, store.Inbox{}, fmt.Errorf(
			"member %q already has an active employment at %s — terminate it before re-hiring", memberName, co.Name)
	}

	emp, inbox, err := hireAndProvision(ctx, deps.Store, member, co, role, now)
	if err != nil {
		return member, store.Employment{}, store.Inbox{}, err
	}
	if deps.Org != nil {
		deps.Org.UpsertEmployment(emp)
		deps.Org.UpsertInbox(inbox)
	}
	deps.reloadRouter(ctx)
	return member, emp, inbox, nil
}

// hireAndProvision creates the Employment + creates-or-reuses the member's
// company inbox. The company inbox belongs to the MEMBER (its MemberID) and is named
// with the company's ID (co.ID) so it survives company renames; it is never
// reclaimed/archived on leave. Rehire path: the scope-matched inbox is reused
// (re-activated if not already active). On a provisioning failure it compensates
// by deleting the orphaned Employment (restoring atomicity).
func hireAndProvision(ctx context.Context, s *store.PG, member store.Member, co store.Company, role string, now int64) (store.Employment, store.Inbox, error) {
	emp, err := s.Hire(ctx, member.ID, co.ID, role, now)
	if err != nil {
		return store.Employment{}, store.Inbox{}, fmt.Errorf("hire employment: %w", err)
	}
	inboxName := co.ID // company inbox: named with the company ID (survives renames)
	scope := MemberOwnedInboxScope(member.Name, co.ID)

	existing, gerr := s.GetInboxByScope(ctx, scope)
	if gerr == nil {
		if existing.Status != "active" {
			if rerr := s.UpdateInboxStatus(ctx, existing.ID, "active"); rerr != nil {
				return emp, store.Inbox{}, compensateDeleteEmployment(ctx, s, emp.ID, fmt.Errorf("reactivate inbox for rehire: %w", rerr))
			}
			// The flip only changes Status — reflect it locally instead of a re-read
			// whose failure path would hand the caller a stale (paused) row to cache.
			existing.Status = "active"
		}
		return emp, existing, nil
	}
	if !errors.Is(gerr, contract.ErrNoRows) {
		return emp, store.Inbox{}, compensateDeleteEmployment(ctx, s, emp.ID, fmt.Errorf("lookup inbox by scope: %w", gerr))
	}

	ib, cerr := s.CreateInbox(ctx, store.Inbox{
		Kind: "member", MemberID: member.ID, Scope: scope, Name: inboxName, // belongs to the member
	}, now)
	if cerr != nil {
		return emp, store.Inbox{}, compensateDeleteEmployment(ctx, s, emp.ID, fmt.Errorf("create member inbox: %w", cerr))
	}
	return emp, ib, nil
}

// compensateDeleteEmployment deletes a stranded Employment after a provisioning
// failure to restore atomicity; if the compensating delete itself fails, surface
// both in slog + the returned error so admin knows manual cleanup is needed.
func compensateDeleteEmployment(ctx context.Context, s *store.PG, empID string, origErr error) error {
	if derr := s.DeleteEmployment(ctx, empID); derr != nil {
		slog.Error("agora hire: compensating Employment delete FAILED — manual cleanup required",
			"employment_id", empID, "original_err", origErr, "delete_err", derr)
		return fmt.Errorf("%w (compensating delete of orphan Employment id=%s ALSO failed: %v — manual delete required)", origErr, empID, derr)
	}
	slog.Warn("agora hire: rolled back Employment after provisioning error", "employment_id", empID, "original_err", origErr)
	return origErr
}

// ---------- Terminate ----------

// TerminateEmployment ends a member's ACTIVE employment at a company (referenced
// by id OR name — normalized like company delete, so it accepts the NAME Hire
// required) — the inverse of Hire. Per the decoupled inbox model it ONLY ends the
// employment: it never touches, pauses, or archives any inbox (inboxes belong to
// the member and persist; the member's grants simply recompute from their
// remaining companies). Member resolution is read-only (get-only, never create).
// Cache write-through + router reload (an active→terminated flip drops the member
// from the company's directory + can change inbox grant resolution).
func TerminateEmployment(ctx context.Context, deps OpDeps, company, memberName string) error {
	mb, err := deps.Store.GetMemberByName(ctx, memberName)
	if err != nil {
		if errors.Is(err, contract.ErrNoRows) {
			return fmt.Errorf("member %q not found", memberName)
		}
		return err
	}
	if deps.Org == nil {
		return fmt.Errorf("agora: org cache unavailable")
	}
	companyID := deps.Org.CompanyIDByIDOrName(company)
	if companyID == "" {
		return fmt.Errorf("company %q not found", company)
	}
	var empID string
	for _, e := range deps.Org.ActiveEmploymentsByMember(mb.ID) {
		if e.CompanyID == companyID {
			empID = e.ID
			break
		}
	}
	if empID == "" {
		return fmt.Errorf("member %q has no active employment at %s", memberName, company)
	}
	if err := deps.Store.Terminate(ctx, empID, clock.UnixEpoch()); err != nil {
		return err
	}
	if deps.Org != nil {
		if updated, gerr := deps.Store.GetEmployment(ctx, empID); gerr == nil {
			deps.Org.UpsertEmployment(updated)
		} else {
			slog.Warn("agora TerminateEmployment: cache read-back failed (snapshot stale until /agora reload)", "employment", empID, "err", gerr)
		}
	}
	deps.reloadRouter(ctx)
	return nil
}

// ---------- Change role ----------

// ChangeRole changes the role of a member's ACTIVE employment at a company
// (referenced by id OR name — normalized like company delete / terminate). Per
// the decoupled inbox model it ONLY changes the role: it never touches any inbox
// (inboxes belong to the member and persist; the member's grants simply recompute
// from the new role's permissions_yaml). Member resolution is read-only (get-only,
// never create). The new role is validated against the company's roles (same
// validator hire uses). Cache write-through + router reload (a role change can
// change the member's directory entry + inbox grant resolution).
func ChangeRole(ctx context.Context, deps OpDeps, company, memberName, newRole string) error {
	mb, err := deps.Store.GetMemberByName(ctx, memberName)
	if err != nil {
		if errors.Is(err, contract.ErrNoRows) {
			return fmt.Errorf("member %q not found", memberName)
		}
		return err
	}
	if deps.Org == nil {
		return fmt.Errorf("agora: org cache unavailable")
	}
	companyID := deps.Org.CompanyIDByIDOrName(company)
	if companyID == "" {
		return fmt.Errorf("company %q not found", company)
	}
	var empID string
	for _, e := range deps.Org.ActiveEmploymentsByMember(mb.ID) {
		if e.CompanyID == companyID {
			empID = e.ID
			break
		}
	}
	if empID == "" {
		return fmt.Errorf("member %q has no active employment at %s", memberName, company)
	}
	co, err := deps.Store.GetCompany(ctx, companyID)
	if err != nil {
		if errors.Is(err, contract.ErrNoRows) {
			return fmt.Errorf("company %s not found", companyID)
		}
		return err
	}
	if err := ValidateRoleInCompany(deps.Org, co, newRole); err != nil {
		return err
	}
	if err := deps.Store.SetEmploymentRole(ctx, empID, newRole, clock.UnixEpoch()); err != nil {
		return err
	}
	if deps.Org != nil {
		if updated, gerr := deps.Store.GetEmployment(ctx, empID); gerr == nil {
			deps.Org.UpsertEmployment(updated)
		} else {
			slog.Warn("agora ChangeRole: cache read-back failed (snapshot stale until /agora reload)", "employment", empID, "err", gerr)
		}
	}
	deps.reloadRouter(ctx)
	return nil
}

// ---------- Member ----------

// CreateMember adds a member by name (optionally with a model-pref tag / prompt
// style), refusing a duplicate name. Cache write-through. Members exist
// independently of companies — they are employed via Hire.
func CreateMember(ctx context.Context, deps OpDeps, name, modelPref, promptStyle string) (store.Member, error) {
	if err := nameOK(name); err != nil {
		return store.Member{}, err
	}
	if _, err := deps.Store.GetMemberByName(ctx, name); err == nil {
		return store.Member{}, fmt.Errorf("agora: member %q already exists", name)
	} else if !errors.Is(err, contract.ErrNoRows) {
		return store.Member{}, err
	}
	m, err := deps.Store.CreateMember(ctx, store.Member{Name: name, ModelPrefTag: modelPref, PromptStyle: promptStyle}, clock.UnixEpoch())
	if err != nil {
		return store.Member{}, err
	}
	if deps.Org != nil {
		deps.Org.UpsertMember(m)
	}
	return m, nil
}

// ---------- Inbox ----------

// CreateMemberOwnedInbox adds a member's OWN inbox — MemberID set, NO employment,
// so it is not tied to any company (persists across hires/leaves, never reclaimed).
// Its grants are resolved at turn time as the intersection of the member's
// companies (see Impl.memberOwnedGrants). Cache write-through.
func CreateMemberOwnedInbox(ctx context.Context, deps OpDeps, memberName, inboxName string) (store.Inbox, error) {
	if err := nameOK(inboxName); err != nil {
		return store.Inbox{}, err
	}
	mb, err := deps.Store.GetMemberByName(ctx, memberName)
	if err != nil {
		return store.Inbox{}, fmt.Errorf("member %q: %w", memberName, err)
	}
	scope := MemberOwnedInboxScope(mb.Name, inboxName)
	if _, gerr := deps.Store.GetInboxByScope(ctx, scope); gerr == nil {
		return store.Inbox{}, fmt.Errorf("inbox %q already exists for %s", inboxName, memberName)
	} else if !errors.Is(gerr, contract.ErrNoRows) {
		return store.Inbox{}, gerr
	}
	ib, err := deps.Store.CreateInbox(ctx, store.Inbox{
		Kind: "member", MemberID: mb.ID, Scope: scope, Name: inboxName, // belongs to the member (company-free)
	}, clock.UnixEpoch())
	if err != nil {
		return store.Inbox{}, err
	}
	if deps.Org != nil {
		deps.Org.UpsertInbox(ib)
	}
	return ib, nil
}

// AddInboxSource binds a chat (source + chat coords) onto an inbox and reloads the
// router. It RE-POINTS: a chat already wired to another inbox is moved to this one
// (the new binding bumps the old) rather than failing on the UNIQUE constraint — a
// chat belongs to exactly one inbox, and re-wiring it is a deliberate operator act
// (wire / add-source). Uses the store's upsert so the duplicate-scope case never
// surfaces a raw 23505.
func AddInboxSource(ctx context.Context, deps OpDeps, inboxID, sourceID string, chatType contract.ChatType, chatID, threadID string) (store.InboxSource, error) {
	out, err := deps.Store.UpsertInboxSource(ctx, store.InboxSource{
		InboxID:  inboxID,
		SourceID: sourceID,
		ChatType: chatType,
		ChatID:   chatID,
		ThreadID: threadID,
	}, clock.UnixEpoch())
	if err != nil {
		return store.InboxSource{}, err
	}
	deps.reloadRouter(ctx)
	return out, nil
}

// RemoveInboxSource un-wires an external chat from an inbox: it removes EVERY source
// whose derived scope (contract.ScopeFor — the same grammar wire/router use) equals
// targetScope, then reloads the router so routing stops. The symmetric inverse of
// AddInboxSource, addressed by the human-readable SCOPE the panel shows, not the opaque
// row id. Normally one row matches; a forum group with several wired topics flattens to
// ONE scope (ScopeFor drops thread_id for groups) — those rows route identically, so
// "unwire this scope" means drop them all (there is no per-topic routing to preserve).
// No source matches (already gone / wrong scope) → contract.ErrNoRows.
func RemoveInboxSource(ctx context.Context, deps OpDeps, inboxID, targetScope string) error {
	srcs, err := deps.Store.ListInboxSources(ctx, inboxID)
	if err != nil {
		return err
	}
	removed := 0
	for _, s := range srcs {
		if contract.ScopeFor(s.SourceID, s.ChatType, s.ChatID, s.ThreadID) != targetScope {
			continue
		}
		if err := deps.Store.RemoveInboxSource(ctx, s.ID); err != nil {
			if errors.Is(err, contract.ErrNoRows) {
				continue // raced away between list and delete — count it as already gone
			}
			return err
		}
		removed++
	}
	if removed == 0 {
		return contract.ErrNoRows
	}
	deps.reloadRouter(ctx)
	return nil
}

// ListInboxes returns a company's inboxes (companyID="" → all).
func ListInboxes(ctx context.Context, deps OpDeps, companyID string) ([]store.Inbox, error) {
	all, err := deps.Store.ListInboxes(ctx)
	if err != nil || companyID == "" {
		return all, err
	}
	var out []store.Inbox
	for _, ib := range all {
		if companyForInbox(deps.Org, ib.ID) == companyID || ib.OwnerCompanyID == companyID {
			out = append(out, ib)
		}
	}
	return out, nil
}

// PauseInbox flips an Inbox to "paused" (drops it from the live routing table).
func PauseInbox(ctx context.Context, deps OpDeps, inboxID string) error {
	return setInboxStatus(ctx, deps, inboxID, "paused")
}

// ResumeInbox flips a paused Inbox back to "active".
func ResumeInbox(ctx context.Context, deps OpDeps, inboxID string) error {
	return setInboxStatus(ctx, deps, inboxID, "active")
}

func setInboxStatus(ctx context.Context, deps OpDeps, inboxID, status string) error {
	if err := deps.Store.UpdateInboxStatus(ctx, inboxID, status); err != nil {
		return err
	}
	if deps.Org != nil {
		if updated, gerr := deps.Store.GetInbox(ctx, inboxID); gerr == nil {
			deps.Org.UpsertInbox(updated)
		} else {
			slog.Warn("agora setInboxStatus: cache read-back failed (snapshot stale until /agora reload)", "inbox", inboxID, "err", gerr)
		}
	}
	deps.reloadRouter(ctx)
	return nil
}

// ---------- Reload ----------

// Reload re-reads the whole org from the store into the OrgCache + rebuilds the
// router — the /agora reload catch-all for out-of-process cli writes.
func Reload(ctx context.Context, deps OpDeps) error {
	if deps.Org != nil {
		if err := deps.Org.Load(ctx, deps.Store); err != nil {
			return err
		}
	}
	if deps.RouterReload != nil {
		return deps.RouterReload(ctx)
	}
	return nil
}

// ---------- Bootstrap ----------

// BootstrapSpec is the input to the idempotent founding.
type BootstrapSpec struct {
	CompanyName string
	FounderName string // "" → "<Company>-Founder"
	// Optional inbox_source wire of the founder's bridge to an external chat.
	BridgeSource   string
	BridgeChatType contract.ChatType
	BridgeChatID   string
	BridgeThreadID string
}

// BootstrapResult is what Bootstrap returns (cli/slash render it).
type BootstrapResult struct {
	Company       store.Company
	Bridge        store.Inbox
	Founder       store.Member
	Employment    store.Employment
	FounderInbox  store.Inbox
	InboxSourceID string // non-empty when the optional bridge→chat wire succeeded
}

// Bootstrap is the idempotent multi-step founding (port of skeleton
// 89-bootstrap.go): company (CreateCompany upserts) + founder member + founder
// employment + founder member-inbox + optional bridge inbox_source wire. Reuses
// the upserts; safe to re-run. Cache write-through throughout; router reload at
// the end (the optional source wire affects routing).
func Bootstrap(ctx context.Context, deps OpDeps, spec BootstrapSpec) (BootstrapResult, error) {
	if err := nameOK(spec.CompanyName); err != nil {
		return BootstrapResult{}, err
	}
	if spec.FounderName != "" {
		if err := nameOK(spec.FounderName); err != nil {
			return BootstrapResult{}, err
		}
	}
	now := clock.UnixEpoch()
	var res BootstrapResult

	// 1. Company (upsert by name) — seed permissions_yaml + playbook when fresh.
	co, _, err := upsertCompany(ctx, deps.Store, spec.CompanyName, now)
	if err != nil {
		return res, fmt.Errorf("upsert company: %w", err)
	}
	if deps.Org != nil {
		deps.Org.UpsertCompany(co)
		_ = deps.Org.SeedCompanyRolesFromYAML(co.ID, []byte(co.PermissionsYAML))
	}

	// 2. Founder role = the first role in the company's permissions_yaml.
	founderRole, ferr := FirstRoleNameInYAML([]byte(co.PermissionsYAML))
	if ferr != nil {
		return res, fmt.Errorf("parse permissions.yaml: %w", ferr)
	}
	if founderRole == "" {
		return res, fmt.Errorf("agora bootstrap: permissions.yaml has no roles — refuse to create a roleless founder")
	}
	// FirstRoleNameInYAML is a lenient node-walker; confirm the role also survives
	// the STRICT parser that feeds identity resolution (RolesConfig.UnmarshalYAML),
	// so a hand-edited permissions_yaml can't seat a founder whose role parses to
	// zero grants at runtime.
	if err := ValidateRoleInCompany(deps.Org, co, founderRole); err != nil {
		return res, fmt.Errorf("agora bootstrap founder role: %w", err)
	}

	// 3. Bridge inbox (upsert by scope) + 4. wire ReportInboxID — the same
	// provisioning sequence CreateCompany uses (single copy, no drift).
	co, bridge, err := provisionBridge(ctx, deps, co, now)
	if err != nil {
		return res, err
	}
	res.Bridge = bridge
	res.Company = co

	// 5. Founder member (upsert by name; per-company default avoids the
	// globally-unique-name collision across multi-company bootstrap).
	memberName := spec.FounderName
	if memberName == "" {
		memberName = spec.CompanyName + "-Founder"
	}
	founder, _, err := upsertMember(ctx, deps.Store, memberName, now)
	if err != nil {
		return res, fmt.Errorf("upsert founder member: %w", err)
	}
	if deps.Org != nil {
		deps.Org.UpsertMember(founder)
	}
	res.Founder = founder

	// 6. Hire (idempotency guard: reuse an existing active employment + inbox).
	var emp store.Employment
	var founderInbox store.Inbox
	if existing, has, eerr := ExistingActiveEmployment(ctx, deps.Store, founder.ID, co.ID); eerr != nil {
		return res, fmt.Errorf("check existing employment: %w", eerr)
	} else if has {
		emp = existing
		ib, ierr := deps.Store.GetInboxByScope(ctx, MemberOwnedInboxScope(founder.Name, co.ID))
		if ierr != nil {
			return res, fmt.Errorf("reuse founder inbox for existing employment: %w", ierr)
		}
		founderInbox = ib
	} else {
		emp, founderInbox, err = hireAndProvision(ctx, deps.Store, founder, co, founderRole, now)
		if err != nil {
			return res, fmt.Errorf("hire and provision: %w", err)
		}
	}
	if deps.Org != nil {
		deps.Org.UpsertEmployment(emp)
		deps.Org.UpsertInbox(founderInbox)
	}
	res.Employment = emp
	res.FounderInbox = founderInbox

	// 7. Optional: bind chat → bridge inbox via inbox_source (generic coords).
	if spec.BridgeSource != "" && spec.BridgeChatID != "" {
		ct := spec.BridgeChatType
		if ct == "" {
			ct = contract.ChatGroup
		}
		// Upsert (not raw insert) so a re-run with the same chat is idempotent:
		// ON CONFLICT returns the pre-existing row instead of a unique violation.
		src, serr := deps.Store.UpsertInboxSource(ctx, store.InboxSource{
			InboxID: bridge.ID, SourceID: spec.BridgeSource, ChatType: ct,
			ChatID: spec.BridgeChatID, ThreadID: spec.BridgeThreadID,
		}, now)
		if serr != nil {
			// Soft-fail: the company is usable without the bridge wire.
			slog.Warn("agora bootstrap: bridge inbox_source bind failed (company usable; wire later)", "company", co.ID, "err", serr)
		} else {
			res.InboxSourceID = src.ID
		}
	}

	deps.reloadRouter(ctx)
	return res, nil
}

// ---------- shared upsert + validation helpers ----------

// bridgeScope is the canonical bridge inbox scope for a company.
func bridgeScope(companyName string) string {
	return fmt.Sprintf("company:%s/bridge:main", companyName)
}

// MemberOwnedInboxScope is the scope of a member's OWN inbox — NO company segment:
// a member-owned inbox is tied to the member, not any company (it persists across
// hires/leaves, is never reclaimed; its grants are the intersection of the member's
// companies, resolved at turn time).
func MemberOwnedInboxScope(memberName, inboxName string) string {
	return fmt.Sprintf("member:%s/inbox:%s", memberName, inboxName)
}

// upsertCompany returns the existing Company by name, else creates it (seeding
// permissions_yaml + playbook). created=true on the create path.
func upsertCompany(ctx context.Context, s *store.PG, name string, now int64) (store.Company, bool, error) {
	if existing, err := s.GetCompanyByName(ctx, name); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, contract.ErrNoRows) {
		return store.Company{}, false, err
	}
	c, err := s.CreateCompany(ctx, store.Company{
		Name: name, PermissionsYAML: SeededPermissionsYAML, Playbook: SeededCompanyPlaybook,
	}, now)
	if err != nil {
		return store.Company{}, false, err
	}
	return c, true, nil
}

// upsertInbox returns the existing Inbox by scope, else creates it.
func upsertInbox(ctx context.Context, s *store.PG, ib store.Inbox, now int64) (store.Inbox, bool, error) {
	if existing, err := s.GetInboxByScope(ctx, ib.Scope); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, contract.ErrNoRows) {
		return store.Inbox{}, false, err
	}
	out, err := s.CreateInbox(ctx, ib, now)
	if err != nil {
		return store.Inbox{}, false, err
	}
	return out, true, nil
}

// upsertMember returns the existing Member by name, else creates it.
func upsertMember(ctx context.Context, s *store.PG, name string, now int64) (store.Member, bool, error) {
	if existing, err := s.GetMemberByName(ctx, name); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, contract.ErrNoRows) {
		return store.Member{}, false, err
	}
	out, err := s.CreateMember(ctx, store.Member{Name: name}, now)
	if err != nil {
		return store.Member{}, false, err
	}
	return out, true, nil
}

// ValidateRoleInCompany checks roleName is defined in the company's roles. Uses
// the OrgCache when available (fast path); falls back to parsing the company's
// PermissionsYAML when the cache is absent / cold.
func ValidateRoleInCompany(org *OrgCache, co store.Company, roleName string) error {
	if org != nil {
		if cfg := org.RolesFor(co.ID); cfg != nil && cfg.Roles != nil {
			if _, ok := cfg.Roles[roleName]; ok {
				return nil
			}
			return fmt.Errorf("role %q not defined in %s permissions_yaml", roleName, co.Name)
		}
	}
	cfg, err := parseRolesYAML([]byte(co.PermissionsYAML))
	if err != nil {
		return fmt.Errorf("parse %s permissions_yaml: %w", co.Name, err)
	}
	if _, ok := cfg.Roles[roleName]; !ok {
		return fmt.Errorf("role %q not defined in %s permissions_yaml", roleName, co.Name)
	}
	return nil
}

// ExistingActiveEmployment returns (employment, true) if Member already holds an
// active Employment at Company (any role); (zero, false) otherwise.
func ExistingActiveEmployment(ctx context.Context, s *store.PG, memberID, companyID string) (store.Employment, bool, error) {
	emps, err := s.ListEmploymentsByMember(ctx, memberID)
	if err != nil {
		return store.Employment{}, false, err
	}
	for _, e := range emps {
		if e.CompanyID == companyID && e.Status == "active" {
			return e, true, nil
		}
	}
	return store.Employment{}, false, nil
}
