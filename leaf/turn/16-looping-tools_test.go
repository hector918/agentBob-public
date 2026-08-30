package turn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"agentbob/contract"
)

// advFakeHistory serves a fixed conversation to the history tool.
type advFakeHistory struct{ msgs []contract.Message }

func (f advFakeHistory) Messages(context.Context, int) ([]contract.Message, error) {
	return f.msgs, nil
}

// advPool scripts the advisor sub-turn's rounds: every round returns `product`
// (or fails when err is set) and calls is the consult count.
type advPool struct {
	fakePool
	product string
	err     error
	calls   int
	gotMsgs []contract.Message
}

func (p *advPool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, msgs []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	p.calls++
	p.gotMsgs = msgs
	if p.err != nil {
		return contract.ChatResponse{}, p.err
	}
	accum := &contract.StreamAccumulator{Text: p.product}
	_ = w(contract.StreamEvent{Text: p.product}, accum)
	_ = w(contract.StreamEvent{Done: true}, accum)
	return contract.ChatResponse{Content: p.product}, nil
}

// advFixture builds a core + spec + state for the harness-triggered advisor. The
// progress watermarks stay at -1 (never progressed), so stale == iter+1.
func advFixture(pool contract.ModelPool) (*core, contract.TurnSpec, *turnState, *recSink) {
	sink := &recSink{}
	spec := contract.TurnSpec{
		Sid: "adv", UserText: "把三张图汇总成报告", Sink: sink, Prompt: sysPrompt("sys"),
		AdvisorCriteria: "结论必须列出至少一个反例",
	}
	return &core{store: newSubStore(), pool: pool}, spec, newTurnState(), sink
}

// TestAdvisorDiagnose_FiresExactlyOnStale6: the consult is the HARNESS's call, on
// the staleness axis — nothing at stale 5, one consult at exactly advisorStale, and
// no re-fire while the same stall deepens (the diagnosis already rides the layer).
func TestAdvisorDiagnose_FiresExactlyOnStale6(t *testing.T) {
	pool := &advPool{product: "图2 是 HEIC，先转码再 ocr；另两张的结论已够写汇总"}
	c, spec, st, sink := advFixture(pool)

	c.maybeAdvisorDiagnose(context.Background(), spec, st, advisorStale-2) // stale 5
	if pool.calls != 0 {
		t.Fatalf("consulted at stale %d — must wait for %d", advisorStale-1, advisorStale)
	}
	c.maybeAdvisorDiagnose(context.Background(), spec, st, advisorStale-1) // stale 6
	if pool.calls != 1 || st.advisorDiagnoses != 1 {
		t.Fatalf("calls=%d diagnoses=%d, want 1/1 at stale %d", pool.calls, st.advisorDiagnoses, advisorStale)
	}
	if built := spec.Prompt.Build(); !strings.Contains(built, "先转码再 ocr") || !strings.Contains(built, "不要引用进回复") {
		t.Fatalf("diagnosis not parked on the prompt layer with its 内部 marker: %q", built)
	}
	if len(sink.trace) == 0 || !strings.Contains(sink.trace[0], "顾问诊断") || !strings.Contains(sink.trace[0], "先转码再 ocr") {
		t.Fatalf("trace = %q, want the diagnosis IN FULL", sink.trace)
	}
	c.maybeAdvisorDiagnose(context.Background(), spec, st, advisorStale) // stale 7 — same episode
	if pool.calls != 1 {
		t.Fatalf("calls=%d — a deepening stall must not re-consult (one per episode)", pool.calls)
	}
}

