package turn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agentbob/contract"
)

// --- fakes -------------------------------------------------------------------

// rubricSkills is a minimal contract.SkillSet for the arming test.
type rubricSkills struct{ skills map[string]contract.Skill }

func (s rubricSkills) List() []contract.SkillInfo {
	out := make([]contract.SkillInfo, 0, len(s.skills))
	for _, sk := range s.skills {
		out = append(out, sk.SkillInfo)
	}
	return out
}
func (s rubricSkills) Get(name string) (contract.Skill, bool) {
	sk, ok := s.skills[name]
	return sk, ok
}
func (s rubricSkills) Read(string) (string, bool) { return "", false }
func (s rubricSkills) Files(string) ([]contract.SkillFile, error) {
	return nil, nil
}

// rubricJudgePool answers the judge Chat with a scripted verdict (or an error, to
// exercise the fail-open path). Embeds *fakePool for the unused methods.
type rubricJudgePool struct {
	*fakePool
	verdict  string
	err      error
	captured *[]contract.Message // when non-nil, records the messages sent to the judge
}

func (p rubricJudgePool) Chat(_ context.Context, _ contract.ModelRequest, msgs []contract.Message) (contract.ChatResponse, error) {
	if p.captured != nil {
		*p.captured = msgs
	}
	if p.err != nil {
		return contract.ChatResponse{}, p.err
	}
	return contract.ChatResponse{Content: p.verdict}, nil
}

// rubricStore is a memory MessageStore: GetReplay returns a canned window, every
// other write is a no-op except AppendMessage (recorded so the declare-failure path
// can be asserted).
type rubricStore struct {
	replay   []contract.Message
	appended []string
}

func (s *rubricStore) GetReplay(context.Context, string, int) ([]contract.Message, error) {
	return s.replay, nil
}
func (s *rubricStore) GetMessages(context.Context, string, int) ([]contract.Message, error) {
	return nil, nil
}
func (s *rubricStore) AppendMessage(_ context.Context, _, _, content string, _ contract.MsgAuthor) error {
	s.appended = append(s.appended, content)
	return nil
}
func (s *rubricStore) AppendUserMsgs(_ context.Context, _ string, msgs []contract.UserMsg) error {
	for _, m := range msgs {
		s.appended = append(s.appended, m.Text)
	}
	return nil
}
func (s *rubricStore) AppendAssistantCalls(context.Context, string, string, []contract.ToolCall, contract.MsgAuthor) error {
	return nil
}
func (s *rubricStore) AppendToolResult(context.Context, string, string, string, string) error {
	return nil
}
func (s *rubricStore) AppendCompactionBatch(context.Context, string, string, []contract.Message) error {
	return nil
}
func (s *rubricStore) DeleteSessionMessages(context.Context, string) error { return nil }
func (s *rubricStore) RecentAttachments(context.Context, string, int) ([]contract.Attachment, error) {
	return nil, nil
}

// --- pure verdict parsing ----------------------------------------------------

func TestParseRubricVerdict(t *testing.T) {
	cases := []struct {
		raw      string
		wantOK   bool
		wantPass bool
		wantWhy  string
	}{
		{`{"pass": true, "reason": ""}`, true, true, ""},
		{`{"pass": false, "reason": "占位符被填成数值"}`, true, false, "占位符被填成数值"},
		// code-fence + preamble around the object is tolerated (first { .. last }).
		{"```json\n{\"pass\": false, \"reason\": \"编造器官\"}\n```", true, false, "编造器官"},
		{"判定如下：{\"pass\": true, \"reason\": \"\"}", true, true, ""},
		// no JSON object → not ok (caller fails open).
		{"我觉得还行吧", false, false, ""},
		{"", false, false, ""},
	}
	for _, tc := range cases {
		v, ok := parseRubricVerdict(tc.raw)
		if ok != tc.wantOK || v.Pass != tc.wantPass || v.Reason != tc.wantWhy {
			t.Errorf("parseRubricVerdict(%q) = (%+v, %v); want pass=%v reason=%q ok=%v",
				tc.raw, v, ok, tc.wantPass, tc.wantWhy, tc.wantOK)
		}
	}
}

