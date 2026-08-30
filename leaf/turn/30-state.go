package turn

import (
	"strings"

	"agentbob/contract"
)

// turnState is the per-turn volatile state (spec §3.5 / A6: core-private, no
// global registry — it lives and dies with one Run call): the exit marker,
// accumulated usage, the §6.4 progress axes, the §6.5 guard counters, the §6.8
// bloat ladder, §6.3.4 side-effect failure tracking, and the rubric gate.
type turnState struct {
	exit     *turnExit      // set by a guard/tool to end the turn; consumed at the loop top
	usage    contract.Usage // accumulated across the turn's rounds
	toolsRun []string       // names of tools dispatched this turn (for the no-LLM salvage brief §6.7-4)
	// userNudge is the busy-arrived message this turn folded in (80-nudge.go), ""
	// when none. The DRIVER extends its own spec.UserText copy, which the in-loop
	// acceptance gates read — but Run's teardown holds an un-extended copy, so
	// salvage re-applies it from here. State, not a second spec copy: st is the one
	// thing the driver and the teardown genuinely share.
	userNudge string

	// §6.4 progress predicate — the iter a tool last produced new info / delivered.
	// -1 = never. Two axes: progress ("advancing, don't nag") vs production
	// ("delivered, a trustworthy done").
	lastProgressIter   int
	lastProductiveIter int
	toolHashes         map[string]uint64 // per-tool last OK-result hash (new hash = progress)

	// §6.5 safety guards.
	lastCallKey   string         // (name|args) of the last successful call; "" resets the run
	sameCallCount int            // consecutive identical successful calls (§6.5.1)
	failStreak    map[string]int // consecutive failures per tool name (§6.5.2)
	// failedSinceProgress drives §6.10 smart escalation: tool failures since the
	// progress watermark last advanced — "struggling NOW", in the same staleness
	// vocabulary as the nudge ladder (pace-invariant: a long turn that recovers
	// sheds the smart bias instead of wearing it for life). Reset on progress
	// (70-guards.go) and on a rubric redo's fresh budget (the drivers).
	failedSinceProgress int

	// §6.8 bloat ladder: a tool round aborted because the model packed too much into
	// one tool_call's arguments. bloatTrips counts the aborts; past the cap, stripTools
	// drops tools from the round so the model is forced to answer in plain text.
	bloatTrips int
	stripTools bool

	// §6.3.4 side-effect failures: failed calls of a SideEffect tool, keyed by
	// (name|args) → the tool name (the footer brief). A later success of the SAME call
	// supersedes (deletes the key). Whatever remains at finish becomes an honest
	// "these did not succeed" footer — contradicting a "claimed delivered" reply.
	sideEffectFails map[string]string

	// rubric gate: a skill carrying a RUBRIC.md that the model engaged this turn (via
	// skill_view) arms its criteria here, keyed by skill name → rubric text. When
	// non-empty, the final-reply door judges the product against these before
	// delivering; a fail re-prompts the model. rubricRetries counts those redos.
	rubricArmed   map[string]string
	rubricRetries int
	// advisor (docs/advisor.md): armed by the LOOPING driver — the advisor is that
	// mode's standard equipment, and arming through state (not a spec.Mode read)
	// keeps the round kernel pace-invariant. advisorDiagnoses counts stall consults
	// (capped per turn); reviewRetries counts delivery redos the reviewer demanded;
	// advisorConcern holds the reviewer's LAST unresolved objection once the redo
	// budget is spent — it ships as an honest footer rather than silently vanishing
	// (the artifact already exists, so declaring failure would be its own lie).
	advisorArmed     bool
	produced         bool // a Delivers/SideEffect tool SUCCEEDED — this turn changed something outside the chat
	advisorDiagnoses int  // consults that produced real advice (capped)
	advisorAttempts  int  // consults DIALLED, including empty/partial ones (separately capped)
	reviewRetries    int
	advisorConcern   string

	// arbiterUsed caps the rubric gate's tiered escalation at ONE evidence-capable
	// second opinion per turn (85-rubric.go): the arbiter sub-turn is 10-100x a
	// blind judge call, and one criteria reading per turn is enough — later blind
	// fails in the same turn stand unescalated.
	arbiterUsed bool

	// sunk is the EXACT byte stream already handed to Sink.ContentDelta this turn —
	// tool-round preambles streamed live plus a live-streamed final reply. The Sink
	// contract (contract/50-sink.go) requires Finish's full to be the concatenation
	// of the ContentDeltas: a Finish whose full does not extend these bytes makes an
	// edit-stream sink that split mid-stream take its divergence branch and re-send
	// the whole reply from offset 0 (already-frozen chunks duplicated). Finish doors
	// whose text continues the stream compose it from this; NEVER trimmed — a frozen
	// split prefix must keep matching byte-for-byte.
	sunk strings.Builder
}

