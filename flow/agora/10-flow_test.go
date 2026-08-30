package agora

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/flow/compose"
	"agentbob/heartwood/prompt"
	"agentbob/trunk"
)

// --- fakes -----------------------------------------------------------------

// fakeAgora is a minimal contract.Agora: InboxForScope hits a single configured
// scope, TurnContext returns a fixed principal/directory/playbook, and
// AuthorizeTools/Skills return a canned bag (recording the inbox they were keyed by).
type fakeAgora struct {
	scope     string // the one scope that routes to an inbox
	inboxID   string
	turn      contract.AgoraTurn
	toolBag   contract.ToolSet
	skillBag  contract.SkillSet
	toolInbox string // captured: which inbox AuthorizeTools was keyed by

	// bridge routing knobs (scope routes as a bridge when isBridge is set).
	isBridge       bool
	bridgeExt      contract.Target
	bridgeDefScope string

	// pause knob: when paused is set, IsPaused returns true for the hit scope.
	// pausedSubs marks additional exact scopes (fan member sub-scopes) as paused.
	paused        bool
	pausedSubs    map[string]bool
	pausedMembers map[string]bool // F161: scopes that are paused MEMBER inboxes (agora claims + bounces)

	// fan knobs: when fan is set, RouteGroupScope reports a fanned group with
	// fanMembers (the "找谁" ask path).
	fan        bool
	fanMembers []string
}

func (a *fakeAgora) InboxForScope(_ context.Context, scope string) (string, bool) {
	if scope == a.scope {
		return a.inboxID, true
	}
	return "", false
}
func (a *fakeAgora) ScopeIsAgora(_ context.Context, scope string) bool {
	return scope != "" && scope == a.scope
}
func (a *fakeAgora) InboxScopes(_ context.Context, inboxID string) []string {
	if inboxID == a.inboxID && a.scope != "" {
		return []string{a.scope}
	}
	return nil
}
func (a *fakeAgora) RouteGroupScope(context.Context, contract.MessageEvent) (string, []string, bool) {
	if a.fan {
		return "", a.fanMembers, true
	}
	return "", nil, false
}
func (a *fakeAgora) CompanyBridgeScope(context.Context, string) (string, bool) { return "", false }
func (a *fakeAgora) RoleProjection(context.Context, string, string) contract.GrantSet {
	return contract.GrantSet{}
}
func (a *fakeAgora) TurnContext(context.Context, string) contract.AgoraTurn { return a.turn }

// MemberProjection supplies the grants matching the pre-canned toolBag/skillBag (the
// judgeWarrant in newFlow then filters the catalog against it). Captures the inbox.
func (a *fakeAgora) MemberProjection(_ context.Context, inboxID string) contract.GrantSet {
	a.toolInbox = inboxID
	g := contract.GrantSet{}
	if a.toolBag != nil {
		for _, s := range a.toolBag.Specs() {
			g["tool:use:"+s.Name] = struct{}{}
		}
	}
	if a.skillBag != nil {
		for _, si := range a.skillBag.List() {
			g["skill:use:"+si.Name] = struct{}{}
		}
	}
	return g
}
func (a *fakeAgora) DecorateToolSpecs(_ context.Context, _ string, specs []contract.ToolSpec) []contract.ToolSpec {
	return specs
}

// judgeWarrant is the single JUDGE in the agora flow test: it filters the catalog
// against the GrantSet agora supplies (real Authorize behavior). docs §14.
type judgeWarrant struct{}

func (judgeWarrant) Grants(context.Context, string) contract.GrantSet             { return nil }
func (judgeWarrant) Check(context.Context, string, string, string) (bool, string) { return true, "" }
func (judgeWarrant) Authorize(_ context.Context, g contract.GrantSet, specs []contract.ToolSpec) contract.ToolSet {
	var keep []contract.ToolSpec
	for _, s := range specs {
		if g.Granted("tool:use:" + s.Name) {
			keep = append(keep, s)
		}
	}
	return fakeToolSet{specs: keep}
}
func (judgeWarrant) AuthorizeSkills(_ context.Context, g contract.GrantSet, infos []contract.SkillInfo) contract.SkillSet {
	var keep []contract.SkillInfo
	for _, si := range infos {
		if g.Granted("skill:use:" + si.Name) {
			keep = append(keep, si)
		}
	}
	return fakeSkillSet{infos: keep}
}
func (judgeWarrant) ResolveCredential(context.Context, contract.GrantSet, string) (any, error) {
	return nil, nil
}
func (judgeWarrant) File(context.Context, string, string) (contract.FileChannel, error) {
	return nil, nil
}
func (judgeWarrant) Exec(context.Context, string, string) (contract.ExecChannel, error) {
	return nil, nil
}
func (judgeWarrant) Cut(string) {}

// BridgeRouting: by default not a bridge (the existing tests run normal turns).
// The bridge tests set bridge*=… to make the configured scope route as a bridge.
func (a *fakeAgora) BridgeRouting(_ context.Context, scope string) (contract.Target, string, string, bool) {
	if !a.isBridge || scope != a.scope {
		return contract.Target{}, "", "", false
	}
	return a.bridgeExt, a.bridgeDefScope, "", true
}

// IsPaused: paused only for the configured hit scope when the knob is set, plus
// any exact scope in pausedSubs (fan member sub-scopes).
func (a *fakeAgora) IsPaused(scope string) bool {
	return (a.paused && scope == a.scope) || a.pausedSubs[scope]
}

// pausedMembers marks scopes as paused MEMBER inboxes (F161) — claimed by agora + bounced,
// distinct from IsPaused's company-level pause.
func (a *fakeAgora) PausedMemberScope(_ context.Context, scope string) bool {
	return a.pausedMembers[scope]
}

