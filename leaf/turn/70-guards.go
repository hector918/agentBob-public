package turn

import (
	"hash/fnv"
	"sort"
	"strings"

	"agentbob/contract"
)

// This file is the progress predicate (spec §6.4) + the safety guards (§6.5), both
// driven from one tool result in the tool-end processing. The predicate feeds the
// nudge thresholds (§6.3); the guards set st.exit when the model is clearly spinning
// (consumed at the next loop top — the round's other results still persist).
//
// The domain-stuck guard (§6.5.3, fetch-family by host) is intentionally absent:
// the rebuild has no fetch-family tool yet for it to key on. It lands with such a
// tool (and a host-declared family), keyed on EXIT_DOMAIN_STUCK.

const (
	sameArgsMax = 3 // §6.5.1: consecutive identical SUCCESSFUL calls before STUCK_LOOP
	sameFailMax = 6 // §6.5.2: consecutive failures of ONE tool (args-agnostic) before STUCK_LOOP
)

// recordToolOutcome updates the progress axes (§6.4) and the safety guards (§6.5)
// from one result. A success that yields a new result hash marks progress; a
// delivery tool's success marks production; identical successful repeats or a long
// failure streak trip a STUCK_LOOP exit.
func (c *core) recordToolOutcome(st *turnState, iter int, spec contract.TurnSpec, call contract.ToolCall, res contract.ToolResult) {
	// §6.3.4 side-effect tracking: a failed SideEffect call is remembered; the SAME
	// call (name|args) succeeding later supersedes it. Independent of the progress /
	// guard bookkeeping below.
	if c.hasSideEffect(spec, call.Name) {
		key := call.Name + "|" + call.Arguments
		if res.OK {
			delete(st.sideEffectFails, key)
		} else {
			st.sideEffectFails[key] = call.Name
		}
	}
	// "This turn changed something outside the conversation" — the advisor review
	// gate's arm condition (docs/advisor.md §4). A SUCCEEDED delivery or side-effect
	// call is exactly that: a file handed over, a message sent, an arrangement
	// created. Tracked as its own flag rather than read off lastProductiveIter,
	// which an acceptance-gate redo legitimately back-dates to the current iter
	// (the drivers' resetBudget) and would fake this predicate into being true.
	if res.OK && (c.delivers(spec, call.Name) || c.hasSideEffect(spec, call.Name)) {
		st.produced = true
	}
	if res.OK {
		// §6.4 progress: a result whose hash differs from this tool's last = new info.
		// (Volatile fields like a duration would defeat this; the rebuild's results
		// carry none yet — normalize here if one is ever added.)
		if h := hash64(res.Data); st.toolHashes[call.Name] != h {
			st.toolHashes[call.Name] = h
			st.lastProgressIter = iter
			st.failedSinceProgress = 0 // §6.10: the turn recovered — shed the smart bias
		}
		if c.delivers(spec, call.Name) { // production also counts as progress
			st.lastProductiveIter = iter
			st.lastProgressIter = iter
			st.failedSinceProgress = 0
		}
		// §6.5.1 same-call repeat: consecutive identical successful calls; any
		// different call (or a failure) resets the run.
		key := call.Name + "|" + call.Arguments
		if key == st.lastCallKey {
			st.sameCallCount++
		} else {
			st.lastCallKey, st.sameCallCount = key, 1
		}
		st.failStreak[call.Name] = 0 // a success clears this tool's failure streak
		if st.exit == nil && st.sameCallCount > sameArgsMax {
			st.exit = &turnExit{state: exitStuckLoop} // detail unused on salvage-bound exits
		}
		return
	}
	// Failure: §6.5.2 consecutive-fail, aggregated by NAME (varying args to bombard
	// the same broken tool must still count).
	st.lastCallKey, st.sameCallCount = "", 0
	st.failedSinceProgress++ // §6.10 smart-escalation counter (reset on progress above)
	st.failStreak[call.Name]++
	if st.exit == nil && st.failStreak[call.Name] > sameFailMax {
		st.exit = &turnExit{state: exitStuckLoop} // detail unused on salvage-bound exits
	}
}

// delivers reports whether the tool declared itself a production/delivery tool.
func (c *core) delivers(spec contract.TurnSpec, name string) bool {
	if spec.Tools == nil {
		return false
	}
	t, ok := spec.Tools.Lookup(name)
	return ok && t.Spec().Delivers
}

// hasSideEffect reports whether the tool declared itself a verifiable-side-effect
// tool (§6.3.4 — its failure must surface in the reply footer).
func (c *core) hasSideEffect(spec contract.TurnSpec, name string) bool {
	if spec.Tools == nil {
		return false
	}
	t, ok := spec.Tools.Lookup(name)
	return ok && t.Spec().SideEffect
}

// sideEffectFooter renders the §6.3.4 honest footer from the un-superseded
// side-effect failures, or "" when there are none. Tool names are deduped + sorted
// for a stable line.
func sideEffectFooter(st *turnState) string {
	if len(st.sideEffectFails) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var names []string
	for _, name := range st.sideEffectFails {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return "\n\n（⚠️ 注意：以下操作未成功执行：" + strings.Join(names, "、") + "）"
}

func hash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
