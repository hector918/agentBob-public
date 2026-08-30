package turn

import (
	"context"

	"agentbob/contract"
	"agentbob/i18n"
)

// This file is the REGULAR loop driver (docs/turn-driver-split.md): the
// budget-bounded reply policy over the shared round kernel (20-round.go) and
// guard family. The round budget is a legitimate exit here — when it runs dry
// the shared teardown in Run salvages an honest answer from what was gathered.
// The loop body was cut verbatim from Run (10-core.go) at the driver split;
// exit-semantics changes belong HERE, kernel/guard changes never do.

// driveRegular runs the budget-bounded round loop. It returns (result, true)
// when the turn ended through an in-loop door (final reply / tool exit / error /
// context-too-long); (zero, false) when the loop exhausted its budget or a
// guard set st.exit — the caller's shared teardown (iter-cap default → cancel
// door → salvage) then finishes the turn.
func (c *core) driveRegular(ctx context.Context, spec contract.TurnSpec, st *turnState) (contract.TurnResult, bool) {
	limit := loopCap
	if spec.IterCap > 0 {
		limit = spec.IterCap // a sub-turn runs a tighter cap (§7)
	}
	base := limit // the per-attempt budget; a rubric redo grants another `base` (below)
	for iter := 0; iter < limit; iter++ {
		if ctx.Err() != nil {
			st.exit = &turnExit{state: exitCancelled}
			break
		}
		if st.exit != nil { // a guard/tool set the exit last round (tools: step 3)
			break
		}
		// §6.9 in-place compaction (best-effort) before the round reads history. Its
		// GetReplay is handed to the round for reuse (one history read per round);
		// nil when it compacted / couldn't read — the round then reads fresh.
		replay := c.maybeCompressInPlace(ctx, spec)
		// Compacted session → arm the own-log read-back (pull 化 P1): the summary
		// is a table of contents; read_conversation_log pulls originals back.
		c.maybeArmHistoryTool(&spec, replay)
		// §6.3 loop-top nudge push + progress-budget exit (computed from §6.4 axes).
		if ex := c.loopTopNudge(spec, st, iter); ex != nil {
			st.exit = ex
			break
		}
		// User-nudge fast-lane: pull at most ONE busy-arrived message into this turn
		// (non-auto sessions only — auto leaves NextNudge nil). Persisted as a real
		// user row and spliced onto this round's replay, so it reaches the model as
		// the conversation's newest user message (80-nudge.go). One-shot per turn,
		// tracked on st.
		replay = c.foldUserNudge(ctx, &spec, st, replay)
		r := c.runRound(ctx, spec, st, iter, replay)
		switch r.kind {
		case roundFinal:
			return contract.TurnResult{Reply: r.text, Usage: st.usage, Outcome: contract.OutcomeFinal}, true
		case roundExit:
			// Outcome derives from the exit reason: a tool exit (clarify/escalate/silent) is
			// OutcomeProcess; a rubric-failed exit is OutcomeDegraded (see exitState.outcome).
			return contract.TurnResult{Reply: r.exit.detail, Usage: st.usage, Outcome: r.exit.state.outcome(), RejectedProduct: r.exit.rejected}, true
		case roundErr:
			// A mid-stream cancel surfaces here as a context error; keep the cancel
			// semantics honest (OutcomeCancelled, like every other cancel door) rather
			// than collapsing it into OutcomeError — so a future consumer / telemetry can
			// tell a stop apart from a hard failure (L-D1 turn). Gate on the TURN ctx
			// (ctx.Err()), not errors.Is on the error chain: a provider's own hardTimeout
			// (an inner ctx) surfaces a wrapped DeadlineExceeded even though THIS turn was
			// never cancelled — that is a backend hang (OutcomeError), not a user stop
			// (F106). Same disambiguation as the pool's recordError.
			if ctx.Err() != nil {
				return contract.TurnResult{Err: r.err, Usage: st.usage, Outcome: contract.OutcomeCancelled}, true
			}
			return contract.TurnResult{Err: r.err, Usage: st.usage, Outcome: contract.OutcomeError}, true
		case roundOversized:
			// The pool rejected the prompt as too large for the model's context window.
			// Force-compact history and retry the round on the shrunk window.
			if !c.forceCompact(ctx, spec) {
				// Can't reduce further — the recent tail alone exceeds the window. Tell
				// the user honestly rather than silently failing as "model unavailable".
				// Nothing was delivered as an answer (OutcomeDegraded), but the notice IS
				// put on the wire, and finalizeTurn also persists it as an assistant row so
				// history matches what the user saw and the tool sequence is closed.
				notice := i18n.T(contextTooLongKey, turnLang(spec))
				_ = c.finalizeTurn(ctx, spec, notice, notice)
				return contract.TurnResult{Usage: st.usage, Outcome: contract.OutcomeDegraded}, true
			}
			continue // retry the round on the compacted history (loop top re-reads)
		case roundContinue:
			if r.resetBudget {
				// An acceptance gate requested a redo → fresh work budget for the
				// corrected attempt: reset the stale baselines (and the §6.10 struggle
				// counter — its "since progress" invariant follows the watermarks) and
				// extend the hard cap by one `base`. Bounded — only a rubric redo rides
				// this on a REGULAR turn (the advisor review gate is looping-only),
				// capped at maxRubricRetries.
				st.lastProgressIter, st.lastProductiveIter = iter, iter
				st.failedSinceProgress = 0
				limit = iter + 1 + base
			}
			continue
		}
	}
	return contract.TurnResult{}, false
}
