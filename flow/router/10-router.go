// Package router is the flow router — the turn-execution entry the session core
// sees. It implements contract.TurnHandler and contract.FlowRegistry: each flow
// registers itself at Start, and the router selects one per turn (highest
// Priority first, the first whose Accepts returns true) and delegates RunTurn to
// it. Selecting which strategy runs is the only policy here; the flows hold the
// strategy, the loop holds the mechanism.
package router

import (
	"context"
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/i18n"
	"agentbob/trunk"
)

// Module is the flow router.
type Module struct {
	mu    sync.RWMutex
	flows []contract.Flow // ordered by Priority, highest first
	state atomic.Int32    // trunk.State

	// accounts is the per-account flow-assignment authority (contract.Accounts).
	// OPTIONAL and resolved LAZILY (sync.Once): accounts only Needs the DB so the
	// trunk may start it after this router; by the first turn it has registered
	// if present. Absent → pure self-selection (the pre-accounts behavior).
	reg          *trunk.Registry
	accountsOnce sync.Once
	accounts     contract.Accounts

	// gateway is resolved LAZILY (OPTIONAL) only to deliver the no-flow-accepts
	// fallback notice below — the router holds no gateway for normal routing. Lazy is
	// REQUIRED here (not just an optimization): stoma provides the gateway and registers
	// AFTER the router, so an eager resolve at Start would see nil.
	gatewayOnce sync.Once
	gateway     contract.Gateway
}

// New returns a flow router.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "flow-router" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.TurnHandler](),
		trunk.TypeOf[contract.FlowRegistry](),
	}
}

func (m *Module) Needs() []reflect.Type { return nil }

// Optional is false: the session core resolves the TurnHandler from here.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

// Start publishes the router as both the TurnHandler (to the session) and the
// FlowRegistry (to the flows, which register into it after this). It starts
// before any flow, so the registry exists when flows register.
func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	m.reg = reg
	trunk.Provide[contract.TurnHandler](reg, m)
	trunk.Provide[contract.FlowRegistry](reg, m)
	m.state.Store(int32(trunk.StateReady))
	return nil
}

// acc lazily resolves the optional accounts authority (see the field doc). Nil
// when Start hasn't wired the registry (direct-construction test paths).
func (m *Module) acc() contract.Accounts {
	if m.reg == nil {
		return nil
	}
	m.accountsOnce.Do(func() { m.accounts, _ = trunk.TryRequire[contract.Accounts](m.reg) })
	return m.accounts
}

