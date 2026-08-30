// Package normal is the default turn-execution flow — the catch-all strategy the
// flow router selects when no special-case flow claims the work. It composes the
// turn (fills the prompt layers, joins the user input, picks the model) and
// drives the turn core; the turn core streams the reply to the source's sink.
//
// The flow is where coupling to context lives: it reads from the prompt factory,
// the turn core, and the source bus, and assembles them. Swapping this flow
// changes the whole orchestration without touching the turn core, the prompt
// builder, or the session.
//
// STRATEGY vs MECHANISM: this file holds only the NORMAL flow's strategy — which
// identity it mints (admin/user via the gate stamp), which authority projects the
// tool/skill bag (warrant), the catch-all Accepts, SessionModeNone. The shared
// composition MECHANISM (static layers, user-text join, OCR/ASR pre-passes, skill
// layer, channel opener) lives in flow/compose and is byte-identical with the agora
// flow (architecture-vision §7: copy orchestration, not logic).
package normal

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/flow/compose"
	"agentbob/trunk"
)

// Module wires the normal flow and registers it with the flow router.
type Module struct {
	home  string // $BOB_HOME — roots the runtime-editable prompt files
	f     *flow
	state atomic.Int32 // trunk.State
}

// New returns a normal-flow module. home is $BOB_HOME; the flow seeds + loads its
// editable prompt-layer files under $home/prompt.
func New(home string) *Module { return &Module{home: home} }

func (m *Module) Name() string { return "flow-normal" }

func (m *Module) Provides() []reflect.Type { return nil }

func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.FlowRegistry](),
		trunk.TypeOf[contract.Turn](),
		trunk.TypeOf[contract.PromptFactory](),
		trunk.TypeOf[contract.Gateway](),
		trunk.TypeOf[contract.PanelRegistry](),
	}
}

// Optional is false: without the default flow no work would be accepted.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	m.f = &flow{
		gw:   trunk.Require[contract.Gateway](reg),
		turn: trunk.Require[contract.Turn](reg),
		reg:  reg,
		cmp: &compose.Composer{
			Home:    m.home,
			Prompts: trunk.Require[contract.PromptFactory](reg),
		},
	}
	// Seed the editable prompt-layer files from the built-in defaults (once); the
	// flow loads them per turn so an operator (or agora's per-role override) edit
	// applies without a recompile.
	m.f.cmp.SeedPromptFiles()
	trunk.Require[contract.FlowRegistry](reg).Register(m.f)
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.panel())
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// flow is the contract.Flow: the catch-all default strategy.
type flow struct {
	gw   contract.Gateway
	turn contract.Turn
	cmp  *compose.Composer // shared composition mechanism

	// tools (catalog) + warrant (authority) are optional, resolved lazily (they
	// may start after this flow). The bag = warrant.Authorize(identity, catalog)
	// when warrant is present; NIL when warrant is absent (fail-closed — see
	// toolBag/skillBag); nil when there's no catalog (text-only turn).
	reg     *trunk.Registry
	tools   lazyDep[contract.ToolCatalog]
	warrant lazyDep[contract.Warrant]
	skills  lazyDep[contract.SkillCatalog]
	urlLib  lazyDep[contract.URLLibrary]
	msgIdx  lazyDep[contract.MessageIndexer]
	retr    lazyDep[contract.RetrievalFeed]
}

// lazyDep resolves an OPTIONAL dependency from the registry once and caches it —
// the dep may start after this flow, so Start can't Require it. Zero value when
// the registry is nil (tests) or the dep is absent.
type lazyDep[T any] struct {
	once sync.Once
	v    T
}

func (l *lazyDep[T]) get(reg *trunk.Registry) T {
	if reg == nil {
		var zero T
		return zero
	}
	l.once.Do(func() { l.v, _ = trunk.TryRequire[T](reg) })
	return l.v
}

