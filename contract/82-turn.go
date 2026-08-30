package contract

import "context"

// UserMsg is one inbound speaker's contribution to a turn: the composed text plus its
// author (sender) and the structured attachments it carried. The flow builds these from
// the event batch; the turn persists one user row each so per-speaker attribution — and
// the attachment record — survives in history. Attachments holds the placed (Path-set)
// attachments only; it is persisted as the row's JSONB so a later turn can reconstruct
// the conversation's attachment set without rescanning the space. NOT fed to the model
// (the model already sees the attachment list in Text); it is the tool/session-side index.
type UserMsg struct {
	Author      MsgAuthor
	Text        string
	Attachments []Attachment
}

// TurnSpec is the fully-composed turn a flow hands the turn core to run: the
// flow-filled prompt builder, the joined user input, the model selection, and
// the sink the reply streams to. The turn core owns everything else about
// running the turn (history, the model call, persistence); the flow only
// composes the spec.
//
// Prompt is the BUILDER, not a built string: the turn core re-Builds it each
// round so a multi-round turn picks up the growing history and (later) the
// round's nudges. The flow fills the layers once; the turn core decides when to
// render.
type TurnSpec struct {
	Sid      string // session id — history key + persistence target
	Scope    string // the turn's session scope — a scope-aware tool (send_message) uses it to resolve agora targets; "" outside a routed turn
	Prompt   Prompt // the flow-filled system-prompt builder; re-Built each round
	UserText string // the flow-joined user input (derived view: rubric / skill-learn / tool ctx; NOT the model input when UserMsgs is set)
	// Lang is the turn's REPLY language, resolved by the ingress cascade (admin
	// override → detect → per-sender memory). The turn core renders its own
	// exception-path notices (internal error / persist fail / degenerate) in it;
	// "" → the core falls back to detecting from UserText.
	Lang string
	// UserMsgs is the per-speaker breakdown of this turn's user input — one entry per
	// inbound event, each with its sender (MsgAuthor). The turn persists ONE user row
	// per entry (a group's speakers stay individually attributed), and the model sees
	// them rendered with a speaker prefix. Empty (sub-turn / non-event wake) → the turn
	// falls back to one unattributed row from UserText.
	UserMsgs []UserMsg
	// Agent is this turn's agent attribution (bob, or an agora member) stamped on the
	// assistant rows it persists. Zero → unattributed.
	Agent    MsgAuthor
	ModelReq ModelRequest
	Sink     Sink // the reply is streamed here and Finish-ed by the turn core
	// Tools is the turn's AUTHORIZED tool projection (the flow built it, already
	// gated). The turn core advertises Tools.Specs() to the model and dispatches
	// emitted calls via Tools.Lookup — it never judges. nil → no tools (the model
	// runs text-only, single-shot).
	Tools ToolSet
	// Channels is the flow-built, identity-bound channel opener (file/exec on a
	// space). The turn core relays it to tools via ToolContext; nil → no channels.
	Channels ChannelOpener
	// Credentials is the flow-built, identity-bound credential opener (the same
	// binding as Channels, viewed as a CredentialOpener). The turn core relays it
	// to tools via ToolContext so a credential-backed tool (wordpress) gets its
	// kind-unique client; nil → no warrant wired.
	Credentials CredentialOpener
	// Attachments is THIS turn's attachment bag (the flow builds it over the event
	// batch). The turn core relays it to tools via ToolContext so a media tool (ocr /
	// vision / epub) can resolve a model-blind reference to the file the user sent — the
	// binary sibling of UserText. nil → no bag wired.
	Attachments AttachmentSet
	// Skills is the turn's AUTHORIZED skill projection (the flow built it via
	// warrant). The turn core relays it to tools via ToolContext (skill_view reads
	// it); the flow itself injects auto-skill bodies into the prompt. nil → no skills.
	Skills SkillSet
	// IterCap overrides the per-turn round limit when > 0 (a sub-turn runs a tighter
	// cap than the main loop). 0 → the driver's default (regular: loopCap; looping:
	// the fuse cap).
	IterCap int
	// Mode selects the turn core's loop driver (docs/turn-driver-split.md). Consumed
	// EXACTLY ONCE at the core's entry dispatch — the round kernel and the guard
	// family never read it (they are pace invariants shared by every driver).
	Mode TurnMode
	// IsSub marks a delegated sub-turn — its tools get a refusing SubRunner so a sub
	// cannot delegate further (depth 1).
	IsSub bool
	// History is the READ-ONLY conversation-log view relayed to tools
	// (ToolContext.History). Set by the turn core only — the sub-runner points a
	// ShareHistory sub at its DELEGATOR's log (the advisor), and a compacted MAIN
	// turn gets its OWN log (the read-back, pull 化 P1). Flows leave it nil.
	// NOT a sub-turn discriminator — use IsSub for that.
	History HistoryReader
	// NextNudge, when non-nil, is the nudge fast-lane: at loop-top the turn pulls
	// ONE busy-arrived plain-text message forward into the current round, PERSISTS it
	// as a real user row and folds it into the round's replay — so it reaches the
	// model as the conversation's newest user message, not as prompt background.
	// nil → no nudge (auto sessions / sub-turns). Copied from TurnWork by the flow.
	// Each call pops the next eligible queued message (exactly-once) or returns nil.
	NextNudge func() *UserMsg
	// Caller is the flow-resolved sender identity (handles / company / room). The
	// turn core relays it verbatim into ToolContext for identity-aware tools
	// (recall_memory) — it never reads it itself. Zero value when the flow did not
	// fill it. See docs/cold-memory-trunk.md §4.
	Caller CallerIdentity
	// AdvisorCriteria is the flow-composed acceptance standard a delivery is judged
	// against by the harness-triggered ADVISOR (docs/advisor.md §4) — the editable
	// global baseline, $BOB_HOME/prompt/advisor.md. It reaches the REVIEW sub-turn's
	// brief and nothing else: not a main prompt layer (the judged model must not be
	// handed its own grading sheet, and a per-turn layer would cost tokens every
	// round), and not the stall-diagnosis brief either (a diagnosis is quoted verbatim
	// into the working model's prompt, which would leak it back the long way round).
	// "" → the advisor judges on its own general standard.
	AdvisorCriteria string
}

