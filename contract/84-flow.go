package contract

import "context"

// Flow is one turn-execution strategy — the thin orchestration that composes
// what the loop core runs (which prompt layers, which model, later which tools)
// and drives it. Strategy lives here; mechanism lives in the loop. Swapping the
// flow changes the whole orchestration without touching the loop, the prompt
// builder, or the session.
//
// Many flows coexist behind the flow router (which implements TurnHandler): the
// router asks each, highest Priority first, whether it Accepts a unit of work,
// and delegates to the first that does. Exactly one flow is the catch-all
// (Priority 0, Accepts everything) so no work is ever unhandled.
type Flow interface {
	Name() string
	// Priority orders flows for selection: higher is consulted first. The
	// catch-all default is 0; a special-case flow uses a higher value.
	Priority() int
	// Accepts reports whether this flow handles the given work. The catch-all
	// returns true unconditionally.
	Accepts(work TurnWork) bool
	// Mode reports the SessionMode this flow imposes on the sessions it handles
	// (the plain flow is SessionModeNone; an automated/agora flow SessionModeAuto).
	// The router surfaces it via TurnHandler.FlowInfo; the session caches + persists it.
	Mode() SessionMode
	// RunTurn executes one turn under this flow's strategy.
	RunTurn(ctx context.Context, work TurnWork)
	// DumpPrompt composes this flow's prompt for the work WITHOUT running the turn,
	// and renders it to text — the read-only /prompt admin command (no model, no
	// persist, no history change). It is a pure function of the (session-built) work:
	// the same TurnWork RunTurn consumes, terminating in a dump instead of a model
	// call. v1 renders the assembled system prompt + the authorized tools/skills (it
	// does not replay history).
	DumpPrompt(ctx context.Context, work TurnWork) string
}

// FlowRegistry collects the turn-execution flows. The flow router provides it;
// each flow module registers itself into it at Start. Registration order does
// not matter — the router orders by Priority.
type FlowRegistry interface {
	Register(f Flow)
}