func (f *flow) catalog() contract.ToolCatalog     { return f.tools.get(f.reg) }
func (f *flow) warr() contract.Warrant            { return f.warrant.get(f.reg) }
func (f *flow) skillCat() contract.SkillCatalog   { return f.skills.get(f.reg) }
func (f *flow) urls() contract.URLLibrary         { return f.urlLib.get(f.reg) }
func (f *flow) msgIndex() contract.MessageIndexer { return f.msgIdx.get(f.reg) }

// retrieval resolves the OPTIONAL cold-memory feed (leaf/retrieval). nil when
// retrieval is disabled → the flow simply doesn't enqueue. See docs/cold-memory-trunk.md §3.
func (f *flow) retrieval() contract.RetrievalFeed { return f.retr.get(f.reg) }

// skillBag projects the turn's authorized skills: warrant.AuthorizeSkills over the
// catalog. Warrant ABSENT → nil (fail-CLOSED): no catalog or no warrant → no skills.
// Warrant is hard-wired in production (cmd/bob), so an absent warrant means a
// degraded/test config — fail closed to match warrant's own empty-grant=deny
// semantics, NOT allow-all (which would expose every skill in the index to everyone).
func (f *flow) skillBag(ctx context.Context, identity string) contract.SkillSet {
	cat := f.skillCat()
	if cat == nil {
		return nil
	}
	w := f.warr()
	if w == nil {
		return nil // fail-closed: no warrant → no authorized skills
	}
	return w.AuthorizeSkills(ctx, w.Grants(ctx, identity), cat.List())
}

// toolBag projects the turn's authorized tool set: warrant.Authorize over the
// catalog. Warrant ABSENT → nil (fail-CLOSED — see skillBag): a degraded/test config
// gets no tools rather than the whole catalog.
func (f *flow) toolBag(ctx context.Context, identity string) contract.ToolSet {
	cat := f.catalog()
	if cat == nil {
		return nil
	}
	w := f.warr()
	if w == nil {
		return nil // fail-closed: no warrant → no authorized tools
	}
	return w.Authorize(ctx, w.Grants(ctx, identity), cat.Specs())
}

func (f *flow) Name() string                   { return "normal" }
func (f *flow) Priority() int                  { return 0 } // catch-all default
func (f *flow) Accepts(contract.TurnWork) bool { return true }

// Mode: the plain flow runs no verification regime — SessionModeNone. The
// busy-queue notices and the nudge fast-lane are both ON under none (only auto
// silences/disables them).
func (f *flow) Mode() contract.SessionMode { return contract.SessionModeNone }