// --- arming ------------------------------------------------------------------

func TestArmRubricOnSkillView(t *testing.T) {
	skills := rubricSkills{skills: map[string]contract.Skill{
		"us": {SkillInfo: contract.SkillInfo{Name: "us"}, Rubric: "RULE"},
		"nr": {SkillInfo: contract.SkillInfo{Name: "nr"}}, // no rubric
	}}
	spec := contract.TurnSpec{Sid: "s", Skills: skills}
	c := &core{}

	// Viewing a rubric-bearing skill arms ONLY that skill.
	st := newTurnState()
	c.armRubricOnSkillView(st, spec, contract.ToolCall{Name: "skill_view", Arguments: `{"name":"us"}`}, contract.ToolResult{OK: true})
	if st.rubricArmed["us"] != "RULE" {
		t.Fatalf("viewing a rubric skill should arm it, got %v", st.rubricArmed)
	}

	// Viewing a rubric-less skill does not arm.
	st = newTurnState()
	c.armRubricOnSkillView(st, spec, contract.ToolCall{Name: "skill_view", Arguments: `{"name":"nr"}`}, contract.ToolResult{OK: true})
	if len(st.rubricArmed) != 0 {
		t.Fatalf("a rubric-less skill must not arm, got %v", st.rubricArmed)
	}

	// A PLAIN OCR (no skill viewed) must NOT arm — the core of the fix: pure OCR is
	// not a rubric-gated product just because a rubric-bearing skill is in scope.
	st = newTurnState()
	c.armRubricOnSkillView(st, spec, contract.ToolCall{Name: "ocr", Arguments: `{}`}, contract.ToolResult{OK: true})
	if len(st.rubricArmed) != 0 {
		t.Fatalf("plain ocr must not arm a rubric, got %v", st.rubricArmed)
	}

	// A failed skill_view (unknown/unauthorized name) does not arm.
	st = newTurnState()
	c.armRubricOnSkillView(st, spec, contract.ToolCall{Name: "skill_view", Arguments: `{"name":"us"}`}, contract.ToolResult{OK: false})
	if len(st.rubricArmed) != 0 {
		t.Fatalf("a failed skill_view must not arm, got %v", st.rubricArmed)
	}
}

// --- the gate ----------------------------------------------------------------

func newGateCore(verdict string, err error) (*core, *rubricStore, *recSink) {
	store := &rubricStore{replay: []contract.Message{
		{Role: "user", Content: "提取超声报告"},
		{Role: "tool", ToolName: "ocr", Content: "肝、胆、胰、脾未见明显异常 脾厚径____mm"},
	}}
	c := &core{store: store, pool: rubricJudgePool{fakePool: &fakePool{}, verdict: verdict, err: err}}
	return c, store, &recSink{}
}

func TestRubricGate_Pass(t *testing.T) {
	c, _, sink := newGateCore(`{"pass":true}`, nil)
	st := newTurnState()
	st.rubricArmed = map[string]string{"us": "RULE"}
	p := sysPrompt("sys")
	spec := contract.TurnSpec{Sid: "s", Prompt: p, Sink: sink}

	if rr := c.rubricGate(context.Background(), spec, st, `[{"IMPRESSION":"...","DESCRIPTION":"..."}]`, nil); rr != nil {
		t.Fatalf("PASS must return nil (proceed to finalize), got %+v", rr)
	}
	if st.rubricRetries != 0 {
		t.Fatalf("PASS must not count a retry, got %d", st.rubricRetries)
	}
}

