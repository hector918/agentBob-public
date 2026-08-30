package turn

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"agentbob/contract"
)

// This file is the rubric gate: a mechanical, out-of-band quality check the turn
// runs before delivering a product, for skills that ship a RUBRIC.md.
//
// A skill is "engaged" this turn when the model reads it via skill_view; if that
// skill carries a rubric (contract.Skill.Rubric, loaded by leaf/skills from
// RUBRIC.md), the turn arms its criteria. At the final-reply door, an armed gate
// judges the product against the rubric (an LLM-as-judge call routed to a model
// tagged "judge" — the pool picks it; absent → the gate fails OPEN). A FAIL
// re-prompts the model to redo (bounded by maxRubricRetries); past the cap the turn
// declares failure rather than shipping ungrounded output (honest harness, I8).
//
// The judge only checks the product against the EVIDENCE the turn already has (the
// user input + this turn's tool results, e.g. an ocr transcript). It cannot tell
// whether that evidence itself is right — a rubric says as much.

const (
	// maxRubricRetries bounds how many times a failed product is sent back for a redo
	// before the turn declares failure. Each redo is one extra round + one judge call.
	maxRubricRetries = 3
	// rubricEvidenceReplay / rubricEvidenceCap bound the evidence the judge sees — the
	// recent history (scoped to THIS turn) and a byte cap on the rendered transcript.
	rubricEvidenceReplay = 30
	rubricEvidenceCap    = 8000
	// rubricAskCap bounds the user's request fed to the judge as its OWN section, so the
	// judge always knows what was asked even when a large tool result (e.g. an OCR
	// transcript) would push the ask out of the tail-capped evidence. Head-kept (the ask's
	// intent leads); the request is normally short, this is only a defensive bound.
	rubricAskCap = 2000
	// rubricRedoLayer is the transient prompt layer carrying the redo critique (reuses
	// the bloat/nudge layer mechanism — no new machinery). Cleared once the gate passes.
	rubricRedoLayer = "rubric_redo"
	// maxReviewRetries bounds the ADVISOR review gate's redos (docs/advisor.md §4).
	// Lower than maxRubricRetries on purpose: each rubric redo costs one blind judge
	// call, each review redo costs a whole advisor SUB-TURN (10-100x).
	maxReviewRetries = 2
)

const rubricJudgeSystem = "你是产出校验判官。依据给定的 RUBRIC 校验准则，对照证据，判断这次产出是否合格。" +
	"\n只输出一个 JSON 对象，不要代码块围栏、不要解释、不要前言：" +
	"\n{\"pass\": true 或 false, \"reason\": \"不合格时一句话说明哪条不达标，合格留空\"}" +
	"\n若产出与本校验准则无关（例如普通寒暄、与该类报告无关的内容），或证据不足以比对，输出 pass=true（放行，不要判不合格）。"

// armRubricOnSkillView arms the gate when the model successfully VIEWS a skill that
// carries a RUBRIC.md — skill_view IS the "I'm using this skill" signal, so that
// skill's product is judged against its rubric at the final-reply door. A turn that
// engages no rubric-bearing skill — including a plain OCR with no skill viewed — is
// never gated, and arming is per-turn (no session/history state, so compaction can't
// disarm it). Only the skill the model actually viewed arms (off-domain products
// pass via the judge's pass=true steer). OCR is decoupled: running image(task=read) alone — the
// common "just read this image" case — never arms a rubric.
func (c *core) armRubricOnSkillView(st *turnState, spec contract.TurnSpec, call contract.ToolCall, res contract.ToolResult) {
	if call.Name != "skill_view" || !res.OK {
		return
	}
	var a struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(call.Arguments), &a) != nil {
		return
	}
	armRubric(st, spec, strings.TrimSpace(a.Name)) // no-op unless that skill carries a rubric
}

// armRubric arms the gate for skill `name` if it carries a rubric. Idempotent; a
// blank name or rubric-less skill is a no-op.
func armRubric(st *turnState, spec contract.TurnSpec, name string) {
	if name == "" || spec.Skills == nil {
		return
	}
	sk, ok := spec.Skills.Get(name)
	if !ok || strings.TrimSpace(sk.Rubric) == "" {
		return
	}
	if st.rubricArmed == nil {
		st.rubricArmed = map[string]string{}
	}
	if _, dup := st.rubricArmed[name]; !dup {
		st.rubricArmed[name] = sk.Rubric
		slog.Info("turn: rubric gate armed", "sid", spec.Sid, "skill", name)
	}
}

