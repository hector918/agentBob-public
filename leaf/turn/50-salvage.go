package turn

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/i18n"
)

// This file is the salvage ladder (spec §6.7): a turn that exhausted its rounds (or,
// later, hit a guard exit) without a final reply still owes the user ONE readable
// line (I8). Entry is NON-cancel only — a cancelled turn returns silently (I14),
// handled before salvage is reached.
//
// Tiers, in order:
//  1. one NO-TOOL model call — the model reads the failed turn's history (incl. tool
//     results) and answers honestly from what it has. This is the value: a real
//     "here's what I found / where I'm stuck" instead of a stock apology.
//  2. no-LLM action brief — if the model is unavailable, name the tools that ran.
//  3. stock floor — the last-resort fixed line.
//  + a process-level honest closing by exit state.
//
// The degenerate path (EXIT_DEGEN_SALVAGE) gets one extra tier first: a strong
// `expert` model (§6.7-3). Deferred: the per-session "gave up expert" marker —
// trying expert once per degenerate turn is the accepted cost until §6.6's
// persistence lands.

// salvageTimeout bounds the ladder on its OWN context: the turn token may be dead or
// near-dead (spec §6.7-7), and this one line must land.
const salvageTimeout = 30 * time.Second

// salvage runs the ladder and delivers exactly one finish. It uses a fresh
// short-timeout context, not the (possibly dead) turn context.
func (c *core) salvage(spec contract.TurnSpec, st *turnState) contract.TurnResult {
	ctx, cancel := context.WithTimeout(context.Background(), salvageTimeout)
	defer cancel()

	// Re-apply the turn's folded nudge to THIS spec copy: the driver extended its own
	// (80-nudge.go), but Run hands salvage the pre-driver copy, and the acceptance
	// gates below render UserText as 「用户的请求」 — judging a salvage brief against an
	// ask that omits what the user added mid-turn is the same blindness the fold
	// removed everywhere else.
	if st.userNudge != "" && !strings.Contains(spec.UserText, st.userNudge) {
		spec.UserText = strings.TrimSpace(spec.UserText + "\n\n" + st.userNudge)
	}

	// Model-steering TRANSIENTS (I1) must not ride into salvage's clean "answer from
	// existing results" prompt. Clear them before buildMessages renders the salvage call.
	// A busy-arrived user nudge needs no protection here any more: it is a persisted
	// user row (80-nudge.go), so salvage's own history read carries it like any other
	// message instead of it depending on a layer surviving this cleanup.
	if spec.Prompt != nil {
		spec.Prompt.SetLayer("nudge", "")
		spec.Prompt.SetLayer("bloat_nudge", "")
		spec.Prompt.SetLayer(rubricRedoLayer, "")
		// The advisor's diagnosis (docs/advisor.md §3) is the loudest of these: it
		// tells the model what to TRY NEXT, which is the exact opposite of salvage's
		// "stop calling tools, answer honestly from what you have" — and the stall
		// path that triggers a diagnosis (stale 6) is the same path that ends in
		// salvage (stale 14), so this collision is the common case, not an edge.
		spec.Prompt.SetLayer(advisorNoteLayer, "")
	}

	nudge := salvageNudge(st.exit.state)
	reply := c.salvageModelCall(ctx, spec, st, nudge) // tier 1
	fromModel := reply != ""                          // a real tier-1 product (vs a stock lower-tier apology)

	// D22: a rubric-armed turn must not deliver an UN-judged product even on the salvage
	// give-up path — RUBRIC.md's whole purpose is the grounding guarantee, and salvage is
	// the path most likely to fabricate. Judge the RAW tier-1 product BEFORE the process
	// closing / side-effect footer are appended — same ordering as finalReply, so a
	// format-strict rubric never grades process boilerplate as part of the product. On a
	// clear FAIL, withhold the (possibly ungrounded) reply and deliver the honest
	// rubric-fail notice instead. No retry — salvage is already out of rounds, so this
	// judges once (pass/unsure → keep the reply; fail → replace). ctx is salvage's own
	// live token, so the judge call lands. ONLY the tier-1 model product needs grounding:
	// the action-brief / stock-floor tiers are self-evidently non-products (no fabrication
	// to catch), so skip the wasted judge call — and the risk of swapping a useful brief
	// for a generic notice — when the reply didn't come from the model (L-rubric-D2).
	blocked := false
	var rejected string // the withheld tier-1 product — carried on the result for the failure learner
	if fromModel && len(st.rubricArmed) > 0 {
		var reasons string
		// BLIND judge only — no arbiter escalation here: salvage runs on its own 30s
		// token (partly consumed by tier-1 already), so an advisor-chassis sub-turn
		// would almost always be cut at the deadline (blind stands anyway) AFTER
		// stalling the door's "one line must land" contract for the full window and
		// pushing finalizeTurn toward the nearly-dead-ctx persist hole.
		if blocked, _, reasons = c.judgeRubric(ctx, spec, st, reply, nil); blocked {
			// Keep the raw withheld product before overwriting: the failure learner must
			// reflect on the REAL flawed output, not the stock apology — same contract as
			// exitRubricFailed's `rejected` (30-state.go / L-rubric-B1).
			rejected = reply
			reply = rubricFailNotice(reasons)
			spec.Sink.TraceDelta("🧪 RUBRIC ✗ salvage 产物未过：" + traceReason(reasons) + "（撤回未达标产物，交诚实失败）")
			slog.Warn("turn: rubric-armed salvage product failed the gate — withholding ungrounded product", "sid", spec.Sid)
		}
	}
	switch {
	case !fromModel:
		reply = actionBrief(st, turnLang(spec)) // tier 2 → tier 3 (stock floor inside)
	case !blocked:
		// A real model answer gets the process-level honest closing appended; the
		// lower tiers and the rubric-fail notice are self-contained and don't double up.
		reply += honestClosing(st.exit.state)
	}
	// §6.3.4: a salvaged turn is exactly where the model is most likely to have left a
	// side-effect failure unacknowledged — append the honest footer here too (on
	// whichever text ships, including a withheld product's fail notice).
	reply += sideEffectFooter(st)

	// ctx here is salvage's own fresh short-timeout token (live), so finalizeTurn
	// persists on it directly — same single terminal tail as every other door. The
	// salvage reply was never streamed, so persist and finish are the same text
	// (Finish REPLACES the window — required here: a degen turn's streamed garbage
	// must be retracted, not extended).
	ferr := c.finalizeTurn(ctx, spec, reply, reply)
	// A salvage (or a withheld rubric-armed product) is a degraded outcome — the flow's
	// failure-learning keys on this, and a delegating parent marks its product partial.
	// RejectedProduct (non-empty only on a rubric-withheld product) rides along so the
	// failure learner sees the flawed product, not the apology delivered in Reply.
	// On a delivery FAILURE the user received nothing — surface Err so the turn isn't
	// booked as a satisfied/delivered outcome (this also suppresses flow/agora's
	// collectFailures, which is correct: nothing was delivered). (F107)
	res := contract.TurnResult{Reply: reply, Usage: st.usage, Outcome: contract.OutcomeDegraded, RejectedProduct: rejected}
	if ferr != nil {
		res.Err = fmt.Errorf("turn: salvage reply computed but delivery failed: %w", ferr)
	}
	return res
}

