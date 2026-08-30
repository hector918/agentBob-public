package session

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"agentbob/contract"
	"agentbob/i18n"
)

// peekScope resolves the routing scope for a READ-ONLY slash peek (/prompt, /session): the
// base scope PLUS the agora virtual-group member sub-scope ("<group>#<member>"), WITHOUT
// ensureLive's revive (a peek must not mutate). Mirrors ResolveSession's member routing but
// read-only — a reply-hit live session's own Key IS the sub-scope; a fresh group event routes
// via the agora seam; a cold/missing session or a non-group chat falls back to the base scope.
// Without it a virtual group resolves to its bare fan scope, where the agora flow can't pick a
// member (empty /prompt) and CurrentSession finds no member session (wrong /session sid).
func (m *Manager) peekScope(ctx context.Context, res Resolution, ev contract.MessageEvent) string {
	if !ev.ChatType.IsGroupChat() {
		return res.Key
	}
	// Same two member-scope primitives ResolveSession uses (shared so they can't drift) —
	// minus ensureLive's revive (a peek must not mutate). A reply-hit live session's own
	// Key IS the sub-scope; a fresh group event routes via the agora seam; otherwise (cold/
	// missing session, no agora) fall back to the base scope.
	if res.SessionID != "" {
		if key, ok := m.replyHitOwnKey(ctx, res.SessionID); ok {
			return key
		}
		return res.Key
	}
	if sub, ok := m.freshGroupSubScope(ctx, ev); ok {
		return sub
	}
	return res.Key
}

// readScope is the single scope authority for a slash command: the base routing
// scope PLUS the agora virtual-group member sub-scope ("<group>#<member>"),
// WITHOUT reviving a cold-archived session (a slash peek/action must not mutate as
// a side effect of the lookup). Every slash handler routes through this so a
// command targets the SAME scope the member turn actually runs under — never the
// bare group scope. (ResolveSession is the live-dispatch variant that additionally
// revives; agora absent / a DM → this is a no-op returning the base scope.)
func (m *Manager) readScope(ctx context.Context, ev contract.MessageEvent) string {
	return m.peekScope(ctx, resolveBase(ctx, m.store, ev), ev)
}

// slashPrompt handles /prompt (admin-only): dump the live prompt this message
// would produce, read-only — no model, no persist, no history change. It builds a
// PROBE TurnWork (the event with its text replaced by the args, resolved to this
// scope/session) and routes it through the handler's DumpPrompt (router → the
// flow that would handle it → compose-don't-run). Reply-to-bot fields ride along,
// so replying to an old bot message inspects THAT session's prompt.
func (m *Manager) slashPrompt(ctx context.Context, sc contract.SlashContext) error {
	if m.handler == nil {
		return sc.Sink.Finish(i18n.T("slash.prompt.no_handler", sc.Lang))
	}
	ev := sc.Event
	ev.Text = strings.TrimSpace(sc.Args)
	// /prompt is a pure inspection: resolve via resolveBase directly, NOT
	// ResolveSession, so peeking at a reply to a cold-archived message does NOT
	// trigger the revive write-side-effect (recreate session + reimport transcript).
	// A read must not mutate; if the target is archived the dump just builds against
	// empty live history. Reviving stays reserved for the real dispatch path.
	res := resolveBase(ctx, m.store, ev)
	scope := m.peekScope(ctx, res, ev) // carry the agora member sub-scope (read-only) — see peekScope
	// /prompt is ALWAYS invoked by an admin (the only role that can run it); if the probe
	// kept the admin's identity, the dump would ALWAYS project the admin's tool/skill bag,
	// never the inspected session's truth (D20(2)). When the peek resolves to a specific
	// session — a reply-to-bot peek, the common usage — adopt THAT session's owner identity
	// so normal-flow compose shows the session's bag + admin status. (agora compose is
	// inbox/scope-derived, so it already shows the member regardless — this only corrects
	// the normal path.) Read-only: SessionByID never mutates.
	var probeMode contract.TurnMode
	if res.SessionID != "" {
		if s, err := m.store.SessionByID(ctx, res.SessionID); err == nil {
			// Same D20(2) principle as the identity adoption below: the dump must
			// project the SESSION's truth — a looping-stamped session's real prompt
			// carries the work-discipline layer, so the probe must carry the stamp.
			probeMode = s.TurnMode
			if s.Source != "" {
				ev.Source, ev.UserID, ev.IsAdmin = s.Source, s.UserID, s.AdminOwned
			}
		}
	}
	work := contract.TurnWork{
		Sid:      res.SessionID,
		Scope:    scope,
		TurnMode: probeMode,
		Events:   []contract.MessageEvent{ev},
	}
	dump := m.handler.DumpPrompt(ctx, work)
	if strings.TrimSpace(dump) == "" {
		return sc.Sink.Finish(i18n.T("slash.prompt.no_flow", sc.Lang))
	}
	return sc.Sink.Finish(i18n.T("slash.prompt.header", sc.Lang) + dump)
}

