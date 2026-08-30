package core

import "context"

// Origin tags whether a capability is part of the binary or admin-managed
// state under $BOB_HOME (§0.11). External capabilities can be installed,
// removed, enabled, disabled via chat / CLI; internal ones can't.
type Origin string

const (
	OriginInternal Origin = "internal"
	OriginExternal Origin = "external"
)

// Layer identifies which on-disk root a Skill came from. Set by the
// registry at load time. Override precedence (Phase 2):
//
//	LayerSkills > LayerExternal > LayerEmbed
//
// When the same name appears in multiple layers the higher-precedence
// version wins (becomes cache[name]) and the lower-precedence ones get
// stashed under shadowed[name] so admin slash can surface them on
// request without polluting the model's skills-index.
type Layer string

const (
	LayerEmbed    Layer = "embed"    // $BOB_HOME/skills/builtin/ — extracted from binary at startup
	LayerSkills   Layer = "skills"   // $BOB_HOME/skills/ — admin-curated, light scan
	LayerExternal Layer = "external" // $BOB_HOME/skills/external/ — community / Hermes, strict scan
)

// Precedence returns the override rank — higher beats lower.
func (l Layer) Precedence() int {
	switch l {
	case LayerSkills:
		return 3
	case LayerExternal:
		return 2
	case LayerEmbed:
		return 1
	}
	return 0
}

// Skill is one loadable Hermes-compatible skill — a directory under
// $BOB_HOME/skills/<name>/ (or skills/external/, or extracted embed at
// $BOB_HOME/skills/builtin/) with a SKILL.md instruction file. The full
// Hermes/agentskills.io frontmatter is parsed into the typed fields below
// at registry load time; see skills/10-registry.go.
//
// Phase 1: expanded from 4 to 18 fields so downstream
// scan / gate / sandbox-deps logic can act on declared metadata without
// re-parsing the YAML.
type Skill struct {
	// --- Phase 1 base (always populated) ---
	Name        string // directory basename, enforced == frontmatter.Name (Phase 2)
	Description string // frontmatter `description`, falls back to first non-heading line
	Path        string // absolute path to the skill directory
	Origin      Origin // OriginExternal for $BOB_HOME skills; OriginInternal for embed-derived
	Layer       Layer  // which root produced this skill — see Layer constants (Phase 2)

	// --- Frontmatter top-level (Hermes / agentskills.io standard) ---
	DescriptionZH string   // optional `description_zh` for future localisation (no runtime use yet)
	Version       string   // semver string; Phase 7 install pipeline parses for upgrade detection
	Author        string   // free text
	License       string   // SPDX expression
	Platforms     []string // OS gating: ["linux", "macos", "windows"]

	// --- metadata.hermes.* (Hermes-compat) ---
	Tags             []string       // metadata.hermes.tags — keywords for search / role tagging
	Category         string         // metadata.hermes.category — coarse grouping
	RelatedSkills    []string       // metadata.hermes.related_skills — suggested companions
	RequiresToolsets []string       // metadata.hermes.requires_toolsets — Phase 6 startup-scan gates skill on actual tool registry intersection
	HermesConfig     map[string]any // metadata.hermes.config — skill-internal config blob

	// --- metadata.bob.* (bob-specific) ---
	BobAudiences     []string       // metadata.bob.audiences — per-session SkillGate visibility tag; supports `*` glob suffix. See docs/skills-system-design.md §5 "audiences-gate contract" + KD6 fix.
	BobPrecedence    int            // metadata.bob.precedence — reserved; current design 0 (model self-decides), parser reads for future
	BobSource        string         // metadata.bob.source — upstream URL for marketplace; reserved, no runtime use yet
	PythonDeps       []string       // metadata.bob.python_deps — Phase 20 install-time pip-installs into $BOB_HOME/skills/python-deps/<name>/.python/
	SystemDeps       []string       // metadata.bob.system_deps — apt packages; never auto-installed, shown in admin DM as advisory
	NetworkAllowlist []string       // metadata.bob.network_allowlist — Phase 10 docker --network none escape valve (per-domain admin-approved)
	BobConfig        map[string]any // metadata.bob.config — bob-side skill config (e.g. comfyui_url for the API-only ComfyUI skill)

	// CredentialsNeeded is the metadata.bob.credentials_needed list — broker
	// credential names this skill expects to be available. NOT a hard
	// dependency: the skill loads regardless. Used for (a) startup WARN when
	// a credential is missing from the broker so admin sees the gap, and
	// (b) runtime hint prepended to skill_view output when the caller's
	// PermIdentity bundle lacks the credential:use:<name> grant. Empty /
	// nil = skill declares no broker dependencies.
	CredentialsNeeded []string

	// ForkScope is metadata.bob.fork_scope — which customization axis
	// skill_manager(customize) routes this skill's fork to (see
	// docs/agora-role-skill-fork.md §5.1). "" / "account" = the per-account
	// axis (docs/skill-fork.md, external-layer only); "role" = the
	// per-(company, role) agora axis (this skill's process branches per
	// company-role). Read via ForkScopeEff so the empty
	// default resolves to account.
	ForkScope string

	// HasRubric is set by the registry at load time (Reload). True when
	// a RUBRIC.md file exists alongside the SKILL.md in this skill's
	// directory. Lets the skill_view染色 path skip a per-call os.Stat
	// (Phase 7 DEC-B3 /). NOT serialized from frontmatter —
	// it's a registry-computed metadata bit, not a skill-author input.
	HasRubric bool

	// ForceOCR is metadata.bob.force_ocr — when true, a session染色'd with
	// this skill pre-OCRs every image attachment (orchestrator forced-OCR
	// pass → core.Attachment.OCRText → rendered inline by describeAtt) so the
	// image-blind main model reads the image BY CONSTRUCTION instead of having
	// to call the `ocr` tool (which it can skip → then "answer" an unread
	// image). For 抽取类 skill (超声波报告 / 发票 / 化验单). Opt-in + default
	// false: ordinary image turns are untouched, so non-textual images (照片
	// /表情/截图) never pay an unwanted OCR pass.
	ForceOCR bool
}