// rubricVerdict is the judge's structured output — a machine-readable bool, far more
// robust than prefix-matching a natural-language line.
type rubricVerdict struct {
	Pass   bool   `json:"pass"`
	Reason string `json:"reason"`
}

// rubricGate judges a final product against the armed rubric(s) and traces the
// verdict so it's visible at a glance. Returns nil to let the caller finalize (pass /
// unsure / judge unavailable); a non-nil roundResult to take instead — roundContinue
// for a redo, or a roundExit declaring failure once the retry cap is spent.
// evReplay is the round's already-read replay for the evidence (nil → read fresh).
func (c *core) rubricGate(ctx context.Context, spec contract.TurnSpec, st *turnState, output string, evReplay []contract.Message) *roundResult {
	blocked, judged, reasons := c.judgeWithEscalation(ctx, spec, st, output, evReplay)
	if !blocked {
		// pass / unjudgeable → drop any pending redo critique, proceed.
		if spec.Prompt != nil {
			spec.Prompt.SetLayer(rubricRedoLayer, "")
		}
		spec.Sink.TraceDelta(rubricPassTrace(judged))
		return nil
	}
	st.rubricRetries++
	if st.rubricRetries > maxRubricRetries {
		// §6.3.4: carry the honest side-effect footer like every other finish door —
		// a rubric failure must not also swallow a "this action didn't succeed" notice.
		notice := rubricFailNotice(reasons) + sideEffectFooter(st)
		// Trace BEFORE finalizeTurn: Finish (inside it) closes the sink's flush loop
		// after one last trace flush — a TraceDelta issued after Finish only ever sits
		// in a buffer nothing drains, so the terminal verdict would be lost on every
		// streamsink channel. Same posture as the salvage door (50-salvage.go). Safe:
		// TraceDelta doesn't persist, so I10's persist-before-Finish is untouched.
		spec.Sink.TraceDelta("🧪 RUBRIC ✗ 未过：" + traceReason(reasons) + "（重试用尽，声明失败）")
		// I10: persist BEFORE finishing, on a durable ctx — the judge call just above may
		// have been cancelled (dead turn ctx); finalizeTurn falls back so the row isn't lost.
		if ferr := c.finalizeTurn(ctx, spec, notice, notice); ferr != nil {
			// Delivery FAILED — the user received nothing. Do NOT report a clean rubric-fail
			// exit: it would book a delivered turn downstream. Surface an error so it isn't
			// counted as delivered (the notice is still persisted to history). (F107)
			return &roundResult{kind: roundErr, err: fmt.Errorf("turn: exit reply computed but delivery failed: %w", ferr)}
		}
		slog.Warn("turn: rubric gate failed past retry cap — declaring failure",
			"sid", spec.Sid, "retries", maxRubricRetries)
		// Carry the rejected product (not the apology) so the failure learner reflects on
		// the real flawed output — this is the most common skill-learning trigger.
		return &roundResult{kind: roundExit, exit: &turnExit{state: exitRubricFailed, detail: notice, rejected: output}}
	}
	if spec.Prompt != nil {
		spec.Prompt.SetLayer(rubricRedoLayer, rubricRedoNudge(output, reasons))
	}
	spec.Sink.TraceDelta(fmt.Sprintf("🧪 RUBRIC ✗ 未过：%s（重做 %d/%d）", traceReason(reasons), st.rubricRetries, maxRubricRetries))
	slog.Info("turn: rubric gate failed — requesting redo", "sid", spec.Sid, "attempt", st.rubricRetries)
	// Fresh work budget for the corrected-answer attempt: a redo is a new attempt at a
	// gated-valid product, so it must not be pre-empted by the stale / hard-iter budget
	// the rejected product already spent. Bounded — redos cap at maxRubricRetries.
	return &roundResult{kind: roundContinue, resetBudget: true}
}