func TestRubricGate_FailThenRedo(t *testing.T) {
	c, _, sink := newGateCore(`{"pass":false,"reason":"占位符被填成真实数值"}`, nil)
	st := newTurnState()
	st.rubricArmed = map[string]string{"us": "RULE"}
	p := sysPrompt("sys")
	spec := contract.TurnSpec{Sid: "s", Prompt: p, Sink: sink}

	rr := c.rubricGate(context.Background(), spec, st, "bad output", nil)
	if rr == nil || rr.kind != roundContinue {
		t.Fatalf("FAIL with retries left must loop (roundContinue), got %+v", rr)
	}
	if st.rubricRetries != 1 {
		t.Fatalf("retry count = %d, want 1", st.rubricRetries)
	}
	if !rr.resetBudget {
		t.Fatalf("a redo must request a work-budget reset so it isn't pre-empted by the stale/iter budget")
	}
	if !strings.Contains(p.Build(), "未通过校验") || !strings.Contains(p.Build(), "占位符被填成真实数值") {
		t.Fatalf("redo critique must be injected into the prompt, got:\n%s", p.Build())
	}
	if sink.done {
		t.Fatalf("a redo must NOT finalize the sink yet")
	}
}

func TestRubricGate_FailPastCapDeclares(t *testing.T) {
	c, store, sink := newGateCore(`{"pass":false,"reason":"编造了 OCR 没有的器官"}`, nil)
	st := newTurnState()
	st.rubricArmed = map[string]string{"us": "RULE"}
	st.rubricRetries = maxRubricRetries // already spent the redos
	p := sysPrompt("sys")
	spec := contract.TurnSpec{Sid: "s", Prompt: p, Sink: sink}

	rr := c.rubricGate(context.Background(), spec, st, "bad output", nil)
	if rr == nil || rr.kind != roundExit {
		t.Fatalf("past cap must exit, got %+v", rr)
	}
	if rr.exit == nil || rr.exit.state != exitRubricFailed || rr.exit.state.outcome() != contract.OutcomeDegraded {
		t.Fatalf("must declare exitRubricFailed (→ OutcomeDegraded), got %+v", rr.exit)
	}
	// The REAL flawed product (not the apology in detail) must be carried for the
	// failure learner (L-rubric-B1).
	if rr.exit.rejected != "bad output" {
		t.Fatalf("rejected product must be carried for the learner, got %q", rr.exit.rejected)
	}
	if strings.Contains(rr.exit.detail, "bad output") {
		t.Fatalf("detail must be the stock apology, not the rejected product, got %q", rr.exit.detail)
	}
	if !sink.done || !strings.Contains(sink.finished, "未能通过校验") {
		t.Fatalf("must finalize an honest failure notice, got finished=%q done=%v", sink.finished, sink.done)
	}
	if len(store.appended) == 0 || !strings.Contains(store.appended[len(store.appended)-1], "未能通过校验") {
		t.Fatalf("failure notice must be persisted, got %v", store.appended)
	}
}

// seqSink records the ORDER of sink events, so a test can assert a trace lands
// BEFORE Finish (after Finish the streamsink flush loop is gone and a trace only
// ever sits in an undrained buffer).
type seqSink struct{ events []string }

func (s *seqSink) ContentDelta(t string) { s.events = append(s.events, "content:"+t) }
func (s *seqSink) TraceDelta(t string)   { s.events = append(s.events, "trace:"+t) }
func (s *seqSink) Finish(full string) error {
	s.events = append(s.events, "finish:"+full)
	return nil
}
func (s *seqSink) SendFile(string, string) error { return nil }
func (s *seqSink) LastSent() string              { return "" }

// TestRubricGate_ExhaustedTraceBeforeFinish (F104): the "retries exhausted" terminal
// trace must be emitted BEFORE finalizeTurn's Finish — Finish closes the sink's flush
// loop after its last trace flush, so a trace issued after it is lost forever on
// every streamsink channel.
func TestRubricGate_ExhaustedTraceBeforeFinish(t *testing.T) {
	store := &rubricStore{replay: []contract.Message{{Role: "user", Content: "提取"}}}
	c := &core{store: store, pool: rubricJudgePool{fakePool: &fakePool{}, verdict: `{"pass":false,"reason":"编造"}`}}
	st := newTurnState()
	st.rubricArmed = map[string]string{"us": "RULE"}
	st.rubricRetries = maxRubricRetries // redos already spent
	sink := &seqSink{}
	spec := contract.TurnSpec{Sid: "s", Prompt: sysPrompt("sys"), Sink: sink}

	rr := c.rubricGate(context.Background(), spec, st, "bad output", nil)
	if rr == nil || rr.kind != roundExit {
		t.Fatalf("past cap must exit, got %+v", rr)
	}
	traceAt, finishAt := -1, -1
	for i, ev := range sink.events {
		if strings.HasPrefix(ev, "trace:") && strings.Contains(ev, "重试用尽") {
			traceAt = i
		}
		if strings.HasPrefix(ev, "finish:") {
			finishAt = i
		}
	}
	if traceAt < 0 || finishAt < 0 {
		t.Fatalf("missing terminal trace or finish, events=%v", sink.events)
	}
	if traceAt > finishAt {
		t.Fatalf("terminal trace emitted AFTER Finish — it never reaches a streamsink channel; events=%v", sink.events)
	}
}