func (a *fakeAgora) MemberScopesForRole(context.Context, string, string) []string { return nil }

func (a *fakeAgora) CompanyActive(context.Context, string) bool { return true }

// fakeToolSet is a minimal ToolSet/ToolCatalog.
type fakeToolSet struct{ specs []contract.ToolSpec }

func (s fakeToolSet) Specs() []contract.ToolSpec        { return s.specs }
func (fakeToolSet) Lookup(string) (contract.Tool, bool) { return nil, false }

// fakeSkillSet is a minimal SkillSet/SkillCatalog.
type fakeSkillSet struct{ infos []contract.SkillInfo }

func (s fakeSkillSet) List() []contract.SkillInfo               { return s.infos }
func (fakeSkillSet) Get(string) (contract.Skill, bool)          { return contract.Skill{}, false }
func (fakeSkillSet) Read(string) (string, bool)                 { return "", false }
func (fakeSkillSet) Files(string) ([]contract.SkillFile, error) { return nil, nil }

// fakeSink captures Finish + trace; everything else no-ops.
type fakeSink struct {
	traces   []string
	finished string
	didFin   bool
}

func (s *fakeSink) ContentDelta(string)        {}
func (s *fakeSink) TraceDelta(t string)        { s.traces = append(s.traces, t) }
func (s *fakeSink) Finish(t string) error      { s.finished = t; s.didFin = true; return nil }
func (s *fakeSink) SendFile(_, _ string) error { return nil }
func (s *fakeSink) LastSent() string           { return "" }

// sentMsg captures one Send call (bridge outbound forwarding).
type sentMsg struct {
	t    contract.Target
	text string
}

// fakeSource hands out a fakeSink. Only NewSink/Name matter for the flow; Send is
// recorded for the bridge-outbound test.
type fakeSource struct {
	sink *fakeSink
	sent []sentMsg
}

func (s *fakeSource) Name() string                                            { return "test" }
func (s *fakeSource) Caps() contract.Caps                                     { return contract.Caps{} }
func (s *fakeSource) Run(context.Context, chan<- contract.MessageEvent) error { return nil }
func (s *fakeSource) NewSink(context.Context, contract.Target, string, string, contract.SinkPrefs) contract.Sink {
	return s.sink
}
func (s *fakeSource) SendButtons(context.Context, contract.Target, string, []contract.Button) (string, error) {
	return "", nil
}
func (s *fakeSource) Send(_ context.Context, t contract.Target, text string) error {
	s.sent = append(s.sent, sentMsg{t: t, text: text})
	return nil
}
func (s *fakeSource) SendFile(context.Context, contract.Target, string, string) error { return nil }
func (s *fakeSource) HealthCheck(context.Context) error                               { return nil }

// fakeGateway returns one source by any name.
type fakeGateway struct{ src contract.Source }

func (g fakeGateway) Events() <-chan contract.MessageEvent { return nil }
func (g fakeGateway) SourceByName(string) contract.Source  { return g.src }
func (g fakeGateway) SourceNames() []string                { return nil }

// mapGateway returns a per-name source (bridge tests need the external source and
// the internal emitter distinguished).
type mapGateway struct{ byName map[string]contract.Source }

func (g mapGateway) Events() <-chan contract.MessageEvent { return nil }
func (g mapGateway) SourceNames() []string                { return nil }
func (g mapGateway) SourceByName(n string) contract.Source {
	if s, ok := g.byName[n]; ok {
		return s
	}
	return nil
}

// fakeEmitter is an internal source that also implements contract.MessageEmitter,
// recording the events re-emitted into the bus (bridge inbound re-route).
type fakeEmitter struct {
	fakeSource
	emitted []contract.MessageEvent
}

func (e *fakeEmitter) Emit(ev contract.MessageEvent) error {
	e.emitted = append(e.emitted, ev)
	return nil
}

// memFileChannel is an in-memory contract.FileChannel over one space's file map
// (the bridge attachment-relay tests).
type memFileChannel struct{ files map[string][]byte }

func (c *memFileChannel) Alive() bool  { return true }
func (c *memFileChannel) Close() error { return nil }
func (c *memFileChannel) List(context.Context, string) ([]contract.FileEntry, error) {
	return nil, nil
}
func (c *memFileChannel) Read(_ context.Context, path string) ([]byte, error) {
	b, ok := c.files[path]
	if !ok {
		return nil, fmt.Errorf("no file %q", path)
	}
	return b, nil
}
func (c *memFileChannel) Write(_ context.Context, path string, data []byte) error {
	c.files[path] = data
	return nil
}
func (c *memFileChannel) Mkdir(context.Context, string) error          { return nil }
func (c *memFileChannel) Remove(context.Context, string) error         { return nil }
func (c *memFileChannel) Rename(context.Context, string, string) error { return nil }
func (c *memFileChannel) AbsPath(string) (string, error)               { return "", nil }

// spaceWarrant is judgeWarrant plus REAL per-space File channels, keyed by space
// name (the bridge relay opens the bridge scope's and the downstream scope's).
type spaceWarrant struct {
	judgeWarrant
	chans map[string]contract.FileChannel
}

func (w spaceWarrant) File(_ context.Context, _ string, space string) (contract.FileChannel, error) {
	if ch, ok := w.chans[space]; ok {
		return ch, nil
	}
	return nil, fmt.Errorf("no space %q", space)
}

// fakeTurn captures the spec and ctx it was handed (ctx carries the billing
// consumer stamp the bridge-origin tests assert on).
type fakeTurn struct {
	spec contract.TurnSpec
	ctx  context.Context
}