// judgeRubric runs the LLM-as-judge call. blocked is the ONLY flow-changing signal —
// true only on a clear pass=false verdict; any judge error (no model tagged "judge",
// backend down) or unparseable output → blocked=false (fail-open: the gate never
// blocks a turn it couldn't validate). judged reports whether a real verdict was
// obtained, so the trace can distinguish a true pass (✓) from a fail-open (⚠️).
func (c *core) judgeRubric(ctx context.Context, spec contract.TurnSpec, st *turnState, output string, evReplay []contract.Message) (blocked, judged bool, reasons string) {
	rubric := joinRubrics(st.rubricArmed)
	evidence := c.rubricEvidence(ctx, spec, evReplay)
	// The user's ask is fed as its OWN section, NOT folded into evidence: evidence is
	// tail-capped (rubricEvidenceCap), so a long tool result would otherwise push the ask
	// — which sits at the head — out, leaving the judge grading a product against a
	// transcript with no idea what was requested (L-rubric-D1).
	ask := truncateBytes(strings.TrimSpace(spec.UserText), rubricAskCap)
	msgs := []contract.Message{
		{Role: "system", Content: rubricJudgeSystem},
		{Role: "user", Content: rubricJudgePrompt(rubric, ask, evidence, output)},
	}
	resp, err := c.pool.Chat(ctx, contract.ModelRequest{Kind: contract.KindLLM, Requires: []string{"judge"}}, msgs)
	if err != nil {
		slog.Warn("turn: rubric judge unavailable — passing (fail-open)", "sid", spec.Sid, "err", err)
		return false, false, ""
	}
	v, ok := parseRubricVerdict(resp.Content)
	if !ok {
		// Empty content is a distinct, actionable failure from garbled JSON: the judge
		// model emitted its whole answer as reasoning/thinking (a separate channel the
		// block path drops), leaving content blank — disabling thinking on the judge
		// entry fixes it. Name the serving entry either way so a silently fail-opening
		// gate is attributable to a specific model (judge tag → nv-nano / gemma / …).
		if strings.TrimSpace(resp.Content) == "" {
			slog.Warn("turn: rubric judge returned empty content — passing (fail-open); judge model likely emitted its verdict as reasoning/thinking (a channel we drop) — disable thinking on the judge entry",
				"sid", spec.Sid, "model", resp.Model)
		} else {
			slog.Warn("turn: rubric judge verdict unparseable — passing (fail-open)", "sid", spec.Sid, "model", resp.Model, "raw", truncateBytes(collapseLine(resp.Content), 120))
		}
		return false, false, ""
	}
	return !v.Pass, true, v.Reason // v.Pass=true → not blocked, no reason
}

// parseRubricVerdict extracts the judge's JSON verdict ({"pass":bool,"reason":...}) —
// the first {...} object in the response, so a code-fence or stray preamble around it
// is tolerated. ok=false when no JSON object parses (→ fail-open).
func parseRubricVerdict(raw string) (rubricVerdict, bool) {
	i := strings.IndexByte(raw, '{')
	j := strings.LastIndexByte(raw, '}')
	if i < 0 || j < i {
		return rubricVerdict{}, false
	}
	var v rubricVerdict
	if json.Unmarshal([]byte(raw[i:j+1]), &v) != nil {
		return rubricVerdict{}, false
	}
	return v, true
}

// rubricPassTrace is the one-line trace for a non-blocking verdict: a real pass vs a
// fail-open (judge unavailable / unparseable).
func rubricPassTrace(judged bool) string {
	if judged {
		return "🧪 RUBRIC ✓ 通过"
	}
	return "🧪 RUBRIC ⚠️ 判官不可用，放行"
}

// traceReason renders a judge reason for a one-line trace (collapsed + bounded).
func traceReason(reasons string) string {
	if r := strings.TrimSpace(reasons); r != "" {
		return truncateBytes(collapseLine(r), 80)
	}
	return "（未给出原因）"
}