// notify delivers a one-line localized user-facing notice (keyed by i18n key) on a
// turn that ran NO flow — either no flow accepted (degraded set) or the caller was
// refused permission for the resolved path. Keeps the I8 "one finish" invariant. The
// gateway is resolved lazily and best-effort: absent gateway / unknown source / a send
// failure just falls back to a silent drop.
func (m *Module) notify(ctx context.Context, work contract.TurnWork, key string) {
	if m.reg == nil {
		return
	}
	m.gatewayOnce.Do(func() { m.gateway, _ = trunk.TryRequire[contract.Gateway](m.reg) })
	if m.gateway == nil {
		return
	}
	src := m.gateway.SourceByName(work.Target.Source)
	if src == nil {
		return
	}
	lang := ""
	// Anchor the notice language on the human sender (first non-internal event),
	// matching the routing anchor — an internal event at the head carries the
	// dispatch text's language, not the human's reply language.
	if anchor, ok := contract.FirstHumanEvent(work.Events); ok {
		lang = anchor.Lang
	}
	_ = src.NewSink(ctx, work.Target, work.Scope, work.Sid, work.Prefs).Finish(i18n.T(key, lang))
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// Register adds a flow, keeping flows ordered by Priority (highest first).
// Registration order does not matter; flows register at their own Start.
func (m *Module) Register(f contract.Flow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flows = append(m.flows, f)
	sort.SliceStable(m.flows, func(i, j int) bool {
		return m.flows[i].Priority() > m.flows[j].Priority()
	})
}

// RunTurn selects the flow for this work (ONCE) and delegates. The single pick
// also self-reports the flow's SessionMode: it calls work.OnMode so the arbiter
// can cache + persist it without a second selection, and NILS the nudge fast-lane
// for an auto flow (never pull a user message into an automated turn).
func (m *Module) RunTurn(ctx context.Context, work contract.TurnWork) {
	f, deny := m.pick(ctx, work)
	if f == nil {
		if deny != "" {
			// Path resolved but the caller lacks permission for it — REFUSE + notify,
			// never reroute. (Keeps the I8 one-finish invariant too.)
			m.notify(ctx, work, deny)
		} else {
			slog.Error("flow-router: no flow accepts work; turn dropped", "sid", work.Sid, "scope", work.Scope)
			m.notify(ctx, work, "flow.router.no_flow_accepted") // I8 one-finish even in a degraded set
		}
		return
	}
	mode := f.Mode()
	if work.OnMode != nil {
		work.OnMode(mode)
	}
	if mode == contract.SessionModeAuto {
		work.NextNudge = nil // an automated turn is never steered by a busy-arrived nudge
	}
	f.RunTurn(ctx, work)
}

// DumpPrompt selects the flow for this work (the same Accepts pick as RunTurn) and
// returns its rendered prompt without running it — the read-only /prompt path. ""
// when no flow accepts the work.
func (m *Module) DumpPrompt(ctx context.Context, work contract.TurnWork) string {
	// pickPath, NOT pick: a read-only inspection is not subject to the runtime permission
	// gate (an admin must be able to /prompt any scope, incl. an agora inbox they're not a
	// member of — pick would otherwise refuse it and the dump would be empty).
	f := m.pickPath(work)
	if f == nil {
		return ""
	}
	return f.DumpPrompt(ctx, work)
}

// FlowInfo selects the flow for this work (the same Accepts pick as RunTurn /
// DumpPrompt) and reports its name + the SessionMode it imposes, without running
// anything — the read-only routing probe behind /session. ok=false when no flow
// accepts the work.
func (m *Module) FlowInfo(ctx context.Context, work contract.TurnWork) (string, contract.SessionMode, bool) {
	f := m.pickPath(work) // read-only probe: path only, no permission gate (see DumpPrompt)
	if f == nil {
		return "", "", false
	}
	return f.Name(), f.Mode(), true
}

// normalFloor is the basic-bob flow. It is NO LONGER free: like any flow it needs an
// explicit per-account entitlement (a hub admin always has it; everyone else is granted
// it — existing accounts via the schema-v4 stamp, new senders via onboarding). A sender
// lacking the chosen path's entitlement is funneled to intro (non-admin) or refused.
const normalFloor = "normal"

// pick selects the PATH for this work and gates it by PERMISSION — two orthogonal axes
// (docs/account-flows-and-intro.md). The PATH is whichever flow Accepts the work
// (scope-decided, highest Priority first, entitlement-INDEPENDENT). PERMISSION is the
// caller's per-account entitlement to that path. A bound caller WITHOUT permission for
// the chosen path is REFUSED — we never reroute to a lesser flow, so a non-entitled
// human at an agora chat is told "not authorized", NOT silently handed generic bob at
// the worker's own scope (the prior entitlement-first reroute, which leaked the worker's
// scope/session — superseded).
//
// Returns: (flow, "") run flow; (nil, denyKey) refuse with that localized notice;
// (nil, "") no flow Accepts at all (degraded-set drop → notify keeps I8).
func (m *Module) pick(ctx context.Context, work contract.TurnWork) (contract.Flow, string) {
	// PATH: scope-decided, no caller identity. (pickPath locks internally; we don't hold
	// the router lock across the AccountFor store read below.)
	path := m.pickPath(work)
	if path == nil {
		return nil, "" // nothing accepts → degraded-set drop
	}

	// PERMISSION gate — only for a HUMAN batch with the accounts authority wired. An
	// all-internal batch (a dispatch wake) carries no human to gate and is self-authorized
	// at send time (AgoraSend.CheckSend), so it runs the path unconditionally.
	acc := m.acc()
	ev, human := contract.FirstHumanEvent(work.Events)
	if acc == nil || !human {
		return path, ""
	}
	info, bound := acc.AccountFor(ctx, ev) // ONE read; entitlement derived from info.Flow below

	// FRONT GATE (non-admin only): an unbound or paused sender goes to the mechanical
	// intro flow BEFORE the path runs — intro never self-selects via Accepts, and tells
	// onboard-vs-suspended apart itself. Admins are platform-known, never onboarded. (No
	// intro registered → refuse: fail closed, see below.)
	//
	// D28 (accept): the anchor is the FIRST human event. In a shared group sid, an admin
	// and an account-unbound non-admin can coalesce into one drain batch; if the admin is
	// the anchor, this guard is skipped and the unbound non-admin is served within the
	// admin's turn — NOT given the intro split that turn. That is the structural-① human-
	// control design (a co-present admin lends authority to the shared session); without
	// an admin present the next turn routes the unbound sender to intro normally.
	// D19: the `!ev.IsAdmin` guard makes a platform operator (admins.yaml) intentionally
	// IMMUNE to account-pause on EVERY path (incl. agora) — the operator is never locked
	// out of their own hub by a pause row. Member/company pause on the agora side is
	// guarded independently (scope + grants), so this immunity doesn't unlock a paused
	// company's work — only the operator's direct hub access.
	if !ev.IsAdmin && (!bound || info.Status == "paused") {
		// Missing intro → suspension fails CLOSED (routeToIntro): refusing beats falling
		// to the permission gate, which never reads Status and would let a paused-but-
		// entitled sender run the full flow (making /accounts pause inert).
		return m.routeToIntro()
	}

	// PERMISSION: every path now needs the caller entitled to it (normal included). A
	// non-entitled NON-admin is funneled to the onboarding flow (intro) so they can be
	// granted it — not a dead refuse (intro mints the bare pending account; the admin
	// grants access via /accounts approve). An ADMIN is never onboarded: a hub admin
	// always has normal (permittedOnPath), and an admin lacking a RESTRICTED grant (agora)
	// is refused, NOT intro'd — agora authority is the company's (scope + grants), not the
	// hub admin's.
	if !permittedOnPath(path.Name(), ev.IsAdmin, bound, info) {
		// Funnel to intro ONLY a non-admin with a KNOWN-empty flow — a bare/pending
		// account mid-onboarding, for whom "registered, awaiting approval" is true. A
		// non-admin who HAS some flow but not THIS path's (e.g. a normal user in an agora
		// scope) is genuinely "not authorized HERE" → refuse; routing them to intro would
		// falsely promise a pending approval no one will action. FlowKnown is required: a
		// store blip (flow UNREAD, not empty) is indistinguishable from bare by Flow alone,
		// and intro'ing an entitled member on a hiccup would send a false "pending approval"
		// notice — a blip keeps the fail-closed refusal. Admins are never onboarded.
		if !ev.IsAdmin && info.FlowKnown && len(contract.EntitledFlows(info.Flow)) == 0 {
			// Missing intro → the funnel falls through to the refusal below (routeToIntro):
			// a "pending approval" promise with no onboarding flow to back it is false.
			return m.routeToIntro()
		}
		return nil, "flow.router.not_permitted"
	}
	return path, ""
}

// pickPath selects the PATH only — the highest-priority flow whose Accepts(work) is
// true, scope-decided with NO permission gate. The read-only probes (DumpPrompt /
// FlowInfo) use this directly: inspecting a scope's prompt/route is not a runtime turn,
// so it must NOT be subject to the per-account permission refusal — an admin must be
// able to inspect ANY scope (e.g. an agora inbox they are not a member of). nil → no
// flow accepts.
func (m *Module) pickPath(work contract.TurnWork) contract.Flow {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, f := range m.flows {
		if f.Accepts(work) {
			return f
		}
	}
	return nil
}

// routeToIntro routes the sender to the mechanical intro flow — the single home of
// the "no intro → refuse" invariant. Intro is Optional(), but both callers (the
// paused/unbound front gate and the bare-account onboarding funnel) are ENFORCEMENT:
// when intro is absent they must fail CLOSED with the standard refusal, never fall
// open into the flow (pre-B34 that fail-open made /accounts pause inert).
func (m *Module) routeToIntro() (contract.Flow, string) {
	if intro := m.flowByName("intro"); intro != nil {
		return intro, ""
	}
	return nil, "flow.router.not_permitted"
}

// flowByName returns the registered flow with the given Name (nil if none).
func (m *Module) flowByName(name string) contract.Flow {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, f := range m.flows {
		if f.Name() == name {
			return f
		}
	}
	return nil
}

// permittedOnPath reports whether the caller may run the chosen path. Every flow now
// needs an explicit entitlement — there is no free floor. A hub ADMIN always has
// "normal" (basic bob: the operator is never locked out / never onboarded), but a
// RESTRICTED flow (agora) has NO admin special-case — its authority is the company's
// (scope + grants), so an admin without +agora is not entitled. An unbound caller (no
// account) is entitled to nothing. A bound account is entitled to exactly the flows its
// (post-v4) flow field names; a KNOWN-empty flow now legitimately means "no access" (a
// bare onboarding account).
//
// F43 (reverses D27's fail-OPEN): a store blip (FlowKnown==false → flow
// UNREAD, not empty) now fails CLOSED — a NON-admin whose entitlement couldn't be read
// is refused for that turn (self-heals next turn when the store recovers), rather than
// served on the unverified assumption they hold "normal". Enforcement must not lean on a
// read that didn't happen. An ADMIN is still let through the basic floor (platform-known,
// never gated on a store read).
func permittedOnPath(name string, isAdmin, bound bool, info contract.AccountInfo) bool {
	if name == normalFloor && isAdmin {
		return true
	}
	if !bound {
		return false
	}
	if !info.FlowKnown {
		// Store blip: flow UNREAD (not empty). Fail CLOSED (F43) — do not auto-grant any
		// path on an entitlement we could not read. The caller is refused this turn and
		// retries when the store recovers. (Admins already returned above for normal.)
		return false
	}
	return slices.Contains(contract.EntitledFlows(info.Flow), name)
}

// Blocked / NotePending are session-arbiter seams about a running turn's state
// (e.g. a manual-verify todo). STUB: single-shot flows never block a session.
// When the orchestrator lands, route these to the loop's blocked-state seam.
func (m *Module) Blocked(context.Context, string) bool { return false }
func (m *Module) NotePending(context.Context, string, contract.MessageEvent, contract.PendingDisposition) {
}

var (
	_ trunk.Module          = (*Module)(nil)
	_ contract.TurnHandler  = (*Module)(nil)
	_ contract.FlowRegistry = (*Module)(nil)
)
