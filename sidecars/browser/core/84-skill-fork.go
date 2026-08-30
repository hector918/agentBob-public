package core

import "context"

// SkillManifest is one account's fork of an external skill, expressed as an
// OVERLAY DELTA over the baseline skill (docs/skill-fork.md §3.1/§6.1). A fork
// shares the baseline NAME (so identity / description / gating are unchanged);
// only the EXECUTION PART (body / scripts / rubric) is customised. Parts the
// manifest does not mention INHERIT the baseline — every part is tri-state:
//
//	Body:   nil = inherit baseline body;        non-nil = replace.
//	        (body is never "removed" — a skill must have a body.)
//	Rubric: nil = inherit baseline RUBRIC;       &"" = explicitly removed (fork
//	        has no rubric → no judge interlock);  &"text" = replace.
//	Scripts: map relPath→content — added / replaced baseline support files.
//	Removed: baseline relPaths to suppress (the overlay way to "delete a file").
//
// It NEVER carries a description — identity always comes from the baseline
// registry entry (docs/skill-fork.md §3). It is stored JSON-marshalled in the
// per-account bob_account_skill_overrides table (main store, FK-cascaded to the
// account). This is NOT an authorization surface (§9 invariants): a fork only
// swaps CONTENT after the baseline skill's grants + inbox gates already passed.
type SkillManifest struct {
	Body    *string           `json:"body,omitempty"`
	Scripts map[string]string `json:"scripts,omitempty"`
	Removed []string          `json:"removed,omitempty"`
	Rubric  *string           `json:"rubric,omitempty"`
}

// IsRemoved reports whether relPath is suppressed by this fork (in Removed and
// not re-added in Scripts). Used by the overlay resolvers in skill_view +
// sandbox-sync to treat a baseline file as absent.
func (m *SkillManifest) IsRemoved(relPath string) bool {
	if m == nil {
		return false
	}
	if _, ok := m.Scripts[relPath]; ok {
		return false // re-added > removed
	}
	for _, r := range m.Removed {
		if r == relPath {
			return true
		}
	}
	return false
}

// RubricState classifies the fork's rubric tri-state against the baseline.
type RubricState int

const (
	RubricInherit  RubricState = iota // manifest.Rubric == nil → use baseline
	RubricRemoved                     // manifest.Rubric == &"" → fork has no rubric
	RubricReplaced                    // manifest.Rubric == &"text" → use that text
)

// RubricResolution returns the fork's rubric tri-state and, when Replaced, the
// replacement body. nil receiver / nil Rubric → Inherit.
func (m *SkillManifest) RubricResolution() (RubricState, string) {
	if m == nil || m.Rubric == nil {
		return RubricInherit, ""
	}
	if *m.Rubric == "" {
		return RubricRemoved, ""
	}
	return RubricReplaced, *m.Rubric
}

// SkillOverrideReader is the read seam skill_view + sandbox-sync consult to
// fold a per-account fork over the baseline skill. accountID "" (unbound sender
// / accounts off) → always (nil, false, nil) / (nil, nil): the baseline stands.
// Satisfied by accounts.Manager; nil-allowed at the call sites (no accounts
// wired → baseline everywhere). See docs/skill-fork.md §7.1.
type SkillOverrideReader interface {
	SkillOverride(ctx context.Context, accountID, skillName string) (*SkillManifest, bool, error)
	// ForkedSkillNames returns the skill names accountID has forked — the
	// sandbox-sync pre-filter (one query up front instead of a per-skill
	// SkillOverride lookup) and the skill_manager(list) fork marking.
	ForkedSkillNames(ctx context.Context, accountID string) ([]string, error)
}

// CompanyRoleSkillOverrideReader is the read seam skill_view consults to fold a
// per-(company, role) fork over the baseline skill on an agora turn
// (docs/agora-role-skill-fork.md §7.1). It resolves the inbox to its
// (company, role) internally (via agora.OrgCache) and reads the override table;
// the prompt/tool layer never imports agora. inboxID "" (non-agora turn) or an
// inbox with no role (bridge / no employment) → (nil, false, nil): the baseline
// stands (the single-tier model has no company-default fallback, §3). Satisfied
// by the agora package; nil-allowed at the call site (no agora wired → baseline
// everywhere).
type CompanyRoleSkillOverrideReader interface {
	OverrideForInbox(ctx context.Context, inboxID, skillName string) (*SkillManifest, bool, error)
}

// CompanyRoleForkAuthor is the write seam skill_manager(customize) uses for the
// agora company-role fork axis (docs/agora-role-skill-fork.md §5/§6). It
// resolves the caller's inbox to the company they belong to (the only company
// they may author for — §6.3) and reads / writes / deletes a (company, role,
// skill) fork. The caller's authority is checked separately (the
// skill:customize:<name> capability); this seam only does company resolution +
// persistence. Satisfied by the agora package; nil → role-axis customize
// reports the feature unavailable.
type CompanyRoleForkAuthor interface {
	// CompanyForInbox returns the company the caller's inbox belongs to.
	// ok=false for an unknown inbox or one with no resolvable company.
	CompanyForInbox(ctx context.Context, inboxID string) (companyID string, ok bool)
	// GetRoleFork returns the current (company, role, skill) fork for the
	// iterate-from-current craft, or (nil, false, nil) when none exists.
	GetRoleFork(ctx context.Context, companyID, roleName, skillName string) (*SkillManifest, bool, error)
	SetRoleFork(ctx context.Context, companyID, roleName, skillName string, m *SkillManifest, nowUnix int64) error
	DeleteRoleFork(ctx context.Context, companyID, roleName, skillName string) (existed bool, err error)
}
