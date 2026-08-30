package core

// BusyTodoIDPrefix marks todos that the gateway auto-seeded when a
// new user message arrived while a turn was in flight (see
// pipeline-gateway's maybeSeedBusyTodo, design: docs/todo-design.md).
//
// These todos are user-facing only — they're a /todos visibility aid
// for what got queued, NOT something the model sees. TodosSource
// (system prompt renderer) skips items with this prefix, so the model
// never reads them and can't double-handle the message (it processes
// it via the normal pending → user text path). After the turn that
// processed the matching message completes, the gateway marks the
// busy-todo completed automatically.
//
// Double-underscore namespace (pythonic-style) chosen because it's
// unlikely to collide with names a model would pick organically. If
// a model DOES happen to supply a todo id starting with this prefix,
// the tool input validator silently renames it (prepending "_user-")
// rather than reject — semantically, the model's intent goes through
// + the namespace stays clean.
const BusyTodoIDPrefix = "__bob_busy__"

// RubricTodoIDPrefix marks the system-seeded RUBRIC "gate" todo that
// skill_view drops into a session the first time it loads a RUBRIC-
// bearing skill (see coretools/40-skills.go). One gate per loaded skill;
// its id is RubricTodoIDPrefix + skillName.
//
// Unlike BusyTodoIDPrefix, the gate is VISIBLE to the model — TodosSource
// renders it so the model knows it must satisfy the skill's RUBRIC and
// mark the gate completed (with evidence) before the session can be
// finalized. The gate is the single mechanical interlock that keeps a
// RUBRIC from being silently skipped when the model never opens a todo
// list of its own (the judge only ever sees claimed-completed todos, so
// without a seeded item there is nothing to judge against the RUBRIC).
//
// The gate is system-owned: computeNewList force-carries it across
// replace-mode writes (anti-delete), keeps its content fixed (the model
// can't rewrite the label), and refuses a model-driven cancel (no
// cancelling your way past the RUBRIC). The model CAN move it
// in_progress / completed (+evidence) — that's how it claims the work
// satisfies the RUBRIC, which the judge then verifies.
//
// Same double-underscore namespace as busy so an organic model-picked id
// won't collide. The gate is system-seeded ONLY: computeNewList renames
// any model-supplied rubric-prefixed id that doesn't match an existing
// gate out of the namespace (prepending "_user-"), so the model can
// reference the real gate (to mark it completed) but can never author a
// new / forged one.
const RubricTodoIDPrefix = "__bob_rubric__"

// TodoStatus is the lifecycle state of a single todo item.
//
// State machine (manual mode default):
//
//	pending ──► in_progress ──► pending_review ──► completed   (button: ✅ pass)
//	                                            └► failed      (button: ❌ reject; fail_count++)
//	                                                  │
//	(any state, anytime) ──► cancelled                ▼
//	                                          fail_count >= 3
//	                                          → escalated=true ("stuck")
//
// In auto mode: in_progress → completed (model self-marks) → judge runs
// → either stays completed (verified=true) or → failed (fail_count++).
//
// Stored as the lowercase string in todos_json and in the tool's
// args / returns.
type TodoStatus string

const (
	TodoPending       TodoStatus = "pending"
	TodoInProgress    TodoStatus = "in_progress"
	TodoPendingReview TodoStatus = "pending_review" // manual mode: waiting for human button click
	TodoCompleted     TodoStatus = "completed"
	TodoCancelled     TodoStatus = "cancelled"
	TodoFailed        TodoStatus = "failed"
)

// IsValid reports whether s is one of the recognised statuses. Used by
// the todo tool's input validation — an unknown status returns an Err
// envelope to the model so it can self-correct.
func (s TodoStatus) IsValid() bool {
	switch s {
	case TodoPending, TodoInProgress, TodoPendingReview, TodoCompleted, TodoCancelled, TodoFailed:
		return true
	}
	return false
}

// IsActive reports whether the todo should be injected into the system
// prompt's "Active TODOs" block. Completed and Cancelled drop out;
// everything else stays so the model keeps it on its radar.
func (s TodoStatus) IsActive() bool {
	switch s {
	case TodoCompleted, TodoCancelled:
		return false
	}
	return true
}

