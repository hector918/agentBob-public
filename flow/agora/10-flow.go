// Package agora is the agora turn-execution flow — ⑤ in docs/agora-port.md, the
// thin STRATEGY layer that finally runs an agora turn end-to-end. It mirrors
// flow/normal's shell (resolve a sink, compose the prompt via flow/compose, build a
// TurnSpec, drive the turn core) but swaps in agora's strategy:
//
//   - Accepts only work whose chat scope routes to an agora inbox (contract.Agora's
//     InboxForScope). When agora is absent (no bootstrap) it accepts NOTHING — the
//     normal catch-all handles everything.
//   - Priority 10, so it is consulted before normal's catch-all 0.
//   - The turn's identity is the agora principal ("member:<name>" for a member-owned
//     inbox, an inbox sentinel otherwise — grants are the intersection across the
//     member's companies), not
//     the admin/user the gate stamps.
//   - The tool/skill bag is agora's SELF-authorization (company permissions_yaml),
//     NOT warrant (docs/agora-port.md §5.3).
//   - The per-turn member IDENTITY card (member name + company/role + PromptStyle, no
//     internal IDs; rendered by agora to a string) REPLACES the identity prompt layer's
//     default "你是 bob" persona, so the agent IS the member. The other static layers
//     (constitution / preferences / tool_policy) hold the behavioral rules and stay.
//   - The per-turn directory + playbook (also rendered by agora to strings) are injected
//     into the PLATFORM prompt layer (a distinct slot). The prompt layer never imports agora.
//
// The SHARED composition mechanism (static layers, user-text join, OCR/ASR
// pre-passes, skill layer, channel opener) is flow/compose, byte-identical with the
// normal flow (architecture-vision §7: copy orchestration, not logic).
package agora

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/flow/compose"
	"agentbob/i18n"
	"agentbob/trunk"
)

// Module wires the agora flow and registers it with the flow router.
type Module struct {
	home  string // $BOB_HOME — roots the runtime-editable prompt files
	f     *flow
	state atomic.Int32 // trunk.State
}

// New returns an agora-flow module. home is $BOB_HOME (shared with the normal flow's
// editable prompt-layer files).
func New(home string) *Module { return &Module{home: home} }

func (m *Module) Name() string { return "flow-agora" }

func (m *Module) Provides() []reflect.Type { return nil }

// Needs mirrors flow-normal's HARD needs (the registry, the turn core, the prompt
// factory, the gateway). contract.Agora is NOT a hard need — it is OPTIONAL
// (TryRequire), so this flow starts even on a deployment with no agora bootstrapped;
// it simply accepts no work then.
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.FlowRegistry](),
		trunk.TypeOf[contract.Turn](),
		trunk.TypeOf[contract.PromptFactory](),
		trunk.TypeOf[contract.Gateway](),
	}
}

// Optional is TRUE (unlike flow-normal): when agora (or any of its soft deps) is
// absent this flow just doesn't claim work — the normal catch-all handles
// everything. There is no "no work accepted" failure mode to guard against, so the
// trunk may run without it.
func (m *Module) Optional() bool { return true }

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
	// Prompt-layer files are seeded by flow-normal (shared $BOB_HOME/prompt dir);
	// seed again is idempotent (never clobbers an existing file) and keeps this flow
	// self-sufficient if it ever runs without the normal flow.
	m.f.cmp.SeedPromptFiles()
	trunk.Require[contract.FlowRegistry](reg).Register(m.f)
	// Self-describe the agora chat-log reader to the webui (OPTIONAL — TryRequire so
	// this flow still starts headless / with no webui). The data is composed HERE in
	// the flow layer (inbox→scopes via agora + paged history via ChatHistory), keeping
	// leaf/agora and the message store decoupled.
	if pr, ok := trunk.TryRequire[contract.PanelRegistry](reg); ok && pr != nil {
		pr.RegisterPanel(m.f.chatPanel())
	}
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// flow is the contract.Flow: the agora strategy.
type flow struct {
	gw   contract.Gateway
	turn contract.Turn
	cmp  *compose.Composer // shared composition mechanism

	// agora is the OPTIONAL org-graph authority — resolved LAZILY (it may start
	// after this flow, and is absent on a deployment with no agora bootstrapped).
	// Absent → Accepts returns false and the normal catch-all handles everything.
	reg        *trunk.Registry
	agora      lazyDep[contract.Agora]
	tools      lazyDep[contract.ToolCatalog]
	skills     lazyDep[contract.SkillCatalog]
	warrant    lazyDep[contract.Warrant]
	urlLib     lazyDep[contract.URLLibrary]
	msgIdx     lazyDep[contract.MessageIndexer]
	msgStore   lazyDep[contract.MessageStore]
	skillFail  lazyDep[contract.SkillFailureSink]
	memberFail lazyDep[contract.MemberFailureSink]
	chatHist   lazyDep[contract.ChatHistory]
	retr       lazyDep[contract.RetrievalFeed]
}

// lazyDep caches ONE optional registry dependency: get resolves it once via
// TryRequire and remembers the result (absent → zero T stays cached). Zero-value
// ready; a nil registry (the direct-construction test path) returns the zero T
// without arming the Once. Collapses the former 12 (field, sync.Once, accessor
// body) triplets into one field + a one-line accessor each.
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

// chat lazily resolves the OPTIONAL contract.ChatHistory (the session module's
// scope-addressed chat-log reader) for the webui agora chat browser. nil when
// session is absent — the panel then shows no rows.
func (f *flow) chat() contract.ChatHistory { return f.chatHist.get(f.reg) }

// retrieval lazily resolves the OPTIONAL contract.RetrievalFeed (the cold-memory
// write seam). nil when retrieval is disabled — EnqueueTurn is then skipped, zero
// behavior change.
func (f *flow) retrieval() contract.RetrievalFeed { return f.retr.get(f.reg) }

// ag lazily resolves the optional contract.Agora. nil when absent (no bootstrap or
// not yet started) — Accepts then claims nothing.
func (f *flow) ag() contract.Agora { return f.agora.get(f.reg) }

func (f *flow) catalog() contract.ToolCatalog { return f.tools.get(f.reg) }

func (f *flow) skillCat() contract.SkillCatalog { return f.skills.get(f.reg) }

// warr resolves the warrant — now the single JUDGE for agora turns too (docs §14):
// agora SUPPLIES the projection (MemberProjection), warrant judges it
// (Authorize/AuthorizeSkills/ResolveCredential). Also vends CHANNELS (File/Exec).
// TryRequire'd as Optional, but absent warrant → toolBag/skillBag/credentials nil
// (fail-closed): a deployment running agora turns needs warrant present.
func (f *flow) warr() contract.Warrant { return f.warrant.get(f.reg) }