// The user's ask must reach the judge as its OWN section even when a single tool result
// (e.g. an OCR transcript) alone exceeds the tail-capped evidence budget — otherwise the
// judge grades a product with no idea what was requested (L-rubric-D1).
func TestJudgeRubric_FeedsUserAskUnderHugeToolResult(t *testing.T) {
	var captured []contract.Message
	store := &rubricStore{replay: []contract.Message{
		{Role: "user", Content: "ASK_MARKER 把这张超声报告补全"},
		{Role: "tool", ToolName: "ocr", Content: strings.Repeat("X", rubricEvidenceCap+5000)}, // tail-cap drops the user head
	}}
	c := &core{store: store, pool: rubricJudgePool{fakePool: &fakePool{}, verdict: `{"pass":true}`, captured: &captured}}
	st := newTurnState()
	st.rubricArmed = map[string]string{"us": "RULE"}
	spec := contract.TurnSpec{Sid: "s", UserText: "ASK_MARKER 把这张超声报告补全"}

	c.judgeRubric(context.Background(), spec, st, "some product", nil)

	var prompt string
	for _, m := range captured {
		if m.Role == "user" {
			prompt = m.Content
		}
	}
	if !strings.Contains(prompt, "## 用户的请求") {
		t.Fatal("judge prompt missing the dedicated user-ask section")
	}
	if !strings.Contains(prompt, "ASK_MARKER") {
		t.Fatal("user ask dropped from the judge prompt under a large tool result (L-rubric-D1)")
	}
}

func TestRubricGate_JudgeUnavailableFailsOpen(t *testing.T) {
	c, _, sink := newGateCore("", errors.New("no model tagged judge"))
	st := newTurnState()
	st.rubricArmed = map[string]string{"us": "RULE"}
	spec := contract.TurnSpec{Sid: "s", Prompt: sysPrompt("sys"), Sink: sink}

	if rr := c.rubricGate(context.Background(), spec, st, "output", nil); rr != nil {
		t.Fatalf("judge error must fail OPEN (nil → proceed), got %+v", rr)
	}
}

// TestRunRound_RubricArmedSuppressesStreaming locks the gate's core invariant: a
// rubric-armed turn must NOT stream its product live (the user would see the
// unjudged draft), yet must still deliver it in one Finish once the gate passes.
// fakePool.Chat returns "" → the judge fails OPEN → the product is delivered.
func TestRunRound_RubricArmedSuppressesStreaming(t *testing.T) {
	store := &rubricStore{replay: []contract.Message{{Role: "user", Content: "提取报告"}}}
	pool := &fakePool{reply: "产出JSON"}
	c := &core{store: store, pool: pool}
	sink := &recSink{}
	st := newTurnState()
	st.rubricArmed = map[string]string{"us": "RULE"}
	spec := contract.TurnSpec{Sid: "s", Prompt: sysPrompt("sys"), Sink: sink}

	rr := c.runRound(context.Background(), spec, st, 0, nil)
	if rr.kind != roundFinal {
		t.Fatalf("expected roundFinal, got %+v", rr)
	}
	if len(sink.deltas) != 0 {
		t.Fatalf("rubric-armed turn must NOT stream live deltas, got %v", sink.deltas)
	}
	if sink.finished != "产出JSON" {
		t.Fatalf("the product must still be delivered via Finish, got %q", sink.finished)
	}
}

