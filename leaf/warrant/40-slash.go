package warrant

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"agentbob/contract"
	"agentbob/i18n"
)

// slashIdentity maps a slash sender to the principal warrant projects on — the
// same two-valued identity the flow uses (admin when IsAdmin, else user). So
// /tools and /skills show EXACTLY the set this sender's turns would get.
func slashIdentity(sc contract.SlashContext) string {
	if sc.IsAdmin {
		return "admin"
	}
	return "user"
}

// slashTools handles /tools: the tools this sender may use, projected through the
// permission matrix (the same Authorize the turn runs), one per line.
func (m *Manager) slashTools(ctx context.Context, sc contract.SlashContext) error {
	identity := slashIdentity(sc)
	ctx = contract.WithIdentity(ctx, identity) // so Authorize's audit line records the sender
	avail := m.Authorize(ctx, m.Grants(ctx, identity), m.catalog.Specs()).Specs()
	var b strings.Builder
	fmt.Fprintf(&b, i18n.T("slash.tools.header", sc.Lang), identity, len(avail))
	if len(avail) == 0 {
		b.WriteString("\n" + i18n.T("slash.tools.none", sc.Lang))
	}
	for _, s := range avail {
		// Name only — the Description is the model's tool contract (a functional
		// prompt), not user-facing copy; showing it would leak un-i18n-able text.
		fmt.Fprintf(&b, "\n"+i18n.T("slash.tools.entry", sc.Lang), s.Name)
	}
	return sc.Sink.Finish(b.String())
}

// slashSkills handles /skills: the skills this sender may use, projected
// through the same matrix; names only.
func (m *Manager) slashSkills(ctx context.Context, sc contract.SlashContext) error {
	if m.skillCat == nil {
		return sc.Sink.Finish(i18n.T("slash.skills.no_subsystem", sc.Lang))
	}
	if strings.TrimSpace(sc.Args) == "reload" {
		return m.slashSkillsReload(ctx, sc)
	}
	identity := slashIdentity(sc)
	ctx = contract.WithIdentity(ctx, identity) // so AuthorizeSkills's audit line records the sender
	avail := m.AuthorizeSkills(ctx, m.Grants(ctx, identity), m.skillCat.List()).List()
	var b strings.Builder
	fmt.Fprintf(&b, i18n.T("slash.skills.header", sc.Lang), identity, len(avail))
	if len(avail) == 0 {
		b.WriteString("\n" + i18n.T("slash.skills.none", sc.Lang))
	}
	for _, s := range avail {
		// Name only; the Description is the model's skill contract, so it is not shown.
		fmt.Fprintf(&b, "\n"+i18n.T("slash.skills.entry", sc.Lang), s.Name)
	}
	return sc.Sink.Finish(b.String())
}

// slashSkillsReload handles /skills reload (admin-only): rescan the skill catalog
// (built-in embed + external dir) so a hand-dropped external skill is picked up
// without a restart, then reconcile the permission matrix so any newly-scanned
// skill gets its default-OFF grant row (otherwise it's fail-closed denied and
// absent from permissions.yaml). The operator then grants it + /permission reload.
func (m *Manager) slashSkillsReload(ctx context.Context, sc contract.SlashContext) error {
	if !sc.IsAdmin {
		return sc.Sink.Finish(i18n.T("slash.skills.reload_admin_only", sc.Lang))
	}
	r, ok := m.skillCat.(contract.SkillReloader)
	if !ok {
		return sc.Sink.Finish(i18n.T("slash.skills.reload_unsupported", sc.Lang))
	}
	if err := r.Reload(); err != nil {
		return sc.Sink.Finish(i18n.T("slash.skills.reload_failed", sc.Lang, err.Error()))
	}
	// Reconcile against the freshly-rescanned catalog: adds default-off rows for new
	// skills, prunes removed ones, persists, and atomic-swaps the live matrix.
	if _, err := m.reloadPolicy(); err != nil {
		return sc.Sink.Finish(i18n.T("slash.skills.reload_failed", sc.Lang, err.Error()))
	}
	return sc.Sink.Finish(i18n.T("slash.skills.reload_ok", sc.Lang, len(m.skillCat.List())))
}