// urls lazily resolves the optional URL library so RunTurn can consume the turn's
// per-sid URL tracking (mirrors flow/normal). Absent → the consume is a no-op.
func (f *flow) urls() contract.URLLibrary { return f.urlLib.get(f.reg) }

func (f *flow) msgIndex() contract.MessageIndexer { return f.msgIdx.get(f.reg) }

func (f *flow) Name() string { return "agora" }

// Priority 10 beats the normal flow's catch-all 0, so agora-routed work is claimed
// here first.
func (f *flow) Priority() int { return 10 }

// Accepts claims work only when agora is present AND the work's scope routes to an
// agora inbox. Absent agora → false → the normal catch-all handles everything.
func (f *flow) Accepts(work contract.TurnWork) bool {
	ag := f.ag()
	if ag == nil {
		return false
	}
	if _, ok := ag.InboxForScope(context.Background(), work.Scope); ok {
		return true
	}
	// F161: a PAUSED member inbox's scope is still agora's — claim it so RunTurn bounces
	// "member paused" instead of falling to the normal catch-all (a reversible inbox
	// pause must NOT silently reroute a company group to generic bob, mirroring how a
	// company-level pause explicitly bounces).
	if ag.PausedMemberScope(context.Background(), work.Scope) {
		return true
	}
	// A fanned group whose scope did NOT resolve to a member (no ^name, no default) still
	// belongs to agora: claim it so RunTurn asks the human "找谁" rather than letting the
	// normal catch-all answer a group wired to agora members.
	if ev, ok := firstGroupEvent(work.Events); ok {
		_, _, fan := ag.RouteGroupScope(context.Background(), ev)
		return fan
	}
	return false
}

// Mode: SessionModeAuto — agora sessions are autonomous workers (queue notices
// silent, nudge fast-lane off), per contract.Flow.Mode's "automated/agora flow"
// note. This holds for the human-facing ⑤ entry too: a person messaging a worker
// still gets an autonomous worker, not the confirm/nudge regime of a plain chat.
func (f *flow) Mode() contract.SessionMode { return contract.SessionModeAuto }

// bounce sends an honest pause notice back on the trigger's reply sink and logs the
// outcome. `key` is the i18n message key, `what` names the pause kind for the log.
// WHEN there is a reply sink the notice is Finished on it; an internal/NoReply
// initiator has no sink, so its drop is logged instead (best-effort, not guaranteed).
func (f *flow) bounce(ctx context.Context, work contract.TurnWork, key, what string) {
	if src := f.gw.SourceByName(work.Target.Source); src != nil {
		if err := src.NewSink(ctx, work.Target, work.Scope, work.Sid, work.Prefs).Finish(i18n.T(key, compose.EventsLang(work.Events))); err != nil {
			slog.Warn("flow-agora: "+what+" bounce failed", "scope", work.Scope, "sid", work.Sid, "err", err)
		}
	} else {
		slog.Info("flow-agora: "+what+" drop not delivered (no reply sink — internal/NoReply initiator)", "scope", work.Scope, "sid", work.Sid)
	}
}