// TestAdvisorDiagnose_TaskCarriesHarnessFactsOnly: the brief hands the advisor what
// the MODEL cannot see (staleness, failures since progress) and nothing else — the
// trajectory it pulls on its own, and the acceptance standard is the REVIEWER's alone
// (a diagnosis is quoted verbatim into the working model's prompt, so criteria here
// would leak the grading sheet back to the graded).
func TestAdvisorDiagnose_TaskCarriesHarnessFactsOnly(t *testing.T) {
	pool := &advPool{product: "换条路"}
	c, spec, st, _ := advFixture(pool)
	st.failedSinceProgress = 5

	c.maybeAdvisorDiagnose(context.Background(), spec, st, advisorStale-1)

	var brief string
	for _, m := range pool.gotMsgs {
		if m.Role == "user" {
			brief = m.Content
		}
	}
	if !strings.Contains(brief, "连续 6 轮") || !strings.Contains(brief, "失败 5 次") {
		t.Fatalf("brief = %q, want the harness-only facts (stale 6 / 5 failures)", brief)
	}
	if strings.Contains(brief, "至少一个反例") {
		t.Fatalf("brief = %q leaked the acceptance standard into the coach's brief", brief)
	}
}

// TestAdvisorDiagnose_OutageAndCap: an advisor outage changes nothing and must NOT
// spend the per-turn budget (the next stall may still earn a consult); a spent
// budget stops consulting entirely.
func TestAdvisorDiagnose_OutageAndCap(t *testing.T) {
	down := &advPool{err: errors.New("no advisor model")}
	c, spec, st, _ := advFixture(down)
	c.maybeAdvisorDiagnose(context.Background(), spec, st, advisorStale-1)
	if st.advisorDiagnoses != 0 {
		t.Fatalf("diagnoses=%d — an outage must not consume the budget", st.advisorDiagnoses)
	}
	if strings.Contains(spec.Prompt.Build(), "顾问") {
		t.Fatal("a failed consult must not park anything on the prompt layer")
	}

	pool := &advPool{product: "建议"}
	c2, spec2, st2, _ := advFixture(pool)
	st2.advisorDiagnoses = maxAdvisorDiagnoses
	c2.maybeAdvisorDiagnose(context.Background(), spec2, st2, advisorStale-1)
	if pool.calls != 0 {
		t.Fatalf("calls=%d — the per-turn cap must stop further consults", pool.calls)
	}
}

// TestAdvisor_NotASubTurnAffordance: depth 1 is a hard rule — a sub-turn never
// consults (it would burn the sub's wall clock discovering the refusal).
func TestAdvisor_NotASubTurnAffordance(t *testing.T) {
	pool := &advPool{product: "建议"}
	c, spec, _, _ := advFixture(pool)
	spec.IsSub = true
	if out := c.runAdvisor(context.Background(), spec, "task", advisorDiagnoseGuide); out != "" || pool.calls != 0 {
		t.Fatalf("out=%q calls=%d, want a no-op inside a sub-turn", out, pool.calls)
	}
}

func TestHistoryTool_IndexSearchRead(t *testing.T) {
	hist := advFakeHistory{msgs: []contract.Message{
		{Role: "user", Content: "帮我查一下库存数据", Author: contract.MsgAuthor{Kind: "human", Name: "老板"}},
		{Role: "assistant", Content: "好的，我先查库存。", ToolCalls: []contract.ToolCall{{ID: "c1", Name: "web", Arguments: `{"q":"inventory"}`}}},
		{Role: "tool", ToolCallID: "c1", ToolName: "web", Content: "库存 42 件"},
		{Role: "assistant", Content: "库存有 42 件。"},
	}}
	tc := contract.ToolContext{History: hist}
	run := func(args string) contract.ToolResult {
		return historyTool{}.Run(context.Background(), tc, json.RawMessage(args))
	}

	if res := (historyTool{}).Run(context.Background(), contract.ToolContext{}, json.RawMessage(`{"action":"index"}`)); res.OK {
		t.Fatal("nil History must refuse")
	}
	idx := run(`{"action":"index"}`)
	if !idx.OK || !strings.Contains(idx.Data, "[0] USER(老板)") || !strings.Contains(idx.Data, "[tools: web]") {
		t.Fatalf("index = %+v", idx)
	}
	// CJK bigram search: 「库存」 must match unspaced Chinese content.
	sr := run(`{"action":"search","query":"库存"}`)
	if !sr.OK || !strings.Contains(sr.Data, "[2]") {
		t.Fatalf("search = %+v", sr)
	}
	// Hidden tool-args are searchable ("inventory" only appears in Arguments)…
	sr = run(`{"action":"search","query":"inventory"}`)
	if !sr.OK || !strings.Contains(sr.Data, "[1]") {
		t.Fatalf("args search = %+v", sr)
	}
	// …but never displayed by read.
	rd := run(`{"action":"read","message_ids":[1,2]}`)
	if !rd.OK || strings.Contains(rd.Data, "inventory") {
		t.Fatalf("read leaked hidden args: %+v", rd)
	}
	if !strings.Contains(rd.Data, "库存 42 件") {
		t.Fatalf("read missing tool result: %+v", rd)
	}
	if res := run(`{"action":"read","message_ids":[99]}`); !res.OK || !strings.Contains(res.Data, "编号不存在") {
		t.Fatalf("bad id = %+v", res)
	}
}