// TestRunRound_UnarmedStreamsLive is the contrast: with no rubric armed, the
// product streams live as usual (the suppression is scoped to armed turns only).
func TestRunRound_UnarmedStreamsLive(t *testing.T) {
	store := &rubricStore{replay: []contract.Message{{Role: "user", Content: "你好"}}}
	pool := &fakePool{reply: "回复"}
	c := &core{store: store, pool: pool}
	sink := &recSink{}
	st := newTurnState() // not armed

	rr := c.runRound(context.Background(), contract.TurnSpec{Sid: "s", Prompt: sysPrompt("sys"), Sink: sink}, st, 0, nil)
	if rr.kind != roundFinal {
		t.Fatalf("expected roundFinal, got %+v", rr)
	}
	if len(sink.deltas) == 0 {
		t.Fatalf("an unarmed turn must stream live deltas")
	}
}

// rubricRedoRunPool scripts the model's per-round output (a skill_view tool call to
// arm, then products) and ALWAYS fails the judge — so a Run drives arm → product →
// rubric fail → redo, repeatedly, until the retry cap.
type rubricRedoRunPool struct {
	*fakePool
	resps []contract.ChatResponse
	i     int
}

func (p *rubricRedoRunPool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	r := contract.ChatResponse{Content: "产出"} // after the scripted rounds, keep emitting a product
	if p.i < len(p.resps) {
		r = p.resps[p.i]
		p.i++
	}
	accum := &contract.StreamAccumulator{Text: r.Content, ToolCalls: r.ToolCalls}
	_ = w(contract.StreamEvent{Text: r.Content}, accum)
	_ = w(contract.StreamEvent{Done: true}, accum)
	return r, nil
}

func (*rubricRedoRunPool) Chat(context.Context, contract.ModelRequest, []contract.Message) (contract.ChatResponse, error) {
	return contract.ChatResponse{Content: `{"pass":false,"reason":"占位符被填成数值"}`}, nil // judge: always fail
}

// TestRun_RubricRedoResetsBudget locks the fix: a rubric redo grants a fresh work
// budget, so the gate reaches its retry cap and declares an honest failure instead of
// being pre-empted by a tight iter budget into a generic salvage line. With IterCap=2
// (base=2) and NO reset, the hard cap would end the turn (exitIterCap → salvage) at
// iter 2, before the rubric's 3 retries — so this test fails if the reset is removed.
func TestRun_RubricRedoResetsBudget(t *testing.T) {
	store := newSubStore()
	skills := rubricSkills{skills: map[string]contract.Skill{
		"us": {SkillInfo: contract.SkillInfo{Name: "us"}, Rubric: "RULE"},
	}}
	set := newTestSet(testTool{name: "skill_view", run: func(json.RawMessage) contract.ToolResult {
		return contract.OKResult("viewed")
	}})
	pool := &rubricRedoRunPool{fakePool: &fakePool{}, resps: []contract.ChatResponse{
		{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "skill_view", Arguments: `{"name":"us"}`}}},
	}}
	c := &core{store: store, pool: pool}
	sink := &recSink{}
	spec := contract.TurnSpec{Sid: "s", Prompt: sysPrompt("sys"), UserText: "提取", Sink: sink, Tools: set, Skills: skills, IterCap: 2}

	res := c.Run(context.Background(), spec)

	if res.Outcome != contract.OutcomeDegraded {
		t.Fatalf("expected a degraded (rubric-failed) outcome, got %+v", res)
	}
	if !strings.Contains(res.Reply, "未能通过校验") {
		t.Fatalf("expected the rubric-fail notice (retry cap reached), got salvage/other: %q", res.Reply)
	}
}

// TestExitStateOutcome pins the surfaced-outcome taxonomy (D22/D23 core): a tool exit is
// a deliberate process reply; cancel is its own class; every give-up (iter-cap / degenerate
// / stuck / domain → salvage, and rubric-failed) is degraded.
func TestExitStateOutcome(t *testing.T) {
	cases := map[exitState]contract.TurnOutcome{
		exitUnset:         contract.OutcomeDegraded, // a forgotten state degrades loudly, never a silent cancel
		exitToolRequested: contract.OutcomeProcess,
		exitCancelled:     contract.OutcomeCancelled,
		exitRubricFailed:  contract.OutcomeDegraded,
		exitIterCap:       contract.OutcomeDegraded,
		exitDegenSalvage:  contract.OutcomeDegraded,
		exitStuckLoop:     contract.OutcomeDegraded,
		exitDomainStuck:   contract.OutcomeDegraded,
	}
	for s, want := range cases {
		if got := s.outcome(); got != want {
			t.Errorf("exitState(%d).outcome() = %v, want %v", s, got, want)
		}
	}
}

