// Package inbound is the inbound flow — the thin orchestration that wires the
// source bus to the rest of the pipeline. It consumes the bus's fanned-in event
// stream and dispatches each event by route: a cheap classify() marks it
// internal (agora) or external (chat), and sends it down the matching path.
// External chat runs the access-axis triage (the gate) and hands passed events
// downstream; internal events take the agora path (no screen, no user session).
//
// Wiring: the consume loop runs gate triage → slash fork → session.Submit; on
// boot it clears the last shutdown's stale in-flight WAL via
// session.RecoverPendingOnBoot. On Stop the loop just halts — buffered-but-unstarted
// events in the bus are dropped (the sender re-sends), not drained to durable
// storage. The session hand-off is resolved via TryRequire[contract.SessionManager]
// (a logging stub until session is present). The admin line (health alerts) and the
// internal/agora route are deferred.
package inbound

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/i18n"
	"agentbob/trunk"
)

// Module is the inbound flow.
type Module struct {
	gw       contract.Gateway
	screener contract.Screener
	slash    contract.SlashRegistry
	// accounts (optional) supplies the per-sender remembered reply language, so a
	// signal-less message (a slash command) replies in the sender's established
	// language. nil → detection-only (graceful degrade).
	accounts contract.Accounts
	// submit hands a passed event downstream. Defaults to a logging stub until
	// the session module is wired in.
	submit func(context.Context, contract.MessageEvent)

	// langMem is a read-through cache over accounts' per-handle lang memory, so the
	// signal-less LangFor lookup (a store SELECT) doesn't ride the single consume loop
	// on every slash/bare-number message (B16 head-of-line-block). Positive hits only:
	// the store column is seed-once (SetHandleLangIfEmpty, no other writer), so a
	// cached value can never go stale. Bounded by langMemCap (crude reset — it
	// repopulates from reads), keyed source+"\x00"+uid.
	langMu  sync.Mutex
	langMem map[string]string

	// albumSeen dedupes the slash fork across album siblings (F120): the media-group
	// buffer stamps the shared caption onto EVERY sibling it emits, so a "/..."
	// caption would otherwise dispatch once per photo. Keyed
	// source+chat+MediaGroupID → dedup-entry expiry; pruned opportunistically.
	albumMu   sync.Mutex
	albumSeen map[string]time.Time

	cancel context.CancelFunc
	done   chan struct{}
	state  atomic.Int32 // trunk.State
}

// langMemCap bounds the in-process lang cache; on overflow the whole map is
// dropped and repopulates from reads (entries are tiny, misses are one SELECT).
const langMemCap = 4096

// New builds the inbound flow.
func New() *Module {
	return &Module{submit: logSubmit, done: make(chan struct{})}
}

func (m *Module) Name() string { return "inbound" }

func (m *Module) Provides() []reflect.Type { return nil }

// Needs the source bus (events to consume) and the screener (the triage). It
// sits at the top of the DAG — nothing depends on it — so it starts last.
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.Gateway](),
		trunk.TypeOf[contract.Screener](),
		trunk.TypeOf[contract.SlashRegistry](),
	}
}

// Optional is false: without the inbound flow nothing drives the pipeline.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

// Start wires the collaborators and spins the consume loop.
func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	m.gw = trunk.Require[contract.Gateway](reg)
	m.screener = trunk.Require[contract.Screener](reg)
	m.slash = trunk.Require[contract.SlashRegistry](reg)
	// Optional: the per-sender reply-language memory (absent → detect-only).
	m.accounts, _ = trunk.TryRequire[contract.Accounts](reg)

	runCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	// Wire the chat hand-off to the session module when it's registered; until
	// then submit stays the logging stub (New's default). The session resolves
	// the source by name itself (ev.Source), so the hand-off is pure data. On
	// boot, clear the previous shutdown's stale in-flight WAL rows (no user notice).
	if sm, ok := trunk.TryRequire[contract.SessionManager](reg); ok {
		m.submit = sm.Submit
		sm.RecoverPendingOnBoot(runCtx)
	} else {
		// The session module wasn't registered before us, so the hand-off stays the
		// logSubmit stub and EVERY inbound message is silently dropped for the whole
		// process lifetime. Today wiring order makes this unreachable (main registers
		// session before inbound), but a reorder would turn bob silently deaf — so fail
		// LOUD here at boot rather than emit a per-message Info stub nobody reads. (L-INGRESS-D1)
		slog.Error("inbound: SessionManager absent at Start — chat hand-off is a no-op, ALL inbound messages will be dropped (check module registration order)")
	}

	// Mark Ready BEFORE spawning run(): run() only ever transitions Ready→Failed
	// (on an unexpected bus close) and never sets Ready, so storing Ready after the
	// spawn could race-overwrite a self-detected StateFailed and leave Health()
	// reporting Ready over a dead pipeline.
	m.state.Store(int32(trunk.StateReady))
	go m.run(runCtx)
	return nil
}