// rubricEvidence renders the source text the judge checks against: this turn's
// transcript (from the START of this turn's user block onward — ALL user input +
// tool results such as an ocr transcript). Falls back to the raw user text if history
// is unreadable. replay, when non-nil, is the round's already-read replay (reused so
// the gate doesn't issue a second GetReplay); nil — a salvage-path judge or a round
// whose loop-top read was consumed by a compaction — reads fresh here. The backward
// scan anchors on the LAST user block either way, so the window size difference
// (historyReplayCap vs rubricEvidenceReplay) doesn't change the evidence.
func (c *core) rubricEvidence(ctx context.Context, spec contract.TurnSpec, replay []contract.Message) string {
	history := replay
	if history == nil {
		var err error
		history, err = c.store.GetReplay(ctx, spec.Sid, rubricEvidenceReplay)
		if err != nil {
			return spec.UserText
		}
	}
	if len(history) == 0 {
		return spec.UserText
	}
	start := 0
	for i := len(history) - 1; i >= 0; i-- {
		// Summary marker rows persist as role=user (row_kind=summary) since the
		// compaction rewrite — a digest of EARLIER turns is not this turn's user
		// input, so the anchor scan and the block extension both skip it (before
		// the rewrite its role=system failed the predicate implicitly).
		if history[i].Role == "user" && history[i].RowKind != contract.RowKindSummary {
			start = i
			// A turn now persists ONE user row PER speaker — extend back through the
			// contiguous trailing user block so the judge sees ALL of this turn's user
			// input, not just the last speaker's.
			for start > 0 && history[start-1].Role == "user" && history[start-1].RowKind != contract.RowKindSummary {
				start--
			}
			break
		}
	}
	var b strings.Builder
	for _, m := range history[start:] {
		if m.Role != "user" && m.Role != "tool" {
			continue // assistant tool-call preambles / drafts are not evidence
		}
		if m.RowKind == contract.RowKindSummary {
			continue // compaction digest — earlier turns, not this turn's evidence
		}
		label := m.Role
		if m.Role == "tool" && m.ToolName != "" {
			label = "tool:" + m.ToolName
		}
		b.WriteString(label)
		b.WriteString("：")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	ev := strings.TrimSpace(b.String())
	if ev == "" {
		return spec.UserText
	}
	return capTailBytes(ev, rubricEvidenceCap)
}

// joinRubrics concatenates the armed rubric texts (almost always one). Sorted-free:
// order is irrelevant to the judge, which reads the union of criteria.
func joinRubrics(armed map[string]string) string {
	parts := make([]string, 0, len(armed))
	for _, r := range armed {
		if r = strings.TrimSpace(r); r != "" {
			parts = append(parts, r)
		}
	}
	return strings.Join(parts, "\n\n---\n\n")
}

func rubricJudgePrompt(rubric, ask, evidence, output string) string {
	return "## 校验准则（RUBRIC）\n" + rubric +
		"\n\n## 用户的请求\n" + ask +
		"\n\n## 证据（本轮工具结果等来源文本，判官据此核对产出是否成立）\n" + evidence +
		"\n\n## 待校验产出\n" + output
}

func rubricRedoNudge(output, reasons string) string {
	r := strings.TrimSpace(reasons)
	if r == "" {
		r = "（未给出具体原因）"
	}
	return "⚠️ 你上一轮的产出未通过校验。\n\n【未通过原因】" + r +
		"\n\n【你上一轮的产出】\n" + output +
		"\n\n请严格按校验准则修正后，重新输出完整产出，不要解释、不要前言。"
}

// advisorRedoNudge is the review gate's critique layer. Deliberately NOT
// rubricRedoNudge: that one says "按校验准则修正后重新输出完整产出，不要解释" — right
// for a rubric (the product IS the text, and the model has read the criteria), wrong
// here on both counts. The review's criteria live only in the reviewer's brief
// (docs/advisor.md §4), so pointing at them would point at nothing the model can see;
// and its typical objection is about an ACTION that didn't happen ("说已发送，但记录
// 里 deliver 那步失败了") — telling the model to just re-type the answer is exactly
// the wrong move.
func advisorRedoNudge(output, reason string) string {
	r := strings.TrimSpace(reason)
	if r == "" {
		r = "（未给出具体原因）"
	}
	return "⚠️ 你刚才的交付没有通过复核。\n\n【未通过原因】" + r +
		"\n\n【你刚才的交付】\n" + output +
		"\n\n请真正解决上面指出的问题：如果是某一步动作没做成（文件没写成、消息没发出去、数据没取到），就重新去做那一步，不要只把文字重写一遍；" +
		"如果是内容缺失，就补齐后再交付。改完后重新给出完整交付。"
}

func rubricFailNotice(reasons string) string {
	r := strings.TrimSpace(reasons)
	tail := ""
	if r != "" {
		tail = "校验意见：" + r + "。"
	}
	return fmt.Sprintf("（这次的产出未能通过校验，已重试 %d 次仍不达标，未敢作为结果交付。%s请人工复核或重发。）",
		maxRubricRetries, tail)
}

// capTailBytes keeps the LAST n bytes of s (the recent tail — where this turn's tool
// results land), prefixed with an elision marker when truncated. Rune-safe: trims
// any partial leading rune the byte cut may have produced.
func capTailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	tail := s[len(s)-n:]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:] // drop a partial leading rune the byte cut may have split
	}
	return "…（较早内容已略）\n" + tail
}

