package session

import (
	"context"
	"errors"
	"log/slog"

	"agentbob/contract"
	"agentbob/leaf/session/store"
)

// Resolution is the outcome of routing one inbound event to a session scope.
//
//   - Key is the routing scope (e.g. "telegram:dm:123", "telegram:group:456").
//   - SessionID is non-empty when a reply pointed at a specific past session
//     (the dispatcher joins that sid rather than the scope's active one).
//   - OpenNew asks the dispatcher to open a FRESH session in Key rather than
//     join the active one (group bare @mention = @=new; email fresh compose).
//   - Drop=true means the event must be silently discarded (agora disabled-
//     company overlay — added when agora lands; never set by resolveBase).
type Resolution struct {
	Key       string
	SessionID string
	Drop      bool
	OpenNew   bool
}

// resolveBase computes the routing key + optional explicit sid using only the
// store (DM / group / reply-reconnect / internal rules). Private and NOT
// agora-aware: the single public authority is ResolveSession, which (when agora
// lands) consults the inbox overlay and then falls through here. Privatising it
// keeps the cross-scope attachment bug — a caller reaching the base directly and
// getting a non-agora answer — impossible.
func resolveBase(ctx context.Context, st store.Store, ev contract.MessageEvent) Resolution {
	// Internal dispatch source carries the target scope verbatim in ChatID. A
	// dispatch may ask to open a FRESH session at the target (force-new, an agent
	// declaring "new task") — read it off the dispatch frame; otherwise resume the
	// scope's active session. Force-new only SUPERSEDES the active session; it never
	// closes the old one (sessions are never closed by design).
	if ev.IsInternal() && ev.ChatID != "" {
		return Resolution{Key: ev.RoutingScope(), OpenNew: ev.Dispatch != nil && ev.Dispatch.ForceNewSession}
	}
	if ev.ChatType == contract.ChatDM {
		// The scope grammar (incl. the "#<thread>" forwarded-alias sub-scope) is
		// the single authority contract.ScopeFor — agora's inbox router computes
		// the same key, so the two can't drift.
		key := ev.RoutingScope()
		// Per-reply DM session routing (email opt-in): a reply to a recorded bob
		// message continues/revives THAT session; a fresh message opens a NEW
		// one. Chat DMs leave DMReplyRouting false → the plain active-session
		// behaviour (a message continues the running conversation).
		if ev.DMReplyRouting {
			for _, mid := range append([]string{ev.ReplyToID}, ev.ReplyRefs...) {
				if mid == "" {
					continue
				}
				sid, _, ok, err := st.SessionForMessage(ctx, key, mid)
				if err != nil {
					// A transient read error is NOT a miss: degrade to the
					// scope's active session (continue), NOT a fresh OpenNew, so
					// a blip can't fork an empty session and split a mail thread.
					slog.Warn("message_index lookup failed (dm reply-routing) — continuing active session", "err", err)
					return Resolution{Key: key}
				}
				if ok {
					return Resolution{Key: key, SessionID: sid}
				}
			}
			// No ancestor hit a recorded bob message → fresh compose → new session.
			return Resolution{Key: key, OpenNew: true}
		}
		return Resolution{Key: key}
	}
	// A group is ONE scope (src:group:<chat>) holding many sessions. A reply that
	// hits one of bob's own recorded messages reconnects to THAT message's session;
	// a fresh @mention (or a reply to a message with no recorded session) opens a
	// fresh one (@=new) — continue an old topic by REPLYING to it. Stops one group
	// session growing unbounded → compress → drift.
	//
	// The message_index is the AUTHORITY on "is this one of bob's messages": it holds
	// only bob's outbound ids (the flows' OnAnchor → RecordSent), so a HIT means the
	// reply targets bob — the source need not also flag ReplyToBot. That flag is only
	// a hint here, NOT a gate: Discord does not guarantee inlining the referenced
	// message's author, so ReplyToBot can be false on a genuine reply-to-bob; gating
	// on it forks a context-less @=new session. Mirrors the DM reply-routing branch
	// (ancestor chain + index lookup as the authority).
	scope := ev.RoutingScope()
	for _, mid := range append([]string{ev.ReplyToID}, ev.ReplyRefs...) {
		if mid == "" {
			continue
		}
		sid, _, ok, err := st.SessionForMessage(ctx, scope, mid)
		if err != nil {
			// A transient read error is NOT a miss: degrade to the scope's active
			// session (continue), NOT a fresh OpenNew, so a blip can't fork an
			// empty session and split a live group thread.
			slog.Warn("message_index lookup failed — continuing active session", "err", err)
			return Resolution{Key: scope}
		}
		if ok {
			return Resolution{Key: scope, SessionID: sid}
		}
	}
	// No reply, or the replied-to message has no recorded bob session → fresh @=new.
	return Resolution{Key: scope, OpenNew: true}
}