// RunTurn composes the agora turn and drives the turn core. Mirrors flow-normal's
// shell; the agora-specific moves are: resolve the per-turn agora context, stamp
// the agora principal, project the bag via agora (not warrant), inject the
// directory + playbook into the prompt. It does NOT route the return leg (worker
// output → caller): that is wired in the session MECHANISM layer (leaf/session's
// targetFor picks the destination, the bridge source's returnSink re-emits) and is
// deliberately NOT a flow/contract.Agora concern — never re-implement it here.
// Only StampInbox (per-message agora history attribution) remains deferred.
func (f *flow) RunTurn(ctx context.Context, work contract.TurnWork) {
	// A BRIDGE inbox NEVER runs an LLM turn — it is a company's "talk-to-human"
	// line that only ROUTES (docs/agora.md §六·五). Check first, before resolving a
	// sink / composing a prompt: an internal/worker message forwards OUTBOUND to the
	// bound operator chat; an external/human message re-routes INBOUND to the
	// downstream member inbox. ag() is nil only on the direct-construction test path
	// (Accepts gates on agora present in production).
	// Pause STOPS the whole company link, bridge INCLUDED — checked before bridge
	// routing (IsPaused resolves a bridge scope to its owning company), so a paused
	// company neither runs member turns NOR relays through its bridge. The sender gets a
	// per-message bounce WHEN there is a reply sink; an internal/NoReply initiator has no
	// sink, so its drop is logged instead (best-effort, not a guaranteed notification).
	if ag := f.ag(); ag != nil {
		if ag.IsPaused(work.Scope) {
			f.bounce(ctx, work, "flow.agora.company_paused", "paused-company")
			return
		}
		// F161: a PAUSED member inbox (owner company still active) — bounce "member paused"
		// EXPLICITLY (like a company pause) rather than run a turn or fall to normal. This
		// also blocks the resume-time session mix-up: while paused, normal never takes over
		// the scope, so a later reply can't re-thread into a normal session.
		if ag.PausedMemberScope(ctx, work.Scope) {
			f.bounce(ctx, work, "flow.agora.member_paused", "paused-member")
			return
		}
		if ext, defScope, company, isBridge := ag.BridgeRouting(ctx, work.Scope); isBridge {
			f.routeBridge(ctx, work, ext, defScope, company)
			return // never run a turn at a bridge scope
		}
		// Virtual-group: a fanned group where the scope did NOT resolve to a member (no
		// ^name, no default) → ask the human who, listing the members. No member turn runs.
		// (When a member WAS picked, the resolver rewrote work.Scope to its sub-scope, so
		// InboxForScope resolves and this is skipped.)
		if _, ok := ag.InboxForScope(ctx, work.Scope); !ok {
			if ev, ok := firstGroupEvent(work.Events); ok {
				if _, members, fan := ag.RouteGroupScope(ctx, ev); fan {
					src := f.gw.SourceByName(work.Target.Source)
					if src == nil {
						slog.Info("flow-agora: fanned group needs member addressing but no reply sink", "scope", work.Scope)
						return
					}
					// The bare fanned-group scope can't resolve a company for the pause gate
					// above; probe PER MEMBER (each sub-scope resolves to that member's own
					// companies — a fan may span companies, so one member's pause must not be
					// generalized): the roster lists only non-paused members, and the
					// company_paused bounce fires only when ALL are paused. All in-memory.
					// botMention: prefix each member token with THIS bot's @handle so a
					// multi-bot group's human addresses the right bot ("@binserv00Bot^name").
					botMention := ""
					if sm, ok := src.(contract.SelfMentioner); ok {
						botMention = sm.SelfMention()
					}
					active := make([]string, 0, len(members))
					for _, mb := range members {
						// Probe under the BASE group scope: work.Scope can itself be a member
						// sub-scope here (a ^name pick that failed the resolver's active gate,
						// or a stale reply-hit "G#oldname"), and a raw concat would probe the
						// never-resolving "G#a#b", silently disabling the whole filter.
						if !ag.IsPaused(anchorScope(work.Scope) + "#" + mb) {
							active = append(active, mb)
						}
					}
					msg := askWhoText(active, ev.Lang, botMention)
					if len(active) == 0 {
						// Every wired member is unavailable — company-paused here, or
						// inbox-paused (RouteGroupScope already dropped those from members):
						// bounce rather than ask the human to pick from an empty roster.
						msg = i18n.T("flow.agora.company_paused", ev.Lang)
					}
					if err := src.NewSink(ctx, work.Target, work.Scope, work.Sid, work.Prefs).Finish(msg); err != nil {
						slog.Warn("flow-agora: ask-who reply failed", "scope", work.Scope, "sid", work.Sid, "err", err)
					}
					return
				}
			}
		}
	}

	// Addressing-hidden slash: the inbound slash fork only recognizes a LEADING "/"
	// (inbound.isSlash), so a "^member /cmd" group message sails past dispatch, the
	// strip below removes the tag, and the bare "/cmd" would land in the model as
	// literal text — which then ROLEPLAYS the command instead of running it (observed:
	// "^member /looping" → the model invented a fake loop protocol). Intercept exactly
	// the events whose strip REVEALS a leading "/": a bare "/cmd" never reaches a turn
	// (inbound dispatched it), and worker-generated "/..." text (no tag) stays ordinary
	// input. Bounce honestly; the turn runs only on what remains (usually nothing).
	if member := memberOfScope(work.Scope); member != "" {
		kept, dropped := dropHiddenSlash(work.Events, member)
		if dropped > 0 {
			slog.Info("flow-agora: addressing-hidden slash bounced, not turn input", "scope", work.Scope, "sid", work.Sid, "dropped", dropped)
			f.bounce(ctx, work, "flow.agora.hidden_slash", "hidden-slash")
			if len(kept) == 0 {
				return
			}
			work.Events = kept
		}
	}

	noReply := dispatchNoReply(work.Events)
	src := f.gw.SourceByName(work.Target.Source)
	var sink contract.Sink
	var reqTags []string
	switch {
	case src != nil:
		sink = src.NewSink(ctx, work.Target, work.Scope, work.Sid, work.Prefs)
		reqTags = src.Caps().RequiredModelTags
		// Reply-routing write side, at SEND-TIME (P1): index each channel's anchor message
		// the instant it's sent so a mid-turn reply folds into the busy sid as a nudge
		// instead of forking a new session. RecordSent is an ON CONFLICT upsert.
		if idx := f.msgIndex(); idx != nil {
			if ar, ok := sink.(interface{ OnAnchor(func(string)) }); ok {
				// Anchor under the GROUP scope (not the member sub-scope): a human's reply
				// arrives addressed to the group, so reply-routing must find the member's sid
				// under the group scope. anchorScope is a no-op for non-group / non-fanned.
				// Detach the record ctx (mirror flow/normal): email reply-coalescing can fire
				// this anchor AFTER the turn ctx is cancelled, and RecordSent must still land.
				recCtx := context.WithoutCancel(ctx)
				ar.OnAnchor(func(id string) { idx.RecordSent(recCtx, anchorScope(work.Scope), id, work.Sid) })
			}
		}
	case noReply:
		// A fire-and-forget wake (NoReply dispatch — the arrangement pull-nudge): RUN the
		// turn with a discard sink, since its value is the tool side-effects
		// (arrangement_pull/submit), not a chat reply. Without this the worker is nudged but
		// never executes (items stay queued). Gated on the POSITIVE NoReply flag, NOT on an
		// empty Target.Source — an absent reply target alone is ALSO targetFor's DROP
		// sentinel for routing FAILURES (lost caller / store error), which must still drop.
		sink = discardSink{}
	default:
		// No reply target and not a fire-and-forget wake → can't deliver, so don't run.
		slog.Warn("flow-agora: unknown source — cannot reply", "source", work.Target.Source, "sid", work.Sid)
		return
	}
	// Open the trace with flow + session id (so a reader sees which session is
	// tracked without typing /session).
	sink.TraceDelta("🧭 " + f.Name() + " · " + work.Sid)

	ctx, at, p, skills, tools := f.composePrompt(ctx, work)

	events := work.Events
	// Strip the "^成员" addressing token from a fanned-group turn's text — it was only the
	// inbound routing selector, so the member shouldn't see it echoed in its own input.
	// Only when this turn resolved to a member sub-scope (memberOfScope != "").
	if member := memberOfScope(work.Scope); member != "" {
		events = stripMemberTag(events, member)
	}

	// Channels (file/exec spaces) stay warrant-bound. Allowed is the hard space gate:
	// the member's own workspace (DefSpace) plus one space per company it belongs to.
	// Credentials, by contrast, are gated by AGORA (the member's company grants), not
	// warrant's matrix — see agoraCreds / docs §14.2.
	bind := compose.BoundChannels{W: f.warr(), Identity: at.Principal, DefSpace: work.Scope, Allowed: compose.SpaceSet(at.Spaces)}
	// Voice/audio is transcribed at ingestion (session submit, F165) — folded into ev.Text
	// before the turn runs, so no per-turn ASR pass here.

	// Per-member cold-memory partition: an agora worker's recall + feed is keyed by
	// its member principal ("member:<name>"), NOT a company — a member spans multiple
	// companies (grants are their intersection), so the member is the stable per-turn
	// partition. Non-member turns (bridge inbox → sentinel principal) don't participate.
	var memberKey string
	if strings.HasPrefix(at.Principal, "member:") {
		memberKey = at.Principal
	}

	spec := contract.TurnSpec{
		Sid:   work.Sid,
		Scope: work.Scope, // the turn's scope — send_message resolves agora targets from it (Stage B)
		// Mode: EVERY agora member turn runs the LOOPING driver (convergence-driven
		// work loop, quiet sink, built-in advisor — docs/turn-driver-split.md §4/§5):
		// a worker turn is a work turn, whoever triggered it. Deliberately NOT read
		// from work.TurnMode — the session's /looping birth stamp is a normal-flow
		// preference; here the flow's strategy forces looping unconditionally (the
		// /session panel may show a regular stamp this flow overrides). This also
		// covers the NoReply (arrangement pull-nudge) one-shot turns: the looping
		// fuse (500) replaces the old noReplyTurnIterCap=150 override that was sized
		// against the regular driver's default 40 — no IterCap needed any more.
		Mode:     contract.TurnModeLooping,
		Lang:     compose.EventsLang(events), // ingress-resolved reply language for core notices
		Prompt:   p,
		UserText: compose.ComposeUserText(events),
		UserMsgs: compose.ComposeUserMsgs(events), // per-speaker user rows (group attribution)
		Agent:    agentAuthor(at.Principal),       // assistant-row attribution = this member (or bob)
		// Requires (hard) from the source's Caps — the pool serves only entries with all
		// these tags. nil for an internal discard wake (no source → no tag constraint).
		// Prefer (soft): session tags else the ambient ["smart"] default — same rule as
		// the normal flow (compose.PreferTags); worker turns are conversations too.
		ModelReq: contract.ModelRequest{Kind: contract.KindLLM, Requires: reqTags, Prefer: compose.PreferTags(work.PreferredModelTags)},
		Sink:     sink,
		// Tool bag = agora's self-authorization (company permissions_yaml), keyed by
		// the resolved inbox — NOT warrant (docs/agora-port.md §5.3). Computed in
		// composePrompt (also drives the tool-selection rubric).
		Tools:  tools,
		Skills: skills,
		// Identity-aware tools (recall_memory) read Caller; per-member partition key
		// (empty for non-member turns → recall fail-closes, which is correct).
		Caller:      contract.CallerIdentity{Company: memberKey},
		Channels:    bind,
		Credentials: agoraCreds{warr: f.warr(), ag: f.ag(), inboxID: at.InboxID},
		Attachments: compose.NewTurnAttachments(events, bind),
		NextNudge:   work.NextNudge,
		// Acceptance standard for the harness-triggered advisor (docs/advisor.md §4).
		// The global baseline ONLY — the company playbook is deliberately NOT folded
		// in (拍板): it is a collaboration map (who routes what to whom)
		// carrying an auto-accumulated role-experience appendix, so promoting it to
		// acceptance criteria buys false rejections over routing etiquette — and a
		// false rejection costs a whole extra advisor sub-turn plus a redo, while the
		// miss it prevents only returns to the status quo of no acceptance at all.
		// Per-company criteria, if ever wanted, need their own field (docs §10).
		// Every agora turn is looping, so the baseline is always loaded.
		AdvisorCriteria: f.cmp.AdvisorCriteria(),
	}
	res := f.turn.Run(ctx, spec)
	if res.Err != nil {
		slog.Warn("flow-agora: turn run failed", "sid", work.Sid, "err", res.Err)
	}
	// F150: mirror flow/normal — report real delivery so the drain cleared-ack fires
	// only on a turn that actually answered (Final/Process/Degraded), not Cancelled/Error.
	if work.OnOutput != nil && res.Err == nil &&
		(res.Outcome == contract.OutcomeFinal || res.Outcome == contract.OutcomeProcess || res.Outcome == contract.OutcomeDegraded) {
		work.OnOutput()
	}
	// Failure-learning (docs/wip-skill-learn-reflect.md + wip-member-learn.md): ONLY on a
	// DEGRADED turn (salvage / rubric-failed — OutcomeDegraded), snapshot this turn once and
	// feed both the per-role (member) and per-skill learners. NOT on a process reply
	// (clarify/escalate is OutcomeProcess, not a failure — keying on Outcome avoids training
	// the learners on a legitimate question as if it were a failure). Agora-only.
	if res.Err == nil && res.Outcome == contract.OutcomeDegraded {
		f.collectFailures(ctx, spec, res)
	}
	// Consume the turn's per-sid URL tracking, same as flow/normal — else the
	// shared urllib.lastBySid entry an agora turn recorded leaks forever (DumpPrompt
	// is read-only and must NOT call this). Clean final → MarkSatisfied; else Forget.
	if lib := f.urls(); lib != nil {
		if res.Err == nil && res.Outcome == contract.OutcomeFinal {
			lib.MarkSatisfied(ctx, work.Sid)
		} else {
			lib.Forget(work.Sid)
		}
	}
	// Reply-routing write side (B-4), same as flow/normal. For an agora turn whose
	// reply goes to an internal/bridge sink, LastSent() is "" (no platform message)
	// → no-op; only a real external chat reply gets indexed.
	if idx := f.msgIndex(); idx != nil {
		if id := sink.LastSent(); id != "" {
			// Index under the GROUP scope, same as the send-time OnAnchor write above —
			// a human's reply arrives addressed to the group, so reply-routing looks up
			// the member's sid under the bare group scope. Writing the raw member
			// sub-scope here (when the reply spanned multiple messages, so LastSent != the
			// OnAnchor id) would strand the last message's index under a scope no reply
			// query uses, forking a new @=new session. anchorScope is a no-op otherwise.
			// WithoutCancel: same as the OnAnchor write above and flow/normal — a
			// cancelled/shutdown turn must still land the reply-routing index row.
			idx.RecordSent(context.WithoutCancel(ctx), anchorScope(work.Scope), id, work.Sid)
		}
	}
	// Cold-memory feed (optional, per-member): enqueue this turn for the retrieval
	// leaf under the member partition — read/write symmetric on memberKey. Fire-and-
	// forget; nil when retrieval is off; skipped for non-member (bridge) turns. Gated
	// on a CLEAN outcome (Final/Process), same as flow/normal: a Degraded apology and
	// Cancelled/Error empty replies don't belong in the recall corpus.
	if memberKey != "" && res.Err == nil &&
		(res.Outcome == contract.OutcomeFinal || res.Outcome == contract.OutcomeProcess) {
		if rf := f.retrieval(); rf != nil {
			anchor := ""
			if len(events) > 0 {
				anchor = events[0].MessageID
			}
			rf.EnqueueTurn(ctx, contract.RetrievalTurn{
				Sid:      work.Sid,
				Company:  memberKey,
				Events:   events,
				Reply:    res.Reply,
				AnchorID: anchor,
			})
		}
	}
	// NOTE: no return-leg routing here ON PURPOSE — the worker-output→caller return
	// is already wired in the session mechanism layer (leaf/session targetFor picks
	// the destination, the bridge source's returnSink re-emits; docs/agora-port.md
	// ④b-2), not in flow/contract.Agora; re-adding it here would double-send. Only
	// StampInbox (agora_inbox_id history attribution) is still deferred.
}

