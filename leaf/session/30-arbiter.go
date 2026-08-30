package session

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// servedPendingTimeout bounds the post-turn WAL-drain write. It runs on an
// INDEPENDENT short ctx (not the turn's, which may be cancelled / shutting down)
// so a turn that finishes right as shutdown lands still drains its served rows —
// otherwise boot recovery has to reap them (silently) one boot later.
const servedPendingTimeout = 5 * time.Second

// startTurn claims the sid (caller already holds m.mu and set busy/sidMeta) and
// runs the turn. It is called SYNCHRONOUSLY from submit's goroutine for the
// first turn; drainSidAfterTurn spawns follow-ups on their own goroutines.
func (m *Manager) runTurnAndDrain(ctx context.Context, src contract.Source, scope, sid string, batch []contract.MessageEvent, hadDrops bool, turn uint64, isDrain bool) {
	// Address the batch by ITS OWN source, not the dispatching turn's: a drain /
	// panic-redispatch batch may have queued behind a turn a DIFFERENT source
	// triggered (an internal bridge wake), and the bridge's caps/send are wrong
	// for the humans' messages — Caps.ReplyTo=false drops the reply anchor and
	// Send is a no-op, so the cleared-ack silently vanishes. Resolved per batch
	// (before the deferred recoverTurn captures src); an all-internal batch or an
	// unknown source falls back to the caller's src.
	src = m.batchSource(batch, src)
	defer m.recoverTurn(ctx, src, scope, sid, turn)
	// §restart-recovery: WAL-record every message THIS turn is about to run, at the
	// moment it goes in-flight. Done here (not at submit) so it uniformly covers the
	// first turn AND drained/redispatched follow-ups — a queued message promoted to a
	// running turn is genuinely in-flight, so a crash mid-turn must leave it a row
	// for boot recovery to reap. removeServedPending clears these when RunTurn
	// returns; internal-source events no-op (not WAL'd). No lock held here (callers
	// unlock before invoking), so the store write is safe.
	for i := range batch {
		m.recordPending(ctx, scope, batch[i])
	}
	// The turn runs under a child ctx whose cancel is the /close-session lever
	// (registered per-sid). The DRAIN keeps the ORIGINAL ctx so a /close-session
	// cancel never trips drainSidAfterTurn's shutdown early-return — close is a
	// per-turn stop, not a shutdown.
	turnCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancels[sid] = cancel
	// A /close-session can land BETWEEN submit setting busy and this registration
	// (the recordPending WAL write above runs lock-free and can be slow). slashCloseSession
	// records that as closing[sid] keyed on busy, so honor it here: cancel before RunTurn
	// sees turnCtx, and the turn exits on the same silent-cancel path as a normal close.
	alreadyClosing := m.closing[sid]
	// F146: the messages already queued when this turn starts. takeNudge (the nudge
	// fast-lane) pulls ONLY events that arrive AFTER this snapshot — a genuine mid-turn
	// interjection — so the pre-queued backlog drains in order rather than the nudge
	// yanking a queued text forward (e.g. past an unprocessed image).
	preTurn := make(map[string]bool, len(m.pending[sid]))
	for i := range m.pending[sid] {
		if id := m.pending[sid][i].MessageID; id != "" {
			preTurn[id] = true
		}
	}
	m.mu.Unlock()
	if alreadyClosing {
		cancel()
	}
	work := contract.TurnWork{
		Sid:      sid,
		Scope:    scope,
		Events:   batch,
		Target:   m.targetFor(ctx, sid, batch[0], src),
		Prefs:    m.resolvePrefs(ctx, scope),
		HadDrops: hadDrops,
	}
	// Sticky model-tag preference (/uncensored, /thinking): an arm pending at the
	// scope AND issued by a user present in THIS batch is consumed here and MERGED
	// into the session's stamp (persisted — sticky for the session's life);
	// otherwise the session's previously stamped tags apply unchanged. Either way
	// the tags feed TurnWork.PreferredModelTags → ModelRequest.Prefer (soft).
	armedTags := m.takeArmedPrefer(scope, batch)
	_, hasHuman := contract.FirstHumanEvent(batch)
	// Unconditional read: the turn-mode BIRTH STAMP (docs/turn-driver-split.md)
	// must reach EVERY turn — including the rare fresh-arm turn the old
	// conditional skipped. The read already ran on virtually every turn; making
	// it unconditional trades that rare saved query for a stamp that can never
	// silently default. Read failure → zero values (regular, no tags) — same
	// fail-soft posture as before. It also doubles as the admin-ownership read for
	// a pure-internal batch (no human event — a dispatch return / cron wake), which
	// has no in-batch admin for flow/normal to read and so inherits the session's
	// persisted admin bit.
	if info, err := m.store.SessionByID(ctx, sid); err == nil {
		work.TurnMode = info.TurnMode
		work.PreferredModelTags = info.PreferModelTags
		if !hasHuman {
			work.SessionAdmin = info.AdminOwned
		}
	}
	// The arm carries a DELTA — the tags to ADD (80-slash.go::toggleSessionTag) —
	// so it merges into the stamp read just above instead of replacing it. Merging
	// HERE rather than at /command time is what keeps a toggle honest when the
	// scope's active session changed in between (a group @ opening a fresh one, a
	// reply reviving a cold one): the merge always runs against the session the arm
	// actually lands on, so a second toggle can never drop the first, and a set
	// computed against a since-replaced session can never be stamped onto this one.
	if len(armedTags) > 0 {
		// tagMu spans a FRESH read and the write: the stamp read above is outside the
		// lock (the ordinary no-arm turn must not serialize on it), so it may already
		// be stale by the time we get here — a /uncensored issued a moment ago could
		// have written between the two. Re-reading under the lock is what makes the
		// merge a real read-modify-write instead of a hopeful one. Taken only on the
		// rare arm-consuming turn, never on the hot path.
		m.tagMu.Lock()
		base := work.PreferredModelTags
		if fresh, err := m.store.SessionByID(ctx, sid); err == nil {
			base = fresh.PreferModelTags
		}
		merged := mergeTags(base, armedTags)
		if err := m.store.SetSessionPreferModelTags(ctx, sid, merged); err != nil {
			slog.Warn("session: persist sticky model tags failed; preference applies to this turn only", "sid", sid, "err", err)
		}
		m.tagMu.Unlock()
		work.PreferredModelTags = merged
	}
	// The flow self-reports its SessionMode via OnMode (the router calls it once,
	// from its single flow pick) — we cache it on sidMeta (the busy-queue reads it to
	// gate notices) and persist it (out-of-turn readers). turnMode mirrors it for the
	// cleared-ack below. The nudge fast-lane is wired unconditionally; the router
	// NILs it for an auto flow, so an automated turn is never nudged.
	turnMode := contract.SessionModeNone
	work.OnMode = func(mode contract.SessionMode) {
		turnMode = mode
		m.mu.Lock()
		if meta, ok := m.sidMeta[sid]; ok && meta.turn == turn {
			meta.mode = mode
			m.sidMeta[sid] = meta
		}
		m.mu.Unlock()
		m.persistMode(ctx, sid, mode)
	}
	// F150: gate the cleared-ack on ACTUAL output, not on a flow merely being selected.
	// OnMode fires for EVERY picked flow (intro included), but intro's dedup/paused no-op
	// selects a flow yet delivers nothing. A flow calls OnOutput only when it truly
	// delivered (normal/agora on Final/Process/Degraded, intro after a successful greet).
	flowProduced := false
	work.OnOutput = func() { flowProduced = true }
	work.NextNudge = func() *contract.UserMsg { return m.takeNudge(sid, scope, preTurn) }
	m.handler.RunTurn(turnCtx, work)
	m.mu.Lock()
	// Capture the abort verdict BEFORE releasing the child ctx: the cleanup c()
	// below sets turnCtx.Err() itself, so reading Err() after it would report every
	// normally-completed turn as aborted (WAL rows never drained, cleared-ack never
	// fired).
	aborted := turnCtx.Err() != nil
	if c, ok := m.cancels[sid]; ok {
		c() // release the child ctx (no-op if /close-session already cancelled)
		delete(m.cancels, sid)
	}
	m.mu.Unlock()
	// Layer 1 (a): RunTurn returned → drain its ingress-WAL rows, uniformly (no
	// shutdown-abort / /close-session distinction). Boot recovery pushes nothing
	// to users (the "please re-send" notice was removed — 60-recovery.go), so
	// keeping rows on a shutdown abort would only produce a log line one boot
	// later. The one path that preserves rows is a crash/panic mid-turn (RunTurn
	// never returns; recoverTurn skips here) — those rows are reaped silently on
	// the next boot.
	m.removeServedPending(scope, sid, batch)
	// Cleared-ack: a DRAIN turn that served a MULTI-message batch (several queued
	// texts answered together) — confirm they're all handled. Single-event drains
	// (one image per turn, or a lone queued text) get NO separate ack: the reply
	// itself is the acknowledgement, and acking each would spam (an N-image album
	// drains one-per-turn). The first turn (original message) is not a drain.
	// Only ack when the turn ACTUALLY produced output and was not cancelled: a turn
	// cancelled by /close-session (turnCtx dead), dropped because no flow accepted
	// (pick==nil → OnOutput never called), or an intro dedup/paused no-op (a flow was
	// selected but delivered nothing) produced no answer, so "已处理 N 条" would be a lie
	// (B21/B22/F150).
	if isDrain && len(batch) > 1 && flowProduced && !aborted {
		// Anchor the ack's language to the first HUMAN event, not the batch's last
		// event: a mixed batch whose tail is an internal (dispatch-return / cron)
		// event carries Lang="", which would fall the notice back to the directory
		// default and reach the human in the wrong language. Same anchor as :70's
		// admin-ownership stamp (F61).
		lang := batch[len(batch)-1].Lang
		if hev, ok := contract.FirstHumanEvent(batch); ok {
			lang = hev.Lang
		}
		m.notifyCleared(ctx, src, work.Target, lang, turnMode, len(batch))
	}
	m.drainSidAfterTurn(ctx, src, scope, sid, turn)
}

