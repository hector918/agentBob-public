package warrant

import (
	"strings"

	"agentbob/contract"
)

// defaultPrincipals seed the matrix when permissions.yaml has none. The flow
// mints "admin"/"user" for non-agora senders (ev.IsAdmin); agora principals
// (company:role) are added by the admin / agora reconcile later.
var defaultPrincipals = []string{"admin", "user"}

// reconcile makes the matrix a PURE MIRROR of the registered capabilities of one
// kind ("tool" / "skill"): a newly-appeared capability is added default-OFF for
// every principal, and a capability that's no longer registered is deleted
// outright. Fully automatic — it never inspects or guesses permissions; the admin
// flips on what they want in permissions.yaml afterwards. Returns whether anything
// changed; the caller persists once after all kinds.
//
// addOnly disables the DELETE half: the caller knows names is INCOMPLETE (the
// skills catalog degraded to builtins-only over an external-dir blip), so a
// missing capability means "temporarily unseen", not "removed" — pruning then
// would delete every external skill's grant rows (incl. admin-flipped ones) and
// persist the loss.
func reconcile(p *policy, kind string, names []string, addOnly bool) bool {
	if len(p.principals) == 0 {
		p.principals = append([]string(nil), defaultPrincipals...)
	}
	changed := false
	want := make(map[string]bool, len(names))
	for _, name := range names {
		want[capString(kind, name)] = true
	}
	// ADD: a newly-registered capability goes in OFF for everyone.
	for cap := range want {
		if _, ok := p.granted[cap]; ok {
			continue
		}
		row := make(map[string]bool, len(p.principals))
		for _, pr := range p.principals {
			row[pr] = false
		}
		p.granted[cap] = row
		changed = true
	}
	// DELETE: a capability of THIS kind that's no longer registered is dropped, so
	// the matrix never accumulates stale rows. Scoped by the "<kind>:use:" prefix
	// so reconciling "tool" never touches "skill" rows (or vice-versa).
	//
	// Guard: an EMPTY registered set means "I don't know" (a catalog module that
	// failed to start / hasn't registered), NOT "everything was removed" — pruning
	// then would wipe every grant of this kind (incl. admin-flipped ones) on a
	// transient failure, to silently re-add default-off next boot. A real
	// deployment always has ≥1 tool/skill, so skip the prune when names is empty.
	if addOnly || len(want) == 0 {
		return changed
	}
	prefix := kind + ":use:"
	for cap := range p.granted {
		if strings.HasPrefix(cap, prefix) && !want[cap] {
			delete(p.granted, cap)
			changed = true
		}
	}
	return changed
}

// skillCatalogDegraded reports whether the skill catalog is serving a KNOWN-
// INCOMPLETE set (its external scan fell back to builtins-only). Discovered via an
// optional type-assert (the contract.SkillReloader pattern) so the SkillCatalog
// contract stays minimal and test stubs needn't implement it; a catalog without the
// method is taken at face value. Both reconcile call sites (boot Start and
// /permission reload) must check this LIVE — a reload can land inside the degraded
// window — and go add-only, or they would prune every external skill's grant rows.
func skillCatalogDegraded(cat contract.SkillCatalog) bool {
	d, ok := cat.(interface{ Degraded() bool })
	return ok && d.Degraded()
}
