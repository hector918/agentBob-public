package agora

import (
	"context"
	"sort"

	"agentbob/contract"
)

// companyIDByIDOrName resolves a company reference (id OR name) to its canonical id; ""
// when unknown. Arrangement stores the company as given (often the NAME), so seams that
// key on id must resolve through this.
func (a *Impl) companyIDByIDOrName(_ context.Context, company string) string {
	if a == nil || a.org == nil || company == "" {
		return ""
	}
	// Cheap RLock+map read, not a full Snapshot deep-copy — this runs on the
	// arrangement selector hot path (roleCanProcess → RoleHasGrant ×2 per bucket). (L-AG-S1)
	return a.org.CompanyIDByIDOrName(company)
}

// RoleProjection collapses a (company, role)'s grants into ONE GrantSet (bundles
// already expanded to tool:use:arrangement_* at config load). company matches id OR
// name; unknown company/role → empty (fail-closed). A SUPPLY operation — agora does
// not judge; the caller checks membership (the arrangement dispatcher:
// proj.Granted("tool:use:arrangement_pull")). A pure in-process read. docs §14.
func (a *Impl) RoleProjection(ctx context.Context, company, role string) contract.GrantSet {
	out := contract.GrantSet{}
	coID := a.companyIDByIDOrName(ctx, company)
	if coID == "" || a.org == nil {
		return out
	}
	cfg := a.org.RolesFor(coID)
	if cfg == nil {
		return out
	}
	def, ok := cfg.Roles[role]
	if !ok {
		return out
	}
	for _, g := range def.Grants {
		out[g] = struct{}{}
	}
	return out
}

// MemberScopesForRole returns the own-inbox scope of every active member holding
// (company, role). company matches a Company by ID or Name. It reads the SAME org
// snapshot the directory / send-resolver read, so "who holds this role" is one
// answer. nil when nothing matches (fail-closed: the arrangement dispatcher then
// nudges no one). See contract.Agora + docs/arrangement.md.
func (a *Impl) MemberScopesForRole(ctx context.Context, company, role string) []string {
	if a == nil || a.org == nil || company == "" || role == "" {
		return nil
	}
	snap := a.org.Snapshot(ctx, nil)
	companyID := ""
	for _, co := range snap.Companies {
		if co.Status == "disabled" {
			continue
		}
		if co.ID == company || co.Name == company {
			companyID = co.ID
			break
		}
	}
	if companyID == "" {
		return nil
	}
	holders := map[string]bool{}
	for _, e := range snap.Employments { // Snapshot already filters to active employments
		if e.CompanyID == companyID && e.RoleName == role {
			holders[e.MemberID] = true
		}
	}
	if len(holders) == 0 {
		return nil
	}
	// Match ONLY each holder's company-scoped own-inbox — MemberOwnedInboxScope(name,
	// companyID), the SAME single inbox the directory / send-resolver resolve — NOT every
	// member-kind inbox the holder happens to own. A holder may own personal inboxes at
	// arbitrary scopes (Kind="member"/active too); returning those would nudge company work
	// to an unrelated inbox AND authorize a claim arriving from it (an auth leak). A
	// PAUSED/archived inbox is still excluded (require active → its member isn't nudged).
	nameByID := make(map[string]string, len(snap.Members))
	for _, mem := range snap.Members {
		nameByID[mem.ID] = mem.Name
	}
	want := make(map[string]bool, len(holders))
	for id := range holders {
		if name := nameByID[id]; name != "" {
			want[MemberOwnedInboxScope(name, companyID)] = true
		}
	}
	var out []string
	for _, ib := range snap.Inboxes {
		if ib.Inbox.Status == "active" && want[ib.Inbox.Scope] {
			out = append(out, ib.Inbox.Scope)
		}
	}
	sort.Strings(out)
	return out
}

// CompanyActive reports whether company (matched by id OR name) exists, is enabled, and
// is not paused — the arrangement dispatcher's per-arrangement gate (a disabled/paused
// company stops dispatch, leaving its items queued to resume on re-enable). nil/unknown
// company → false. A pure in-process read.
func (a *Impl) CompanyActive(ctx context.Context, company string) bool {
	if a == nil || a.org == nil || company == "" {
		return false
	}
	coID := a.org.CompanyIDByIDOrName(company)
	if coID == "" {
		return false // unknown company
	}
	co, _ := a.org.CompanyByID(coID)
	return co.Status != "disabled" && !a.IsPausedCompany(coID)
}
