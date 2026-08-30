package agora

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/leaf/agora/store"
)

// InboxRouter is the in-memory index mapping a chat scope → agora Inbox. It is
// built from bob_agora_inbox_sources (each binds one external chat via generic
// coordinates) + the OrgCache's inbox rows, and refreshed by Reload whenever
// admin adds/removes a source row.
//
// Lookups are atomic-pointer reads of the cached map, so the hot routing path
// adds essentially no latency (a single map lookup, no lock). Reload swaps the
// cache pointer under r.mu so concurrent Reload calls serialise.
//
// Source of truth: inbox ROWS come from the OrgCache (the module's single RAM
// mirror — never re-read from pg here); only inbox_sources are read from the
// store (the OrgCache does not hold them). The router keys its scope index by
// contract.ScopeFor over each inbox_source's coordinates — the SAME scope
// grammar the trunk session resolver uses (leaf/session/20-resolver.go), so the
// two can never drift. The router itself has ZERO per-source knowledge.
type InboxRouter struct {
	org    *OrgCache
	loader SourceLoader // reads inbox_sources; nil → empty router (agora source-less)
	cache  atomic.Pointer[routerCache]
	mu     sync.Mutex // serialises concurrent Reload calls
}

// SourceLoader is the narrow store surface Reload needs — kept narrow so the
// router can be unit-tested against a stub without a DB.
type SourceLoader interface {
	ListInboxSources(ctx context.Context, inboxID string) ([]store.InboxSource, error)
}

// routerCache is the inner snapshot swapped in by Reload. Immutable once
// published — callers only read.
type routerCache struct {
	// scopeToInbox maps a canonical chat scope ("telegram:group:-100") → inbox_id.
	// Built via contract.ScopeFor over each inbox_source's coordinates.
	scopeToInbox map[string]string
	// inboxesByID is the inbox table by id, copied from the OrgCache snapshot at
	// Reload time (so the router need not reach back into the cache per lookup).
	inboxesByID map[string]store.Inbox
	// inboxesByScope is the inbox table by its OWN Inbox.Scope — for the internal
	// source path (send_message/report encode a target inbox scope verbatim).
	inboxesByScope map[string]store.Inbox
	// groupMembers maps a chat scope → the member bindings on it (one per wired member
	// inbox, with display name + default flag). The basis for virtual-group routing: a
	// group scope with >1 entry is addressed by member (#name / reply / default), and a
	// sub-scope "<scope>#<member>" resolves the named member here.
	groupMembers map[string][]groupMember
}

// groupMember is one member inbox bound to a chat scope (for #member routing).
type groupMember struct {
	Name      string
	InboxID   string
	IsDefault bool
}

// NewInboxRouter builds a router with an empty cache; the caller MUST call
// Reload before first lookup. A nil loader yields a usable (but source-empty)
// router so an agora deployment with no inbox_sources still resolves nothing
// cleanly. org is the entity source of truth (inbox rows).
func NewInboxRouter(org *OrgCache, loader SourceLoader) *InboxRouter {
	r := &InboxRouter{org: org, loader: loader}
	r.cache.Store(&routerCache{
		scopeToInbox:   map[string]string{},
		inboxesByID:    map[string]store.Inbox{},
		inboxesByScope: map[string]store.Inbox{},
		groupMembers:   map[string][]groupMember{},
	})
	return r
}