// rubricSalvagePool arms a rubric (skill_view at round 0), then empties out to the iter
// cap → salvage; the salvage tier-1 Chat returns a product, but the JUDGE Chat (Requires
// "judge") fails — exercising D22 (a rubric-armed salvage product is judged + withheld).
type rubricSalvagePool struct {
	*fakePool
	round int
}

func (p *rubricSalvagePool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	var r contract.ChatResponse
	if p.round == 0 {
		r = contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "c1", Name: "skill_view", Arguments: `{"name":"us"}`}}}
	} // rounds ≥1: empty → roundContinue → iter cap
	p.round++
	accum := &contract.StreamAccumulator{Text: r.Content, ToolCalls: r.ToolCalls}
	_ = w(contract.StreamEvent{Text: r.Content}, accum)
	_ = w(contract.StreamEvent{Done: true}, accum)
	return r, nil
}

func (p *rubricSalvagePool) Chat(_ context.Context, req contract.ModelRequest, _ []contract.Message) (contract.ChatResponse, error) {
	for _, tag := range req.Requires {
		if tag == "judge" {
			return contract.ChatResponse{Content: `{"pass":false,"reason":"占位符被填成数值"}`}, nil // judge FAILS
		}
	}
	return contract.ChatResponse{Content: "编造的答案"}, nil // salvage tier-1 product
}

// TestRun_RubricArmedSalvageWithheld (D22): a rubric-armed turn that gives up via salvage
// must NOT deliver an un-judged (possibly fabricated) product — the salvage product is
// judged once, and on a fail it is withheld in favor of the honest rubric-fail notice.
func TestRun_RubricArmedSalvageWithheld(t *testing.T) {
	store := newSubStore()
	skills := rubricSkills{skills: map[string]contract.Skill{
		"us": {SkillInfo: contract.SkillInfo{Name: "us"}, Rubric: "RULE"},
	}}
	set := newTestSet(testTool{name: "skill_view", run: func(json.RawMessage) contract.ToolResult {
		return contract.OKResult("viewed")
	}})
	c := &core{store: store, pool: &rubricSalvagePool{fakePool: &fakePool{}}}
	sink := &recSink{}
	spec := contract.TurnSpec{Sid: "s", Prompt: sysPrompt("sys"), UserText: "提取", Sink: sink, Tools: set, Skills: skills, IterCap: 3}

	res := c.Run(context.Background(), spec)

	if res.Outcome != contract.OutcomeDegraded {
		t.Fatalf("armed salvage → OutcomeDegraded, got %+v", res)
	}
	if strings.Contains(res.Reply, "编造的答案") {
		t.Fatalf("a rubric-FAILED salvage product must be WITHHELD, not delivered: %q", res.Reply)
	}
	if !strings.Contains(res.Reply, "未能通过校验") {
		t.Fatalf("withheld salvage must deliver the honest rubric-fail notice: %q", res.Reply)
	}
	// F105: the withheld product must ride RejectedProduct — the failure learner
	// reflects on the REAL flawed output, not the stock apology in Reply.
	if res.RejectedProduct != "编造的答案" {
		t.Fatalf("RejectedProduct=%q, want the withheld tier-1 product for the failure learner", res.RejectedProduct)
	}
}

// --- advisor review gate (docs/advisor.md §4) --------------------------------

