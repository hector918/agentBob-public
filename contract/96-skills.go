package contract

// This file is the skills seam. A skill is fundamentally TEXT + METADATA — an
// instruction body the model reads, plus typed switches the flow enforces. It only
// gains FILES when it carries scripts (the minority); that file/script path rides
// the spaces mechanism and is a followup (see docs/skills.md). Skills mirror tools:
// a dumb catalog the flow queries, projected per-identity by warrant (skill:use:X),
// so visibility == usability from one source.

// SkillOrigin is where a skill comes from — TWO kinds only.
type SkillOrigin string

const (
	OriginInternal SkillOrigin = "internal" // built-in, shipped in the binary (embed)
	OriginExternal SkillOrigin = "external" // admin-installed under the external dir
)

// SkillInfo is the catalog/index view — what List and the projection expose. A
// skill is listed in the prompt index; the model pulls its full body on demand via
// skill_view. The body is read with Read; the full record with Get.
type SkillInfo struct {
	Name        string
	Description string
	// Triggers are recall cues rendered into the prompt index next to the name —
	// anchors (jargon / proper nouns / commands, language-agnostic), discriminators
	// (what sets THIS skill apart from same-domain siblings; multilingual), and at most
	// secondary generic-domain words. They raise "task → recall this skill" without the
	// model having to read the body. See docs/skill-recall-index.md.
	Triggers []string
	Origin   SkillOrigin
}

// Skill is the full record: the info, the directory the body (and any scripts) live
// in, and the optional rubric. The body itself is read lazily via SkillSet.Read.
type Skill struct {
	SkillInfo
	Path string
	// Rubric is the skill's RUBRIC.md grounding criteria ("" when the skill ships
	// none). When a skill carrying a rubric is engaged in a turn (the model reads it
	// via skill_view), the turn mechanically judges its final product against this
	// rubric before delivering — fail → redo (see the turn core's rubric gate).
	// Loaded verbatim by the catalog; never summarized.
	Rubric string
}

// SkillSet is the turn's AUTHORIZED skill projection — symmetric with ToolSet. The
// flow gets it from warrant; the prompt index and skill_view both read it, so
// visibility and usability share one source.
type SkillSet interface {
	List() []SkillInfo             // authorized skills (for the prompt index)
	Get(name string) (Skill, bool) // full record incl. rubric/path
	Read(name string) (string, bool)
	// Files returns a skill's auxiliary files (everything under its dir except
	// SKILL.md) — the scripts/templates the model needs on disk to run it. The
	// script mechanism (docs/skills-rebuild.md §7) materializes these into the
	// session space at skills/<name>/<rel>, so the model runs `terminal python
	// skills/<name>/scripts/x.py`. Empty for a pure-text skill; an unauthorized /
	// unknown name returns an error.
	Files(name string) ([]SkillFile, error)
}

// SkillFile is one auxiliary file shipped with a skill. Rel is the forward-slash
// path relative to the skill's dir (e.g. "scripts/search_arxiv.py"); Data is its bytes.
type SkillFile struct {
	Rel  string
	Data []byte
}

// SkillCatalog is the full registered skill set, provided by leaf/skills. It IS a
// SkillSet; the flow projects from it and warrant authorizes the projection per
// identity. internal (embed) + external (dir), external overriding internal on a
// name clash.
type SkillCatalog interface {
	SkillSet
}

// SkillFailureSink records a failed-turn SNAPSHOT for a skill the model engaged —
// the failure-driven learning signal for ordinary (rubric-less) skills
// (docs/wip-skill-learn-reflect.md). Provided by leaf/skills; called by the AGORA
// flow (not the turn, not flow/normal) when an agora member's turn ends OutcomeDegraded: the
// flow reads that turn's messages, finds the skill_view'd skills + a medium
// trajectory snapshot, and reports one per engaged skill. leaf/learn's distiller then
// reflects on the real trajectory (hermes-style) rather than a thin label. snapshot is
// a multi-line text record (user ask + action outline + degraded output).
type SkillFailureSink interface {
	RecordFailure(skill, snapshot string)
}

// SkillReloader is the OPTIONAL rescan capability a SkillCatalog may also support:
// re-read the built-in embed + the external dir so a hand-dropped external skill is
// picked up without a restart. Not part of SkillCatalog (test stubs needn't
// implement it) — a caller type-asserts to it (e.g. /skills reload); a catalog
// without it simply can't be rescanned live. Implemented by leaf/skills.FileSkills.
type SkillReloader interface {
	Reload() error
}