func (t *fakeTurn) Run(ctx context.Context, spec contract.TurnSpec) contract.TurnResult {
	t.ctx = ctx
	t.spec = spec
	return contract.TurnResult{}
}

// dumpFactory hands out real prompt builders so the prompt renders actual layers.
type dumpFactory struct{}

func (dumpFactory) New() contract.Prompt { return prompt.NewBuilder() }

// --- helpers ---------------------------------------------------------------

// newFlow builds a flow wired to a registry holding the given agora + catalogs.
func newFlow(ag contract.Agora, tools contract.ToolCatalog, skills contract.SkillCatalog, turnp contract.Turn, src contract.Source) *flow {
	reg := trunk.NewRegistry()
	// The single JUDGE: agora supplies projections, warrant judges (docs §14).
	trunk.Provide[contract.Warrant](reg, judgeWarrant{})
	if ag != nil {
		trunk.Provide[contract.Agora](reg, ag)
	}
	if tools != nil {
		trunk.Provide[contract.ToolCatalog](reg, tools)
	}
	if skills != nil {
		trunk.Provide[contract.SkillCatalog](reg, skills)
	}
	return &flow{
		gw:   fakeGateway{src: src},
		turn: turnp,
		reg:  reg,
		cmp:  &compose.Composer{Prompts: dumpFactory{}},
	}
}

// newFlowWithWarrant mirrors newFlow but injects a CUSTOM warrant (the bridge
// attachment-relay tests need working File channels, not judgeWarrant's nils).
func newFlowWithWarrant(ag contract.Agora, w contract.Warrant, turnp contract.Turn, src contract.Source) *flow {
	reg := trunk.NewRegistry()
	trunk.Provide[contract.Warrant](reg, w)
	if ag != nil {
		trunk.Provide[contract.Agora](reg, ag)
	}
	return &flow{
		gw:   fakeGateway{src: src},
		turn: turnp,
		reg:  reg,
		cmp:  &compose.Composer{Prompts: dumpFactory{}},
	}
}

const (
	hitScope = "telegram:group:42"
	inboxID  = "inbox-co-boss"
)

func okAgora() *fakeAgora {
	return &fakeAgora{
		scope:   hitScope,
		inboxID: inboxID,
		turn: contract.AgoraTurn{
			InboxID:   inboxID,
			Principal: "company:acme/role:boss",
			Identity:  "你是 小红。\n你的任职：\n- 顺意公司 · 角色：客服",
			Directory: "- 张三（同事）",
			Playbook:  "本公司专做超声报告。",
		},
		toolBag:  fakeToolSet{specs: []contract.ToolSpec{{Name: "send_message", Description: "发消息"}}},
		skillBag: fakeSkillSet{infos: []contract.SkillInfo{{Name: "ultrasound", Description: "超声"}}},
	}
}

// --- tests -----------------------------------------------------------------

func TestStrategy(t *testing.T) {
	f := &flow{}
	if f.Name() != "agora" {
		t.Errorf("Name = %q, want agora", f.Name())
	}
	if f.Priority() != 10 {
		t.Errorf("Priority = %d, want 10 (beats normal's 0)", f.Priority())
	}
	if f.Mode() != contract.SessionModeAuto {
		t.Errorf("Mode = %v, want SessionModeAuto (agora workers are autonomous)", f.Mode())
	}
}

// TestAcceptsHitMiss: Accepts is true only when agora is present and the scope
// routes to an inbox; absent agora → false (the normal catch-all takes over).
func TestAcceptsHitMiss(t *testing.T) {
	f := newFlow(okAgora(), nil, nil, &fakeTurn{}, &fakeSource{sink: &fakeSink{}})
	if !f.Accepts(contract.TurnWork{Scope: hitScope}) {
		t.Error("Accepts should be true for an agora-routed scope")
	}
	if f.Accepts(contract.TurnWork{Scope: "telegram:dm:other"}) {
		t.Error("Accepts should be false for a non-agora scope")
	}

	// Absent agora → accepts nothing.
	noAg := newFlow(nil, nil, nil, &fakeTurn{}, &fakeSource{sink: &fakeSink{}})
	if noAg.Accepts(contract.TurnWork{Scope: hitScope}) {
		t.Error("Accepts should be false when agora is absent")
	}
}