// Stop halts the consume loop. Buffered-but-unconsumed events are NOT persisted:
// only the message a turn was actively running gets a WAL row (in-flight — see
// session.recordPending), which boot recovery silently clears; a received-but-
// unstarted event sitting in the bus at shutdown is simply dropped (the sender
// re-sends if they notice no reply).
func (m *Module) Stop(ctx context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	<-m.done
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// run is the single consume loop: pull each fanned-in event and route it.
func (m *Module) run(ctx context.Context) {
	defer close(m.done)
	slog.Info("inbound flow serving")
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-m.gw.Events():
			if !ok {
				// If our ctx is already cancelled this is an orderly Stop (trunk tore us
				// down first) — exit quietly and let Stop set StateStopped. Self-contained:
				// don't rely on the reverse-Stop ordering alone to avoid mis-marking a clean
				// shutdown as failed.
				if ctx.Err() != nil {
					return
				}
				// The bus closed its stream while we were NOT shutting down (ctx still
				// live). The whole entry pipeline is now dead — no event will ever be
				// consumed again — so mark UNHEALTHY (not the orderly StateStopped Stop
				// sets) and log loudly, else trunk Health() keeps reporting Ready while
				// bob is silently deaf to every inbound message.
				slog.Warn("inbound: event bus closed unexpectedly — entry pipeline stopped consuming")
				m.state.Store(int32(trunk.StateFailed))
				return
			}
			m.handle(ctx, ev)
		}
	}
}

// route is the flow-dispatch marker — which handling path an event takes. The
// dispatcher judges it once from a cheap signal on the event and sends the
// event down the matching path.
type route int

const (
	routeChat     route = iota // external source: (screen unless Trusted) → session
	routeInternal              // internal/agora source: trusted, addressed or control — not a user session
)

// classify assigns the route marker. The only flow-route signal today is
// internal-vs-external (ev.Source); finer agora sub-routing rides on the agora
// envelope and lands with agora. Reply/continuation signals (ReplyToBot,
// DMReplyRouting) are NOT flow-route markers — a reply is still a chat event;
// which session it continues is resolved downstream by the session module.
func classify(ev contract.MessageEvent) route {
	if ev.IsInternal() {
		return routeInternal
	}
	return routeChat
}

// isSlash reports whether ev is a "/..." command (vs turn input).
func isSlash(ev contract.MessageEvent) bool {
	return strings.HasPrefix(strings.TrimSpace(ev.Text), "/")
}

// slashAlbumTTL is how long a dispatched album slash is remembered for sibling
// dedup. It must outlast the telegram media-group buffer's late-sibling window
// (flushedStateTTL = 5 min) — past that, a straggler no longer picks up the
// cached caption anyway, so it can't arrive as a slash duplicate.
const slashAlbumTTL = 10 * time.Minute

// albumSlashDup reports whether a slash event is a repeat album sibling — the same
// source+chat+MediaGroupID already dispatched within slashAlbumTTL. The first
// sibling wins; repeats drop at the fork so the command runs exactly once (F120).
// Expired entries are pruned on each album-slash hit — albums are rare and the
// entries tiny, so the map stays bounded without a sweeper.
func (m *Module) albumSlashDup(ev contract.MessageEvent) bool {
	if ev.MediaGroupID == "" {
		return false
	}
	key := ev.Source + "\x00" + ev.ChatID + "\x00" + ev.MediaGroupID
	now := time.Now()
	m.albumMu.Lock()
	defer m.albumMu.Unlock()
	for k, exp := range m.albumSeen {
		if now.After(exp) {
			delete(m.albumSeen, k)
		}
	}
	if _, ok := m.albumSeen[key]; ok {
		return true
	}
	if m.albumSeen == nil {
		m.albumSeen = make(map[string]time.Time)
	}
	m.albumSeen[key] = now.Add(slashAlbumTTL)
	return false
}