// salvageModelCall makes the salvage model call(s): the failed turn's history plus
// a by-exit-state nudge, NO tools. Salvage exemption (spec §6.7-2): no source hard
// tags — a readable line beats the tag contract once every normal path has failed.
// The degenerate path gets one extra tier first (§6.7-3): a strong `expert` model
// often completes straight off the gathered context. Returns "" so the caller drops
// to the no-LLM brief on error / empty / a salvage reply that ITSELF degenerated.
func (c *core) salvageModelCall(ctx context.Context, spec contract.TurnSpec, st *turnState, nudge string) string {
	msgs, err := c.buildMessages(ctx, spec, nil) // fresh read: salvage runs after the loop, past its per-round reads
	if err != nil {
		return "" // can't read history → no salvage; caller falls back to the no-LLM brief
	}
	// Fold the salvage nudge into the slot-0 system message (buildMessages keeps system
	// at position 0) rather than appending a TRAILING system message — a strict non-
	// Anthropic chat template (vLLM/llama.cpp) may reject a system message that isn't
	// leading, which would fail the salvage call and drop the user to the no-LLM brief.
	if len(msgs) > 0 && msgs[0].Role == "system" {
		msgs[0].Content = strings.TrimSpace(msgs[0].Content + "\n\n" + nudge)
	} else {
		msgs = append([]contract.Message{{Role: "system", Content: nudge}}, msgs...)
	}
	// §6.7-3 expert tier — degenerate only. (Per-session "gave up expert" marker is
	// deferred with the rest of §6.6's persistence; trying it once per degenerate
	// turn is the accepted cost until then.)
	if st.exit.state == exitDegenSalvage {
		if reply := c.salvageOne(ctx, spec, msgs, []string{"expert"}); reply != "" {
			return reply
		}
	}
	// Final tier rides the turn's own soft tags (ambient ["smart"] / session
	// prefs) instead of a bare request: msgs is the near-full history compacted
	// for THAT winner's window, so a bare pick landing on a high-priority
	// small-window entry would 400 and drop the user to the no-LLM brief —
	// systematically, on exactly the long sessions salvage exists for.
	return c.salvageOne(ctx, spec, msgs, spec.ModelReq.Prefer)
}