// routeBridge handles a turn whose scope is a BRIDGE inbox: it ROUTES, never runs
// a turn (docs/agora.md §六·五).
//   - OUTBOUND (the triggering event came from the INTERNAL source — a worker's
//     send_message to the bridge): forward the text to the bridge's bound external
//     chat (the operator). No external binding → log + drop.
//   - INBOUND (an external/human reply in that chat): re-emit an internal event to
//     the bridge's default downstream inbox so a real member-inbox turn handles it.
//     No default target → log + drop.
//
// It NEVER calls f.turn.Run.
func (f *flow) routeBridge(ctx context.Context, work contract.TurnWork, ext contract.Target, defScope, company string) {
	text := compose.ComposeUserText(work.Events)
	if strings.TrimSpace(text) == "" {
		// Nothing to route — don't spam the operator (outbound) or wake the
		// downstream inbox (inbound) with an empty message.
		slog.Debug("flow-agora: bridge route with empty text — nothing to route", "scope", work.Scope)
		return
	}

	internal := len(work.Events) > 0 && work.Events[0].Source == contract.SourceNameInternal
	if internal {
		// OUTBOUND: forward to the operator's external chat.
		if ext.Source == "" {
			slog.Warn("flow-agora: bridge has no external binding — dropping outbound", "scope", work.Scope, "sid", work.Sid)
			return
		}
		src := f.gw.SourceByName(ext.Source)
		if src == nil {
			slog.Warn("flow-agora: bridge external source unknown — dropping outbound", "scope", work.Scope, "source", ext.Source, "sid", work.Sid)
			return
		}
		// Stamp the owning company so several companies' bridges can share ONE operator
		// chat and stay distinguishable.
		out := text
		if company != "" {
			out = "【" + company + "】\n" + text
		}
		if err := src.Send(ctx, ext, out); err != nil {
			slog.Warn("flow-agora: bridge outbound send failed", "scope", work.Scope, "source", ext.Source, "err", err)
			return
		}
		slog.Info("flow-agora: bridge outbound forwarded", "scope", work.Scope, "source", ext.Source, "chat", ext.ChatID)
		return
	}

	// INBOUND: re-route the human's message to the downstream member inbox.
	if defScope == "" {
		slog.Warn("flow-agora: bridge has no default target — dropping inbound", "scope", work.Scope, "sid", work.Sid)
		return
	}
	if defScope == work.Scope {
		// Misconfig: the bridge's default target is itself → re-routing would echo
		// the operator's message straight back out. Drop loud rather than loop.
		slog.Warn("flow-agora: bridge default target is itself — dropping inbound (misconfig)", "scope", work.Scope)
		return
	}
	internalSrc := f.gw.SourceByName(contract.SourceNameInternal)
	if internalSrc == nil {
		slog.Warn("flow-agora: internal source absent — cannot re-route bridge inbound", "scope", work.Scope)
		return
	}
	emitter, ok := internalSrc.(contract.MessageEmitter)
	if !ok {
		slog.Warn("flow-agora: internal source is not a MessageEmitter — cannot re-route bridge inbound", "scope", work.Scope)
		return
	}
	// Carry the batch's PLACED attachments across the bridge (D23): placement (at
	// session submit) moved them into THIS bridge scope's space, but the downstream
	// member turn's space gate never includes the bridge space — so the [本轮附件]
	// lines in text would be phantom paths. Copy each file into the downstream
	// scope's space (same space-relative path) and carry Attachments on the emitted
	// event so downstream tools can anchor them. When the carry succeeds, the relay
	// text is re-rendered WITHOUT those attachments' [本轮附件] lines — the member
	// turn re-renders the list from the carried Attachments, so keeping them in the
	// text would show every file twice. Any copy failure degrades to the old
	// text-only relay (WARN inside): the text keeps its rendered attachment lines
	// as the fallback record of what was sent.
	atts := placedEventAttachments(work.Events)
	if len(atts) > 0 {
		if f.copyBridgeAttachments(ctx, work.Scope, defScope, atts) {
			text = compose.ComposeUserText(stripCarriedAttachments(work.Events))
		} else {
			atts = nil
		}
	}
	ev := contract.MessageEvent{
		ChatID:      defScope,
		Text:        text,
		Attachments: atts,
		Lang:        compose.EventsLang(work.Events),
	}
	// Attribute the operator (the human whose message is being relayed) so the
	// downstream turn's billing/attribution keys on them, not an anonymous event.
	if human, ok := contract.FirstHumanEvent(work.Events); ok {
		ev.UserID, ev.UserName = human.UserID, human.UserName
		// Carry the operator's PLATFORM billing handle too: Emit forces
		// Source="internal", so the downstream member turn's AccountHandle() would
		// otherwise bill a ghost ("internal", uid) row no real account ever binds
		// to. composePrompt's all-internal branch decodes this and bills the
		// operator's real (family, uid) handle. Skipped for an anonymous event
		// (nothing to bill).
		if fam, uid := human.AccountHandle(); uid != "" {
			ev.StableUID = encodeBridgeOrigin(fam, uid)
		}
	}
	if err := emitter.Emit(ev); err != nil {
		slog.Warn("flow-agora: bridge inbound re-emit failed", "scope", work.Scope, "target", defScope, "err", err)
		return
	}
	slog.Info("flow-agora: bridge inbound re-routed", "scope", work.Scope, "target", defScope, "attachments", len(atts))
}