// TestHistoryTool_CapsAndPaging pins the design caps (docs/turn-driver-split.md
// §5): read ≤20 ids / 80k chars, search top_k clamp, index offset paging, and
// rune-safe entry truncation.
func TestHistoryTool_CapsAndPaging(t *testing.T) {
	big := strings.Repeat("库存数据分析报告。", 3000) // ~27k bytes of pure CJK
	var msgs []contract.Message
	for i := 0; i < 30; i++ {
		msgs = append(msgs, contract.Message{Role: "assistant", Content: big})
	}
	tc := contract.ToolContext{History: advFakeHistory{msgs: msgs}}
	run := func(args string) contract.ToolResult {
		return (historyTool{}).Run(context.Background(), tc, json.RawMessage(args))
	}

	// Entry truncation is rune-safe: valid UTF-8 after the 16k cut.
	rd := run(`{"action":"read","message_ids":[0]}`)
	if !rd.OK || !strings.Contains(rd.Data, "已截断") {
		t.Fatalf("read = %+v", rd)
	}
	if !utf8.ValidString(rd.Data) {
		t.Fatal("truncated entry is not valid UTF-8")
	}

	// Read caps: 25 ids requested → only histReadIDCap processed; total-cap note
	// fires long before all 20 land (20×16k > 80k).
	rd = run(`{"action":"read","message_ids":[0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24]}`)
	if !rd.OK || !strings.Contains(rd.Data, "达到上限") {
		t.Fatalf("total cap note missing: len=%d", len(rd.Data))
	}
	if len(rd.Data) > histReadTotal+histEntryCap {
		t.Fatalf("read output %d exceeds cap+one-entry slack", len(rd.Data))
	}

	// Index paging: offset walks earlier; limit clamps.
	idx := run(`{"action":"index","offset":10,"limit":5}`)
	if !idx.OK || !strings.Contains(idx.Data, "[19]") || strings.Contains(idx.Data, "[20]") {
		t.Fatalf("paging window wrong:\n%s", idx.Data)
	}
	if !strings.Contains(idx.Data, "offset=15") {
		t.Fatalf("next-page hint missing:\n%s", idx.Data)
	}

	// Search top_k clamp: ask for 999, everything matches, ≤50 returned.
	sr := run(`{"action":"search","query":"库存","top_k":999}`)
	if !sr.OK || strings.Count(sr.Data, "\n[") > histSearchTopKM+1 {
		t.Fatalf("top_k clamp failed: %d lines", strings.Count(sr.Data, "\n["))
	}
}

// toolRecPool records the tool specs advertised on the first round.
type toolRecPool struct {
	fakePool
	seen []string
}

func (p *toolRecPool) ChatStreamWatch(_ context.Context, req contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	if p.seen == nil {
		for _, sp := range req.Tools {
			p.seen = append(p.seen, sp.Name)
		}
	}
	accum := &contract.StreamAccumulator{Text: "done"}
	_ = w(contract.StreamEvent{Text: "done"}, accum)
	_ = w(contract.StreamEvent{Done: true}, accum)
	return contract.ChatResponse{Content: "done"}, nil
}