// TestRunTurnUsesAgora: RunTurn stamps the agora principal, projects the bag via
// agora (keyed by the resolved inbox), and injects directory/playbook into the
// prompt — asserted through the captured TurnSpec.
func TestRunTurnUsesAgora(t *testing.T) {
	ag := okAgora()
	turnp := &fakeTurn{}
	f := newFlow(ag, fakeToolSet{specs: []contract.ToolSpec{{Name: "send_message"}, {Name: "now"}}},
		fakeSkillSet{infos: []contract.SkillInfo{{Name: "ultrasound"}}},
		turnp, &fakeSource{sink: &fakeSink{}})

	work := contract.TurnWork{
		Sid:    "sid1",
		Scope:  hitScope,
		Target: contract.Target{Source: "test"},
		Events: []contract.MessageEvent{{Text: "你好"}},
	}
	f.RunTurn(context.Background(), work)

	// Tool bag came from agora, keyed by the resolved inbox (not warrant).
	if ag.toolInbox != inboxID {
		t.Errorf("AuthorizeTools keyed by %q, want %q", ag.toolInbox, inboxID)
	}
	if turnp.spec.Tools == nil || len(turnp.spec.Tools.Specs()) != 1 || turnp.spec.Tools.Specs()[0].Name != "send_message" {
		t.Errorf("tool bag not the agora bag: %+v", turnp.spec.Tools)
	}
	// Channel opener bound to the agora principal.
	bc, ok := turnp.spec.Channels.(compose.BoundChannels)
	if !ok || bc.Identity != "company:acme/role:boss" {
		t.Errorf("channels not bound to agora principal: %+v", turnp.spec.Channels)
	}
	// Directory + playbook injected into the system prompt (platform layer).
	sys := turnp.spec.Prompt.Build()
	if !strings.Contains(sys, "本公司专做超声报告") || !strings.Contains(sys, "张三") {
		t.Errorf("prompt missing agora directory/playbook:\n%s", sys)
	}
	// Every agora member turn runs the LOOPING driver (flow strategy, not the
	// session stamp) with the work-discipline layer in the prompt and no IterCap
	// override (the looping fuse is the budget).
	if turnp.spec.Mode != contract.TurnModeLooping {
		t.Errorf("Mode = %q, want looping (all agora turns are work turns)", turnp.spec.Mode)
	}
	if turnp.spec.IterCap != 0 {
		t.Errorf("IterCap = %d, want 0 (looping fuse is the budget)", turnp.spec.IterCap)
	}
	if !strings.Contains(sys, "长任务工作模式") {
		t.Errorf("prompt missing the looping work-discipline layer:\n%s", sys)
	}
}

// TestRunTurnPausedSkips: when the scope's company is paused, RunTurn finishes
// with the one-time notice and does NOT run the model (the message link stops —
// no reroute to the normal flow).
func TestRunTurnPausedSkips(t *testing.T) {
	ag := okAgora()
	ag.paused = true
	turnp := &fakeTurn{}
	sink := &fakeSink{}
	f := newFlow(ag, fakeToolSet{specs: []contract.ToolSpec{{Name: "send_message"}}},
		fakeSkillSet{}, turnp, &fakeSource{sink: sink})

	f.RunTurn(context.Background(), contract.TurnWork{
		Sid:    "sid-paused",
		Scope:  hitScope,
		Target: contract.Target{Source: "test"},
		Events: []contract.MessageEvent{{Text: "你好"}},
	})

	if turnp.spec.Sid != "" {
		t.Error("a turn was run for a paused company — must be skipped")
	}
	// i18n: the notice goes through i18n.T; with no catalog loaded in the test it returns
	// the key, so assert on the key rather than the resolved Chinese text.
	if !sink.didFin || !strings.Contains(sink.finished, "flow.agora.company_paused") {
		t.Errorf("paused notice not sent: didFin=%v finished=%q", sink.didFin, sink.finished)
	}

	// Not paused → the turn runs normally (control).
	ag.paused = false
	turnp2 := &fakeTurn{}
	f2 := newFlow(ag, fakeToolSet{specs: []contract.ToolSpec{{Name: "send_message"}}},
		fakeSkillSet{}, turnp2, &fakeSource{sink: &fakeSink{}})
	f2.RunTurn(context.Background(), contract.TurnWork{
		Sid: "sid-live", Scope: hitScope, Target: contract.Target{Source: "test"},
		Events: []contract.MessageEvent{{Text: "你好"}},
	})
	if turnp2.spec.Sid != "sid-live" {
		t.Error("turn should run when company is not paused")
	}
}

// TestRunTurnPausedMember (F161): a PAUSED member inbox's scope is CLAIMED by agora
// (Accepts true) and RunTurn bounces "member paused" — it must NOT run a turn and must
// NOT fall to the normal catch-all (mirroring company pause; a reversible member pause
// doesn't reroute a company group to generic bob).
func TestRunTurnPausedMember(t *testing.T) {
	ag := okAgora()
	ag.pausedMembers = map[string]bool{hitScope: true}
	turnp := &fakeTurn{}
	sink := &fakeSink{}
	f := newFlow(ag, fakeToolSet{specs: []contract.ToolSpec{{Name: "send_message"}}},
		fakeSkillSet{}, turnp, &fakeSource{sink: sink})

	if !f.Accepts(contract.TurnWork{Scope: hitScope, Events: []contract.MessageEvent{{Text: "hi", ChatType: contract.ChatGroup}}}) {
		t.Fatal("a paused member scope must be CLAIMED by agora, not left to normal")
	}
	f.RunTurn(context.Background(), contract.TurnWork{
		Sid: "sid-mp", Scope: hitScope, Target: contract.Target{Source: "test"},
		Events: []contract.MessageEvent{{Text: "你好"}},
	})
	if turnp.spec.Sid != "" {
		t.Error("a turn was run for a paused member — must be skipped")
	}
	if !sink.didFin || !strings.Contains(sink.finished, "flow.agora.member_paused") {
		t.Errorf("member-paused notice not sent: didFin=%v finished=%q", sink.didFin, sink.finished)
	}
}

// TestRouteBridgeOutbound: an INTERNAL (worker) event at a bridge scope forwards
// the text to the bound external chat via the source's one-off Send — NO turn run.
func TestRouteBridgeOutbound(t *testing.T) {
	ag := okAgora()
	ag.isBridge = true
	ag.bridgeExt = contract.Target{Source: "telegram", ChatID: "900"}
	ag.bridgeDefScope = "co:acme/alice"

	ext := &fakeSource{sink: &fakeSink{}}
	turnp := &fakeTurn{}
	f := newFlow(ag, nil, nil, turnp, ext)
	f.gw = mapGateway{byName: map[string]contract.Source{"telegram": ext}}

	f.RunTurn(context.Background(), contract.TurnWork{
		Sid:    "sid-out",
		Scope:  hitScope,
		Target: contract.Target{Source: "test"},
		Events: []contract.MessageEvent{{Source: contract.SourceNameInternal, Text: "登录请求：请处理"}},
	})

	if turnp.spec.Sid != "" {
		t.Error("a turn was run at a bridge scope (outbound) — must not happen")
	}
	if len(ext.sent) != 1 {
		t.Fatalf("outbound Send count = %d, want 1", len(ext.sent))
	}
	if ext.sent[0].t.ChatID != "900" || ext.sent[0].text != "登录请求：请处理" {
		t.Errorf("outbound Send = %+v, want chat 900 / the worker text", ext.sent[0])
	}
}

