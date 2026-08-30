package contract

import "context"

// MessageIndexer records that a bot reply was delivered for a session, keyed by
// the session SCOPE + the platform message id → session id (the three coordinates
// that matter; scope already encodes source+chat, so session never stores
// platform/chat separately). A later reply to that message routes back to the
// same session. Provided by leaf/session (it owns the session↔message mapping and
// the read side — the resolver); CALLED by a flow after it delivers a reply,
// because the FLOW holds the sink (the sent message id via Sink.LastSent) and the
// wire scope, which the session module never sees. Best-effort: a failure is
// logged inside, not returned (a missed index just means that reply opens a fresh
// session). Absent → no reply-to-continue routing.
type MessageIndexer interface {
	RecordSent(ctx context.Context, sessionScope, msgID, sessionID string)
}

// SessionManager is the session subsystem's entry point: it resolves which
// session an inbound event belongs to, serializes work per session (one turn at
// a time), and persists session state. Registered on the trunk by the session
// module; the inbound flow hands passed chat events to Submit.
type SessionManager interface {
	// Submit accepts one inbound chat event for dispatch. It returns promptly —
	// scope resolution, arbitration, and the turn run on their own goroutines —
	// so the caller's consume loop is never blocked. The event carries its
	// origin source by name (ev.Source); the session resolves the live source
	// through the bus, so no source object is threaded in.
	Submit(ctx context.Context, ev MessageEvent)
	// RecoverPendingOnBoot clears the durable in-flight WAL rows left by the last
	// shutdown so they don't resurface or accumulate. A restart does NOT push any
	// "please re-send" notice to users — the entry WAL is recovered silently. The
	// inbound flow calls it once on boot; it is synchronous and quick.
	RecoverPendingOnBoot(ctx context.Context)
	// RunningAtScope reports how many turns are currently in flight at a session
	// scope. The arrangement dispatcher consults it to nudge a member to pull work ONLY
	// when that member's scope is idle (0) — so it never piles a second pull turn on
	// a member already running one, while leaving the member free to handle other
	// scopes/channels concurrently. A pure in-process read.
	RunningAtScope(scope string) int
}

// SessionResume wakes a session to continue its task — the executor side of the
// browser-takeover hand-back (and any future "an external event finished, resume the
// agent" case). Provided by leaf/session (it owns the sid↔scope mapping, the mode, and
// the gateway emitter); the takeover hand-back calls it through leaf/webui. session
// APPLIES the flow-declared resume policy: today only an auto-mode session is woken
// (it has no human present to re-trigger it); a manual/none session is left for the
// user's next message. Resolves the sid's scope internally and emits an internal
// MessageEvent (processed as a real inbound → a fresh turn), so no bespoke resume
// registry / turn hook is needed.
type SessionResume interface {
	ResumeSession(ctx context.Context, sid, note string)
}

// SessionMode is a session's verification / acknowledgement regime, declared by
// the flow that handles the session (see Flow.Mode). It is decoupled from the
// (not-yet-ported) todo subsystem: today its only consumers are the busy-queue's
// notice gating and the nudge fast-lane — both OFF under SessionModeAuto — and
// the todo verification axis reuses the same attribute when it lands.
type SessionMode string

const (
	// SessionModeManual: queue notices on, nudge on (human verification regime).
	SessionModeManual SessionMode = "manual"
	// SessionModeAuto: queue notices silent, nudge off (automated / agora).
	SessionModeAuto SessionMode = "auto"
	// SessionModeNone: queue notices on, nudge on, no verification (plain flow).
	SessionModeNone SessionMode = "none"
)

// IsValid reports whether m is a known session mode.
func (m SessionMode) IsValid() bool {
	switch m {
	case SessionModeManual, SessionModeAuto, SessionModeNone:
		return true
	}
	return false
}