// traceHas reports whether any trace line contains sub.
func traceHas(s *recSink, sub string) bool {
	for _, line := range s.trace {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

// reviewFixture builds the state an ARMED review sees: looping-armed, a delivery
// tool already produced something, no rubric.
func reviewFixture(pool contract.ModelPool) (*core, contract.TurnSpec, *turnState, *recSink) {
	sink := &recSink{}
	st := newTurnState()
	st.advisorArmed = true
	st.produced = true // a Delivers/SideEffect tool succeeded — there IS an artifact
	spec := contract.TurnSpec{
		Sid: "rev", UserText: "写报告并发我", Sink: sink, Prompt: sysPrompt("sys"),
		AdvisorCriteria: "声称做过的事必须有据",
	}
	return &core{store: newSubStore(), pool: pool}, spec, st, sink
}

// TestAdvisorReview_ArmingConditions: the gate costs a sub-turn, so it only fires
// on a looping turn that actually produced an artifact — never on a research turn,
// never inside a sub-turn, never when the driver didn't arm it.
func TestAdvisorReview_ArmingConditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*contract.TurnSpec, *turnState)
	}{
		{"driver never armed", func(_ *contract.TurnSpec, st *turnState) { st.advisorArmed = false }},
		{"no artifact produced", func(_ *contract.TurnSpec, st *turnState) { st.produced = false }},
		{"inside a sub-turn", func(spec *contract.TurnSpec, _ *turnState) { spec.IsSub = true }},
	} {
		pool := &advPool{product: `{"pass":false,"reason":"不合格"}`}
		c, spec, st, _ := reviewFixture(pool)
		tc.mutate(&spec, st)
		if rr := c.advisorReviewGate(context.Background(), spec, st, "产出"); rr != nil {
			t.Fatalf("%s: gate returned %+v, want nil (not armed)", tc.name, rr)
		}
		if pool.calls != 0 {
			t.Fatalf("%s: ran %d advisor rounds — an unarmed gate must not cost a sub-turn", tc.name, pool.calls)
		}
	}
}

// TestAdvisorReview_PassShipsWithNoFooter: a pass finalizes the delivery, traces the
// verdict IN FULL, and leaves no concern footer behind.
func TestAdvisorReview_PassShipsWithNoFooter(t *testing.T) {
	pool := &advPool{product: `{"pass":true,"reason":"报告的三节结论都能在第2/4/6轮工具结果里找到"}`}
	c, spec, st, sink := reviewFixture(pool)

	if rr := c.advisorReviewGate(context.Background(), spec, st, "报告已写入并发送"); rr != nil {
		t.Fatalf("gate = %+v, want nil (pass → finalize)", rr)
	}
	if advisorConcernFooter(st) != "" {
		t.Fatalf("a passed review must leave no footer: %q", advisorConcernFooter(st))
	}
	if !traceHas(sink, "第2/4/6轮工具结果") {
		t.Fatalf("trace = %q, want the verdict reason IN FULL", sink.trace)
	}
}

// TestAdvisorReview_RedoThenShipWithConcern: a fail buys a bounded redo with a fresh
// budget; once the redo budget is spent the delivery SHIPS with an honest footer —
// the artifact already exists, so declaring failure would be its own lie.
func TestAdvisorReview_RedoThenShipWithConcern(t *testing.T) {
	pool := &advPool{product: `{"pass":false,"reason":"报告称已发送，但记录里 deliver 那步失败了"}`}
	c, spec, st, _ := reviewFixture(pool)

	for i := 1; i <= maxReviewRetries; i++ {
		rr := c.advisorReviewGate(context.Background(), spec, st, "产出")
		if rr == nil || rr.kind != roundContinue || !rr.resetBudget {
			t.Fatalf("attempt %d: gate = %+v, want a redo with a fresh budget", i, rr)
		}
		if !strings.Contains(spec.Prompt.Build(), "deliver 那步失败") {
			t.Fatalf("attempt %d: the critique never reached the redo layer: %q", i, spec.Prompt.Build())
		}
	}
	// Budget spent → ship, don't fail.
	if rr := c.advisorReviewGate(context.Background(), spec, st, "产出"); rr != nil {
		t.Fatalf("past the redo cap the gate returned %+v, want nil (deliver with a footer)", rr)
	}
	footer := advisorConcernFooter(st)
	if !strings.Contains(footer, "deliver 那步失败") || !strings.Contains(footer, "保留意见") {
		t.Fatalf("footer = %q, want the unresolved objection surfaced to the user", footer)
	}
}