// RunTurn composes the turn and drives the turn core. The sink is resolved through
// the source bus by the work's target — the flow asks stoma to deliver; it does
// not hold source objects.
func (f *flow) RunTurn(ctx context.Context, work contract.TurnWork) {
	src := f.gw.SourceByName(work.Target.Source)
	if src == nil {
		slog.Warn("flow-normal: unknown source — cannot reply", "source", work.Target.Source, "sid", work.Sid)
		return
	}
	sink := src.NewSink(ctx, work.Target, work.Scope, work.Sid, work.Prefs)
	// Reply-routing write side, at SEND-TIME (P1): index each channel's anchor
	// message (trace + content) the instant it's sent — not just the final content
	// id after the turn. Without this, a reply to the trace line (or to the live
	// stream mid-turn) misses the index, resolveBase OpenNew, and forks a NEW
	// session instead of folding into the busy sid as a nudge. RecordSent is an
	// ON CONFLICT upsert, so the post-turn LastSent record below stays (idempotent).
	// Optional interface — no contract.Sink/Source signature change.
	if idx := f.msgIndex(); idx != nil {
		if ar, ok := sink.(interface{ OnAnchor(func(string)) }); ok {
			// Detach the ctx: the anchor may fire AFTER this turn's ctx is cancelled —
			// email reply-coalescing defers the send past the turn (75-coalesce.go), and
			// the reply-routing record must still land. For a send-time anchor (every
			// streaming source) the turn ctx is still live, so this is a no-op there.
			recCtx := context.WithoutCancel(ctx)
			ar.OnAnchor(func(id string) { idx.RecordSent(recCtx, work.Scope, id, work.Sid) })
		}
	}
	// Open the trace with the flow + the session id this turn runs under — so a
	// trace reader sees which session is being tracked without typing /session. A
	// trace annotation (dropped when the scope's trace pref is off).
	sink.TraceDelta("🧭 " + f.Name() + " · " + work.Sid)

	// Compose the prompt + skill projection for this work (identity-stamped ctx, the
	// filled prompt builder, the authorized skill set). This is a pure function of the
	// work — /prompt's DumpPrompt reuses it.
	ctx, identity, p, skills, tools := f.composePrompt(ctx, work)

	events := work.Events
	// One identity-bound binding, two faces: ChannelOpener (file/exec spaces) and
	// CredentialOpener (kind-unique credential clients). Built once so the two
	// TurnSpec fields can never drift apart.
	bind := compose.BoundChannels{W: f.warr(), Identity: identity, DefSpace: work.Scope}

	// Voice/audio is transcribed at INGESTION (session submit, F165) — the transcript is
	// already folded into ev.Text by the time the turn runs, so there is no per-turn ASR
	// pass here any more.
	spec := contract.TurnSpec{
		Sid:      work.Sid,
		Scope:    work.Scope,                 // the turn's scope (generic; harmless for normal, used by scope-aware tools)
		Mode:     work.TurnMode,              // session birth stamp → loop driver (docs/turn-driver-split.md); "" = regular
		Lang:     compose.EventsLang(events), // ingress-resolved reply language for core notices
		Prompt:   p,
		UserText: compose.ComposeUserText(events),
		UserMsgs: compose.ComposeUserMsgs(events),                           // per-speaker user rows (group attribution)
		Agent:    contract.MsgAuthor{Kind: "agent", ID: "bob", Name: "bob"}, // assistant-row attribution
		// Requires (hard) from the source's Caps — e.g. email's required_model_tags;
		// the pool serves only entries with all these tags, else fails (no generic
		// fallback, absent an admin tag-fallback rule). Prefer (soft): the session's
		// armed tags, else the ambient ["smart"] default (compose.PreferTags) so main
		// conversations rank smart-class entries first without hard-excluding the
		// small models that stay the last-resort degrade path.
		ModelReq: contract.ModelRequest{Kind: contract.KindLLM, Requires: src.Caps().RequiredModelTags, Prefer: compose.PreferTags(work.PreferredModelTags)},
		Sink:     sink,
		Tools:    tools,
		Skills:   skills,
		// Identity-bound; default space = session scope. Tools open channels /
		// resolve credentials through it without seeing warrant/identity.
		Channels:    bind,
		Credentials: bind,
		// This turn's attachments (flattened across the event batch) so the ocr tool
		// can anchor a model-blind image reference to a real LocalPath.
		Attachments: compose.NewTurnAttachments(events, bind),
		// Nudge fast-lane: relay the arbiter's per-turn puller (nil under auto, so
		// the turn core never nudges an automated session).
		NextNudge: work.NextNudge,
		// Sender identity for identity-aware tools (recall_memory): normal flow is
		// always personal (Company=""); DM → caller's own handle, group → room scope.
		Caller: callerIdentity(events, work.Scope),
		// Acceptance standard for the harness-triggered advisor (docs/advisor.md §4).
		// Only a looping turn ever runs the advisor, so a regular turn doesn't pay the
		// file read. Normal flow has no company playbook — the global baseline is all.
		AdvisorCriteria: advisorCriteria(f.cmp, work.TurnMode),
	}
	res := f.turn.Run(ctx, spec)
	if res.Err != nil {
		slog.Warn("flow-normal: turn run failed", "sid", work.Sid, "err", res.Err)
	}
	// F150: report real delivery so the arbiter's drain cleared-ack ("已处理 N 条") fires
	// only when the turn actually answered — Final/Process/Degraded all put something on
	// the wire; Cancelled/Error delivered nothing.
	if work.OnOutput != nil && res.Err == nil &&
		(res.Outcome == contract.OutcomeFinal || res.Outcome == contract.OutcomeProcess || res.Outcome == contract.OutcomeDegraded) {
		work.OnOutput()
	}
	// Signal ② — the turn ended with a clean FINAL ANSWER (OutcomeFinal): NOT a degraded
	// give-up, and NOT a process reply (a clarify/escalate exit asked a question / handed
	// off, so it must NOT count as a satisfied URL — D23). Tell the URL library the last
	// URL this sid fetched satisfied the need, so it ranks higher in future recall. No-op
	// when the library is absent or the sid recorded no URL this turn.
	if lib := f.urls(); lib != nil {
		if res.Err == nil && res.Outcome == contract.OutcomeFinal {
			lib.MarkSatisfied(ctx, work.Sid)
		} else {
			// Not a clean Final (process / degraded / error / cancelled): still consume the
			// sid's tracked url so the lib's per-sid map doesn't leak the entry MarkSatisfied
			// would have cleared.
			lib.Forget(work.Sid)
		}
	}
	// Reply-routing write side (B-4): index the bot message we just delivered
	// (scope + the sink's platform msg id → sid) so a later reply to it reconnects
	// to this session. The flow holds the sink + scope; session owns the store.
	// id == "" (no platform message / non-chat sink) → nothing to anchor.
	if idx := f.msgIndex(); idx != nil {
		if id := sink.LastSent(); id != "" {
			// Detach the ctx (same as the OnAnchor record above): the turn ctx is
			// cancelled on /close-session and shutdown, but RunTurn returns NORMALLY
			// on cancel and the reply-routing record must still land — otherwise a
			// reply to the tail message forks a new session.
			idx.RecordSent(context.WithoutCancel(ctx), work.Scope, id, work.Sid)
		}
	}
	// Cold-memory feed (optional): enqueue this turn's user events + assistant reply
	// for the retrieval leaf. Identity was already resolved in spec.Caller (write/read
	// symmetric); fire-and-forget — a down feed never affects the turn. nil when
	// retrieval is disabled. See docs/cold-memory-trunk.md §3. Gated on a CLEAN outcome
	// (Final answer or a deliberate Process non-answer like a clarify/escalate): a
	// Degraded give-up Reply is a stock apology and Cancelled/Error delivered nothing,
	// so neither belongs in the recall corpus (mirror the URL-lib gate above).
	if rf := f.retrieval(); rf != nil && res.Err == nil &&
		(res.Outcome == contract.OutcomeFinal || res.Outcome == contract.OutcomeProcess) {
		anchor := ""
		if len(events) > 0 {
			anchor = events[0].MessageID
		}
		rf.EnqueueTurn(ctx, contract.RetrievalTurn{
			Sid:      work.Sid,
			Company:  spec.Caller.Company,
			Room:     spec.Caller.Room,
			Events:   events,
			Reply:    res.Reply,
			AnchorID: anchor,
		})
	}
}