// bridgeRelayIdentity is the principal stamped on the bridge relay's file channels.
// Local file channels confine by space dir and ignore identity; this names the
// relay in logs/pooling (routeBridge never mints an agora principal — no turn runs).
const bridgeRelayIdentity = "agora-bridge"

// placedEventAttachments collects the batch's PLACED attachments — Path set and the
// staged LocalPath cleared, i.e. the files living in this scope's space that a
// FileChannel can read. A failed placement (LocalPath kept, Path a staging
// placeholder) is excluded: it is not reachable through any space.
func placedEventAttachments(events []contract.MessageEvent) []contract.Attachment {
	var out []contract.Attachment
	for _, ev := range events {
		for _, a := range ev.Attachments {
			if a.Path != "" && a.LocalPath == "" {
				out = append(out, a)
			}
		}
	}
	return out
}

// stripCarriedAttachments returns a copy of events with each CARRIED attachment
// REMOVED (predicate: Path set, staged LocalPath clear = successfully placed and
// riding the emitted event), so ComposeUserText renders no [本轮附件] line for them —
// the downstream member turn re-lists them from Attachments, so the text must not list
// them a second time. FAILED / undownloaded attachments are KEPT so describeAttachments
// still notes them as unreadable (F56) — they can't ride the event, so the text is the
// only place the member learns something was shared. (Before F56 this cleared Path
// instead of dropping; F56 then renders a Path=="" named attachment as a failure line,
// so a cleared carried attachment would spuriously read "couldn't read it".) Copies
// (events AND each Attachments slice) isolate the edit from the shared backing arrays.
func stripCarriedAttachments(events []contract.MessageEvent) []contract.MessageEvent {
	out := make([]contract.MessageEvent, len(events))
	copy(out, events)
	for i := range out {
		if len(out[i].Attachments) == 0 {
			continue
		}
		kept := make([]contract.Attachment, 0, len(out[i].Attachments))
		for _, a := range out[i].Attachments {
			if a.Path != "" && a.LocalPath == "" {
				continue // carried → rides the event; don't re-list in the relay text
			}
			kept = append(kept, a)
		}
		out[i].Attachments = kept
	}
	return out
}