// batchSource resolves the source a batch's pre/post-turn I/O (reply anchoring,
// queue notices, the cleared-ack) should speak through: the first non-internal
// event's origin, re-resolved by name. Falls back to the dispatching turn's src
// when the batch is all-internal or the lookup misses.
func (m *Manager) batchSource(batch []contract.MessageEvent, fallback contract.Source) contract.Source {
	for _, ev := range batch {
		if ev.IsInternal() {
			continue
		}
		if s := m.srcByName(ev.Source); s != nil {
			return s
		}
	}
	return fallback
}

// persistMode best-effort writes the session's flow-declared mode to the store,
// skipping the write when it already matches (idempotent — no per-turn write
// amplification). The busy-queue itself reads the cached sidMeta.mode, not this
// column; persistence is for out-of-turn readers (webui / the todo axis later).
func (m *Manager) persistMode(ctx context.Context, sid string, mode contract.SessionMode) {
	if cur, err := m.store.GetSessionMode(ctx, sid); err == nil && cur == mode {
		return
	}
	if err := m.store.SetSessionMode(ctx, sid, mode); err != nil {
		slog.Debug("session: persist mode failed", "sid", sid, "err", err)
	}
}

// removeServedPending drains the ingress WAL for the events a turn just served —
// the batch's WAL rows PLUS any busy-arrived text the turn pulled forward via nudge
// (recorded in m.nudged at pop time). The nudge ids carry NO WAL row — a busy-queued
// message is never recorded (40-submit.go) — so removing them is a no-op kept for
// uniformity with the batch. Keyed by (scope, message id), the key recordPending
// wrote. Best-effort: a failure only leaves rows for the next boot's silent
// recovery to reap, never a lost message. A panic skips this call (recoverTurn handles it); the batch's
// rows then survive to the next boot, where they're cleared silently — boot recovery
// re-sends nothing, so the sender re-sends if they notice no reply.
func (m *Manager) removeServedPending(scope, sid string, batch []contract.MessageEvent) {
	ids := make([]string, 0, len(batch)+1)
	for _, ev := range batch {
		if ev.MessageID != "" {
			ids = append(ids, ev.MessageID)
		}
	}
	m.mu.Lock()
	ids = append(ids, m.nudged[sid]...)
	delete(m.nudged, sid)
	m.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), servedPendingTimeout)
	defer cancel()
	if err := m.store.RemovePending(ctx, scope, ids); err != nil {
		slog.Warn("turn served but pending WAL rows not cleared; leftover rows are reaped silently on the next boot", "scope", scope, "err", err)
	}
}