// advisorCriteria loads the advisor's acceptance standard for a LOOPING turn, "" for
// a regular one: the advisor is the looping driver's standard equipment and a regular
// turn can never reach either of its triggers (docs/advisor.md), so loading a standard
// nothing will read is pure waste.
func advisorCriteria(cmp *compose.Composer, mode contract.TurnMode) string {
	if mode != contract.TurnModeLooping {
		return ""
	}
	return cmp.AdvisorCriteria()
}

// callerIdentity resolves the turn's sender identity for identity-aware tools.
// normal flow is always personal (Company=""): a DM clamps to the caller's own
// handle(s); a group recalls the shared room (scope). Minimal version — one handle
// per event (account-merged handles land later). See docs/cold-memory-trunk.md §4.
func callerIdentity(events []contract.MessageEvent, scope string) contract.CallerIdentity {
	var c contract.CallerIdentity
	// Anchor the DM-vs-group decision on the human sender (first non-internal);
	// a coalesced internal event at the head must not flip a group turn's recall
	// to per-handle. Room is set only for a REAL group (IsGroupChat) — internal
	// emits never carry ChatType, so "not DM" would misfile a pure-internal DM
	// batch under a Room key no DM read ever queries. With no human anchor
	// (cron wake / dispatch return), classify by the scope grammar instead
	// ("<src>:group:<id>" vs "<src>:dm:<id>", contract.ScopeFor). B28: a
	// pure-internal DM batch now yields EMPTY Handles (the loop below skips
	// internal events), so recall_memory fails CLOSED (58-recall.go) rather than
	// folding a bridge return's SHARED pseudo-handle into the DM clamp — this
	// supersedes the D28-era "falls through to the sender's personal partition".
	if anchor, ok := contract.FirstHumanEvent(events); ok {
		if anchor.ChatType.IsGroupChat() {
			c.Room = scope // group: recall the whole room, not the individual
			return c
		}
	} else if strings.Contains(scope, ":group:") {
		c.Room = scope
		return c
	}
	seen := map[string]bool{}
	for _, ev := range events {
		// B28: skip INTERNAL events. A bridge return carries UserID="inbox:<scope>"
		// + Source=internal, so AccountHandle yields a SHARED "internal:inbox:<scope>"
		// handle; folded into a DM's recall clamp it would let one user retrieve
		// another user's worker-return content. Skipping them means a pure-internal
		// batch produces no handles → recall fails closed (correct, see the doc above).
		if ev.IsInternal() {
			continue
		}
		src, uid := ev.AccountHandle()
		if uid == "" {
			continue
		}
		if h := src + ":" + uid; !seen[h] {
			seen[h] = true
			c.Handles = append(c.Handles, h)
		}
	}
	return c
}