// bridgeOriginPrefix marks a bridge-relayed operator's billing handle carried on
// MessageEvent.StableUID across the internal re-emit ("<prefix><family>:<uid>").
// The bridge source forces Source="internal" on Emit, which would fold the
// operator's AccountHandle() into a ghost ("internal", uid) row that no real
// account ever binds — so routeBridge encodes the ORIGIN handle here and
// composePrompt's all-internal branch decodes it for WithConsumer. Both ends live
// in this flow; no other producer of internal events sets a StableUID.
const bridgeOriginPrefix = "bridge-origin:"

// encodeBridgeOrigin renders an (accounts platform family, stable uid) billing
// handle into the StableUID carrier form.
func encodeBridgeOrigin(family, uid string) string {
	return bridgeOriginPrefix + family + ":" + uid
}

// decodeBridgeOrigin parses a StableUID carrier back into the operator's billing
// handle. ok=false for anything that is not a well-formed carrier (no prefix,
// empty family or uid) — the caller then falls back to the plain AccountHandle.
func decodeBridgeOrigin(s string) (family, uid string, ok bool) {
	rest, found := strings.CutPrefix(s, bridgeOriginPrefix)
	if !found {
		return "", "", false
	}
	if i := strings.IndexByte(rest, ':'); i > 0 && i < len(rest)-1 {
		return rest[:i], rest[i+1:], true
	}
	return "", "", false
}

// bridgeOriginConsumer scans an (all-internal) batch for the first event carrying
// a bridge-relayed operator handle. ok=false when none — a true agent→agent
// dispatch / cron wake, which stays unbilled or on the plain fallback.
func bridgeOriginConsumer(events []contract.MessageEvent) (family, uid string, ok bool) {
	for _, ev := range events {
		if f, u, ok := decodeBridgeOrigin(ev.StableUID); ok {
			return f, u, true
		}
	}
	return "", "", false
}

// copyBridgeAttachments copies placed attachments from the bridge scope's space into
// the downstream scope's space, KEEPING each space-relative path so the [本轮附件]
// lines in the relayed text resolve downstream. Both sides go through warrant
// FileChannels (each escape-confined to its own space root — never a raw cross-root
// os copy). All-or-nothing: false on the first failure, and the caller degrades to
// the text-only relay. A same-name file already in the downstream inbox is
// overwritten (the operator's fresh copy wins — bounded; placement dedupes names
// only within one space).
func (f *flow) copyBridgeAttachments(ctx context.Context, bridgeScope, defScope string, atts []contract.Attachment) bool {
	w := f.warr()
	if w == nil {
		slog.Warn("flow-agora: bridge attachment relay skipped (no warrant) — relaying text only", "scope", bridgeScope)
		return false
	}
	src, err := w.File(ctx, bridgeRelayIdentity, bridgeScope)
	if err != nil || src == nil {
		slog.Warn("flow-agora: bridge attachment relay: open bridge space failed — relaying text only", "scope", bridgeScope, "err", err)
		return false
	}
	defer src.Close()
	dst, err := w.File(ctx, bridgeRelayIdentity, defScope)
	if err != nil || dst == nil {
		slog.Warn("flow-agora: bridge attachment relay: open target space failed — relaying text only", "scope", bridgeScope, "target", defScope, "err", err)
		return false
	}
	defer dst.Close()
	for _, a := range atts {
		data, err := src.Read(ctx, a.Path)
		if err != nil {
			slog.Warn("flow-agora: bridge attachment relay: read failed — relaying text only", "scope", bridgeScope, "path", a.Path, "err", err)
			return false
		}
		if err := dst.Write(ctx, a.Path, data); err != nil {
			slog.Warn("flow-agora: bridge attachment relay: write failed — relaying text only", "scope", bridgeScope, "target", defScope, "path", a.Path, "err", err)
			return false
		}
	}
	return true
}

// composePrompt builds the deterministic part of the agora turn — the resolved
// agora context, the agora-principal-stamped ctx, the filled prompt builder (with
// the directory + playbook injected into the platform layer — NOT identity, which
// the package header warns would wipe the agent's identity), and the agora-authorized
// skill projection + its prompt layer. A PURE function of the work: RunTurn runs it
// then drives the turn; DumpPrompt runs it then renders (no run).
func (f *flow) composePrompt(ctx context.Context, work contract.TurnWork) (context.Context, contract.AgoraTurn, contract.Prompt, contract.SkillSet, contract.ToolSet) {
	// Per-turn agora context: the resolved inbox, the minted principal, and the
	// pre-rendered directory + playbook. agora returns a no-grants principal when the
	// scope does not resolve to a live member inbox (a hub identity can never leak in).
	at := f.turnContext(ctx, work.Scope)
	if at.InboxID == "" {
		// Accepts also claims fanned groups (to ask "找谁"), and that ask path returns
		// before here — so an empty InboxID at this point is a rare race (an OrgCache/router
		// reload, or a member going inactive, between Accepts/the ask check and now).
		// Fail-closed (no-grants principal, empty bag); log so the race is visible rather
		// than a silent tool-less turn.
		slog.Warn("flow-agora: scope no longer resolves to an inbox (raced reload?) — running locked-down", "scope", work.Scope)
	}

	// Stamp the agora principal on ctx — it scopes channel vending + billing +
	// logging (NOT the tool bag; agora self-authorizes by inboxID).
	ctx = contract.WithIdentity(ctx, at.Principal)
	// Stamp the per-MEMBER identity too — the browser tool keys a worker's login MASTER by
	// it (per-member, finer than the per-role principal). "" → unchanged.
	ctx = contract.WithMember(ctx, at.MemberID)
	// Bill the human sender (first non-internal event), not a coalesced internal
	// event at the head. An all-internal batch may still be a HUMAN's message: a
	// bridge inbound relay re-emits the operator's message through the internal
	// source, carrying their real platform handle in the StableUID origin frame
	// (routeBridge) — decode it so the operator is billed, not a ghost
	// ("internal", uid) row. Otherwise fall back to Events[0]. (The principal
	// above is inbox-derived, NOT sender-derived — only billing keys here.)
	if anchor, ok := contract.FirstHumanEvent(work.Events); ok {
		s, uid := anchor.AccountHandle()
		ctx = contract.WithConsumer(ctx, s, uid)
	} else if s, uid, ok := bridgeOriginConsumer(work.Events); ok {
		ctx = contract.WithConsumer(ctx, s, uid)
	} else if len(work.Events) > 0 {
		s, uid := work.Events[0].AccountHandle()
		ctx = contract.WithConsumer(ctx, s, uid)
	}

	p := f.cmp.NewBuilder()
	f.cmp.SetStaticLayers(p)
	// Identity layer = THIS member's identity card (member name + company/role + PromptStyle),
	// REPLACING the default "你是 bob" persona SetStaticLayers seeded. The other static layers
	// (constitution / preferences / tool_policy) carry the behavioral rules and stay. "" (a
	// non-member scope) → keep the default identity.
	//
	// D21 (intended, documented): an operator edit to $home/prompt/identity.md applies to
	// NORMAL turns but is REPLACED here on a member turn — a worker is "你是 alice / 公司X
	// 角色Y", not generic bob, so the per-member card is authoritative for agora. identity.md
	// is the normal-flow persona only; per-member identity is edited via the member card
	// (agora role learning / PromptStyle), not the global identity.md.
	if at.Identity != "" {
		p.SetLayer("identity", at.Identity)
	}
	p.SetLayer("skills", "")         // pin slot — BEFORE tool_selection: recall the method before reaching for a tool (docs/skill-recall-index.md §5)
	p.SetLayer("tool_selection", "") // pin slot (after tool_policy) before the bag fills it
	p.SetLayer("memory", "")         // reserved — memory subsystem not ported yet
	// platform layer = the agora directory + playbook (agora pre-rendered them to
	// strings; the prompt layer never imports agora). This is where the agora flow
	// diverges from the normal flow's empty platform layer.
	p.SetLayer("platform", renderAgoraPlatform(at))
	// Looping work discipline, UNCONDITIONAL (unlike flow/normal's stamp gate):
	// every agora member turn runs the looping driver (see RunTurn's TurnSpec.Mode),
	// so every turn gets the mode's contract layer. No advisor protocol — the advisor
	// is harness-triggered, not model-triggerable (docs/advisor.md §7).
	p.SetLayer("work_discipline", compose.LoopingDiscipline)
	p.SetLayer("runtime", compose.RenderRuntime())

	// Project the turn's authorized tools (via agora) and fill the scenario→tool rubric
	// from the VISIBLE tools' SelectionHints. Computed BEFORE ComposeSkills: the skill
	// index gates on skill_view being in the bag. Returned so RunTurn/DumpPrompt reuse
	// the same bag (no double Authorize; dump == real turn).
	tools := f.toolBag(ctx, at.InboxID)
	// Project the turn's authorized skills via AGORA (company grants ∩ visibility),
	// not warrant. Empty (fail-closed) when the inbox has no live employment/role.
	skills := f.skillBag(ctx, at.InboxID)
	compose.ComposeSkills(p, skills, tools)
	compose.ComposeToolSelection(p, tools)
	return ctx, at, p, skills, tools
}