// resolveLang picks an inbound event's reply language. Cascade: an admin override
// pin (i18n overrides.yaml) → the message's OWN detected language (and seed the
// sender's stable per-handle memory on a real language) → the sender's remembered
// language (so a signal-less message — a slash command, a bare number — still
// replies in their language) → "default". Keyed per-SENDER (accounts handle), so it
// is correct in a group too (last speaker can't set the whole chat's language).
// i18n stays a pure detector; the memory lives per-sender in accounts. accounts
// absent → detection only (graceful degrade).
func (m *Module) resolveLang(ctx context.Context, ev contract.MessageEvent) string {
	if o := i18n.Override(ev.Source, ev.ChatID); o != "" {
		return o
	}
	src, uid := ev.AccountHandle()
	haveAcct := m.accounts != nil && src != "" && uid != ""
	switch d := i18n.Detect(ev.Text); d {
	case "": // signal-less (slash / digits) → the sender's remembered language
		if haveAcct {
			key := src + "\x00" + uid
			m.langMu.Lock()
			l, hit := m.langMem[key]
			m.langMu.Unlock()
			if hit {
				return l
			}
			if l, ok := m.accounts.LangFor(ctx, src, uid); ok {
				m.langMu.Lock()
				if m.langMem == nil || len(m.langMem) >= langMemCap {
					m.langMem = make(map[string]string)
				}
				m.langMem[key] = l
				m.langMu.Unlock()
				return l
			}
		}
		return "default"
	case "default": // a latin signal = English; never stored, never recalled
		return "default"
	default: // a real language (e.g. zh) → use it AND seed the stable per-sender memory
		if haveAcct {
			// Seed-once, best-effort, result unused — fire it OFF the single consume loop
			// (a store write fires on every real-language message; a slow/contended accounts
			// store must not head-of-line-block all inbound). Detached ctx so it survives the
			// handler return. (The signal-less LangFor READ above goes through langMem, so
			// steady-state it is memory-only — one SELECT per sender per process.)
			go m.accounts.RememberLang(context.WithoutCancel(ctx), src, uid, d)
		}
		return d
	}
}

// handle is the dispatcher: judge the route, send the event down its path.
func (m *Module) handle(ctx context.Context, ev contract.MessageEvent) {
	switch classify(ev) {
	case routeInternal:
		// Internal/agora events reply on the bridge's no-op sink (no human awaiting a
		// localized reply), so language detection would be wasted work — skip it.
		m.handleInternal(ctx, ev)
	default:
		m.handleChat(ctx, ev)
	}
}