// Reload rebuilds the cache from the OrgCache (inbox rows) + the store
// (inbox_sources). Safe to call concurrently — the mutex serialises rebuilds
// and atomic.Pointer.Store publishes the new snapshot under it. A load error
// is returned; the caller logs + keeps the prior snapshot in service.
func (r *InboxRouter) Reload(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Inbox rows come from the OrgCache snapshot (single RAM mirror). Snapshot
	// excludes archived inboxes; a paused/active inbox still indexes (the
	// active-only addressability gate runs at Resolve time).
	snap := r.org.Snapshot(ctx, nil)
	c := &routerCache{
		scopeToInbox:   map[string]string{},
		inboxesByID:    make(map[string]store.Inbox, len(snap.Inboxes)),
		inboxesByScope: make(map[string]store.Inbox, len(snap.Inboxes)),
		groupMembers:   map[string][]groupMember{},
	}
	for _, ib := range snap.Inboxes {
		c.inboxesByID[ib.Inbox.ID] = ib.Inbox
		if ib.Inbox.Scope != "" {
			c.inboxesByScope[ib.Inbox.Scope] = ib.Inbox
		}
	}

	if r.loader != nil {
		srcs, err := r.loader.ListInboxSources(ctx, "")
		if err != nil {
			return err
		}
		// Group bindings by chat scope, deduped by DISTINCT inbox: several rows can
		// bind the SAME inbox to one scope (a forum group's topics flatten to one
		// scope — ScopeFor drops thread_id for groups), and those route identically.
		// A scope bound to exactly ONE inbox routes 1:1 (DM + single-member group).
		// A scope bound to MULTIPLE inboxes is a virtual-group fan: the 1:1 map can't
		// pick, so it is left unresolved there and routed by member addressing
		// (#member / reply / default) via groupMembers below.
		byScope := map[string][]store.InboxSource{}
		for _, s := range srcs {
			sc := contract.ScopeFor(s.SourceID, s.ChatType, s.ChatID, s.ThreadID)
			dup := false
			for i, prev := range byScope[sc] {
				if prev.InboxID == s.InboxID {
					// Same inbox via another topic row — merge the default flag only.
					byScope[sc][i].IsDefault = prev.IsDefault || s.IsDefault
					dup = true
					break
				}
			}
			if !dup {
				byScope[sc] = append(byScope[sc], s)
			}
		}
		for sc, list := range byScope {
			if len(list) == 1 {
				c.scopeToInbox[sc] = list[0].InboxID
			} else {
				// A designed config (virtual-group fan), not an anomaly — informational.
				slog.Info("agora: chat scope bound to multiple inboxes — addressed by member (#name / reply / default)", "scope", sc, "inboxes", len(list))
			}
			// groupMembers for EVERY chat scope (the addressable member set: name + default
			// flag). Single-member scopes resolve 1:1 above AND appear here, so a
			// "<scope>#<member>" sub-scope resolves uniformly. A bridge inbox (no member)
			// has no name → not addressable by #member, skip it.
			for _, s := range list {
				ib, ok := c.inboxesByID[s.InboxID]
				if !ok || ib.MemberID == "" {
					continue
				}
				mb, ok := r.org.MemberByID(ib.MemberID)
				if !ok || mb.Name == "" {
					continue
				}
				c.groupMembers[sc] = append(c.groupMembers[sc], groupMember{Name: mb.Name, InboxID: s.InboxID, IsDefault: s.IsDefault})
			}
		}
	}

	r.cache.Store(c)
	slog.Info("agora: inbox router reloaded",
		"inboxes", len(c.inboxesByID), "source_scopes", len(c.scopeToInbox))
	return nil
}

// inboxAddressable reports whether inboxID currently resolves to an addressable
// inbox: it is in the router snapshot, Status == "active", and its owner company
// is not disabled. This is THE resolve-time gate ResolveByScope applies, factored
// out so member SELECTION (RouteGroupScope) pre-filters with the same rule and can
// never pick a member the resolver then refuses (a paused member picked as
// default/^name would strand every message in a fresh ask-who).
func (r *InboxRouter) inboxAddressable(inboxID string) bool {
	if r == nil {
		return false
	}
	c := r.cache.Load()
	if c == nil {
		return false
	}
	ib, ok := c.inboxesByID[inboxID]
	return ok && ib.Status == "active" && r.IsInboxOwnerActive(ib.ID)
}