// turnContext resolves the per-turn agora context. agora is present here (Accepts
// gated on it), but guard nil for the direct-construction test path.
func (f *flow) turnContext(ctx context.Context, scope string) contract.AgoraTurn {
	if ag := f.ag(); ag != nil {
		return ag.TurnContext(ctx, scope)
	}
	return contract.AgoraTurn{}
}

// toolBag projects the turn's authorized tool set: agora SUPPLIES the member's
// projection (company permissions_yaml, cross-company intersection) + decorates space
// hints; warrant JUDGES catalog ∩ projection (the single judge — docs §14). No catalog
// / no warrant → nil (fail-closed, text-only turn).
func (f *flow) toolBag(ctx context.Context, inboxID string) contract.ToolSet {
	cat := f.catalog()
	ag := f.ag()
	w := f.warr()
	if cat == nil || ag == nil || w == nil {
		return nil
	}
	specs := ag.DecorateToolSpecs(ctx, inboxID, cat.Specs())
	return w.Authorize(ctx, ag.MemberProjection(ctx, inboxID), specs)
}

// skillBag projects the turn's authorized skill set: agora supplies the projection,
// warrant judges. No catalog / no warrant → nil.
func (f *flow) skillBag(ctx context.Context, inboxID string) contract.SkillSet {
	cat := f.skillCat()
	ag := f.ag()
	w := f.warr()
	if cat == nil || ag == nil || w == nil {
		return nil
	}
	return w.AuthorizeSkills(ctx, ag.MemberProjection(ctx, inboxID), cat.List())
}

// DumpPrompt composes this flow's prompt for the work and renders it (read-only —
// no model, no persist), the /prompt admin command. Mirrors flow-normal's dump but
// shows the agora principal + the directory/playbook + the agora-authorized bags.
func (f *flow) DumpPrompt(ctx context.Context, work contract.TurnWork) string {
	_, at, p, skills, tools := f.composePrompt(ctx, work)
	// Mirror RunTurn's user-text prep: strip the "^member" addressing token on a member
	// sub-scope so the dump shows what the member actually sees (D20-1), then the shared
	// DumpUserText (audio note, no ASR). The agora-identity block rides as RenderDump extras.
	events := work.Events
	if member := memberOfScope(work.Scope); member != "" {
		events = stripMemberTag(events, member)
	}
	extras := fmt.Sprintf("# agora 身份\n\n- 主体：%s\n- inbox：%s", at.Principal, at.InboxID)
	return compose.RenderDump(p.Build(), compose.DumpUserText(events), tools, skills, extras)
}