// arbiterGuide is the escalated judge's sub-turn persona (SubSpec.Guide): unlike
// the blind judge it can PULL evidence — verify the product's claims against the
// actual conversation record before ruling. Same JSON verdict contract as the
// blind judge so one parser serves both.
const arbiterGuide = "你是产出校验的裁定人：初审判官判这份产出不合格，现在由你复核终裁。\n" +
	"- 你有 read_conversation_log 工具，可以只读查看主对话的完整记录——先核对产出里的关键声明是否有据（引用是否真实存在、结论是否被工具结果支撑），再下结论。\n" +
	"- 复核要独立：初审可能误判（把合格产出错杀），也可能漏判；以证据为准。\n" +
	"- 只输出一个 JSON 对象，不要代码块围栏、不要解释：{\"pass\": true 或 false, \"reason\": \"不合格时一句话指出哪条不达标；推翻初审放行时一句话说明依据\"}"

// arbiterProductCap bounds the product text embedded in the arbiter's task brief.
const arbiterProductCap = 16000

// judgeWithEscalation is the tiered rubric judgment (docs/turn-driver-split.md §5):
// the cheap blind judge rules first; a clear FAIL earns ONE evidence-capable second
// opinion per turn on the advisor chassis, whose verdict SUPERSEDES — it can rescue
// a false fail (the blind judge graded against harness-fed evidence only) or confirm
// with a record-backed reason. Pass and fail-open never escalate (a judge hiccup
// must not buy a 10-100x sub-turn), so the cheap path stays cheap.
func (c *core) judgeWithEscalation(ctx context.Context, spec contract.TurnSpec, st *turnState, output string, evReplay []contract.Message) (blocked, judged bool, reasons string) {
	blocked, judged, reasons = c.judgeRubric(ctx, spec, st, output, evReplay)
	if !blocked {
		return blocked, judged, reasons
	}
	v, ok := c.arbiterSecondOpinion(ctx, spec, st, output, reasons)
	if !ok {
		return blocked, judged, reasons // no second opinion obtained — the blind verdict stands
	}
	if v.Pass {
		// No trace line here: arbiterSecondOpinion already emitted the verdict IN FULL
		// (docs/advisor.md §6), and it only ever runs after a blind FAIL — so a 合格
		// verdict on that line already reads as the overturn.
		slog.Info("turn: rubric arbiter overturned the blind fail", "sid", spec.Sid)
		return false, true, ""
	}
	if r := strings.TrimSpace(v.Reason); r != "" {
		reasons = r // the record-backed reason feeds the redo critique
	}
	return true, true, reasons
}