// resolvePrefs reads the scope's persisted sink prefs (the /trace + /stream
// toggles). A read miss / error falls back to DefaultSinkPrefs (both ON) — a
// prefs hiccup must never mute a reply or drop trace; it just loses the toggle.
func (m *Manager) resolvePrefs(ctx context.Context, scope string) contract.SinkPrefs {
	prefs, err := m.store.GetScopePrefs(ctx, scope)
	if err != nil {
		slog.Warn("session: read scope prefs failed; using defaults", "scope", scope, "err", err)
		return contract.DefaultSinkPrefs()
	}
	return prefs
}

// recoverTurn releases the sid's arbiter state if a turn panicked — but only if
// THIS turn still owns the sid (a fresh turn that claimed it keeps its own state).
// It also REDISPATCHES, in-process, the messages that queued during the panicked
// turn (S-6) so a healthy process keeps answering instead of stranding them until
// the next restart.
func (m *Manager) recoverTurn(ctx context.Context, src contract.Source, scope, sid string, turn uint64) {
	r := recover()
	if r == nil {
		return
	}
	slog.Error("turn panic — clearing busy state to unblock sid", "scope", scope, "sid", sid, "panic", r)
	m.mu.Lock()
	var redispatch []contract.MessageEvent
	var token uint64
	var hadDrops bool
	if meta, ok := m.sidMeta[sid]; ok && meta.turn == turn {
		delete(m.busy, sid)
		delete(m.sidMeta, sid)
		hadDropped := m.dropped[sid]
		delete(m.dropped, sid)
		// Drop the in-memory nudged-id tracking. There are no nudge WAL rows to keep —
		// a busy-queued message is never WAL'd — so on a panic the nudge is simply
		// dropped, like the turn's own batch, and the sender re-sends. Just release the
		// slice so it can't leak.
		delete(m.nudged, sid)
		// A turn that panicked mid-/close-session must clear the closing flag too —
		// the normal drain does (drainSidAfterTurn), but a panic skips that path, and
		// a stuck closing[sid] bricks the sid (every future message dropped) and leaks.
		closing := m.closing[sid]
		delete(m.closing, sid)
		// S-6: redispatch the messages that queued DURING the panicked turn. They were
		// queued (not in-flight) so they carry no WAL row yet — but runTurnAndDrain
		// WAL-records each at turn start when we re-run them below, so a crash in the
		// redispatched turn still falls to boot recovery. Re-run them now under a NEW
		// turn token instead of dropping them to restart-only recovery. Bounded: each
		// distinct message runs at most once in-process; if a redispatched turn also
		// panics, its survivors redispatch and the victim falls to WAL recovery — no
		// tight loop. Skipped on /close-session (session ending) or shutdown.
		if pending := m.pending[sid]; !closing && ctx.Err() == nil && len(pending) > 0 {
			var remaining []contract.MessageEvent
			redispatch, remaining, hadDrops = pickNextDrainBatch(pending, hadDropped)
			if len(remaining) > 0 {
				m.pending[sid] = remaining
				// Batch split (image / media-group drain): pickNextDrainBatch defers
				// the drops flag to the LAST batch (hadDropsOut=false here), so
				// re-latch it for the follow-up drains — same semantics as the normal
				// drain path, which keeps m.dropped while pending remains.
				if hadDropped {
					m.dropped[sid] = true
				}
			} else {
				delete(m.pending, sid)
			}
			m.turnSeq++
			token = m.turnSeq
			m.busy[sid] = true
			m.sidMeta[sid] = sidMetaRow{Scope: scope, StartedAt: clock.Now(), turn: token}
		} else {
			delete(m.pending, sid)
		}
	}
	// A panic skips runTurnAndDrain's post-RunTurn cancel cleanup; release here so
	// the cancel func isn't leaked (and a future close on a reused sid is clean).
	if c, ok := m.cancels[sid]; ok {
		c()
		delete(m.cancels, sid)
	}
	m.mu.Unlock()

	if len(redispatch) > 0 {
		m.handleWg.Add(1)
		go func() {
			defer m.handleWg.Done()
			m.runTurnAndDrain(ctx, src, scope, sid, redispatch, hadDrops, token, true)
		}()
	}
}