// handleChat runs the access-axis triage on an external event and hands a
// passed one downstream.
func (m *Module) handleChat(ctx context.Context, ev contract.MessageEvent) {
	// Stamp the reply language ONCE for the chat path (contract.MessageEvent.Lang) so
	// every downstream reader — the gate's bare-code redeem reply, slash, and the normal
	// turn flow's notices — speaks the sender's language without re-detecting.
	ev.Lang = m.resolveLang(ctx, ev)
	src := m.gw.SourceByName(ev.Source)
	if src == nil {
		// Fail CLOSED: an unresolved source must NOT skip the screen as if Trusted. (Should
		// be unreachable — every wire source emits Source==Name() — but the screen is the
		// only access-axis gate, so a nil lookup drops rather than silently admits.)
		slog.Warn("inbound: dropping chat event from unresolved source", "source", ev.Source, "user", ev.UserID)
		return
	}
	// Every source goes through the centralized screen EXCEPT a Trusted one. The only
	// Trusted source that reaches here is local; bridge is internal and was already
	// diverted to handleInternal by classify, so it never enters this path. Opt-out +
	// fail-closed: an unset/new source is screened by default, so a forgotten flag
	// can't silently admit strangers.
	//
	// A Trusted source ALSO skips the ev.IsAdmin re-stamp below: there is no admins.yaml
	// policy to re-derive it from, so ev.IsAdmin is left as the source set it. A Trusted
	// source OWNS that stamp and is responsible for getting it right (see contract.Caps.
	// Trusted) — local stamps the terminal user admin; a future Trusted source must stamp
	// deliberately, since true = full operator control with no central check here.
	onboardingPass := false // gate PASSED an un-allowlisted sender via a source's onboarding opt-in
	if !src.Caps().Trusted {
		v := m.screener.Screen(ctx, ev)
		if v.Action != contract.ScreenPass {
			// Fail CLOSED: everything that isn't an explicit pass — Drop, Redeemed, or
			// any future/unknown ScreenAction — stops here. Drop logs at Info; an
			// unknown action logs at Warn so a new enum value is loud, not silently
			// admitted (ScreenPass is iota 0, so a fall-through would mean admit).
			switch v.Action {
			case contract.ScreenDrop:
				slog.Info("inbound: dropped", "source", ev.Source, "user", ev.UserID, "reason", v.Reason)
			case contract.ScreenRedeemed:
				// no log: the redeem confirmation below is the signal
			default:
				slog.Warn("inbound: unknown screen action, dropping", "action", v.Action, "source", ev.Source, "user", ev.UserID)
			}
			// A first-contact un-allowlisted sender gets a friendly one-time bounce
			// (DM-only, like a redeem reply); denylisted senders carry no Reply, and
			// deliverRedeem no-ops on an empty one. Async (like the slash fork below):
			// the send can block on a platform 429 floor, and it must not
			// head-of-line-block the single consume loop. Best-effort already.
			go m.deliverRedeem(ctx, src, ev, v.Reply)
			return
		}
		onboardingPass = v.Onboarding // un-approved sender admitted only for onboarding
		// Stamp admin from the gate (the per-source admin authority) so AdminOnly
		// slash commands work on gated sources. Non-gated sources stamp their own
		// IsAdmin (local = the terminal user, always admin).
		ev.IsAdmin = m.screener.IsAdmin(ev.Source, ev.ChatID, ev.UserID)
	}
	// Slash fork (after the gate, before the turn): a "/..." event is a command,
	// not turn input — dispatch it out-of-band instead of submitting a turn.
	// Async (like Submit) so a slow reply send doesn't head-of-line-block the
	// single consume loop; a slash reply failure is benign (the user re-runs).
	//
	// EXCEPT an onboarding-pass sender (un-allowlisted, not yet approved): their "/..." is
	// NOT dispatched as a command — that would run side-effectful non-admin slashes
	// (/new, /close-session, /uncensored, /trace) for a stranger. It falls through to the
	// turn instead, where the router funnels them to intro (onboarding).
	//
	// ev.IsAdmin carves an admin out of that suppression: the gate's allowlist never
	// consults admins.yaml, so an admins-listed-but-un-allowlisted sender on an
	// onboarding-open source IS an onboarding-pass — but an admin is a platform-known
	// operator who never onboards, so their slash (e.g. /accounts approve) dispatches
	// instead of being swallowed into the intro turn. (D27)
	if isSlash(ev) {
		// Album guard (F120), ONCE for the whole slash: the media-group buffer stamps a
		// "/..." caption onto every sibling photo, so the command arrives once per photo.
		// Handle it exactly once whatever the outcome below (dispatch OR an honest bounce),
		// dropping the repeat siblings — hoisted ABOVE the branch so the bounce paths dedup
		// too (else a paused/pending sender's album caption fires N identical bounces).
		if m.albumSlashDup(ev) {
			return
		}
		// A slash is a command dispatched out-of-band — but WHO may run it turns on the
		// account behind this handle, not just whether the gate onboarding-passed them.
		// The old `!onboardingPass || IsAdmin` gate was blind to identity: it swallowed a
		// real (entitled-but-un-allowlisted-here) member's slash into an intro turn (F40),
		// and let a PAUSED sender's side-effect slashes (/bindaccount, /takeover, /new)
		// through because pause only gates the router's turn path, not this fork (F41).
		// Consult AccountFor ONCE and branch honestly.
		//
		// ev.IsAdmin short-circuits: a platform operator (admins.yaml) always runs, even
		// un-allowlisted on an onboarding source — their /accounts approve must not be
		// swallowed into an intro turn (D27).
		if ev.IsAdmin {
			m.dispatchSlashFork(ctx, ev, src)
			return
		}
		var acct contract.AccountInfo
		var bound bool
		if m.accounts != nil {
			acct, bound = m.accounts.AccountFor(ctx, ev)
		}
		switch {
		case bound && acct.Status == "paused":
			// F41: a paused account keeps its allowlist row, but pause must gate ACTION,
			// not only turns — bounce honestly, do NOT dispatch a side-effect slash.
			// D16: deliverRedeem is DM-only (no-op in a plain group), so bounce only WHERE
			// we can reach them; in a group fall THROUGH to the turn instead — the router's
			// paused front gate routes it to intro, which distinguishes suspended vs
			// onboarding (mirrors the pending_approval branch below, no silent black hole).
			if ev.ChatType == contract.ChatDM || src.Caps().RedeemConfirmAll {
				go m.deliverRedeem(ctx, src, ev, i18n.T("gate.slash.paused", ev.Lang))
				return
			}
			// else: fall through to the turn (group → intro suspended nudge)
		case bound && !acct.FlowKnown:
			// B27: a bound row whose entitlement is UNconfirmed — a transient store blip
			// makes AccountFor return bound=true with a ZERO AccountInfo (Status="",
			// FlowKnown=false). Do NOT dispatch a side-effect slash on an unconfirmed row:
			// fall THROUGH to the turn, where flow/router permittedOnPath fails CLOSED on
			// !FlowKnown for non-admins (F43) and self-heals next message. Hoisted ABOVE
			// the !onboardingPass dispatch so an allowlisted sender's blip can't slip a
			// command through here either (admins already returned above via ev.IsAdmin).
		case !onboardingPass:
			// A properly allowlisted (or Trusted) sender: dispatch as always.
			m.dispatchSlashFork(ctx, ev, src)
			return
		// --- onboarding-pass territory (un-allowlisted, admitted only for onboarding) ---
		case !bound:
			// Genuinely new (no account): fall THROUGH to the turn, where the router
			// funnels them to intro — the "/..." becomes onboarding input.
		case len(contract.EntitledFlows(acct.Flow)) == 0:
			// F40: registered but no entitlement yet (bare account pending admin approval).
			// Don't silently swallow the command into an intro turn — say so honestly WHERE
			// we can reach them. deliverRedeem is DM-only (no-op in a plain group), so in a
			// group fall THROUGH to the turn instead: intro (F151) posts a neutral "DM me"
			// nudge there — strictly better than a silent black hole (no notice + no turn).
			// The "join" path already exists (admin /accounts approve, or the gate panel).
			if ev.ChatType == contract.ChatDM || src.Caps().RedeemConfirmAll {
				go m.deliverRedeem(ctx, src, ev, i18n.T("gate.slash.pending_approval", ev.Lang))
				return
			}
			// else: fall through to the turn (group → intro nudge, as the old gate did)
		default:
			// F40: bound + entitled — a real member whose per-bot allowlist just doesn't
			// list them here (e.g. approved on a sibling bot in the same family). Their
			// slash WORKS: dispatch. (router already runs them as normal, not intro.)
			m.dispatchSlashFork(ctx, ev, src)
			return
		}
	}
	// Stamp the provisional-sender marker so submit holds back a full inbound's side
	// effects (👀 / busy notice / per-@ session row) for an onboarding-pass sender (F152).
	ev.Onboarding = onboardingPass
	m.submit(ctx, ev)
}