// slashPermission handles /permission: admin-only operator actions on the
// permission matrix. Today the one subcommand is `reload` (re-read
// permissions.yaml without a restart); the verb is required so future
// subcommands (e.g. show/grant) slot in without changing the surface.
func (m *Manager) slashPermission(ctx context.Context, sc contract.SlashContext) error {
	if !sc.IsAdmin {
		return sc.Sink.Finish(i18n.T("slash.permission.admin_only", sc.Lang))
	}
	switch strings.TrimSpace(sc.Args) {
	case "reload":
		changed, err := m.reloadPolicy()
		if err != nil {
			// The live matrix is untouched — a broken file never replaces a good one.
			return sc.Sink.Finish(i18n.T("slash.permission.reload_failed", sc.Lang, err.Error()))
		}
		if changed {
			return sc.Sink.Finish(i18n.T("slash.permission.reload_ok_changed", sc.Lang))
		}
		return sc.Sink.Finish(i18n.T("slash.permission.reload_ok", sc.Lang))
	default:
		return sc.Sink.Finish(i18n.T("slash.permission.usage", sc.Lang))
	}
}

// reloadPolicy re-reads permissions.yaml, reconciles it against the registered
// tools/skills (so newly-registered capabilities get their default-off rows like
// at boot), and atomically swaps it into the live matrix. A parse error returns
// without swapping — the operator keeps the good matrix and sees what's broken
// (unlike boot, which must fail-closed because there is no prior matrix to keep).
// changed reports whether reconcile added rows (and thus rewrote the file).
//
// reloadPolicy serializes against concurrent policy writes (other reloads, the webui
// panel save) via writeMu, then runs the unlocked body. A caller that must combine a
// file write WITH the reload as one atomic step (the panel) takes writeMu itself and
// calls reloadPolicyLocked directly.
func (m *Manager) reloadPolicy() (changed bool, err error) {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	return m.reloadPolicyLocked()
}

// reloadPolicyLocked is reloadPolicy's body; the caller MUST hold writeMu.
func (m *Manager) reloadPolicyLocked() (changed bool, err error) {
	store := newFileStore(m.home)
	p, lerr := store.Load()
	if lerr != nil {
		return false, lerr
	}
	toolNames := make([]string, 0)
	for _, s := range m.catalog.Specs() {
		toolNames = append(toolNames, s.Name)
	}
	changed = reconcile(p, "tool", toolNames, false)
	if m.skillCat != nil {
		skillNames := make([]string, 0)
		for _, info := range m.skillCat.List() {
			skillNames = append(skillNames, info.Name)
		}
		// LIVE degraded check: a /permission reload can land inside the window where
		// the skills catalog serves builtins-only — reconcile add-only there so the
		// unseen external skills' grant rows aren't pruned (same guard as boot).
		changed = reconcile(p, "skill", skillNames, skillCatalogDegraded(m.skillCat)) || changed
	}
	if changed {
		if werr := store.Save(p); werr != nil {
			// In-memory matrix is authoritative; a persist failure just means the new
			// default rows aren't on disk yet — log, don't fail the reload.
			slog.Warn("warrant: reload could not persist reconciled permissions — in-memory policy active", "err", werr)
		}
	}
	old := m.policy.Load()
	m.policy.Store(p)
	// Diff-cut (revocation): close the live pooled exec/ssh channels of any principal
	// that LOST a grant in this reload, so a permission removal takes effect now rather
	// than lingering on a pooled channel (up to channelIdle, or a whole in-flight turn).
	// Additive reloads (reconcile's default-off rows, a newly granted capability) cut no
	// one — only a principal whose grant set shrank is recycled. Reuses Manager.Cut →
	// pool.Recycle; an unchanged principal's channels are never touched.
	for _, id := range lostGrants(old, p) {
		m.Cut(id)
	}
	// A successful reload only ever swaps in a parseable matrix, so whatever
	// boot-time breakage set policyBroken is now resolved. GUARDRAIL (S-13):
	// brokenOnce (which gates the one-time broken alert) is deliberately NOT reset
	// here — policyBroken only ever goes true→false at runtime today. Any FUTURE
	// path that can set policyBroken=true at runtime (e.g. a hot DB-backend failure)
	// MUST also replace brokenOnce, or the alert will be permanently silent.
	m.policyBroken.Store(false)
	return changed, nil
}