// resolveInboxInclusive maps a scope to its agora inbox IGNORING the addressable gate, so
// a PAUSED / disabled-owner inbox still resolves. Mirrors ResolveByScope's three paths —
// own-scope, member sub-scope, chat inbox_source (with the email-alias "#" retry) —
// WITHOUT the inboxAddressable filter. Basis for PausedMemberScope (F161).
func (r *InboxRouter) resolveInboxInclusive(scope string) (store.Inbox, bool) {
	if r == nil || scope == "" {
		return store.Inbox{}, false
	}
	c := r.cache.Load()
	if c == nil {
		return store.Inbox{}, false
	}
	if ib, ok := c.inboxesByScope[scope]; ok {
		return ib, true
	}
	if base, member, ok := contract.SplitMemberSubScope(scope); ok {
		for _, gm := range c.groupMembers[base] {
			if gm.Name == member {
				ib, ok := c.inboxesByID[gm.InboxID]
				return ib, ok
			}
		}
		return store.Inbox{}, false
	}
	inboxID, ok := c.scopeToInbox[scope]
	if !ok {
		if i := strings.IndexByte(scope, '#'); i >= 0 {
			return r.resolveInboxInclusive(scope[:i])
		}
		return store.Inbox{}, false
	}
	ib, ok := c.inboxesByID[inboxID]
	return ib, ok
}

// PausedMemberScope reports whether scope maps to a MEMBER agora inbox that EXISTS but is
// PAUSED (Status != "active") while its owner company is still ACTIVE — the reversible
// inbox-level pause. Such a scope stays AGORA's (the flow bounces "member paused"), NOT
// the normal catch-all's, so a temporary member pause doesn't silently reroute a company
// group to generic bob (F161). Company-level pause is separate (IsPaused → company_paused
// bounce). A disabled owner company is NOT this case — it goes the company path.
//
// Kind=="member" ONLY: a paused BRIDGE inbox is not a "member" — bouncing it as a paused
// member would be the wrong wording AND would short-circuit BridgeRouting. Bridge-pause
// behavior is left exactly as it was pre-F161 (falls through to the normal path).
func (r *InboxRouter) PausedMemberScope(scope string) bool {
	ib, ok := r.resolveInboxInclusive(scope)
	if !ok || ib.Kind != "member" {
		return false
	}
	return ib.Status != "active" && r.IsInboxOwnerActive(ib.ID)
}

// ResolveByScope maps a chat scope (src:dm:id / src:group:id) to an active
// Inbox via the inbox_sources index, or by the inbox's OWN scope (internal
// dispatch path). Returns (zero, false) when no source/inbox matches or the
// matched inbox is not active.
func (r *InboxRouter) ResolveByScope(scope string) (store.Inbox, bool) {
	if r == nil || scope == "" {
		return store.Inbox{}, false
	}
	c := r.cache.Load()
	if c == nil {
		return store.Inbox{}, false
	}
	// Internal/native path: the scope IS an inbox's own scope verbatim.
	if ib, ok := c.inboxesByScope[scope]; ok {
		if !r.inboxAddressable(ib.ID) {
			return store.Inbox{}, false
		}
		return ib, true
	}
	// Group member sub-scope: "<group-scope>#<member>" (virtual-group fan; the member
	// was picked at routing time — RouteGroupScope). Resolve the named member among the
	// group's bindings. Only a GROUP base carries a member here (the grammar's one
	// parser is contract.SplitMemberSubScope); a DM "#<alias>" suffix (email
	// forwarded-alias) falls through to the strip-and-retry below.
	if base, member, ok := contract.SplitMemberSubScope(scope); ok {
		for _, gm := range c.groupMembers[base] {
			if gm.Name == member {
				ib, ok := c.inboxesByID[gm.InboxID]
				if !ok || !r.inboxAddressable(ib.ID) {
					return store.Inbox{}, false
				}
				return ib, true
			}
		}
		// No current member matches this sub-scope name. This is INTENTIONALLY fatal
		// (no strip-to-base fallback like the chat path below): members are addressed
		// EXPLICITLY by "^name" (RouteGroupScope), so a stale "#member" only arises when
		// that member was renamed/removed AFTER a reply-anchor was recorded. Falling such
		// a key back to the base would silently route the reply to the default/another
		// member without the human re-addressing — contradicting the resolver's explicit,
		// no-implicit-fallback model. Letting it miss re-roots to a fresh session; the
		// human re-addresses with "^name" (or the default picks up). (audit.)
		return store.Inbox{}, false
	}
	// Chat path: scope → inbox_source → inbox.
	inboxID, ok := c.scopeToInbox[scope]
	if !ok {
		// Email forwarded-alias DMs carry a "#<alias>" sub-scope suffix (the
		// session resolver appends ev.ThreadID — leaf/session/20-resolver.go); the
		// inbox_source matches the base sender scope, so retry once without the
		// suffix. The stripped scope has no '#', so this recurses at most once.
		if i := strings.IndexByte(scope, '#'); i >= 0 {
			return r.ResolveByScope(scope[:i])
		}
		return store.Inbox{}, false
	}
	ib, ok := c.inboxesByID[inboxID]
	if !ok || !r.inboxAddressable(ib.ID) {
		// Only an active inbox whose owner company is NOT disabled is addressable —
		// a paused/archived inbox or a disabled-company inbox is decommissioned:
		// fresh inbound matching its source must NOT spawn a new session on it. The
		// owner-active gate (B-7) makes "company disabled → drop new inbound" real,
		// not just block cross-company sends; IsInboxOwnerActive is fail-open.
		return store.Inbox{}, false
	}
	return ib, true
}