// TestRouteBridgeInbound: an EXTERNAL (human) event at a bridge scope re-emits an
// internal event to the bridge's default downstream inbox — NO turn run.
func TestRouteBridgeInbound(t *testing.T) {
	ag := okAgora()
	ag.isBridge = true
	ag.bridgeExt = contract.Target{Source: "telegram", ChatID: "900"}
	ag.bridgeDefScope = "co:acme/alice"

	emitter := &fakeEmitter{fakeSource: fakeSource{sink: &fakeSink{}}}
	turnp := &fakeTurn{}
	f := newFlow(ag, nil, nil, turnp, emitter)
	f.gw = mapGateway{byName: map[string]contract.Source{contract.SourceNameInternal: emitter}}

	f.RunTurn(context.Background(), contract.TurnWork{
		Sid:    "sid-in",
		Scope:  hitScope,
		Target: contract.Target{Source: "telegram"},
		Events: []contract.MessageEvent{{Source: "telegram", Text: "已登录，继续", UserID: "u7", StableUID: "tg7"}},
	})

	if turnp.spec.Sid != "" {
		t.Error("a turn was run at a bridge scope (inbound) — must not happen")
	}
	if len(emitter.emitted) != 1 {
		t.Fatalf("inbound re-emit count = %d, want 1", len(emitter.emitted))
	}
	if emitter.emitted[0].ChatID != "co:acme/alice" || emitter.emitted[0].Text != "已登录，继续" {
		t.Errorf("inbound re-emit = %+v, want target co:acme/alice / the human text", emitter.emitted[0])
	}
	// The operator's PLATFORM billing handle rides across the re-emit (the bridge
	// source forces Source="internal", which would otherwise bill a ghost
	// ("internal", uid) row): family from the source, uid from the stable uid.
	if got := emitter.emitted[0].StableUID; got != "bridge-origin:telegram:tg7" {
		t.Errorf("re-emit StableUID = %q, want bridge-origin:telegram:tg7", got)
	}
}

// TestBridgeRelayedTurnBillsOperator: a member turn woken by an all-internal batch
// carrying the bridge-origin frame bills the OPERATOR's platform handle, not the
// ghost ("internal", uid) fallback; a plain internal dispatch (no frame) keeps the
// Events[0] fallback.
func TestBridgeRelayedTurnBillsOperator(t *testing.T) {
	turnp := &fakeTurn{}
	f := newFlow(okAgora(), nil, nil, turnp, &fakeSource{sink: &fakeSink{}})
	f.RunTurn(context.Background(), contract.TurnWork{
		Sid: "sid-bill", Scope: hitScope, Target: contract.Target{Source: "test"},
		Events: []contract.MessageEvent{{
			Source: contract.SourceNameInternal, Text: "已登录，继续",
			UserID: "u7", StableUID: "bridge-origin:telegram:tg7",
		}},
	})
	if turnp.spec.Sid != "sid-bill" {
		t.Fatal("relayed member turn did not run")
	}
	if s, uid, ok := contract.ConsumerFrom(turnp.ctx); !ok || s != "telegram" || uid != "tg7" {
		t.Errorf("consumer = (%q,%q,%v), want (telegram,tg7,true) — the operator, not a ghost internal handle", s, uid, ok)
	}

	// Control: no origin frame → the plain Events[0] fallback stays.
	turnp2 := &fakeTurn{}
	f2 := newFlow(okAgora(), nil, nil, turnp2, &fakeSource{sink: &fakeSink{}})
	f2.RunTurn(context.Background(), contract.TurnWork{
		Sid: "sid-plain", Scope: hitScope, Target: contract.Target{Source: "test"},
		Events: []contract.MessageEvent{{Source: contract.SourceNameInternal, Text: "干活", UserID: "u7"}},
	})
	if s, uid, ok := contract.ConsumerFrom(turnp2.ctx); !ok || s != "internal" || uid != "u7" {
		t.Errorf("plain internal consumer = (%q,%q,%v), want (internal,u7,true)", s, uid, ok)
	}
}