// composePrompt builds the deterministic part of the turn — identity-stamped ctx,
// the filled prompt builder (all canonical layers, top-down render order), and the
// authorized skill projection + its prompt layer. A PURE function of the work (no
// model, no side effects): RunTurn runs it then drives the turn; DumpPrompt runs it
// then renders (no run).
func (f *flow) composePrompt(ctx context.Context, work contract.TurnWork) (context.Context, string, contract.Prompt, contract.SkillSet, contract.ToolSet) {
	// Resolve identity + billing handle from the trigger event, and stamp both on
	// ctx — the principal gates the tool bag (and channel access); the handle bills.
	identity := flowIdentity(work)
	ctx = contract.WithIdentity(ctx, identity)
	// Bill the human sender (the first non-internal event), not a coalesced internal
	// event at the head of a drain batch; fall back to Events[0] for an all-internal turn.
	if anchor, ok := contract.FirstHumanEvent(work.Events); ok {
		s, uid := anchor.AccountHandle()
		ctx = contract.WithConsumer(ctx, s, uid)
	} else if len(work.Events) > 0 {
		s, uid := work.Events[0].AccountHandle()
		ctx = contract.WithConsumer(ctx, s, uid)
	}

	// Fill the prompt layers; hand the BUILDER (not a built string) to the turn
	// core, which re-Builds it each round. Pin ALL canonical layer slots IN
	// top-down order here — SetLayer fixes a layer's render position on first set,
	// so this sequence IS the render order (Build() skips empty content but keeps
	// the slot). Layers whose subsystem isn't ported yet (memory, platform) are
	// reserved empty; skills is reserved empty before ComposeSkills fills it.
	p := f.cmp.NewBuilder()
	// The four static layers are LOADED from their runtime files ($home/prompt/
	// <name>.md, seeded at Start from the const defaults) — so edits apply without a
	// recompile, and agora's per-(company,role) overrides have a file to write.
	f.cmp.SetStaticLayers(p)
	p.SetLayer("skills", "")         // pin slot — BEFORE tool_selection: recall the method before reaching for a tool (docs/skill-recall-index.md §5)
	p.SetLayer("tool_selection", "") // pin slot (after tool_policy) before the bag fills it
	p.SetLayer("memory", "")         // reserved — memory subsystem not ported yet
	p.SetLayer("platform", "")       // reserved — platform subsystem not ported yet
	// Looping work discipline (docs/turn-driver-split.md): only a looping-stamped
	// session gets it — the layer is half of what makes the mode a WORK mode (the
	// other half is the driver's quiet sink + fuse). Regular sessions never see it.
	if work.TurnMode == contract.TurnModeLooping {
		// Work discipline only — the advisor is no longer model-triggerable, so there
		// is no consult protocol to teach (docs/advisor.md §7). Text shared with the
		// agora flow (compose — agora runs EVERY turn looping).
		p.SetLayer("work_discipline", compose.LoopingDiscipline)
	}
	p.SetLayer("runtime", compose.RenderRuntime()) // computed date anchor — see RenderRuntime

	// Project the turn's authorized tools (needed before ComposeSkills: the skill index
	// gates on skill_view being in the bag) and fill the scenario→tool selection rubric
	// from exactly the VISIBLE tools' SelectionHints. Returned so RunTurn/DumpPrompt
	// reuse the same bag (no double Authorize; dump == real turn).
	tools := f.toolBag(ctx, identity)
	// Project the turn's authorized skills and fill the skill index layer (the model
	// reads a skill's body on demand via skill_view — rendered only when skill_view is
	// authorized).
	skills := f.skillBag(ctx, identity)
	compose.ComposeSkills(p, skills, tools)
	compose.ComposeToolSelection(p, tools)
	return ctx, identity, p, skills, tools
}

