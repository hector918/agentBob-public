package session

import (
	"context"
	"fmt"
	"log/slog"
)

// ResolveActiveSidWithCaller returns the scope's active sid (creating one if absent),
// deduping concurrent calls for the same scope so two dispatch goroutines can't create
// twin sids — plus a caller frame applied ONLY
// when this resolves a MISS (no active session → a fresh worker session opens
// with caller_session_id = callerSid). An EXISTING (resumed) session keeps its
// original caller — the caller edge is set once at creation, never overwritten on
// resume (docs/agora-delegation-return.md §3). callerSid="" routes through the
// plain open (external wire events have no caller).
func (m *Manager) ResolveActiveSidWithCaller(ctx context.Context, scope, source, userID, callerSid string) (string, error) {
	v, err, _ := m.sfActive.Do(scope, func() (interface{}, error) {
		return m.resolveActiveSidOnce(ctx, scope, source, userID, callerSid)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func (m *Manager) resolveActiveSidOnce(ctx context.Context, scope, source, userID, callerSid string) (string, error) {
	cur, ok, err := m.store.CurrentSession(ctx, scope)
	if err != nil {
		// A read error is NOT "no current session". ok=false reports a
		// legitimately empty scope; a non-nil err is a transient hiccup. Falling
		// through to OpenSession on a blip would fork a new session every time,
		// splitting one conversation into duplicates. Propagate instead.
		return "", fmt.Errorf("resolve active sid: read current session for scope %q: %w", scope, err)
	}
	if ok {
		// Resumed an existing session — keep its original caller (set at creation).
		return cur, nil
	}
	return m.OpenSessionInScopeWithCaller(ctx, scope, source, userID, callerSid)
}

// OpenSessionInScope opens a fresh session and makes it the scope's active one
// (OpenSession → SetCursor), shared by /new and dispatch auto-open.
//
// The scope LLM semaphore's active marker (waiter prioritization) and the
// default todo_mode are the turn module's concerns, applied when the turn runs —
// dropped from the session core's open path.
func (m *Manager) OpenSessionInScope(ctx context.Context, scope, source, userID string) (string, error) {
	newSid, err := m.store.OpenSession(ctx, scope, source, userID, "")
	if err != nil {
		return "", err
	}
	return m.finishOpenInScope(ctx, scope, newSid)
}

// OpenSessionInScopeWithCaller is OpenSessionInScope plus the agora dispatch
// caller frame (callerSid → caller_session_id). callerSid="" routes through the
// plain open.
func (m *Manager) OpenSessionInScopeWithCaller(ctx context.Context, scope, source, userID, callerSid string) (string, error) {
	if callerSid == "" {
		return m.OpenSessionInScope(ctx, scope, source, userID)
	}
	newSid, err := m.store.OpenSessionWithCaller(ctx, scope, source, userID, "", callerSid)
	if err != nil {
		return "", err
	}
	return m.finishOpenInScope(ctx, scope, newSid)
}

func (m *Manager) finishOpenInScope(ctx context.Context, scope, newSid string) (string, error) {
	if cErr := m.store.SetCursor(ctx, newSid); cErr != nil {
		slog.Warn("SetCursor after OpenSession failed; new session may not become active until the next cursor move (latest-started is only a tie-breaker among equal-cursor rows, so a prior session with a set cursor still wins)",
			"scope", scope, "sid", newSid, "err", cErr)
	}
	return newSid, nil
}
