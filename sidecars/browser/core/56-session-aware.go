package core

// SessionAwareTool optionally varies a tool's spec — or hides it
// entirely — per session.
//
// Tools that don't implement this interface advertise their static
// Spec() to every session. Tools that DO implement it can:
//
//   - return nil  → omit from this session's tool list (model can't see
//     or call the tool this turn)
//   - return &spec → use this trimmed/customised spec for this session
//
// Used to (a) hide interactive browser_* tools until a navigate has
// happened in the session (the model otherwise misuses them on user-
// attached images, since they look superficially relevant), and (b) trim
// per-mode-only fields from the todo tool's schema once the session's
// verification mode is locked.
//
// The check happens once per turn, in the registry's SpecsForSession()
// path. The actx supplied is the per-event AgentCtx — fields the
// implementation typically reads: SessionScope, IsAdmin, Store.
type SessionAwareTool interface {
	SpecForSession(actx AgentCtx) *ToolSpec
}