// TurnMode selects the turn core's loop driver — two exit-semantics policies over
// one shared round kernel + guard family (docs/turn-driver-split.md).
type TurnMode string

const (
	// TurnModeRegular ("", the zero value) is the budget-bounded reply loop: the
	// round budget is a legitimate exit — salvage answers honestly from what was
	// gathered when it runs dry.
	TurnModeRegular TurnMode = ""
	// TurnModeLooping is the convergence-driven work loop: the designed exit is a
	// converged product (claimed done + armed acceptance passed); the round budget
	// degrades to a fuse — hitting it is pathological, not a normal ending.
	TurnModeLooping TurnMode = "looping"
)

// TurnResult reports a completed turn. Reply is the full assistant text (already
// streamed to the sink); Err is a non-fatal run error (the turn core has already
// delivered a user-facing notice for it).
type TurnResult struct {
	Reply string
	Usage Usage
	Err   error
	// Outcome classifies HOW the turn ended — the single first-class signal the flow
	// reads, so the three distinct cases a flow must tell apart are no longer collapsed
	// into one overloaded "partial" bit (which conflated a deliberate process reply with
	// a clean answer, or with a degraded give-up). See TurnOutcome.
	Outcome TurnOutcome
	// RejectedProduct is the output a quality gate (the rubric) judged non-compliant
	// before declaring failure. Reply on such a turn is the stock apology the user
	// sees; the actual flawed product is held HERE so the failure learner reflects on
	// the real trajectory (docs/wip-skill-learn-reflect.md) instead of grading its own
	// apology. Empty on every clean / non-rubric turn.
	RejectedProduct string
}

// TurnOutcome is how a turn ended. It is the first-class classification driving every
// cross-layer decision: which gate applies (rubric judges PRODUCT outcomes), whether a
// flow counts the turn as a satisfied answer (OutcomeFinal only), whether agora feeds
// its failure-learners (OutcomeDegraded only), and what a delegating parent sees. The
// turn's detailed internal exit reason (exitState) maps onto this surfaced class.
type TurnOutcome int

const (
	// OutcomeFinal — a clean final answer (rubric-passed if a rubric was armed). The only
	// outcome that counts as a satisfied answer.
	OutcomeFinal TurnOutcome = iota
	// OutcomeProcess — a deliberate NON-answer: a tool ended the turn to ask the user a
	// question (clarify), hand off (escalate), or stay silent. Not a product, not degraded.
	OutcomeProcess
	// OutcomeDegraded — a best-effort give-up: salvage (iter-cap / degenerate / stuck) or a
	// rubric-armed product that never passed. The failure-learning + "partial product" case.
	OutcomeDegraded
	// OutcomeCancelled — user/system stop; nothing was delivered (Err carries the cancel).
	OutcomeCancelled
	// OutcomeError — a hard run error (Err set).
	OutcomeError
)

// Turn is the turn-execution mechanism — the turn core. It drives ONE bounded
// turn (the gear) through its lifecycle: read history, assemble the messages,
// call the model, stream the reply to the sink, persist the exchange. Bounded by
// construction — every driver runs under a hard round cap (regular: the loopCap
// budget; looping: the fuse, see TurnMode) and always converges to one reply.
// It reads + appends history via MessageStore (session owns the table).
// Registered on the trunk by the turn module; a flow Requires it and drives it.
//
// Runs the full loop: round iteration (model → tool dispatch → repeat), salvage,
// compaction, and the §6 Ring-1 hardening guards — all present. A flow drives the
// turn core; the core knows nothing about which flow drove it.
type Turn interface {
	Run(ctx context.Context, spec TurnSpec) TurnResult
}