// armedPreferCap bounds the armedPrefer map drop-whole (an armed scope that never sees
// another turn is never consumed → it would otherwise leak): mirrors discord
// dmTypeCache / feishu parentCache. A soft one-shot hint is cheap to re-issue if a
// flood clears it. See docs/accumulation-inventory.md #41.
const armedPreferCap = 512

// slashUncensored handles /uncensored: toggle the "uncensored" soft model tag.
func (m *Manager) slashUncensored(ctx context.Context, sc contract.SlashContext) error {
	return m.toggleSessionTag(ctx, sc, "uncensored", "slash.uncensored")
}

// slashThinking handles /thinking: toggle the "thinking" soft model tag, routing the
// session to an entry that reasons before answering. The thought itself rides the
// TRACE channel (leaf/turn/20-round.go) — visible when the scope's /trace pref is on,
// never part of the reply and never fed back as history.
func (m *Manager) slashThinking(ctx context.Context, sc contract.SlashContext) error {
	return m.toggleSessionTag(ctx, sc, "thinking", "slash.thinking")
}

// toggleSessionTag TOGGLES one sticky soft model tag for the event's scope. keyPrefix
// names the command's i18n family (".armed" / ".off" / ".failed").
//
// ON: arm the scope — the issuer's next turn consumes the arm (arbiter.takeArmedPrefer)
// and stamps it onto that turn's session (persisted; every later turn there feeds the
// tags to ModelRequest.Prefer). Scope-keyed arm rather than stamping the active session
// here, because in a group @=new means the issuer's next message may OPEN a new session
// — the arm rides along to wherever that turn lands.
//
// OFF: a pending arm or a stamped active session both count as on; the tag is removed
// from each. Removal is per-TAG, not a wipe of the column: a scope can carry several
// independent toggles at once (/uncensored + /thinking), and clearing the whole set
// would silently switch the others off too.
//
// The tags are SOFT throughout — they land in ModelRequest.Prefer, so arbitrating two
// toggles that no single entry satisfies is the picker's job (it ranks by how many
// Prefer tags an entry hits), not this layer's. Nothing here refuses a combination.
func (m *Manager) toggleSessionTag(ctx context.Context, sc contract.SlashContext, tag, keyPrefix string) error {
	scope := m.readScope(ctx, sc.Event) // member-aware, read-only (the same scope a turn here resolves to)

	// Inspecting the arm and REMOVING it happen in one critical section, before any
	// store I/O. Splitting them would leave a window in which the arbiter's
	// takeArmedPrefer consumes the very arm being turned off (slash dispatch runs
	// out-of-band, so a turn can start underneath): the removal would then find
	// nothing, the reply would say "off", and the tag would be stamped onto the
	// session for its whole life. armedHere always routes to the OFF branch below,
	// so there is no path where a removal here would need undoing.
	key := armKey{scope: scope, userID: sc.Event.UserID}

	// tagMu spans the session row's read AND write below: per-tag independence made
	// both writers read-modify-write, so two toggles issued together — or one racing
	// a turn start that merges an arm — would otherwise lose an edit.
	m.tagMu.Lock()
	defer m.tagMu.Unlock()

	m.mu.Lock()
	armTags, hasArm := m.armedPrefer[key]
	armedHere := hasArm && containsTag(armTags, tag)
	if armedHere {
		if rest := withoutTag(armTags, tag); len(rest) == 0 {
			delete(m.armedPrefer, key)
		} else {
			m.armedPrefer[key] = rest
		}
	}
	m.mu.Unlock()

	// A stamped active session also means ON. The STAMP belongs to the session, not
	// to a user, so this half is deliberately not issuer-scoped: anyone in the scope
	// can turn off a tag the session is carrying. Read-only misses (no session /
	// store error) just mean "nothing stamped here".
	var (
		sid       string
		stampTags []string
		stamped   bool
	)
	if cur, ok, err := m.store.CurrentSession(ctx, scope); err == nil && ok {
		if info, ierr := m.store.SessionByID(ctx, cur); ierr == nil {
			sid, stampTags = cur, info.PreferModelTags
			stamped = containsTag(stampTags, tag)
		}
	}

	if armedHere || stamped {
		if stamped {
			if err := m.store.SetSessionPreferModelTags(ctx, sid, withoutTag(stampTags, tag)); err != nil {
				slog.Warn("session: clear sticky model tag failed", "sid", sid, "tag", tag, "err", err)
				_ = sc.Sink.Finish(i18n.T(keyPrefix+".failed", sc.Lang))
				return err
			}
		}
		return sc.Sink.Finish(i18n.T(keyPrefix+".off", sc.Lang))
	}

	// The arm carries only the DELTA — the merge against whatever the session already
	// has happens at CONSUMPTION (30-arbiter.go), where the landing sid is known.
	// Computing the merged set here would fix it against TODAY's active session, and
	// between arming and consumption the scope's active session can change (a group @
	// opening a new one, a reply reviving a cold one) — the stale set would then be
	// stamped onto a session that never carried those tags, silently.
	m.mu.Lock()
	if cur, exists := m.armedPrefer[key]; exists {
		// This issuer's own pending arm — add to it. No cap check: the key already
		// exists, so the map cannot grow here.
		m.armedPrefer[key] = append(append([]string{}, cur...), tag)
	} else {
		if len(m.armedPrefer) >= armedPreferCap {
			m.armedPrefer = map[armKey][]string{} // drop-whole cap — bound the never-consumed leak
		}
		m.armedPrefer[key] = []string{tag}
	}
	m.mu.Unlock()
	return sc.Sink.Finish(i18n.T(keyPrefix+".armed", sc.Lang))
}

