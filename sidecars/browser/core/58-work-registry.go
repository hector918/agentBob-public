package core

import (
	"context"
	"errors"
)

// ErrUserCanceled is the cancel cause set by WorkRegistry.CancelSession when
// a user runs `/close-session`. Distinguishes user-initiated cancellation
// from parent-ctx cancellation (e.g. gateway SIGINT) via context.Cause(ctx).
var ErrUserCanceled = errors.New("cancelled by user (/close-session)")

// WorkRegistry tracks in-flight turns across all sessions. It is the data
// model behind `/close-session` (cancel an in-flight turn).
//
// Cancellation cascades through stdlib context propagation: EnterTurn returns
// a ctx derived from its parent, EnterToolCall a ctx derived from the turn's
// ctx — so cancelling the turn cancels every child for free.
type WorkRegistry interface {
	// EnterTurn registers a new turn for (sessionScope, sid) and returns a
	// derived ctx plus a `finish` function the caller MUST call (via defer)
	// to deregister. sid is the specific session generation the turn runs
	// against (v0.2 multi-session model — see docs/session-design.md);
	// sessionScope is the scope. `label` is human-readable for snapshots /
	// logs.
	EnterTurn(parent context.Context, sessionScope, sid, label string) (turnCtx context.Context, finish func())

	// EnterToolCall derives a per-child ctx (e.g. one tool call) from an
	// EnterTurn-returned ctx so a turn-level cancel cascades. label names
	// the child for logs. The caller MUST call finish (via defer) to
	// release the derived ctx.
	EnterToolCall(turnCtx context.Context, label string) (toolCtx context.Context, finish func())

	// CancelSid cancels every in-flight turn for ONE specific sid. Used by
	// `/close-session` (default form — close just the active sid). Returns
	// true if anything was actually cancelled.
	CancelSid(sid string) bool

	// CancelSession cancels every in-flight turn for ALL sids under
	// sessionScope. Used by `/close-session all` (nuke everything in the
	// scope). Returns true if anything was cancelled.
	CancelSession(sessionScope string) bool
}
