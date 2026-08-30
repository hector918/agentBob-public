package turn

import (
	"context"
	"log/slog"
	"strings"

	"agentbob/contract"
)

// This file is the nudge system (spec §6.3) + model-routing tags (§6.10). Both kinds
// of nudge are applied at the SINGLE point — the loop top — but they are different
// KINDS of thing and no longer share a lane:
//
//   - HARNESS nudges (the staleness ladder below) are machine-generated steering
//     text. They ride a transient "nudge" prompt layer, never persisted (I1), and
//     fire on STALENESS, not raw iter: a turn still producing new tool info is never
//     nagged, however deep.
//   - The USER nudge (foldUserNudge) is a human's message that arrived while the
//     turn was running. It is conversation CONTENT, so it is persisted as a user row
//     and enters through history — putting it on the transient steering lane is what
//     made it evaporate unanswered.
//
// Routing tags bias the model pick on tool rounds.
//
// IN SCOPE: §6.3.1 stuck (switch-family / give-up) + §6.3.2 progress-budget (menu →
// grace → STUCK_LOOP exit). The §6.3.4 side-effect footer lives elsewhere
// (ToolSpec.SideEffect + the 70-guards tracking → sideEffectFooter). DEFERRED:
// §6.3.3 act-or-confirm (needs a scene fence + production declaration the rebuild
// lacks, else it taxes every research turn).

// The nudge ladder is PURE staleness — no tier reads raw iter — so every loop
// driver shares it unchanged. 9/11 preserve the historical first-fire points of
// the old `stale>2 && iter≥8/10` gates on a never-progressed turn (watermarks
// start at -1, so stale = iter+1 there); a turn that progressed first now earns
// the same staleness grace instead of being told to give up three stale rounds
// after its last success. The §6.10 bias keys on failedSinceProgress — pace-
// invariant like the ladder; see turnState (30-state.go).
const (
	switchFamilyStale = 9  // §6.3.1: stale (no-progress) rounds → "try a different approach"
	giveUpStale       = 11 // §6.3.1: stale rounds → "stop calling tools, answer honestly"
	loopBudget        = 12 // §6.3.2: stale ≥ this → no-progress menu
	budgetGrace       = 2  // §6.3.2: extra rounds after the menu before the exit
	smartEscalation   = 4  // §6.10: failures since last progress → also prefer "smart"
)

// userNudgeMark annotates a busy-arrived message folded into a running turn. It is
// a FACT about the message ("the user said this while you were working"), not an
// instruction — so it belongs in the row's content, where it stays true forever.
// The behaviour it should produce ("answer it in the final reply") is a standing
// instruction and lives in the discipline layer (flow/compose.LoopingDiscipline);
// mixing the two would bake a directive into permanent history. PROMPT string
// (code, Chinese — not a user-facing notice, so not in i18n).
const userNudgeMark = "[工作途中追加] "

// foldUserNudge implements the user-nudge fast-lane: it pulls AT MOST ONE busy-
// arrived message (spec.NextNudge, wired only for non-auto sessions) and folds it
// into the conversation as a REAL user message — persisted as a user row, then
// spliced onto THIS round's replay so the current round already sees it.
//
// It used to inject the text into a transient "user_nudge" PROMPT layer instead.
// Two things were wrong with that, and they compounded:
//
//   - Position. A human's question sat in the place a model reads as standing
//     background, competing with the discipline layer that tells it not to break
//     off mid-task — and, on a long turn, with a context window full of tool
//     output. The model is far likelier to answer what arrives as the newest USER
//     message than what arrives as another paragraph of system prompt.
//   - No record. The message reached no store at all (queued ≠ in-flight, so no WAL
//     row either — 55-queue.go), while takeNudge had already consumed it from the
//     pending queue. A model that simply didn't address it destroyed it: no answer,
//     no history row, no re-queue, and the next turn had no idea it was ever said.
//
// Persisting it fixes both halves at once. Note this is the SECOND fix at this
// seam — the layer was first shown for one round then ejected, which lost the
// message before the final reply; making it persist for the whole turn did not
// help, which is what says the problem was never how LONG it was shown.
//
// spec.UserText is extended too: the acceptance gates (rubric / advisor delivery
// review, 85-rubric.go) render UserText as 「用户的请求」, so a nudge left out of it
// is invisible to the reviewer — which would grade an answer to it as off-topic.
// Bounded, and stated so it isn't a silent cap: those gates truncate the rendered
// ask to rubricAskCap bytes keeping the HEAD, so an ORIGINAL ask already past that
// cap can still push the appended nudge out of the reviewer's view. The history row
// and what the model reads are unaffected — only the reviewer's copy of the ask.
//
// Returns the replay to hand the round: the nudge appended when one was pulled. A
// nil replay stays nil — the round then reads history fresh and picks up the row
// we just wrote. st.userNudge is both the record (salvage re-reads it) and the
// one-shot guard: a pulled nudge is never empty, so a non-empty field means "this
// turn already took its one".
func (c *core) foldUserNudge(ctx context.Context, spec *contract.TurnSpec, st *turnState, replay []contract.Message) []contract.Message {
	if spec.NextNudge == nil || st.userNudge != "" {
		return replay
	}
	um := spec.NextNudge()
	if um == nil || strings.TrimSpace(um.Text) == "" {
		return replay
	}
	text := userNudgeMark + um.Text
	st.userNudge = text
	// The durable copy. Fail-soft: losing the row must not also lose the message from
	// THIS turn, so the replay splice below runs either way.
	if err := c.store.AppendUserMsgs(ctx, spec.Sid, []contract.UserMsg{{Author: um.Author, Text: text}}); err != nil {
		slog.Warn("turn: could not persist a nudged user message — it reaches this turn but not history", "sid", spec.Sid, "err", err)
	}
	spec.UserText = strings.TrimSpace(spec.UserText + "\n\n" + text)
	if replay == nil {
		return nil
	}
	// Chronologically correct: replay was read at loop top, so the previous round's
	// assistant+tool rows are already complete and this appends after them — no
	// tool-call pairing is split (I2/I3), and renderSpeakers attributes it from Author
	// like any other user row.
	return append(replay, contract.Message{
		Role:    "user",
		Content: text,
		Author:  um.Author,
		RowKind: contract.RowKindMsg,
	})
}