// containsTag reports whether tags holds tag.
func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// withoutTag returns tags minus every occurrence of tag, as a fresh slice (callers
// hold the original in a map entry or a store row — neither may alias the result).
// nil when nothing is left, which is what the store reads as "clear the column".
func withoutTag(tags []string, tag string) []string {
	var out []string
	for _, t := range tags {
		if t != tag {
			out = append(out, t)
		}
	}
	return out
}

// mergeTags returns base plus every tag of add not already in base, order-stable and
// duplicate-free, as a fresh slice. This is how a pending arm's DELTA lands on the
// session it actually reaches (30-arbiter.go).
func mergeTags(base, add []string) []string {
	out := append([]string{}, base...)
	for _, t := range add {
		if !containsTag(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// slashNew handles /new: open a FRESH session in the event's scope and make it
// active. It resolves the scope itself (the single scope authority) — the slash
// dispatch hands a raw event, not a pre-resolved scope. Runs out-of-band (no
// arbiter): OpenSessionInScope is a store write that doesn't disturb an in-flight
// turn on the old active sid; the cursor move just routes the NEXT message.
func (m *Manager) slashNew(ctx context.Context, sc contract.SlashContext) error {
	scope := m.readScope(ctx, sc.Event) // member-aware: in a virtual group, reset the ADDRESSED member's session, not the bare group scope
	if _, err := m.OpenSessionInScope(ctx, scope, sc.Event.Source, sc.Event.UserID); err != nil {
		_ = sc.Sink.Finish(i18n.T("slash.new.failed", sc.Lang))
		return err
	}
	return sc.Sink.Finish(i18n.T("slash.new.ok", sc.Lang))
}

// slashSession handles /session: a READ-ONLY peek at the event's routing — the
// resolved scope key and the scope's active sid (if any). It reads CurrentSession
// directly rather than ResolveActiveSid so a peek never fabricates a session (the
// SlashContext contract's rule), matching the doc: "a peek doesn't fabricate a
// session." Runs out-of-band; no arbiter, no writes.
func (m *Manager) slashSession(ctx context.Context, sc contract.SlashContext) error {
	// readScope (no revive): a read must not mutate — ResolveSession's ensureLive→restore
	// would REVIVE a cold-archived session just because the peek replied to one of its
	// messages. It also carries the agora virtual-group member sub-scope — else
	// CurrentSession reads the BARE group key (a fan scope with no member session) and
	// shows the wrong/empty sid instead of the worker's.
	scope := m.readScope(ctx, sc.Event)
	var b strings.Builder
	b.WriteString(i18n.T("slash.session.header", sc.Lang))
	fmt.Fprintf(&b, "\n"+i18n.T("slash.session.scope", sc.Lang), scope)
	fmt.Fprintf(&b, "\n"+i18n.T("slash.session.user", sc.Lang), sc.Event.UserID)
	var curSid string
	if sid, ok, err := m.store.CurrentSession(ctx, scope); err != nil {
		b.WriteString("\n" + i18n.T("slash.session.sid_read_failed", sc.Lang))
	} else if ok {
		curSid = sid
		fmt.Fprintf(&b, "\n"+i18n.T("slash.session.sid", sc.Lang), sid)
	} else {
		b.WriteString("\n" + i18n.T("slash.session.no_session", sc.Lang))
	}
	// flow + mode via the read-only routing probe: the flow name is never persisted
	// (only the mode is), and a peek runs no turn, so we ask the router which flow
	// WOULD handle this event and what mode it imposes (FlowInfo == DumpPrompt's
	// Accepts pick, no model / no persist). ok=false → no flow accepts; omit silently.
	if m.handler != nil {
		probe := contract.TurnWork{Scope: scope, Events: []contract.MessageEvent{sc.Event}}
		if name, mode, ok := m.handler.FlowInfo(ctx, probe); ok {
			fmt.Fprintf(&b, "\n"+i18n.T("slash.session.flow", sc.Lang), name)
			fmt.Fprintf(&b, "\n"+i18n.T("slash.session.mode", sc.Lang), describeMode(mode, sc.Lang))
		}
	}
	// Switchboard: every per-scope / per-session toggle in one glance. The turn-
	// mode line shows the session's birth stamp NEXT TO the scope default — they
	// differ exactly when /looping was flipped after this session's birth (the
	// "changed but not yet in effect" state the birth-stamp design makes possible;
	// the panel is how an operator sees it instead of guessing).
	scopeMode, _ := m.store.GetScopeTurnMode(ctx, scope) // error → "" reads as regular
	stamp := "—"                                         // no live session → no stamp to show (the NEXT session takes the default)
	var stampTags []string
	if curSid != "" {
		stampMode := contract.TurnModeRegular
		if info, err := m.store.SessionByID(ctx, curSid); err == nil {
			stampMode = info.TurnMode
			stampTags = info.PreferModelTags
		}
		stamp = describeTurnMode(stampMode)
	}
	fmt.Fprintf(&b, "\n"+i18n.T("slash.session.turnmode", sc.Lang),
		stamp, describeTurnMode(scopeMode))
	prefs := m.resolvePrefs(ctx, scope)
	fmt.Fprintf(&b, "\n"+i18n.T("slash.session.sinkprefs", sc.Lang),
		onOff(prefs.Trace, sc.Lang), onOff(prefs.Stream, sc.Lang))
	// Context gauge (read-only, zero store reads): the turn core's in-memory
	// per-sid reading — LAST TURN's real prompt size (resp.Usage, so it includes
	// system prompt / tool specs / template overhead) and the last sizing budget
	// (the static winner's window). 0 → "—": fresh boot, just compacted, or the
	// sid hasn't run a round yet. Soft seam: a turn impl without ContextGauge
	// (or no turn yet) simply omits the line.
	if curSid != "" && m.turnRef != nil {
		if tg, ok := m.turnRef().(interface {
			ContextGauge(sid string) (int, int, int)
		}); ok {
			toks, budget, rows := tg.ContextGauge(curSid)
			fmt.Fprintf(&b, "\n"+i18n.T("slash.session.context", sc.Lang), gaugeNum(toks), gaugeNum(budget), gaugeNum(rows))
		}
	}
	if len(stampTags) > 0 {
		fmt.Fprintf(&b, "\n"+i18n.T("slash.session.modeltags", sc.Lang), strings.Join(stampTags, ", "))
	}
	return sc.Sink.Finish(b.String())
}

// describeTurnMode renders a TurnMode for the /session switchboard. The enum
// words are untranslated identifiers (the /looping confirmations use the same
// bare words), so no i18n indirection.
func describeTurnMode(mode contract.TurnMode) string {
	if mode == contract.TurnModeLooping {
		return "looping"
	}
	return "regular"
}

// gaugeNum renders a context-gauge number; 0 = unknown → "—" (same dash the
// turn-mode stamp uses for "nothing to show").
func gaugeNum(n int) string {
	if n <= 0 {
		return "—"
	}
	return strconv.Itoa(n)
}

// onOff renders a boolean toggle for the /session switchboard.
func onOff(v bool, lang string) string {
	if v {
		return i18n.T("slash.session.toggle.on", lang)
	}
	return i18n.T("slash.session.toggle.off", lang)
}

// describeMode renders a SessionMode with a short behavior gloss for the /session
// peek, so the reader learns how the session behaves (queue/verify regime), not
// just the bare enum. An empty/unknown mode reads as not-yet-determined.
func describeMode(mode contract.SessionMode, lang string) string {
	switch mode {
	case contract.SessionModeManual:
		return i18n.T("slash.session.mode_manual", lang)
	case contract.SessionModeAuto:
		return i18n.T("slash.session.mode_auto", lang)
	case contract.SessionModeNone:
		return i18n.T("slash.session.mode_none", lang)
	default:
		return i18n.T("slash.session.mode_unknown", lang)
	}
}

// slashCloseSession handles /close-session: cancel the turn currently running in
// the sender's scope (cascade-cancels its in-flight tool calls via ctx). It scans
// the arbiter's busy set for a sid in this scope — robust to a cursor move (/new)
// since it stops whatever is actually running, not the cursor-active sid. Setting
// closing[sid] abandons that sid's queued follow-up work; the cancelled turn exits
// silently (I8/I14) and its drain does the cleanup. The confirmation here is the
// only user-visible reply (the turn itself says nothing on cancel).
func (m *Manager) slashCloseSession(ctx context.Context, sc contract.SlashContext) error {
	// readScope (member-aware, no revive): a running member turn's sidMeta.Scope is its
	// "<group>#<member>" sub-scope, so the busy-scan must target that — not the bare
	// group scope — or /close-session can never match a fanned virtual-group member.
	scope := m.readScope(ctx, sc.Event)
	m.mu.Lock()
	var target string
	for sid, meta := range m.sidMeta {
		if meta.Scope == scope && m.busy[sid] {
			target = sid
			break
		}
	}
	// busy is the truth of "a turn is running" — record the close intent against it
	// REGARDLESS of whether the cancel func is registered yet. submit sets busy under
	// the lock, but runTurnAndDrain registers m.cancels[sid] only AFTER its WAL write
	// (a slow store op with no lock held). A /close-session landing in that window
	// must still take effect: closing[sid] is honored at cancel-registration time
	// (30-arbiter.go), so the turn is cancelled even if cancel is nil here. Keying the
	// ok/none reply on busy (not on the racy cancels entry) also stops a real running
	// turn from being reported as "nothing running".
	var cancel context.CancelFunc
	if target != "" {
		m.closing[target] = true
		cancel = m.cancels[target] // may be nil if we raced the registration
	}
	// A fresh start drops every pending toggle arm at this scope — all issuers', not
	// just the caller's: /close-session ends the conversation for everyone in it, so
	// leaving somebody's arm to land on the NEXT one would carry a preference across
	// a boundary the user just drew.
	for k := range m.armedPrefer {
		if k.scope == scope {
			delete(m.armedPrefer, k)
		}
	}
	m.mu.Unlock()
	if target == "" {
		return sc.Sink.Finish(i18n.T("slash.close.none", sc.Lang))
	}
	if cancel != nil {
		cancel()
	}
	// Echo the closed sid: a scope can hold two busy sids at once (the active session
	// plus a reply-to-an-old-message turn), and the busy-scan picks the first match, so
	// the operator must see WHICH turn was stopped. The sid is an opaque id — appended
	// raw (not translated). (L-session-D1)
	return sc.Sink.Finish(i18n.T("slash.close.ok", sc.Lang) + " · " + target)
}

// slashTrace handles /trace [on|off]: toggle (or set) the scope's process-trace
// pref — whether "what bob is doing" annotations (tool progress, iteration
// markers) are shown alongside the reply. Persisted per scope so it survives
// restarts (the SinkPrefs contract). Bare /trace flips the current state.
func (m *Manager) slashTrace(ctx context.Context, sc contract.SlashContext) error {
	scope := m.readScope(ctx, sc.Event) // member-aware: toggle the ADDRESSED member's prefs, the scope its turn reads
	prefs, err := m.store.GetScopePrefs(ctx, scope)
	if err != nil {
		return sc.Sink.Finish(i18n.T("slash.trace.failed", sc.Lang))
	}
	switch strings.ToLower(strings.TrimSpace(sc.Args)) {
	case "on":
		prefs.Trace = true
	case "off":
		prefs.Trace = false
	case "":
		prefs.Trace = !prefs.Trace
	default:
		return sc.Sink.Finish(i18n.T("slash.trace.usage", sc.Lang))
	}
	if err := m.store.SetScopePrefs(ctx, scope, prefs); err != nil {
		return sc.Sink.Finish(i18n.T("slash.trace.failed", sc.Lang))
	}
	if prefs.Trace {
		return sc.Sink.Finish(i18n.T("slash.trace.on", sc.Lang))
	}
	return sc.Sink.Finish(i18n.T("slash.trace.off", sc.Lang))
}

// slashLooping handles /looping [on|off]: toggle (or set) the scope's DEFAULT
// turn mode (docs/turn-driver-split.md). The default is BIRTH-STAMPED onto a
// session at creation and immutable for that session's life — so the toggle
// affects only FUTURE sessions (/close-session, or a fresh @ in a group, starts
// one under the new default); the confirmation says so, and /session shows the
// stamp vs the default. Bare /looping flips. Admin-only (registration): looping
// raises a turn's round budget from 40 to the 500 fuse.
func (m *Manager) slashLooping(ctx context.Context, sc contract.SlashContext) error {
	scope := m.readScope(ctx, sc.Event) // member-aware, same as /trace
	cur, err := m.store.GetScopeTurnMode(ctx, scope)
	if err != nil {
		return sc.Sink.Finish(i18n.T("slash.looping.failed", sc.Lang))
	}
	var next contract.TurnMode
	switch strings.ToLower(strings.TrimSpace(sc.Args)) {
	case "on":
		next = contract.TurnModeLooping
	case "off":
		next = contract.TurnModeRegular
	case "":
		if cur == contract.TurnModeLooping {
			next = contract.TurnModeRegular
		} else {
			next = contract.TurnModeLooping
		}
	default:
		return sc.Sink.Finish(i18n.T("slash.looping.usage", sc.Lang))
	}
	if err := m.store.SetScopeTurnMode(ctx, scope, next); err != nil {
		return sc.Sink.Finish(i18n.T("slash.looping.failed", sc.Lang))
	}
	if next == contract.TurnModeLooping {
		return sc.Sink.Finish(i18n.T("slash.looping.on", sc.Lang))
	}
	return sc.Sink.Finish(i18n.T("slash.looping.off", sc.Lang))
}

// slashWhoami handles /whoami: the sender's identity as the gate/dispatch sees it
// (source, user id, chat type, admin flag) plus where their messages route (the
// resolved scope). The debug companion to /session — confirms how an inbound is
// being identified and routed. Read-only.
func (m *Manager) slashWhoami(ctx context.Context, sc contract.SlashContext) error {
	ev := sc.Event
	role := "user"
	if sc.IsAdmin {
		role = "admin"
	}
	var b strings.Builder
	fmt.Fprintf(&b, i18n.T("slash.whoami.identity", sc.Lang),
		ev.Source, ev.UserID, ev.ChatType, role)
	// readScope (member-aware, no revive): show the SAME scope the turn routes to — in a
	// virtual group that is the member sub-scope, so /whoami agrees with /session.
	fmt.Fprintf(&b, "\n"+i18n.T("slash.whoami.scope", sc.Lang), m.readScope(ctx, ev))
	// account: shown only when the accounts subsystem is wired. Bound → name it;
	// unbound → say so (so the sender can see why /bindaccount complains).
	if m.accounts != nil {
		if acc := m.accounts(); acc != nil {
			if ai, ok := acc.AccountFor(ctx, ev); ok {
				flow := ai.Flow
				switch {
				case !ai.FlowKnown:
					// transient store blip: flow unread this turn — don't claim any entitlement.
					flow = "(unknown)"
				case flow == "":
					// KNOWN-empty = a bare/onboarding account: NO entitlement yet, not
					// "normal" (EntitledFlows has no implicit floor — mirrors the
					// accounts panel's "(pending)"). The router funnels it to intro.
					flow = "(pending)"
				}
				fmt.Fprintf(&b, "\n"+i18n.T("slash.whoami.account", sc.Lang), ai.DisplayName, ai.ID, flow)
			} else {
				b.WriteString("\n" + i18n.T("slash.whoami.account_none", sc.Lang))
			}
		}
	}
	return sc.Sink.Finish(b.String())
}