// arbiterSecondOpinion runs the escalated verdict as a depth-1 sub-turn on the
// advisor chassis (tag `advisor`, ShareHistory → read_conversation_log). ok=false
// when no verdict was obtained — sub-turns never escalate (a sub calling
// runSubTurn would be depth 2), the per-turn cap was spent, or the sub failed /
// answered unparseably; the caller then keeps the blind verdict (fail-open at the
// ESCALATION level only — the gate itself stays closed).
func (c *core) arbiterSecondOpinion(ctx context.Context, spec contract.TurnSpec, st *turnState, output, blindReason string) (rubricVerdict, bool) {
	if spec.IsSub || st.arbiterUsed {
		return rubricVerdict{}, false
	}
	st.arbiterUsed = true
	spec.Sink.TraceDelta("🧪 RUBRIC ↑ 初审未过，升档裁定人复核…")
	task := "## 校验准则（RUBRIC）\n" + joinRubrics(st.rubricArmed) +
		"\n\n## 用户的请求\n" + truncateBytes(strings.TrimSpace(spec.UserText), rubricAskCap) +
		"\n\n## 初审判定\n不合格：" + traceReason(blindReason) +
		"\n\n## 待复核产出\n" + truncateBytes(output, arbiterProductCap)
	res := c.runSubTurn(ctx, spec, contract.SubSpec{
		Task:  task,
		Guide: arbiterGuide, // read_conversation_log rides ShareHistory (内置), no suite naming

		ModelRequires: []string{"advisor"},
		ShareHistory:  true,
	})
	if res.Err != nil || strings.TrimSpace(res.Product) == "" {
		slog.Warn("turn: rubric arbiter unavailable — blind verdict stands", "sid", spec.Sid, "err", res.Err)
		return rubricVerdict{}, false
	}
	v, ok := parseRubricVerdict(res.Product)
	if !ok {
		slog.Warn("turn: rubric arbiter verdict unparseable — blind verdict stands", "sid", spec.Sid, "raw", truncateBytes(collapseLine(res.Product), 120))
		return rubricVerdict{}, false
	}
	// Full-text verdict on the trace stream (docs/advisor.md §6): the arbiter READ the
	// record to reach this, and its reasoning is the payload worth reading — the
	// one-line gate traces around it are summaries, not the advisor's output.
	spec.Sink.TraceDelta("🧪 RUBRIC ↑ 裁定人复核：" + passLabel(v.Pass) + "\n" + strings.TrimSpace(v.Reason))
	return v, true
}

// passLabel renders a verdict's boolean for a trace line.
func passLabel(pass bool) string {
	if pass {
		return "合格"
	}
	return "不合格"
}