// dispatchSlashFork logs any ignored attachments, then dispatches the command
// out-of-band (async). The album-dup guard runs ONCE in handleChat's slash fork before
// branching (so the bounce paths dedup too), NOT here. Factored out of the slash fork so
// every "this sender may run the command" branch shares one dispatch path.
func (m *Module) dispatchSlashFork(ctx context.Context, ev contract.MessageEvent, src contract.Source) {
	// The album's attachments do not ride a slash (a command dispatches out-of-band; no
	// turn ever sees them) — log so the drop isn't silent. Whether an attachment-bearing
	// slash should instead run as turn input is a pending semantic decision; today the
	// command wins and the attachments are ignored.
	if len(ev.Attachments) > 0 {
		slog.Info("inbound: slash command carries attachments — ignored (a slash dispatches out-of-band, no turn sees them)",
			"source", ev.Source, "chat", ev.ChatID, "attachments", len(ev.Attachments))
	}
	go m.dispatchSlash(ctx, ev, src)
}

// dispatchSlash resolves a reply sink and hands a "/..." event to the registry.
// The handler resolves any session context it needs itself (the slash context
// carries the raw event, not a pre-resolved scope).
//
// D36 (deliberate-accept): this is fire-and-forget on the consume-loop ctx and is NOT
// drained by Stop, which drains only the consume loop (m.done); turn execution is owned
// and drained by the session module. So
// a write-side slash racing shutdown can run with an already-cancelled ctx and partially
// apply. The dangerous orphan case — /accounts new+bind — is already safe: its cleanup
// runs on a detached ctx (B24). The residual is a multi-step non-transactional agora op
// (e.g. /agora bootstrap) leaving a half-built org — but that is a shutdown-race-rare,
// one-time setup that re-bootstrap (or agora wipe-on-bump) recovers, so we accept it
// rather than add a slash drain to Stop for it.
func (m *Module) dispatchSlash(ctx context.Context, ev contract.MessageEvent, src contract.Source) {
	// src is guaranteed non-nil: the sole caller (handleChat) already dropped nil sources.
	// ev.Lang is already stamped at ingress (handle).
	// Slash replies are one-off, not session turns → no scope/sid recorded.
	// Thread the reply to the command message in a group (D37), mirroring the normal
	// turn's replyTarget — so a /session or /trace answer correlates with its command in
	// a busy group. Gated on the source advertising ReplyTo.
	t := ev.ReplyTarget()
	if src.Caps().ReplyTo {
		t.ReplyToID = ev.MessageID
	}
	sink := src.NewSink(ctx, t, "", "", contract.DefaultSinkPrefs())
	m.slash.Dispatch(ctx, contract.SlashContext{
		Event:   ev,
		Sink:    sink,
		IsAdmin: ev.IsAdmin,
		Lang:    ev.Lang,
	})
}