// DumpPrompt composes this flow's prompt for the work and renders it (read-only —
// no model, no persist). v1 = the assembled system prompt + this turn's user input
// + the authorized tools/skills; it does not replay history. The /prompt command.
func (f *flow) DumpPrompt(ctx context.Context, work contract.TurnWork) string {
	_, _, p, skills, tools := f.composePrompt(ctx, work)
	// Shared dump skeleton + user-text prep (compose.RenderDump/DumpUserText): normal has
	// no member tag to strip and no extras. DumpUserText flags audio the real turn would
	// transcribe (the dump runs no ASR).
	return compose.RenderDump(p.Build(), compose.DumpUserText(work.Events), tools, skills, "")
}

// flowIdentity mints the warrant principal for this turn. The normal flow is the
// non-agora path. A turn ESCALATES to "admin" if ANY human event in the batch is
// an admin — a drain batch coalesces several senders into one turn, and a
// co-present admin lending power in a shared session is by design (the admin holds
// /close-session as the control lever). A pure-internal batch (a dispatch return /
// cron wake — no human present) inherits the session's admin ownership
// (work.SessionAdmin) so an admin's own session resumed headlessly after a
// dispatch round-trip keeps admin. Internal events never carry IsAdmin, so they
// never set it themselves. (The agora flow mints "company:role" principals and
// does not use this — it self-authorizes by inboxID.)
func flowIdentity(work contract.TurnWork) string {
	sawHuman := false
	for _, e := range work.Events {
		if e.IsInternal() {
			continue
		}
		sawHuman = true
		if e.IsAdmin {
			return "admin"
		}
	}
	if !sawHuman && work.SessionAdmin {
		return "admin"
	}
	return "user"
}

var (
	_ trunk.Module  = (*Module)(nil)
	_ contract.Flow = (*flow)(nil)
)