// salvageOne runs one no-tool salvage call with optional preferred tags, returning
// "" on error, empty, or a reply that itself degenerated (§6.6③: the salvage model
// can fall into the same wall).
func (c *core) salvageOne(ctx context.Context, spec contract.TurnSpec, msgs []contract.Message, prefer []string) string {
	resp, err := c.pool.Chat(ctx, contract.ModelRequest{Kind: contract.KindLLM, Prefer: prefer}, msgs)
	if err != nil {
		slog.Warn("turn: salvage model call failed", "sid", spec.Sid, "prefer", prefer, "err", err)
		return ""
	}
	out := strings.TrimSpace(resp.Content)
	if out == "" || isDegenerate(out) {
		return ""
	}
	return out
}

// salvageNudge picks the instruction by exit state — all share the core: answer
// honestly from existing tool results, no fabrication, no more tools.
func salvageNudge(state exitState) string {
	const base = "禁止再调用任何工具。只基于上面对话里已有的工具结果，诚实作答，不要编造你没有的信息。"
	switch state {
	case exitIterCap:
		return "你已多次尝试仍未给出最终答复。" + base +
			"告诉用户：你了解到什么、卡在哪、为什么；数据不足就如实说，让用户决定下一步。"
	default:
		return base + "把已有的进展归拢成一个对用户有用的答复。"
	}
}

// honestClosing appends a process-level note by exit state — process-level honesty
// (the turn hit a limit), NOT a semantic claim of completion.
func honestClosing(state exitState) string {
	switch state {
	case exitIterCap:
		return "\n\n（这次在多轮尝试后收尾，可能不完整——回复「继续」我可以接着处理。）"
	default:
		return ""
	}
}

// actionBrief is the no-LLM tier: name the distinct tools this turn ran so the user
// at least knows what was attempted. Empty trail → the stock floor (§6.7-5),
// rendered in the turn's notice language (turnLang).
func actionBrief(st *turnState, lang string) string {
	names := uniqStrings(st.toolsRun)
	if len(names) == 0 {
		return i18n.T(salvageStockKey, lang)
	}
	return fmt.Sprintf("本次调用了 %s，但没能给出完整结果。请再试一次，或换个说法告诉我你的目标。",
		strings.Join(names, "、"))
}

// uniqStrings returns the distinct elements of s, preserving first-seen order.
func uniqStrings(s []string) []string {
	seen := make(map[string]bool, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