// Fork-scope axis values for Skill.ForkScope (metadata.bob.fork_scope).
const (
	ForkScopeAccount = "account" // per-account fork (docs/skill-fork.md); the default
	ForkScopeRole    = "role"    // per-(company, role) fork (docs/agora-role-skill-fork.md)
)

// ForkScopeEff returns the skill's effective fork axis: an empty/unknown
// frontmatter value defaults to ForkScopeAccount, so existing skills keep the
// per-account behaviour and only an explicit `fork_scope: role` opts into the
// agora company-role axis.
func (s Skill) ForkScopeEff() string {
	if s.ForkScope == ForkScopeRole {
		return ForkScopeRole
	}
	return ForkScopeAccount
}

// SkillRegistry is the agent's view of installed skills. The default
// implementation (FileSkills) scans three on-disk roots — embed,
// skills, skills/external — and applies the Layer precedence override
// (see Layer.Precedence). Thread-safe.
//
// KD9 fix: SkillsForSession + RegisterGate are now part
// of this single interface (was a sibling `SessionAwareSkillRegistry`).
// The split + optional type-assert pattern was a stealth audiences-gate
// bypass: any caller that forgot to assert would fall back to `List()`
// and inject every skill into the model prompt, ignoring per-session
// visibility rules. Single interface = impossible to mis-wire.
type SkillRegistry interface {
	// List returns winning skills only (after override), sorted by Name.
	// CALLERS BEWARE: this returns the FULL list ignoring per-session
	// gates. Reserve it for admin / debug paths (e.g. `bob skills`
	// CLI). Anywhere the model sees the list, use SkillsForSession.
	List() []Skill
	// Get returns the winning skill by Name (case-sensitive).
	Get(name string) (Skill, bool)
	// Read returns the raw SKILL.md bytes for the winning version of `name`.
	Read(name string) ([]byte, error)
	// Reload re-scans every on-disk root.
	Reload() error
	// Shadows returns the lower-precedence Skills hidden by override for
	// `name`. Empty when nothing shadowed (or `name` not found at all).
	// Used by admin `/skills` and `/skill <name>` to surface conflicts
	// without leaking shadow info to the model's skills-index.
	Shadows(name string) []Skill

	// SkillsForSession returns winners filtered by the per-session
	// AudiencesGate (see docs/skills-system-design.md §5) AND the
	// permission Gate when set. Sorted by Name. Use this anywhere a
	// list of skills feeds the model — the system prompt, skill_view
	// enumeration, sandbox-sync, etc. — so visibility rules apply
	// consistently.
	//
	// ctx carries the PermIdentity bundle (core.WithIdentities) the
	// permission Gate consults; pass background when no caller identity
	// is known (admin/debug paths).
	SkillsForSession(ctx context.Context, actx AgentCtx) []Skill

	// RegisterGate attaches a per-name SkillGate that overrides the
	// default AudiencesGate output. M0: no caller registers; reserved
	// for skills that activate only when a particular dependency is
	// configured (e.g. "show notion-integration only if notion
	// credential is in the broker").
	RegisterGate(name string, gate SkillGate)

	// SetGate wires the permission Gate (Step 4 —). Nil =
	// no enforcement (nil-Gate convention). Call once at startup
	// before any session resolves visibility.
	SetGate(g Gate)
}