// TestRouteBridgeInboundAttachments: an external event with PLACED attachments at a
// bridge scope gets its files copied into the downstream scope's space (same
// space-relative path, so the text's [本轮附件] lines stay dereferenceable) and the
// re-emitted event carries Attachments + Lang + UserID.
func TestRouteBridgeInboundAttachments(t *testing.T) {
	ag := okAgora()
	ag.isBridge = true
	ag.bridgeDefScope = "co:acme/alice"

	bridgeCh := &memFileChannel{files: map[string][]byte{"inbox/photo.jpg": []byte("jpegbytes")}}
	defCh := &memFileChannel{files: map[string][]byte{}}
	w := spaceWarrant{chans: map[string]contract.FileChannel{
		hitScope:        bridgeCh,
		"co:acme/alice": defCh,
	}}

	emitter := &fakeEmitter{fakeSource: fakeSource{sink: &fakeSink{}}}
	turnp := &fakeTurn{}
	f := newFlowWithWarrant(ag, w, turnp, emitter)
	f.gw = mapGateway{byName: map[string]contract.Source{contract.SourceNameInternal: emitter}}

	f.RunTurn(context.Background(), contract.TurnWork{
		Sid:    "sid-in-att",
		Scope:  hitScope,
		Target: contract.Target{Source: "telegram"},
		Events: []contract.MessageEvent{{
			Source: "telegram", Text: "看下这张", Lang: "zh", UserID: "u7", UserName: "老板",
			Attachments: []contract.Attachment{{Kind: "image", MIME: "image/jpeg", FileName: "photo.jpg", Path: "inbox/photo.jpg"}},
		}},
	})

	if turnp.spec.Sid != "" {
		t.Error("a turn was run at a bridge scope (inbound) — must not happen")
	}
	if len(emitter.emitted) != 1 {
		t.Fatalf("re-emit count = %d, want 1", len(emitter.emitted))
	}
	if got := string(defCh.files["inbox/photo.jpg"]); got != "jpegbytes" {
		t.Errorf("attachment not copied into the downstream space (got %q)", got)
	}
	ev := emitter.emitted[0]
	if len(ev.Attachments) != 1 || ev.Attachments[0].Path != "inbox/photo.jpg" {
		t.Errorf("emitted Attachments = %+v, want the relayed inbox path", ev.Attachments)
	}
	if ev.Lang != "zh" || ev.UserID != "u7" {
		t.Errorf("emitted Lang/UserID = %q/%q, want zh/u7", ev.Lang, ev.UserID)
	}
	// Attachments rode the event, so the relayed TEXT must not render the
	// [本轮附件] list too — the downstream member turn re-renders it from
	// Attachments, and keeping both showed every file twice.
	if !strings.Contains(ev.Text, "看下这张") || strings.Contains(ev.Text, "[本轮附件]") {
		t.Errorf("relay text should keep the message but drop the attachment list: %q", ev.Text)
	}
}

// TestRouteBridgeInboundAttachmentCopyFails: a relay copy failure degrades to the
// text-only relay — the event still goes out, without Attachments.
func TestRouteBridgeInboundAttachmentCopyFails(t *testing.T) {
	ag := okAgora()
	ag.isBridge = true
	ag.bridgeDefScope = "co:acme/alice"

	// The bridge space has NO such file → the relay's Read fails.
	w := spaceWarrant{chans: map[string]contract.FileChannel{
		hitScope:        &memFileChannel{files: map[string][]byte{}},
		"co:acme/alice": &memFileChannel{files: map[string][]byte{}},
	}}
	emitter := &fakeEmitter{fakeSource: fakeSource{sink: &fakeSink{}}}
	f := newFlowWithWarrant(ag, w, &fakeTurn{}, emitter)
	f.gw = mapGateway{byName: map[string]contract.Source{contract.SourceNameInternal: emitter}}

	f.RunTurn(context.Background(), contract.TurnWork{
		Sid:   "sid-in-degrade",
		Scope: hitScope,
		Events: []contract.MessageEvent{{
			Source: "telegram", Text: "看下这张",
			Attachments: []contract.Attachment{{Kind: "image", Path: "inbox/gone.jpg"}},
		}},
	})

	if len(emitter.emitted) != 1 {
		t.Fatalf("re-emit count = %d, want 1", len(emitter.emitted))
	}
	if emitter.emitted[0].Attachments != nil {
		t.Errorf("degraded relay must carry no attachments: %+v", emitter.emitted[0].Attachments)
	}
	if emitter.emitted[0].Text == "" {
		t.Error("degraded relay must still carry the text")
	}
	// The degrade keeps the rendered [本轮附件] lines — with no Attachments on the
	// event they are the only record of what the operator sent.
	if !strings.Contains(emitter.emitted[0].Text, "[本轮附件]") {
		t.Errorf("degraded relay text should keep the attachment lines: %q", emitter.emitted[0].Text)
	}
}

// TestFanAskWhoFiltersPaused: the fanned-group "找谁" path probes pause PER MEMBER —
// a paused member is dropped from the roster; only when ALL members are paused does
// the company_paused bounce replace the ask.
func TestFanAskWhoFiltersPaused(t *testing.T) {
	const fanScope = "telegram:group:777"
	ag := okAgora()
	ag.fan = true
	ag.fanMembers = []string{"alice", "bob"}
	ag.pausedSubs = map[string]bool{fanScope + "#alice": true}

	sink := &fakeSink{}
	turnp := &fakeTurn{}
	f := newFlow(ag, nil, nil, turnp, &fakeSource{sink: sink})
	work := contract.TurnWork{
		Sid: "sid-fan", Scope: fanScope,
		Target: contract.Target{Source: "test"},
		Events: []contract.MessageEvent{{ChatType: contract.ChatGroup, Text: "谁来处理", Lang: "zh"}},
	}
	f.RunTurn(context.Background(), work)

	if turnp.spec.Sid != "" {
		t.Error("the fan ask path must not run a turn")
	}
	if !sink.didFin {
		t.Fatal("ask-who reply not sent")
	}
	if strings.Contains(sink.finished, "^alice") {
		t.Errorf("paused member listed in roster: %q", sink.finished)
	}
	if !strings.Contains(sink.finished, "^bob") {
		t.Errorf("active member missing from roster: %q", sink.finished)
	}

	// ALL members paused → the company_paused bounce (i18n key, no catalog in tests).
	ag.pausedSubs[fanScope+"#bob"] = true
	sink2 := &fakeSink{}
	f2 := newFlow(ag, nil, nil, &fakeTurn{}, &fakeSource{sink: sink2})
	f2.RunTurn(context.Background(), work)
	if !sink2.didFin || !strings.Contains(sink2.finished, "flow.agora.company_paused") {
		t.Errorf("all-paused fan should bounce company_paused: %q", sink2.finished)
	}

	// An EMPTY roster (every wired member inbox-paused — RouteGroupScope filtered
	// them all out) bounces company_paused too, never an empty "找谁".
	agEmpty := okAgora()
	agEmpty.fan = true
	agEmpty.fanMembers = nil
	sink3 := &fakeSink{}
	f3 := newFlow(agEmpty, nil, nil, &fakeTurn{}, &fakeSource{sink: sink3})
	f3.RunTurn(context.Background(), work)
	if !sink3.didFin || !strings.Contains(sink3.finished, "flow.agora.company_paused") {
		t.Errorf("empty-roster fan should bounce company_paused: %q", sink3.finished)
	}
}