// advisorReviewGate is the acceptance gate for a LOOPING delivery that carries an
// ARTIFACT but no rubric (docs/advisor.md §4) — the convergence contract promises
// "claimed done + acceptance passed", and without a rubric that acceptance was empty:
// the model's own word shipped unchecked. The advisor reads the record and rules.
//
// Arming (all three): the looping driver armed the advisor, this is not itself a
// sub-turn, and the turn actually PRODUCED something (st.produced — a Delivers or
// SideEffect tool succeeded: a file handed over, a message sent, an arrangement
// created). A research / Q&A turn never pays for a sub-turn. Deliberately wider
// than Delivers alone, which in this tree means deliver_file and nothing else —
// an agora worker's whole output is a send_message, and reviewing nothing there
// would repeat the very "path nobody walks" failure this redesign replaced.
//
// Deliberately NOT stacked on the rubric gate: when criteria exist the cheap blind
// judge (plus its one arbiter escalation) owns acceptance — judging one delivery by
// two standards doubles the cost and lets two verdicts contradict each other. The
// caller enforces the either/or.
//
// Returns nil to let the caller finalize (pass / fail-open / concerns exhausted), or
// a roundResult to take instead (a redo).
func (c *core) advisorReviewGate(ctx context.Context, spec contract.TurnSpec, st *turnState, output string) *roundResult {
	if !st.advisorArmed || spec.IsSub || !st.produced {
		return nil
	}
	// Announce BEFORE the consult (same courtesy as the arbiter's 升档 line): a looping
	// turn is quiet, so without this the user watches a finished job sit silent for the
	// length of a whole sub-turn with no hint that anything is happening.
	spec.Sink.TraceDelta("🧾 交付前复核中…")
	out := c.runAdvisor(ctx, spec, advisorReviewTask(spec, output), advisorReviewGuide)
	if out == "" {
		// Fail-open, same posture as the rubric judge: a gate never blocks what it
		// could not verify. Logged as well as traced — trace is off by default per
		// scope, and a gate that silently stopped gating must not be invisible.
		slog.Warn("turn: advisor reviewer unavailable — delivering unreviewed (fail-open)", "sid", spec.Sid)
		spec.Sink.TraceDelta("🧾 顾问复核 ⚠️ 顾问不可用，放行")
		return nil
	}
	v, ok := parseRubricVerdict(out)
	if !ok {
		slog.Warn("turn: advisor review verdict unparseable — passing (fail-open)", "sid", spec.Sid,
			"raw", truncateBytes(collapseLine(out), 120))
		// Bounded, unlike the verdict traces below: a non-verdict answer can be a whole
		// essay, and this line is diagnostic noise, not the payload worth reading.
		spec.Sink.TraceDelta("🧾 顾问复核 ⚠️ 判词无法解析，放行：" + traceReason(out))
		return nil
	}
	reason := strings.TrimSpace(v.Reason)
	if v.Pass {
		if spec.Prompt != nil {
			spec.Prompt.SetLayer(rubricRedoLayer, "")
		}
		st.advisorConcern = "" // an earlier redo's objection was resolved
		spec.Sink.TraceDelta("🧾 顾问复核 ✓ 通过：\n" + reason)
		return nil
	}
	if reason == "" {
		// A bare {"pass":false} still owes the user an explanation. Without this the
		// spent-budget branch below would set an EMPTY concern, advisorConcernFooter
		// would render nothing, and the delivery would ship silently while the trace
		// line claimed a concern was attached. (rubricFailNotice / rubricRedoNudge
		// carry the same guard.)
		reason = "顾问判定不合格但未给出具体原因"
	}
	st.reviewRetries++
	if st.reviewRetries > maxReviewRetries {
		// Ship WITH the objection, don't declare failure. This gate only arms once a
		// Delivers tool has already succeeded — the file is written, the message is
		// sent — so an apology claiming nothing happened would be its own lie. The
		// honest move is the §6.3.4 footer shape: deliver, and say what is unresolved.
		st.advisorConcern = reason
		spec.Sink.TraceDelta("🧾 顾问复核 ✗ 仍未消解（重做已用尽，附保留意见交付）：\n" + reason)
		slog.Warn("turn: advisor review unresolved past retry cap — delivering with a concern footer",
			"sid", spec.Sid, "retries", maxReviewRetries)
		return nil
	}
	if spec.Prompt != nil {
		// Same layer as the rubric's critique, not a twin: the two gates are mutually
		// exclusive per delivery, so a second layer buys nothing — and it would go
		// STALE unreachably (a review redo that then engages a rubric-bearing skill
		// routes every later delivery through rubricGate, which only ever clears its
		// own layer, leaving the old critique rendering to the end of the turn).
		spec.Prompt.SetLayer(rubricRedoLayer, advisorRedoNudge(output, reason))
	}
	spec.Sink.TraceDelta(fmt.Sprintf("🧾 顾问复核 ✗ 未过（重做 %d/%d）：\n%s", st.reviewRetries, maxReviewRetries, reason))
	slog.Info("turn: advisor review failed — requesting redo", "sid", spec.Sid, "attempt", st.reviewRetries)
	// Fresh work budget for the corrected attempt — identical to the rubric redo:
	// the redo is a new attempt at a gated-valid product, not a continuation of the
	// rounds the rejected one spent.
	return &roundResult{kind: roundContinue, resetBudget: true}
}

// advisorReviewTask is the acceptance brief: the standard, the ask, and the product.
// The EVIDENCE is deliberately absent — unlike the blind judge (which is fed a capped
// transcript) the reviewer pulls what it needs from the record itself, so a long tool
// result can't push the ask out of a truncated window.
func advisorReviewTask(spec contract.TurnSpec, output string) string {
	criteria := ""
	if c := strings.TrimSpace(spec.AdvisorCriteria); c != "" {
		criteria = "\n\n## 验收准则\n" + c
	}
	return "主对话声称任务完成，以下是准备交付给用户的产出。请读对话记录核实后裁定能否交付。" +
		criteria +
		"\n\n## 用户的请求\n" + truncateBytes(strings.TrimSpace(spec.UserText), rubricAskCap) +
		"\n\n## 待验收的交付\n" + truncateBytes(output, arbiterProductCap)
}

// advisorConcernFooter renders the reviewer's unresolved objection as an honest
// delivery footer (docs/advisor.md §4), or "" when the review passed / never ran.
// Same contract as sideEffectFooter: appended to the product the user receives, so a
// delivery that survived on a spent redo budget still says what is doubtful about it.
func advisorConcernFooter(st *turnState) string {
	concern := strings.TrimSpace(st.advisorConcern)
	if concern == "" {
		return ""
	}
	concern = strings.TrimRight(concern, "。.！!；;")
	return "\n\n（复核保留意见：" + concern + "。以上产出已按现状交付，请自行留意。）"
}