// RunningAtScope reports how many turns are currently in flight at scope — the
// per-scope concurrency read the arrangement dispatcher consults so it never nudges a
// second pull turn onto a member already running one (it nudges only scopes at 0).
// Derived from the busy set on demand (sidMeta lifecycle matches busy), so it needs
// no separate counter and can't drift across the panic / follow-up drain paths. The
// busy set is small (bounded by live turns), so the scan is cheap. A pure read.
func (m *Manager) RunningAtScope(scope string) int {
	if scope == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for sid := range m.busy {
		if meta, ok := m.sidMeta[sid]; ok && meta.Scope == scope {
			n++
		}
	}
	return n
}

// drainSidAfterTurn clears the sid's busy state when a turn finishes, and starts
// a follow-up turn if messages queued while it ran.
func (m *Manager) drainSidAfterTurn(ctx context.Context, src contract.Source, scope, sid string, turn uint64) {
	m.mu.Lock()
	if ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	// owns: this turn still holds the sid's busy bit. A fresh turn may have
	// claimed it (cold-revive after cancellation); we must not steal its slot.
	meta, hasMeta := m.sidMeta[sid]
	owns := hasMeta && meta.turn == turn
	if m.closing[sid] {
		// /close-session in flight: never spawn a follow-up against an ended
		// session. Only the owning turn's drain does the cleanup.
		if !owns {
			m.mu.Unlock()
			return
		}
		delete(m.pending, sid)
		delete(m.dropped, sid)
		delete(m.busy, sid)
		delete(m.sidMeta, sid)
		delete(m.closing, sid)
		m.mu.Unlock()
		return
	}
	if !owns {
		m.mu.Unlock()
		return
	}
	if len(m.pending[sid]) > 0 {
		next, remaining, hd := pickNextDrainBatch(m.pending[sid], m.dropped[sid])
		if len(remaining) == 0 {
			delete(m.pending, sid)
			delete(m.dropped, sid)
		} else {
			m.pending[sid] = remaining
		}
		m.mu.Unlock()
		// DEFERRED (turn module): the dead-sid liveness check +
		// transferDrainToLiveSid (the corruption-escape D7 fix) come with the
		// turn — a stub turn never ends a session, so no sid dies mid-chain.
		m.handleWg.Add(1)
		go func() {
			defer m.handleWg.Done()
			m.runTurnAndDrain(ctx, src, scope, sid, next, hd, turn, true)
		}()
		return
	}
	delete(m.busy, sid)
	delete(m.sidMeta, sid)
	// A nudge can empty the pending queue mid-turn (takeNudge deletes m.pending[sid]),
	// landing here with a deferred drops flag still set — clear it like the closing and
	// panic paths do, or it latches forever (webui warn + phantom drop notice later).
	delete(m.dropped, sid)
	m.mu.Unlock()
}