// TestFanAskWhoSubScopeProbesBase: the ask-who pause probe keys member sub-scopes
// off the BASE group scope even when work.Scope is already a member sub-scope (a
// ^name pick that failed the resolver's active gate, or a stale reply-hit key) —
// a raw work.Scope+"#"+member concat would probe the never-resolving "G#a#b" and
// silently disable the paused-member filter.
func TestFanAskWhoSubScopeProbesBase(t *testing.T) {
	const fanScope = "telegram:group:777"
	ag := okAgora()
	ag.fan = true
	ag.fanMembers = []string{"bob", "carol"}
	ag.pausedSubs = map[string]bool{fanScope + "#carol": true}

	sink := &fakeSink{}
	turnp := &fakeTurn{}
	f := newFlow(ag, nil, nil, turnp, &fakeSource{sink: sink})
	f.RunTurn(context.Background(), contract.TurnWork{
		Sid:    "sid-fan-sub",
		Scope:  fanScope + "#alice", // a member sub-scope that no longer resolves
		Target: contract.Target{Source: "test"},
		Events: []contract.MessageEvent{{ChatType: contract.ChatGroup, Text: "^alice 在吗", Lang: "zh"}},
	})

	if turnp.spec.Sid != "" {
		t.Error("the fan ask path must not run a turn")
	}
	if !sink.didFin {
		t.Fatal("ask-who reply not sent")
	}
	if strings.Contains(sink.finished, "^carol") {
		t.Errorf("paused member listed — the probe used the raw sub-scope base: %q", sink.finished)
	}
	if !strings.Contains(sink.finished, "^bob") {
		t.Errorf("active member missing from roster: %q", sink.finished)
	}
}

// TestDumpPrompt: the /prompt dump includes the agora principal, directory, and
// playbook, plus the agora-authorized tool/skill bags.
func TestDumpPrompt(t *testing.T) {
	f := newFlow(okAgora(), fakeToolSet{specs: []contract.ToolSpec{{Name: "send_message", Description: "发消息"}}},
		fakeSkillSet{infos: []contract.SkillInfo{{Name: "ultrasound", Description: "超声"}}},
		&fakeTurn{}, &fakeSource{sink: &fakeSink{}})

	dump := f.DumpPrompt(context.Background(), contract.TurnWork{
		Scope:  hitScope,
		Events: []contract.MessageEvent{{Text: "你好"}},
	})
	for _, want := range []string{
		"# 系统提示", "你好",
		"你是 小红", "顺意公司", "客服", // member identity card (replaces the default persona)
		"本公司专做超声报告", "张三", // playbook + directory
		"company:acme/role:boss", inboxID, // agora identity
		"send_message", // tool bag
		"ultrasound",   // skill bag
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump missing %q\n---\n%s", want, dump)
		}
	}
	// The member card REPLACED the default identity layer — "你是 bob" must be gone.
	if strings.Contains(dump, "你是 bob") {
		t.Errorf("default bob identity not replaced by member card\n---\n%s", dump)
	}
}

func TestStripLeadingMemberTag(t *testing.T) {
	cases := []struct{ text, member, want string }{
		{"^alice 看下购物车", "alice", "看下购物车"},
		{"  ^alice 看下", "alice", "看下"},
		{"^alice", "alice", ""},
		{"看下 ^alice", "alice", "看下 ^alice"}, // not leading → unchanged
		{"^bob hi", "alice", "^bob hi"},     // different member → unchanged
		{"^张三 来活了", "张三", "来活了"},
		{"^alice, 看下", "alice", "看下"},           // trailing punct on token
		{"^alice帮我查下库存", "alice", "帮我查下库存"},     // F159: no space, CJK glued → prefix-strip
		{"^Alice 看下", "alice", "看下"},            // case-insensitive
		{"^alice\n第二行", "alice", "第二行"},         // newline preserved as separator
		{"#hashtag 不剥", "alice", "#hashtag 不剥"}, // a real hashtag is left alone
	}
	for _, c := range cases {
		if got := stripLeadingMemberTag(c.text, c.member); got != c.want {
			t.Errorf("stripLeadingMemberTag(%q,%q) = %q, want %q", c.text, c.member, got, c.want)
		}
	}
}