// ResolveSession routes an inbound event to a session scope — the single public
// authority. Under the chat-scoped model the session KEY is always the real
// chat scope from resolveBase. The agora overlay (drop fresh inbound for a
// disabled company) is added when agora lands; today this wraps resolveBase with
// the cold-rehydrate step (below).
//
// resolveBase stays PURE (store-only routing); the liveness test + restore live
// HERE. When a reply resolved to an explicit sid that is no longer live in
// bob_sessions (cold-archived), we try to revive it: a was-alive archive row is
// rehydrated in place (keep the sid); a was-ended / missing one is not revivable,
// so we drop the sid and ask the dispatcher to open a fresh session in the scope.
func (m *Manager) ResolveSession(ctx context.Context, ev contract.MessageEvent) Resolution {
	r := resolveBase(ctx, m.store, ev)
	// Virtual-group member routing (agora optional; nil seam → groups route 1:1). A GROUP
	// event resolves to a specific member's SUB-scope "<group>#<member>" so each member
	// has its own session + resolvable inbox:
	//   - fresh (no reply hit): pick by "#name" / group default via the agora seam.
	//   - reply hit: the joined session was opened under a member sub-scope, but resolveBase
	//     only knew the bare group scope (the member's outbound anchored under the group
	//     scope so the reply could find the sid). Use the joined session's OWN key so
	//     work.Scope (= Resolution.Key) carries the member, not the ambiguous group scope.
	if !r.Drop && r.SessionID == "" && ev.ChatType.IsGroupChat() {
		if sub, ok := m.freshGroupSubScope(ctx, ev); ok {
			r.Key = sub
		}
	}
	if r.SessionID == "" || r.Drop {
		return r
	}
	triedSid := r.SessionID // preserved across ensureLive so a revive-fail can still recover the member (F6)
	r = m.ensureLive(ctx, r)
	// Reply-hit: AFTER ensureLive the joined session is LIVE (revived if it was cold), so
	// read its OWN key — a fanned-group member session's key is the "<group>#<member>"
	// sub-scope that work.Scope must carry (resolveBase knew only the bare group scope; a
	// COLD member session's SessionByID would have errored before the revive).
	if r.SessionID != "" && ev.ChatType.IsGroupChat() && m.groupRoute() != nil {
		if key, ok := m.replyHitOwnKey(ctx, r.SessionID); ok {
			r.Key = key
		}
	} else if r.OpenNew && ev.ChatType.IsGroupChat() && m.groupRoute() != nil {
		// F6: revive FAILED on a virtual-group reply (was-ended / purged). Don't fall to
		// the bare group scope (→ downstream ask-who while the SAME text without a reply
		// routes to a member). Recover the dead session's OWN member sub-scope from the
		// archive so the reply reopens under the right member; if the archive is gone,
		// route by the reply's ^name / default member; only then keep the bare scope.
		if key, ok := m.archivedMemberSubScope(ctx, triedSid, r.Key); ok {
			r.Key = key
		} else if sub, ok := m.freshGroupSubScope(ctx, ev); ok {
			r.Key = sub
		}
	}
	return r
}

// archivedMemberSubScope reads a dead (revive-failed) session's own scope from the
// archive and returns it ONLY when it is a member sub-scope of base ("<base>#<member>")
// — so a reply to a purged virtual-group session reopens under the member it belonged
// to, not the ambiguous bare group scope (F6). ("", false) when unarchived, on a read
// error, or when the archived scope is not a member sub-scope of this group.
func (m *Manager) archivedMemberSubScope(ctx context.Context, sid, base string) (string, bool) {
	scope, ok, err := m.store.ArchivedSessionScope(ctx, sid)
	if err != nil || !ok || scope == "" {
		return "", false
	}
	if b, _, isMember := contract.SplitMemberSubScope(scope); !isMember || b != base {
		return "", false // not this group's member sub-scope — don't cross-route
	}
	return scope, true
}

// freshGroupSubScope picks a FRESH group event's member sub-scope ("<group>#<member>")
// via the agora group-route seam. The single primitive shared by ResolveSession's
// fresh-route block and peekScope (80-slash.go), so the two can't drift. ("", false) when
// there is no agora seam, no route, or an empty sub — the caller keeps the base group scope.
func (m *Manager) freshGroupSubScope(ctx context.Context, ev contract.MessageEvent) (string, bool) {
	gr := m.groupRoute()
	if gr == nil {
		return "", false
	}
	sub, _, ok := gr.RouteGroupScope(ctx, ev)
	if !ok || sub == "" {
		return "", false
	}
	return sub, true
}

// replyHitOwnKey returns a reply-hit session's OWN scope key (a fanned-group member
// session's key is its "<group>#<member>" sub-scope). The single primitive shared by
// ResolveSession's reply-hit block (after ensureLive revives a cold session) and peekScope.
// ("", false) on a read error or an empty key — the caller keeps the base scope.
func (m *Manager) replyHitOwnKey(ctx context.Context, sessionID string) (string, bool) {
	s, err := m.store.SessionByID(ctx, sessionID)
	if err != nil || s.Key == "" {
		return "", false
	}
	return s.Key, true
}

// ensureLive guarantees r.SessionID names a live session, reviving it from the
// archive when it is cold. A live row → r unchanged. A non-live sid → restore
// (was-alive) keeps the sid; a failed/declined restore (was-ended / gone / no
// archiver) drops the sid and returns OpenNew for a fresh session.
func (m *Manager) ensureLive(ctx context.Context, r Resolution) Resolution {
	if _, err := m.store.SessionByID(ctx, r.SessionID); err == nil {
		return r // live row exists — join it
	} else if !errors.Is(err, contract.ErrNoRows) {
		// A transient read error is NOT a miss: degrade to joining the named sid
		// rather than forking a fresh session on a blip (the dispatcher's own
		// liveness handling covers a genuinely-gone sid).
		slog.Warn("session: liveness check failed — joining named session", "sid", r.SessionID, "err", err)
		return r
	}
	if m.restore != nil {
		// singleflight by sid: two concurrent replies to the same archived message
		// share ONE Restore, so the transcript can't be double-inserted (the loser
		// joins the winner's result and finds the now-live session).
		revived, _, _ := m.sfRestore.Do(r.SessionID, func() (any, error) {
			return m.restore(ctx, r.SessionID), nil
		})
		if revived.(bool) {
			return r // revived in place — keep the sid
		}
	}
	// Was-ended / gone / no archiver → not revivable → fresh session in the scope.
	return Resolution{Key: r.Key, OpenNew: true}
}