// staleRounds is the §6.4 staleness axis: rounds since either progress watermark
// last advanced (watermarks start at -1, so a never-progressed turn reads iter+1).
// The single definition every loop-top consumer shares — the nudge ladder and the
// advisor's stall trigger (16-looping-tools.go) must not drift apart.
func staleRounds(st *turnState, iter int) int {
	return iter - max(st.lastProgressIter, st.lastProductiveIter)
}

// loopTopNudge pushes the appropriate nudge into the prompt's transient layer for
// THIS round, and returns a STUCK_LOOP exit when the progress budget is fully dry.
// Computed from the two progress axes (§6.4); nothing fires while the turn advances.
func (c *core) loopTopNudge(spec contract.TurnSpec, st *turnState, iter int) *turnExit {
	stale := staleRounds(st, iter)

	// §6.3.2 budget dry: menu had its grace and nothing moved → end → salvage. This
	// is a safety guard, independent of prompt presence — computed before the nil
	// guard so a prompt-less flow still gets the stuck-loop exit.
	if stale >= loopBudget+budgetGrace {
		if spec.Prompt != nil {
			spec.Prompt.SetLayer("nudge", "")
		}
		return &turnExit{state: exitStuckLoop} // detail unused on salvage-bound exits
	}

	if spec.Prompt == nil {
		return nil // no layer to push nudges into
	}
	nudge := ""
	switch {
	case stale >= loopBudget:
		nudge = noProgressMenuNudge // §6.3.2 menu (more specific than the generic stuck text)
	case stale >= giveUpStale:
		nudge = giveUpNudge // §6.3.1 higher tier wins
	case stale >= switchFamilyStale:
		nudge = switchFamilyNudge
	}
	spec.Prompt.SetLayer("nudge", nudge) // "" clears it (the layer drops from Build)
	// From the give-up tier on, the harness has ruled that this turn stops working and
	// starts answering ("禁止再调用工具"). A parked advisor diagnosis says the opposite
	// — "here's what to try next" — and sits LATER in the prompt (its layer was created
	// later), so it would read as the more recent instruction. Drop it: the consult had
	// its chance five rounds ago. No-op on a regular turn / a turn that never consulted.
	if stale >= giveUpStale {
		spec.Prompt.SetLayer(advisorNoteLayer, "")
	}
	return nil
}

// routeToolRound joins (does not replace) the §6.10 routing tags onto a tool round's
// preferences: a tool-calling-stable model, escalating to a smart model after
// repeated tool failures this turn. Soft (Prefer) — a missing tag just doesn't bias.
func routeToolRound(req contract.ModelRequest, st *turnState) contract.ModelRequest {
	req.Prefer = appendPrefer(req.Prefer, "toolcall")
	if st.failedSinceProgress >= smartEscalation {
		req.Prefer = appendPrefer(req.Prefer, "smart")
	}
	return req
}

func appendPrefer(prefer []string, tag string) []string {
	for _, p := range prefer {
		if p == tag {
			return prefer
		}
	}
	// Clone before appending: req.Prefer shares its backing array with the flow's
	// TurnWork.PreferredModelTags (spec.ModelReq is a shallow copy). A bare append
	// could write past len into that shared array; route tags must never mutate a
	// flow-owned slice.
	out := make([]string, len(prefer), len(prefer)+1)
	copy(out, prefer)
	return append(out, tag)
}

// Nudge texts. All wrapped in 内部 markers (G4) so a small model won't paste them
// verbatim into the user-facing reply. Kept generic — the rebuild has no tool
// families to name (the skeleton's scrapling→browser hints don't apply here).
const (
	switchFamilyNudge = "[内部 — 不要引用进回复] 你已多次调用工具仍未给出答案——换个思路或换个工具，" +
		"别在同一个方法上死磕；看到失败结果带 hint 字段时按 hint 指向走。"

	giveUpNudge = "[内部 — 不要引用进回复] 你已多次失败，**禁止再调用工具**。" +
		"立刻基于上面已有的工具结果给用户最诚实的答复：能告诉他什么、卡在哪、为什么；数据不足就如实说。"

	noProgressMenuNudge = "[内部 — 不要引用进回复] 你已连续多轮没有取得新进展，别再重复同样的尝试。" +
		"把已有的进展归拢；缺关键信息就用 clarify 问用户一个具体问题；否则基于现有结果给出最诚实的答复，说明卡在哪。"
)