// TestDropHiddenSlash locks the addressing-hidden-slash intercept: only an event whose
// "^member" strip REVEALS a leading "/" is dropped (it would land in the model as literal
// command text) — a bare "/cmd" (inbound dispatched it, or worker-generated text) and a
// mid-text "/" stay ordinary turn input.
func TestDropHiddenSlash(t *testing.T) {
	ev := func(text string) contract.MessageEvent { return contract.MessageEvent{Text: text} }
	cases := []struct {
		name    string
		texts   []string
		member  string
		kept    []string
		dropped int
	}{
		{"hidden slash dropped", []string{"^alice /looping"}, "alice", nil, 1},
		{"glued hidden slash dropped", []string{"^alice/session"}, "alice", nil, 1},
		{"bare slash kept (no tag → not hidden)", []string{"/looping"}, "alice", []string{"/looping"}, 0},
		{"mid-text slash kept", []string{"^alice 看下 /looping 是啥"}, "alice", []string{"^alice 看下 /looping 是啥"}, 0},
		{"different member tag kept", []string{"^bob /looping"}, "alice", []string{"^bob /looping"}, 0},
		{"mixed batch: slash dropped, work kept", []string{"^alice /trace", "^alice 看下购物车"}, "alice", []string{"^alice 看下购物车"}, 1},
	}
	for _, c := range cases {
		events := make([]contract.MessageEvent, 0, len(c.texts))
		for _, s := range c.texts {
			events = append(events, ev(s))
		}
		kept, dropped := dropHiddenSlash(events, c.member)
		if dropped != c.dropped {
			t.Errorf("%s: dropped = %d, want %d", c.name, dropped, c.dropped)
		}
		if len(kept) != len(c.kept) {
			t.Fatalf("%s: kept %d events, want %d", c.name, len(kept), len(c.kept))
		}
		for i := range kept {
			if kept[i].Text != c.kept[i] {
				t.Errorf("%s: kept[%d] = %q, want %q (kept events must stay VERBATIM — stripMemberTag runs later)", c.name, i, kept[i].Text, c.kept[i])
			}
		}
	}
}

func TestMemberOfScope(t *testing.T) {
	cases := []struct{ scope, want string }{
		{"telegram:group:777#alice", "alice"},
		{"telegram:group:777", ""},
		{"email:dm:x@y#sales", ""}, // DM alias, not a group member
		{"telegram:dm:200", ""},
	}
	for _, c := range cases {
		if got := memberOfScope(c.scope); got != c.want {
			t.Errorf("memberOfScope(%q) = %q, want %q", c.scope, got, c.want)
		}
	}
}

// TestMemberTagPunctInSync locks this flow's memberTagTrailingPunct (the set
// stripLeadingMemberTag trims) byte-equal to leaf/agora's mirrored copy (the set
// parseMemberTag trims for ROUTING). arch forbids the cross import, so the literal
// is mirrored — this test reads the leaf source and fails on any divergence
// (routing and stripping disagreeing means a token routes but leaks into the
// member's prompt, or strips without ever routing).
// TestIsNameByteInSync locks the isNameByte name-boundary predicate BYTE-IDENTICAL across
// leaf/agora (parseMemberTag) and flow/agora (stripLeadingMemberTag). If they drift, the
// routed member and the stripped text disagree on where a glued `^name` ends — a subtle
// mis-route. Mirrors TestMemberTagPunctInSync for the sibling const.
func TestIsNameByteInSync(t *testing.T) {
	leaf := extractFuncSrc(t, "../../leaf/agora/40-impl.go", "isNameByte")
	flow := extractFuncSrc(t, "10-flow.go", "isNameByte")
	if leaf != flow {
		t.Fatalf("isNameByte diverged across packages — keep BYTE-IDENTICAL:\n leaf/agora:\n%s\n flow/agora:\n%s", leaf, flow)
	}
}

// extractFuncSrc returns the full gofmt'd source text of the top-level func named fn in
// file (signature + body, excluding any doc comment). Both copies are gofmt'd, so
// logically-identical functions compare equal.
func extractFuncSrc(t *testing.T, file, fn string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Name.Name != fn {
			continue
		}
		return string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])
	}
	t.Fatalf("func %s not found in %s — renamed? keep the mirrored pair discoverable", fn, file)
	return ""
}

func TestMemberTagPunctInSync(t *testing.T) {
	const leafFile = "../../leaf/agora/40-impl.go"
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, leafFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", leafFile, err)
	}
	found := ""
	for _, d := range af.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != "memberTagTrailingPunct" || i >= len(vs.Values) {
					continue
				}
				bl, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					t.Fatalf("leaf memberTagTrailingPunct is not a string literal")
				}
				v, uerr := strconv.Unquote(bl.Value)
				if uerr != nil {
					t.Fatalf("unquote leaf memberTagTrailingPunct: %v", uerr)
				}
				found = v
			}
		}
	}
	if found == "" {
		t.Fatalf("memberTagTrailingPunct const not found in %s — renamed? keep the mirrored pair discoverable", leafFile)
	}
	if found != memberTagTrailingPunct {
		t.Fatalf("member-tag trailing-punct sets diverged — keep BYTE-IDENTICAL:\n leaf/agora: %q\n flow/agora: %q", found, memberTagTrailingPunct)
	}
}

// anchorScope strips the virtual-group "#member" suffix (reply-routing keys under the
// bare group scope) and is a no-op for every other scope — including a DM "#thread".
func TestAnchorScope(t *testing.T) {
	cases := []struct{ in, want string }{
		{"telegram:group:9#alice", "telegram:group:9"}, // strip member suffix
		{"telegram:group:9", "telegram:group:9"},       // no suffix → unchanged
		{"telegram:dm:1", "telegram:dm:1"},             // DM → unchanged
		{"telegram:dm:1#42", "telegram:dm:1#42"},       // DM "#thread" is NOT a group member → unchanged
		{"co:acme/bob", "co:acme/bob"},                 // agora inbox own-scope → unchanged
	}
	for _, c := range cases {
		if got := anchorScope(c.in); got != c.want {
			t.Errorf("anchorScope(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