// TurnWork is one unit of work the arbiter hands to the turn handler: the events
// to process for a session plus the context the turn needs to execute and reply.
// It is pure data — the reply target names its source (Target.Source); the flow
// resolves the live delivery channel through the bus, so no source object rides
// along.
type TurnWork struct {
	Sid    string
	Scope  string
	Events []MessageEvent
	Target Target
	Prefs  SinkPrefs
	// SessionAdmin is the session's persisted admin-ownership bit, stamped by the
	// arbiter ONLY for a pure-internal batch (no human event — a dispatch return /
	// cron wake). flow/normal reads it to keep admin authority on a headless
	// continuation of an admin's session; a batch with a human present is judged by
	// that human's IsAdmin instead, so this stays false there.
	SessionAdmin bool
	// PreferredModelTags is a SOFT per-turn model-tag preference — a flow passes it as
	// ModelRequest.Prefer (bias, never a hard Requires). The arbiter populates it from
	// the session's persisted stamp, MERGED with any pending toggle arm consumed by
	// this turn (/uncensored, /thinking — each toggles ONE tag, so a scope can carry
	// several at once). Because they are soft, arbitrating a combination no single
	// entry satisfies is the picker's job, not the session layer's. nil when the
	// session carries no preference.
	PreferredModelTags []string
	// TurnMode is the session's BIRTH-STAMPED loop-driver mode
	// (docs/turn-driver-split.md): stamped at session creation from the scope's
	// default (the /looping toggle) and immutable for the session's life — flipping
	// the scope default affects only future sessions, so no session ever changes
	// mode mid-conversation. The arbiter reads the stamp per turn; flow/normal
	// copies it to TurnSpec.Mode. Zero value = regular.
	TurnMode TurnMode
	// HadDrops is true when the pending batch overflowed before this turn. Set by
	// the arbiter; a flow surfaces a "dropped some messages" notice when the
	// orchestrator lands (not consumed by the single-shot flow yet).
	HadDrops bool
	// NextNudge, when non-nil, lets the running turn pull ONE busy-arrived plain-
	// text message forward into its current round (the nudge fast-lane). The arbiter
	// wires it; the flow router NILS it for an auto session (never nudge an automated
	// turn). Each call pops the next eligible message from the pending queue (removing
	// it — exactly-once) and returns it as a UserMsg, or nil when none is available.
	// A UserMsg (not a bare string) because the turn PERSISTS it as a real user row:
	// the message is conversation content, so it needs its speaker attribution.
	NextNudge func() *UserMsg
	// OnMode, when non-nil, is how the flow self-reports the SessionMode it imposes:
	// the flow router calls it ONCE (with the selected flow's Mode) before running
	// the turn, so the arbiter caches it on the session (busy-queue notice gating)
	// and persists it — without a second flow selection. The arbiter wires it.
	OnMode func(SessionMode)
	// OnOutput, when non-nil, is how a flow reports it actually DELIVERED output this
	// turn: normal/agora call it on a delivered outcome (Final/Process/Degraded), intro
	// after a successful greet Finish. The arbiter gates the drain cleared-ack ("已处理
	// N 条") on it — merely SELECTING a flow (OnMode, called by the router for every
	// pick incl. intro) is not enough, since intro's dedup no-op selects a flow yet
	// produces nothing. A flow that delivered nothing simply never calls it. Wired by
	// the arbiter.
	OnOutput func()
}

// PendingDisposition tells the turn how the arbiter handled a busy-arrived
// event, so it can surface the event to the running turn appropriately.
type PendingDisposition int

const (
	PendingQueued  PendingDisposition = iota // appended to the pending batch
	PendingBlocked                           // session blocked from new turns; event queued
	PendingDropped                           // pending batch overflowed; event dropped
)

// TurnHandler executes turns on behalf of the session arbiter. The session core
// is generic over it: the arbiter knows WHEN to run a turn (one per sid, when
// not blocked), the handler knows HOW. Provided by the flow router (which
// selects a flow per turn); the session resolves it lazily. A session is a
// session even when no turn ever runs — this seam is the only thing that
// connects the two.
type TurnHandler interface {
	// RunTurn executes one turn for the work unit. Called when the arbiter
	// admits a turn (at most one per sid at a time). Returns when the turn is
	// done so the arbiter can drain the next pending batch.
	RunTurn(ctx context.Context, work TurnWork)
	// Blocked reports whether a session is currently blocked from starting a new
	// turn (e.g. a manual todo awaiting verification). The arbiter queues the
	// event instead of starting a turn when true.
	//
	// ⚠️ WIRING REQUIREMENT (D3, flow-review): today every implementation
	// returns false, so the block path is latent. Whoever first makes Blocked return
	// true MUST also wire an UNBLOCK → re-drive seam: on a Blocked event, submit only
	// NotePending (no m.pending append, no busy), but the only m.pending
	// consumer (drainSidAfterTurn) fires when an OWNING TURN ends — a blocked session
	// has none, so the queued event would be orphaned forever. Land the unblock-drains-
	// pending path in the SAME change that starts returning true; do not enable
	// blocking without it.
	Blocked(ctx context.Context, sid string) bool
	// NotePending surfaces an event that arrived while the session was busy to
	// the running turn, so its model can react mid-task. disposition says how
	// the arbiter handled it.
	NotePending(ctx context.Context, sid string, ev MessageEvent, disposition PendingDisposition)
	// DumpPrompt routes the work to the selected flow's DumpPrompt — the
	// build-don't-run path behind the /prompt admin command. Parallel to RunTurn,
	// read-only (no model / persist / history). "" when no flow accepts the work.
	DumpPrompt(ctx context.Context, work TurnWork) string
	// FlowInfo reports the flow that would handle this work and the SessionMode it
	// imposes — the read-only routing probe behind /session. ok=false when no flow
	// accepts the work. Parallel to DumpPrompt (same Accepts pick, no turn run, no
	// persist): the flow name is never persisted (only the mode is, via OnMode), so
	// a peek must probe to name the flow.
	FlowInfo(ctx context.Context, work TurnWork) (name string, mode SessionMode, ok bool)
}