// TestBuiltin_AdvisorIsNotAModelTool: the advisor is harness-triggered machinery,
// never an advertised capability — neither driver puts it in the model's bag
// (docs/advisor.md §7). The looping driver instead ARMS it on the turn state, which
// is what gates the stall diagnosis and the delivery review.
func TestBuiltin_AdvisorIsNotAModelTool(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode contract.TurnMode
	}{{"looping", contract.TurnModeLooping}, {"regular", contract.TurnModeRegular}} {
		pool := &toolRecPool{}
		c := &core{store: newSubStore(), pool: pool}
		c.Run(context.Background(), contract.TurnSpec{
			Sid: "bi-" + tc.name, Prompt: sysPrompt("s"), UserText: "x", Sink: &recSink{}, Mode: tc.mode,
		})
		for _, n := range pool.seen {
			if n == "advisor" {
				t.Fatalf("%s turn advertised an advisor tool — it must not be model-callable", tc.name)
			}
		}
	}
}

// TestBuiltin_ShareHistorySubGetsHistoryTool: a ShareHistory sub-turn receives
// read_conversation_log directly from the sub-runner — no suite naming, no
// parent-bag dependency (the parent here has NO tools at all).
func TestBuiltin_ShareHistorySubGetsHistoryTool(t *testing.T) {
	pool := &toolRecPool{}
	c := &core{store: newSubStore(), pool: pool}
	parent := contract.TurnSpec{Sid: "bi-parent", Sink: &recSink{}}
	res := c.runSubTurn(context.Background(), parent, contract.SubSpec{Task: "审一下", ShareHistory: true})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(pool.seen) != 1 || pool.seen[0] != "read_conversation_log" {
		t.Fatalf("sub tools = %v, want [read_conversation_log] riding ShareHistory", pool.seen)
	}
}

// TestMaybeArmHistoryTool (pull 化 P1): a compacted session (replay starts at
// a summary marker) arms the own-log read-back — read_conversation_log in the
// suite + History pointed at the OWN sid; un-compacted sessions never pay the
// spec tokens; arming is idempotent and never re-points an advisor sub's
// delegator view.
func TestMaybeArmHistoryTool(t *testing.T) {
	c := &core{store: newSubStore()} // any non-nil MessageStore — arming refuses a store-less core
	fresh := []contract.Message{{Role: "user", Content: "hi"}}
	compacted := []contract.Message{{Role: "system", Content: summaryPrefix + "\nS", RowKind: contract.RowKindSummary}, {Role: "user", Content: "hi"}}

	// Un-compacted → untouched.
	spec := contract.TurnSpec{Sid: "s1"}
	c.maybeArmHistoryTool(&spec, fresh)
	if spec.History != nil || spec.Tools != nil {
		t.Fatal("fresh session must not arm the history tool")
	}

	// Compacted → armed on the OWN sid, tool in the suite.
	c.maybeArmHistoryTool(&spec, compacted)
	sh, ok := spec.History.(storeHistory)
	if !ok || sh.sid != "s1" {
		t.Fatalf("History = %#v, want storeHistory on own sid", spec.History)
	}
	if _, ok := spec.Tools.Lookup("read_conversation_log"); !ok {
		t.Fatal("read_conversation_log must be in the armed suite")
	}
	// Idempotent: no duplicate spec on a second loop-top.
	c.maybeArmHistoryTool(&spec, compacted)
	n := 0
	for _, sp := range spec.Tools.Specs() {
		if sp.Name == "read_conversation_log" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("tool specs = %d, want 1 (idempotent arm)", n)
	}

	// An advisor sub (History pre-set to the DELEGATOR) is never re-pointed.
	sub := contract.TurnSpec{Sid: "child", History: storeHistory{sid: "parent"}}
	c.maybeArmHistoryTool(&sub, compacted)
	if sub.History.(storeHistory).sid != "parent" {
		t.Fatal("a ShareHistory sub must keep its delegator view")
	}

	// A sub-turn (IsSub) is NEVER armed even when its scratch compacts: its
	// rows live in the subStore and the read-back is a parent affordance.
	isSub := contract.TurnSpec{Sid: "child2", IsSub: true}
	c.maybeArmHistoryTool(&isSub, compacted)
	if isSub.History != nil || isSub.Tools != nil {
		t.Fatal("a sub-turn must not arm the own-log read-back")
	}

	// A store-less core never arms a poisoned reader.
	noStore := &core{}
	spec2 := contract.TurnSpec{Sid: "s2"}
	noStore.maybeArmHistoryTool(&spec2, compacted)
	if spec2.History != nil {
		t.Fatal("a core without a store must not arm")
	}

	// The suite fence: a sub suite naming the core tool does NOT receive the
	// parent's armed own-variant (History would be nil there — burned rounds).
	if fenced := narrowToSuite(spec.Tools, []string{"read_conversation_log"}); len(fenced.Specs()) != 0 {
		t.Fatal("narrowToSuite must drop the core-injected history tool")
	}
}