// pickNextDrainBatch decides the next follow-up batch. A media-group or
// image-bearing batch is processed one event at a time (the agent sees each
// image turn cleanly); otherwise the whole queue drains as one batch.
func pickNextDrainBatch(pending []contract.MessageEvent, hadDropsIn bool) (next, remaining []contract.MessageEvent, hadDropsOut bool) {
	if len(pending) == 0 {
		return nil, nil, false
	}
	if pending[0].MediaGroupID != "" || batchHasImage(pending) {
		next = []contract.MessageEvent{pending[0]}
		remaining = pending[1:]
		if len(remaining) == 0 {
			return next, nil, hadDropsIn
		}
		return next, remaining, false
	}
	// Text drain: batch by SENDER, not the whole queue (F149). A group is ONE shared
	// sid, so its pending queue mixes several people (different account classes); the
	// router routes the whole batch by its FIRST human event, so a stranger + a member
	// in one batch mis-handles the non-anchor (a member message swallowed into intro, or
	// a stranger's content processed in normal and billed to the member). One sender ⊂
	// one account class, so a single-sender batch is never cross-class. (The image path
	// above already drains one event per turn = inherently single-sender.) hadDrops
	// rides the LAST sub-batch, same as the image path.
	next, remaining = splitBySender(pending)
	if len(remaining) == 0 {
		return next, nil, hadDropsIn
	}
	return next, remaining, false
}

