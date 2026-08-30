package core

// SkillGate decides if (and how) a skill is visible to a particular
// session. Returns nil to hide the skill from this session; returns a
// (possibly customized) Skill to show it.
//
// Mirrors the SessionAwareTool pattern but runs at registry level —
// filesystem-loaded skills can't implement Go interfaces directly, so
// gating is name-keyed registration via SkillRegistry.RegisterGate.
//
// KD9 fix: the prior `SessionAwareSkillRegistry` sibling
// interface (which used to live in this file) was merged into the
// canonical `SkillRegistry` interface in 55-skills.go. Callers no
// longer type-assert — every SkillRegistry has SkillsForSession.
type SkillGate func(actx AgentCtx) *Skill