// TestAdvisorReview_FailsOpen: an unavailable or unparseable reviewer never blocks a
// delivery — same posture as the rubric judge (a gate cannot block what it could not
// verify), and the fail-open is traced so a silent pass stays attributable.
func TestAdvisorReview_FailsOpen(t *testing.T) {
	for _, pool := range []*advPool{
		{err: errors.New("no advisor model")},
		{product: "我觉得还行吧"}, // no JSON verdict
	} {
		c, spec, st, sink := reviewFixture(pool)
		if rr := c.advisorReviewGate(context.Background(), spec, st, "产出"); rr != nil {
			t.Fatalf("gate = %+v, want nil (fail-open)", rr)
		}
		if st.reviewRetries != 0 || advisorConcernFooter(st) != "" {
			t.Fatalf("a fail-open must not count as a rejection: retries=%d footer=%q", st.reviewRetries, advisorConcernFooter(st))
		}
		if !traceHas(sink, "放行") {
			t.Fatalf("trace = %q, want the fail-open traced", sink.trace)
		}
	}
}

// TestAdvisorReview_NotStackedOnRubric: when a rubric IS armed the cheap blind judge
// owns acceptance — the delivery must not ALSO be reviewed by the advisor (double
// cost, two verdicts that can contradict). docs/advisor.md §5.
func TestAdvisorReview_NotStackedOnRubric(t *testing.T) {
	pool := &advPool{product: `{"pass":false,"reason":"顾问不该被问到"}`}
	c, spec, st, _ := reviewFixture(pool)
	st.rubricArmed = map[string]string{"报告": "结论必须有引用支撑"}

	// fakePool.Chat returns empty → the blind judge fails open → finalReply proceeds.
	r := c.finalReply(context.Background(), spec, st, contract.ChatResponse{Content: "产出"}, nil, false)
	if r.kind != roundFinal {
		t.Fatalf("round = %+v, want Final (blind judge failed open)", r)
	}
	if pool.calls != 0 {
		t.Fatalf("the advisor ran %d rounds on a rubric-armed delivery — the gates must be either/or", pool.calls)
	}
}

// TestAdvisorReview_ConcernReachesTheUser: the spent-budget path is only honest if
// the objection actually lands in the delivered text — assert through finalReply,
// not just the footer helper. Also covers a bare {"pass":false} with no reason: the
// user must still be told the delivery is doubted.
func TestAdvisorReview_ConcernReachesTheUser(t *testing.T) {
	pool := &advPool{product: `{"pass":false}`} // verdict without a reason
	c, spec, st, sink := reviewFixture(pool)
	st.reviewRetries = maxReviewRetries // budget already spent → next fail ships

	r := c.finalReply(context.Background(), spec, st, contract.ChatResponse{Content: "报告已交付"}, nil, false)
	if r.kind != roundFinal {
		t.Fatalf("round = %+v, want Final (ship, don't declare failure — the artifact exists)", r)
	}
	if !strings.Contains(sink.finished, "报告已交付") || !strings.Contains(sink.finished, "复核保留意见") {
		t.Fatalf("delivered text = %q, want the product PLUS the reviewer's unresolved objection", sink.finished)
	}
}

// TestAdvisorReview_PartialConsultIsNoVerdict: a reviewer sub-turn that ran out of
// rounds returns its own salvage apology, not a verdict — it must fail OPEN, never
// be parsed as a judgement (docs/advisor.md §4).
func TestAdvisorReview_PartialConsultIsNoVerdict(t *testing.T) {
	pool := &advPool{product: "本次没能给出完整结果"} // partial-shaped prose, no JSON
	c, spec, st, _ := reviewFixture(pool)

	if rr := c.advisorReviewGate(context.Background(), spec, st, "产出"); rr != nil {
		t.Fatalf("gate = %+v, want nil (unparseable → fail-open)", rr)
	}
	if st.reviewRetries != 0 {
		t.Fatalf("reviewRetries=%d — a non-verdict must not count as a rejection", st.reviewRetries)
	}
}