// splitBySender partitions pending into the anchor sender's events and the rest, each
// in original order — so one text drain batch carries a single sender. Non-contiguous
// same-sender events are collected together (a user's rapid messages still answer as
// one batch); cross-sender order is not preserved (it never was — F2/F3 accepted).
func splitBySender(pending []contract.MessageEvent) (next, remaining []contract.MessageEvent) {
	anchor := pending[0]
	for _, ev := range pending {
		if sameSender(ev, anchor) {
			next = append(next, ev)
		} else {
			remaining = append(remaining, ev)
		}
	}
	return next, remaining
}

// sameSender reports whether two events share an origin — the same person (Source +
// UserID). Internal events (Source="internal", UserID="") group together and split
// away from humans.
func sameSender(a, b contract.MessageEvent) bool {
	return a.Source == b.Source && a.UserID == b.UserID
}

func batchHasImage(batch []contract.MessageEvent) bool {
	for _, ev := range batch {
		for _, a := range ev.Attachments {
			// An image delivered as a document (HEIC, feishu's separate
			// document-image) counts too — same one-image-per-turn drain.
			if a.Kind == "image" || (a.Kind == "document" && strings.HasPrefix(a.MIME, "image/")) {
				return true
			}
		}
	}
	return false
}

// targetFor picks where THIS turn's reply renders — by the SESSION's归宿, NOT the
// triggering event. This distinction is load-bearing: an internal wake (a worker's
// return, or a cron/proactive emit) carries ev.Source="internal" + ev.ChatID=the
// woken session's OWN scope, so the naive replyTarget(ev,src) would address the
// reply back at the session itself — an unbounded self-feeding loop. So:
//
//   - real chat event (ev.Source != internal) → replyTarget(ev,src): thread the
//     reply to the chat (reply-to id when the source supports it).
//   - internal wake → route by the SESSION:
//     · has a caller (worker) → return to the caller's scope via the internal
//     bridge (completing the agent→agent round-trip);
//     · source-bound session resumed internally (e.g. a top-level session woken
//     by a worker's return) → reply to its OWN source chat, reconstructed from
//     its scope (so the human still sees the answer);
//     · neither resolvable → drop (Target{} → the flow's nil-source guard), never
//     self-address.
//
// A GENERAL session-graph mechanism (the caller edge is the same lineage edge as
// parent) — not agora-specific. The SessionByID reads happen ONLY on the internal-
// wake path; the common real-chat turn takes replyTarget directly (no store read).
func (m *Manager) targetFor(ctx context.Context, sid string, ev contract.MessageEvent, src contract.Source) contract.Target {
	if !ev.IsInternal() {
		return m.replyTarget(ev, src) // real chat event → thread the reply
	}
	info, err := m.store.SessionByID(ctx, sid)
	if err != nil {
		slog.Warn("session: targetFor lookup failed; dropping internal-woken reply", "sid", sid, "err", err)
		return contract.Target{} // drop (Source="" → flow can't render → no reply)
	}
	if info.CallerID != "" {
		caller, cerr := m.store.SessionByID(ctx, info.CallerID)
		if cerr != nil || caller.Key == "" {
			slog.Warn("session: worker caller unresolved; dropping reply (no return address)", "sid", sid, "caller", info.CallerID, "err", cerr)
			return contract.Target{} // drop — must NOT self-loop
		}
		// Internal bridge: ChatID = caller's scope verbatim (the return sink emits an
		// internal event there, re-entering the gateway so the caller resumes).
		return contract.Target{Source: contract.SourceNameInternal, ChatID: caller.Key}
	}
	// Source-bound session resumed by an internal event → reply to its OWN source
	// chat (reconstructed from its scope — NOT the internal triggering event).
	if t, ok := targetFromScope(info.Source, info.Key); ok {
		return t
	}
	slog.Warn("session: cannot reconstruct source target for internal-woken reply; dropping", "sid", sid, "source", info.Source, "scope", info.Key)
	return contract.Target{} // drop
}