// renderAgoraPlatform assembles the agora PLATFORM prompt layer from the per-turn
// directory + playbook (both already rendered to strings by agora). Empty when the
// turn carries neither — the layer slot stays pinned but Build drops empty content.
func renderAgoraPlatform(at contract.AgoraTurn) string {
	var b strings.Builder
	if d := strings.TrimSpace(at.Playbook); d != "" {
		b.WriteString("# 公司运作说明\n\n")
		b.WriteString(d)
		b.WriteString("\n")
	}
	if d := strings.TrimSpace(at.Directory); d != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("# 通讯录\n\n")
		b.WriteString(d)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// agentAuthor builds the assistant-row attribution for an agora turn: the member when
// the principal is a member, else bob (a non-member principal / bridge sentinel never
// runs a member turn, but fall back rather than store an empty agent).
func agentAuthor(principal string) contract.MsgAuthor {
	if strings.HasPrefix(principal, "member:") {
		return contract.MsgAuthor{Kind: "agent", ID: principal, Name: strings.TrimPrefix(principal, "member:")}
	}
	return contract.MsgAuthor{Kind: "agent", ID: "bob", Name: "bob"}
}

// firstGroupEvent returns the first GROUP chat event in the batch (the one virtual-group
// member routing keys on). ok=false when the batch has no group event (internal wakes /
// DM), so the caller skips member routing.
func firstGroupEvent(events []contract.MessageEvent) (contract.MessageEvent, bool) {
	for _, e := range events {
		if e.ChatType.IsGroupChat() {
			return e, true
		}
	}
	return contract.MessageEvent{}, false
}

// agoraCreds is the agora flow's contract.CredentialOpener: the GATE is the inbox
// member's composed company grants (agora is the authority, not warrant's matrix —
// docs §14.2); the build MECHANISM is warrant's shared ResolveCredential (vault scan
// by kind → keep granted → unique → broker.Build). A member thus resolves only its
// own company-granted credentials. nil warrant/agora, or no live member, → refused
// (fail-closed). The tool never sees the secret or the credential name.
type agoraCreds struct {
	warr    contract.Warrant
	ag      contract.Agora
	inboxID string
}

func (o agoraCreds) Build(ctx context.Context, kind string) (any, error) {
	if o.warr == nil || o.ag == nil {
		return nil, errors.New("agora: 凭证不可用（无 warrant/agora）")
	}
	// agora SUPPLIES the member projection; warrant JUDGES it (the single judge).
	return o.warr.ResolveCredential(ctx, o.ag.MemberProjection(ctx, o.inboxID), kind)
}

var _ contract.CredentialOpener = agoraCreds{}

// askWhoText is the "找谁" reply for a fanned group with no addressed member: list the
// members so the human can pick with the "^成员名" routing token. One member per LINE
// (a long roster stays readable). botMention (e.g. "@binserv00Bot"), when non-empty,
// prefixes each token so the line is the COMPLETE address to copy — needed in a
// multi-bot group where the human must @ the RIGHT bot ("@binserv00Bot^taobao-one").
// Empty (source has no typeable self-mention) → bare "^name".
func askWhoText(members []string, lang, botMention string) string {
	tagged := make([]string, 0, len(members))
	for _, m := range members {
		tagged = append(tagged, botMention+"^"+m)
	}
	intro := strings.TrimRight(i18n.T("flow.agora.ask_who", lang), " ")
	return intro + "\n" + strings.Join(tagged, "\n")
}

// anchorScope is the scope a member's outbound is indexed under for reply-routing. For a
// virtual-group member sub-scope "<group>#<member>" (contract.SplitMemberSubScope) the
// human's reply arrives addressed to the GROUP, so strip the member suffix; a no-op for
// any other scope.
func anchorScope(scope string) string {
	if group, _, ok := contract.SplitMemberSubScope(scope); ok {
		return group
	}
	return scope
}

// memberOfScope returns the member name from a virtual-group member sub-scope
// "<group>#<member>", or "" for any other scope (the inverse of anchorScope).
func memberOfScope(scope string) string {
	if _, member, ok := contract.SplitMemberSubScope(scope); ok {
		return member
	}
	return ""
}

// stripMemberTag returns a shallow COPY of events with a LEADING "^<member>" addressing
// token removed from each text (the common "^alice 看下…" form). The copy isolates the
// Text edit from the shared (WAL/pending) backing slice; note Attachments still share the
// backing array (Text is the only field edited here). A mid-text tag is left as-is.
func stripMemberTag(events []contract.MessageEvent, member string) []contract.MessageEvent {
	out := make([]contract.MessageEvent, len(events))
	copy(out, events)
	for i := range out {
		out[i].Text = stripLeadingMemberTag(out[i].Text, member)
	}
	return out
}

// dropHiddenSlash partitions a member-addressed batch: events whose "^member" strip
// would reveal a LEADING "/" (an addressing-hidden slash command — see RunTurn's
// intercept) are dropped, the rest kept VERBATIM (stripMemberTag still runs later on
// the kept ones). Returns the kept events and how many were dropped.
func dropHiddenSlash(events []contract.MessageEvent, member string) (kept []contract.MessageEvent, dropped int) {
	for i := range events {
		// stripLeadingMemberTag already left-trims the residual, so a leading "/" is
		// checked directly; a no-op strip (no tag / different name) never drops.
		s := stripLeadingMemberTag(events[i].Text, member)
		if s != events[i].Text && strings.HasPrefix(s, "/") {
			dropped++
			continue
		}
		kept = append(kept, events[i])
	}
	return kept, dropped
}

// memberTagTrailingPunct is the trailing punctuation trimmed off a "^name" addressing
// token. KEEP IN SYNC — BYTE-IDENTICAL — with leaf/agora's copy (parseMemberTag,
// leaf/agora/40-impl.go): routing (there) and stripping (here) must trim the same set,
// or a token that routes still leaks into the member's prompt (or the reverse: a token
// strips but never routed). arch forbids the cross import (TestModuleImportBoundary),
// so the literal is mirrored; TestMemberTagPunctInSync locks the two equal.
const memberTagTrailingPunct = ",.;:!?()，。；：！？、）"

func stripLeadingMemberTag(text, member string) string {
	t := strings.TrimLeft(text, " \t")
	if !strings.HasPrefix(t, "^") { // "^" = bob's member-routing token (see leaf/agora memberTagPrefix)
		return text
	}
	// PREFIX-strip the resolved member's name (F159): routing prefix-matched "^alice帮我查"
	// to member "alice" with no space, so the strip must remove exactly "^alice" (+ any
	// trailing tag punctuation) and leave the glued content. The char after the name must
	// be a name BOUNDARY (non-[A-Za-z0-9_-] or end) — else this leading token is a
	// different/longer name and we leave the text untouched.
	body := t[1:]
	if len(body) < len(member) || !strings.EqualFold(body[:len(member)], member) {
		return text
	}
	rest := body[len(member):]
	if rest != "" && isNameByte(rest[0]) {
		return text
	}
	return strings.TrimLeft(strings.TrimLeft(rest, memberTagTrailingPunct), " \t\n\r")
}

// isNameByte reports whether b is a valid member-name byte (entityNameRe = [A-Za-z0-9_-]).
// KEEP IN SYNC with leaf/agora's copy — the "^name" boundary must be judged identically on
// the routing side (parseMemberTag) and the stripping side (here).
func isNameByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
}

// dispatchNoReply reports whether any event in this turn's batch is a fire-and-forget
// dispatch (NoReply) — a wake whose value is the turn's tool side-effects, not a reply
// (the arrangement pull-nudge). The POSITIVE signal that lets a no-reply-target turn RUN
// with a discard sink, while genuine routing failures (no flag) still drop.
func dispatchNoReply(events []contract.MessageEvent) bool {
	for _, ev := range events {
		if ev.Dispatch != nil && ev.Dispatch.NoReply {
			return true
		}
	}
	return false
}

// discardSink is a no-op contract.Sink for internal tool-driven wakes that have no
// reply target (arrangement nudge / cron / self-wake): the turn runs for its tool
// side-effects, and any chat reply is discarded.
type discardSink struct{}

func (discardSink) ContentDelta(string)           {}
func (discardSink) TraceDelta(string)             {}
func (discardSink) Finish(string) error           { return nil }
func (discardSink) SendFile(string, string) error { return nil }
func (discardSink) LastSent() string              { return "" }

var (
	_ trunk.Module  = (*Module)(nil)
	_ contract.Flow = (*flow)(nil)
	_ contract.Sink = discardSink{}
)