// handleInternal routes an internal/bridge event. These are trusted (no screen)
// and address a target scope verbatim (the session resolver maps src=internal →
// scope=ev.ChatID). A plain internal emit (send_message Stage A / cron /
// proactive) wakes an ordinary turn at that scope — no gate, no slash fork.
// A future agora dispatch-queue interception would hook in here BEFORE this
// fallback (not yet wired); today every internal event runs as a normal turn.
func (m *Module) handleInternal(ctx context.Context, ev contract.MessageEvent) {
	// One audit point on the internal path: the reply lands on the bridge's no-op
	// sink (Stage A — no return routing yet), so this Debug is the only trace that
	// an internal event was accepted and woke a turn.
	slog.Debug("inbound: submitting internal event", "scope", ev.ChatID)
	m.submit(ctx, ev)
}

// deliverRedeem sends a bare-code redeem confirmation back per platform policy:
// always in a DM; in a group only when the source opted in (RedeemConfirmAll).
// Best-effort — a send failure is logged, not surfaced. ctx is the consume-loop ctx
// (Stop cancels it): a confirmation in-flight at shutdown is INTENTIONALLY dropped
// (the bind itself already persisted; the sender re-sends the code and gets a fresh
// "already bound" reply). Unlike RememberLang this is not detached — a one-time
// confirmation isn't worth surviving shutdown.
func (m *Module) deliverRedeem(ctx context.Context, src contract.Source, ev contract.MessageEvent, reply string) {
	if reply == "" {
		return
	}
	if ev.ChatType != contract.ChatDM && !src.Caps().RedeemConfirmAll {
		return
	}
	if err := src.Send(ctx, ev.ReplyTarget(), reply); err != nil {
		slog.Warn("inbound: redeem reply send failed", "source", ev.Source, "err", err)
	}
}

// logSubmit is the placeholder hand-off until the session module is wired.
func logSubmit(_ context.Context, ev contract.MessageEvent) {
	slog.Info("inbound: would submit to session (not yet wired)", "source", ev.Source, "text", ev.Text)
}

var _ trunk.Module = (*Module)(nil)