// targetFromScope reconstructs a delivery Target from a session's source + routing
// scope — the inverse of contract.ScopeFor for the "src:dm:id" / "src:group:id"
// (+ optional "#thread") chat grammar. Used only to reply to a source-bound
// session that was resumed by an internal event (no real chat event to thread).
// ok=false when the scope isn't a chat scope for this source (e.g. an agora inbox
// own-scope, which has no native delivery target → the reply drops).
func targetFromScope(source, scope string) (contract.Target, bool) {
	for _, sep := range []string{":dm:", ":group:"} {
		prefix := source + sep
		if strings.HasPrefix(scope, prefix) {
			rest := scope[len(prefix):]
			chatID, thread := rest, ""
			if sep == ":dm:" {
				if i := strings.IndexByte(rest, '#'); i >= 0 {
					chatID, thread = rest[:i], rest[i+1:] // only DM has the "#thread" grammar
				}
			} else if base, _, ok := contract.SplitMemberSubScope(scope); ok {
				// ":group:" — the "#member" suffix is a virtual-group ROUTING artifact
				// (contract.SplitMemberSubScope), not a wire thread id. Drop it: deliver to
				// the group chat, unthreaded (else the member name leaks into Target.ThreadID
				// and a forum/topic source mis-delivers).
				chatID = base[len(prefix):]
			}
			return contract.Target{Source: source, ChatID: chatID, ThreadID: thread}, true
		}
	}
	return contract.Target{}, false
}

// takeArmedPrefer consumes and returns the scope's pending /uncensored arm, but
// only when the batch carries a human event from the ARMING user — the arm then
// belongs to this turn, whose session the arbiter stamps. Any other turn at the
// scope (a bystander's message, an internal wake) leaves the arm pending for the
// issuer's own turn, so a stranger's session is never stamped by someone else's
// /uncensored. nil when nothing is consumed.
func (m *Manager) takeArmedPrefer(scope string, batch []contract.MessageEvent) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range batch {
		if batch[i].IsInternal() {
			continue
		}
		key := armKey{scope: scope, userID: batch[i].UserID}
		if tags, ok := m.armedPrefer[key]; ok {
			delete(m.armedPrefer, key)
			return tags
		}
	}
	return nil
}

// replyTarget builds the reply Target for an event, adding a reply-to id when
// the source supports it.
func (m *Manager) replyTarget(ev contract.MessageEvent, src contract.Source) contract.Target {
	t := ev.ReplyTarget()
	if src.Caps().ReplyTo && ev.MessageID != "" {
		t.ReplyToID = ev.MessageID
	}
	return t
}