// ScopesForInbox returns every chat scope that routes to inboxID: the inbox's own
// native scope (internal dispatch) plus each wired inbox_source scope. The reverse
// of ResolveByScope — for the webui chat-log reader (flow/agora pages the merged
// history across these scopes). nil when the inbox is unknown. No active/owner gate
// here (a paused inbox still has browsable history); read-only over the snapshot.
func (r *InboxRouter) ScopesForInbox(inboxID string) []string {
	if r == nil || inboxID == "" {
		return nil
	}
	c := r.cache.Load()
	if c == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	if ib, ok := c.inboxesByID[inboxID]; ok && ib.Scope != "" {
		out = append(out, ib.Scope)
		seen[ib.Scope] = true
	}
	for scope, id := range c.scopeToInbox {
		if id == inboxID && !seen[scope] {
			out = append(out, scope)
			seen[scope] = true
		}
	}
	// Fanned-group bindings: a member's history in a SHARED group is under the sub-scope
	// "<group>#<member>". Only fanned scopes need this — a single-member scope routes 1:1
	// (already emitted above via scopeToInbox) and its session uses the plain scope, so
	// skip any scope that resolved 1:1.
	for scope, members := range c.groupMembers {
		if _, single := c.scopeToInbox[scope]; single {
			continue
		}
		for _, gm := range members {
			if gm.InboxID != inboxID {
				continue
			}
			sub := scope + "#" + gm.Name
			if !seen[sub] {
				out = append(out, sub)
				seen[sub] = true
			}
		}
	}
	return out
}

// groupMembersOf returns the addressable member bindings on a chat scope (nil when the
// scope has none). The basis for RouteGroupScope's #member / default pick; a scope with
// >1 entry is a virtual-group fan addressed by member.
func (r *InboxRouter) groupMembersOf(scope string) []groupMember {
	if r == nil {
		return nil
	}
	c := r.cache.Load()
	if c == nil {
		return nil
	}
	return c.groupMembers[scope]
}

// IsInboxOwnerActive reports whether the inbox's owner Company is NOT disabled.
// Fail-open: an unknown inbox / company → true (better to deliver to a maybe-
// disabled company than drop everything while the cache lags). Reads through the
// OrgCache so it sees the latest company status without a router Reload.
func (r *InboxRouter) IsInboxOwnerActive(inboxID string) bool {
	if r == nil || r.org == nil || inboxID == "" {
		return true
	}
	companyID := companyForInbox(r.org, inboxID)
	if companyID == "" {
		return true // unknown ownership → fail-open
	}
	co, ok := r.org.CompanyByID(companyID)
	if !ok {
		return true
	}
	return co.Status != "disabled"
}