// newTurnState builds the per-turn state with the progress axes at -1 (never) and
// the guard maps ready.
func newTurnState() *turnState {
	return &turnState{
		lastProgressIter:   -1,
		lastProductiveIter: -1,
		toolHashes:         map[string]uint64{},
		failStreak:         map[string]int{},
		sideEffectFails:    map[string]string{},
	}
}

// turnExit is a trajectory marker for why a turn ended (spec §2.1). It is a
// label, not a switch key — the only thing the driver branches on is whether the
// state is exitCancelled (silent) vs everything else (salvage).
type turnExit struct {
	state  exitState
	detail string // exitToolRequested / exitRubricFailed: the user-facing reply
	// rejected (exitRubricFailed only) is the product the rubric judged non-compliant —
	// distinct from detail (the stock apology shipped to the user). Carried to
	// TurnResult.RejectedProduct so the failure learner reflects on the real flawed
	// output, not the apology.
	rejected string
}

type exitState int

const (
	// exitUnset deliberately holds the zero slot: a &turnExit{} that forgot to set
	// its state must NOT alias a real exit reason. exitCancelled used to sit at 0,
	// which made any future forgotten-state exit read as a silent cancel — skipping
	// salvage and returning nothing to the user. An unset exit now falls through
	// the driver's default (salvage → OutcomeDegraded): loud, not silent. Every
	// current construction site sets state explicitly; this is the guard rail.
	exitUnset         exitState = iota
	exitCancelled               // cancel signal (user /stop or system stop)
	exitIterCap                 // ran out of rounds
	exitToolRequested           // a tool asked to end the turn (tools: step 3)
	exitDegenSalvage            // degenerate output tripped salvage (§6.6)
	exitStuckLoop               // a safety/budget guard ended the turn (§6.5/§6.3.2)
	exitDomainStuck             // same-host cross-mode failure run (§6.5.3)
	exitRubricFailed            // a rubric-gated product never passed within the retry cap
)

// outcome maps the detailed internal exit reason onto the surfaced contract.TurnOutcome
// the flow reads. exitToolRequested is a deliberate process reply (clarify / escalate /
// stay_silent); exitCancelled is a stop; everything else (iter-cap / degenerate / stuck /
// domain → salvage, and rubric-failed) is a degraded give-up. A clean final answer is not
// an exitState — the driver sets OutcomeFinal at the roundFinal door directly.
func (s exitState) outcome() contract.TurnOutcome {
	switch s {
	case exitToolRequested:
		return contract.OutcomeProcess
	case exitCancelled:
		return contract.OutcomeCancelled
	default:
		return contract.OutcomeDegraded
	}
}

// roundResult is one round's single return channel (spec A2): exactly one kind,
// so the driver's branch order can't drift across separate signals.
type roundResult struct {
	kind roundKind
	text string    // roundFinal: the reply (already persisted + finished)
	exit *turnExit // roundExit: who requested the exit (already finished)
	err  error     // roundErr: the store/model error to surface
	// resetBudget (roundContinue only): an acceptance gate requested a redo, so the Run
	// loop grants the corrected-answer attempt a FRESH work budget (stale baselines +
	// hard iter cap) — the redo is a new attempt, not charged the rounds spent on the
	// rejected product. Two gates set it (they are mutually exclusive per delivery):
	// the rubric gate (maxRubricRetries) and the advisor review gate (maxReviewRetries,
	// looping only), so the cap can grow at most that many times per turn.
	resetBudget bool
	// Per-round usage is accumulated into turnState.usage in runRound; the driver
	// returns that. Nudges don't ride here — they are pushed at the loop top into a
	// transient prompt layer (80-nudge.go).
}

type roundKind int

const (
	roundContinue roundKind = iota // loop again (empty reply, or tools ran)
	roundFinal                     // a final reply was produced
	roundExit                      // a tool requested the turn's end
	roundErr                       // a store/model error to surface to the caller
	// roundOversized: the model rejected the prompt as too large for its context
	// window (pool ErrContextExceeded). The Run loop force-compacts history and
	// retries the round — or, if it can't shrink further, ends with an honest
	// "conversation too long" notice instead of a bogus model-unavailable one.
	roundOversized
)