// TestAdvisorDiagnose_MuteAdvisorParksNothing: a sub-turn that burns its rounds
// without answering comes back as a PARTIAL salvage brief ("没能给出完整结果") —
// honest, but it is not advice. Parking it under "以下是顾问给出的诊断" would put
// words in the advisor's mouth; the attempt is spent, the diagnosis budget is not.
func TestAdvisorDiagnose_MuteAdvisorParksNothing(t *testing.T) {
	pool := &advPool{product: ""} // every sub round empty → the sub salvages
	c, spec, st, sink := advFixture(pool)

	c.maybeAdvisorDiagnose(context.Background(), spec, st, advisorStale-1)

	if st.advisorDiagnoses != 0 {
		t.Fatalf("diagnoses=%d — a mute advisor produced no advice to count", st.advisorDiagnoses)
	}
	if st.advisorAttempts != 1 {
		t.Fatalf("attempts=%d, want 1 — the dial-out happened and must be budgeted", st.advisorAttempts)
	}
	if strings.Contains(spec.Prompt.Build(), "顾问") {
		t.Fatalf("a salvaged sub-turn brief was parked as a diagnosis: %q", spec.Prompt.Build())
	}
	for _, line := range sink.trace {
		if strings.Contains(line, "顾问诊断") {
			t.Fatalf("traced %q as advice", line)
		}
	}
}

// TestAdvisorDiagnose_AttemptCapStopsAMuteAdvisor: failed consults don't spend the
// DIAGNOSIS budget (the next stall deserves a real one), so a separate attempt cap
// is what stops a permanently mute advisor from being re-dialled every stall for
// the whole fuse.
func TestAdvisorDiagnose_AttemptCapStopsAMuteAdvisor(t *testing.T) {
	pool := &advPool{err: errors.New("advisor down")}
	c, spec, st, _ := advFixture(pool)
	st.advisorAttempts = maxAdvisorAttempts

	c.maybeAdvisorDiagnose(context.Background(), spec, st, advisorStale-1)
	if pool.calls != 0 {
		t.Fatalf("calls=%d — the attempt cap must stop dialling a dead advisor", pool.calls)
	}
}

// TestAdvisorNote_DroppedWhenTheHarnessSaysStop: from the give-up tier on, the ladder
// forbids further tool calls; a parked "here's what to try next" diagnosis sits later
// in the prompt and would read as the newer instruction, so it must be dropped.
func TestAdvisorNote_DroppedWhenTheHarnessSaysStop(t *testing.T) {
	c, spec, st, _ := advFixture(&advPool{product: "换条路：先转码"})

	c.maybeAdvisorDiagnose(context.Background(), spec, st, advisorStale-1)
	if !strings.Contains(spec.Prompt.Build(), "先转码") {
		t.Fatal("diagnosis never parked")
	}
	if ex := c.loopTopNudge(spec, st, giveUpStale-1); ex != nil {
		t.Fatalf("unexpected exit %+v", ex)
	}
	if strings.Contains(spec.Prompt.Build(), "先转码") {
		t.Fatalf("the diagnosis survived the give-up tier and contradicts it: %q", spec.Prompt.Build())
	}
}