// TodoMode is the verification regime for a session's todo list.
//
//   - TodoModeManual (default): every completed todo lands in
//     pending_review and waits for a human button click. No external
//     judge model is called. Strongest "user-in-the-loop" mode.
//   - TodoModeAuto: completed todos are batched (every N) and sent to
//     a "judge"-tagged model for evidence verification. Pass → stays
//     completed; fail → status=failed, fail_count++. Spends judge
//     model budget but no human attention.
//   - TodoModeNone: pure honor system. Model says completed → it's
//     completed. No pending_review, no judge call. Lightest. Use when
//     you trust the model + the cost of an undetected false-positive
//     completion is low (single-user prototyping, low-stakes flows).
//
// Stored as the lowercase string in sessions.todo_mode.
type TodoMode string

const (
	TodoModeManual TodoMode = "manual"
	TodoModeAuto   TodoMode = "auto"
	TodoModeNone   TodoMode = "none"
)

// IsValid reports whether m is a recognised mode.
func (m TodoMode) IsValid() bool {
	return m == TodoModeManual || m == TodoModeAuto || m == TodoModeNone
}

// Todo is one item in a session's todo list.
//
// Author-supplied fields (id / content / status / evidence) come from
// the model's todo() tool args. Internal fields (created_by / verified
// / judged_at / fail_count / judge_reason / escalated) are maintained
// by the tool + judge subsystem and the model SHOULD NOT pass them in
// (they're ignored on input).
//
// Persistence: a session's full list is JSON-encoded into
// sessions.todos_json (one row per session); see store/30-sessions.go's
// GetTodos / SetTodos. Cross-generation carry (compression) is the
// agent's responsibility — the store does not auto-copy.
type Todo struct {
	// Author-supplied:
	ID       string     `json:"id"`
	Content  string     `json:"content"`
	Status   TodoStatus `json:"status"`
	Evidence string     `json:"evidence,omitempty"` // required when transitioning to completed

	// Internal (tool/judge maintained):
	CreatedBy     string  `json:"created_by,omitempty"`      // user_id of the creator (group-chat trace; required for the "only original requester or admin can review" gate)
	CreatedByName string  `json:"created_by_name,omitempty"` // display name at creation time (for prompt rendering; falls back to CreatedBy when empty — old rows pre-have only CreatedBy)
	MessageID     string  `json:"message_id,omitempty"`      // source message id this todo was seeded from (only set for busy-trigger todos; empty for model-created). Used by the gateway's auto-complete pass to identify "which queued user message did this turn just process", swapping fragile content-equality for stable id-equality.
	FailCount     int     `json:"fail_count,omitempty"`      // judge / human rejection count
	JudgeReason   string  `json:"judge_reason,omitempty"`    // last fail explanation
	Verified      bool    `json:"verified,omitempty"`        // judge / human approved (only meaningful for completed)
	JudgedAt      float64 `json:"judged_at,omitempty"`       // unix-seconds of the last judge run (or human review)
	Escalated     bool    `json:"escalated,omitempty"`       // true when fail_count >= StuckAfterFails — judge stops, human attention required

	// JudgeUnavailableRetries counts how many times the judge model
	// call returned "unavailable" for this todo while it was waiting
	// for verification. Used by the RUBRIC subsystem to pause-and-retry
	// naturally (without background goroutines): each retry stays in
	// needsJudge until either judge recovers (verdict applied) or this
	// counter hits N=3 (sustained unavailable → Escalated, unblock).
	// See docs/rubric-design.md §8.
	JudgeUnavailableRetries int `json:"judge_unavailable_retries,omitempty"`
}

// RubricSnapshot is the frozen-at-execution-time copy of a skill's
// RUBRIC.md body, captured the first time the skill is染色'd in a
// session. Insulates the session from later admin edits / file
// deletion / skill upgrades — judges always evaluate against the
// rubric content that was actually loaded for this session.
//
// Phase 7 replaces the prior "snapshot just the skill
// name, re-read RUBRIC.md at judge time" model. Reasoning:
//   - judge-time file I/O on every judge call is wasteful when the
//     session already saw the body at染色 time
//   - admin edits / skill rebuilds mid-session would silently shift
//     the verification criteria under a running todo loop
//   - file deletion would break the judge prompt entirely
//
// Idempotency: re-snapshotting the same skill name preserves the
// original body (frozen at first snapshot). This is the natural
// extension of §13 row 6 "染色不可逆" — the set is irreversible AND
// the per-skill content is immutable for the session's lifetime.
//
// See docs/rubric-design.md §5 (post-Phase 7 redesign).
type RubricSnapshot struct {
	Name       string `json:"name"`        // skill name
	Body       string `json:"body"`        // RUBRIC.md content at snapshot time
	SnapshotAt int64  `json:"snapshot_at"` // unix seconds
}
