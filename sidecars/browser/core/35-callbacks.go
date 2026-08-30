package core

import "context"

// Button is one inline button rendered next to a message. Text is what the
// user sees; Callback is the opaque id assigned by a CallbackResolver's
// Register call — the source plumbs it through (Telegram callback_data,
// local-cli TUI choice index) and routes a click back through Resolve.
//
// Callers should NEVER fabricate Callback ids — they only come from
// Manager.Register, which couples them to a session + handler.
type Button struct {
	Text     string
	Callback string
}

// CallbackResolver is the slice of the callbacks Manager that Sources
// need: route an incoming click (Telegram callback_query, local-cli TUI
// selection) to whatever subsystem registered the button group. The
// implementation lives in callbacks; this interface
// keeps Sources from depending on that package directly.
type CallbackResolver interface {
	// Resolve dispatches a click. callbackID came from a prior Register
	// (transported via the source's wire format). byUserID identifies
	// the clicker; byIsAdmin is whether the clicker is a source-level
	// admin (per the source's per-platform admins.yaml). Returns true
	// if the id was known and handled, false if expired / unknown /
	// already consumed.
	Resolve(ctx context.Context, callbackID, byUserID string, byIsAdmin bool) bool
}

// Confirmer is the async "post the user a question with N buttons"
// helper exposed to tools via AgentCtx. Wraps Source.SendButtons +
// callbacks.Manager.Register — the click is dispatched later to a
// startup-registered kind handler (first user: todo's manual-mode
// review). The prior synchronous Ask track was removed (D18)
// — it had no production caller.
type Confirmer interface {
	// Post sends the buttons and returns immediately (msg id from the
	// source, when applicable).
	// kind names a startup-registered KindHandler (see callbacks.
	// Manager.RegisterHandler) that will dispatch the click. payload
	// is the kind-specific JSON the handler decodes on click —
	// CAPTURES NOTHING from the calling stack, so callers must encode
	// everything the handler needs (ids, thresholds, user attribution)
	// up front. First user is the todo manual-mode review (#98 /
	// rewrite) — posts buttons after a turn ends; user
	// clicks anytime later, possibly even after a bob restart (rows
	// are persisted via core.SessionStore.RegisterCallback).
	//
	// Expiry: rows past their lifetime are silently DELETEd by the
	// sweep — the handler is NEVER fired on expiry. Subsystems that
	// want a "user didn't respond" reaction must compare timestamps
	// in their own state.
	Post(ctx context.Context, text string, options []string, kind string, payload []byte) (msgID string, err error)
}
