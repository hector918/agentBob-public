package core

// Session end-reason constants. Stored in sessions.end_reason via
// SessionStore.EndSession(ctx, sid, reason). One canonical string per
// reason so observers / admin tooling / `bob sessions --filter
// end_reason=X` reads see a stable vocabulary.
//
// Existing strings kept as-is for backward compat with rows already in
// SQLite / Postgres (pre-this-commit deployments). New code SHOULD use
// the constants; legacy call sites are documented alongside but not
// mass-rewritten (a no-op refactor risks merge conflicts vs other
// in-flight branches and the string values are identical).
//
// Categories:
//
//   - user-initiated: user_close / all_close (/close-session)
//   - safety guard: iter_cap / stuck_loop / url_exhausted / domain_stuck
//   - infra: ctx_cancelled / generation_rotate (compression)
//   - validation (LEGACY): validation_failed / validation_skipped /
//     judge_unavailable — set by the agora session-validation /
//     resume-watcher lifecycle that was removed in the chat-scoped rewrite
//     (docs/agora.md §一·五). No current call site stamps these; kept only
//     so admin filters / old store rows carrying the strings stay valid.
//   - model-initiated (LEGACY): finalized — the finalize_session tool and
//     its pending_finalize_at / sweep-finalize machinery were removed in
//     the same rewrite. No current call site; string kept for old rows.
//   - rotation: new (set by NewGeneration when retiring the prior row)
const (
	// EndReasonUserClose — /close-session on a single session_scope.
	// Legacy string in pipeline-gateway/40-slash.go:612: "closed_by_user".
	EndReasonUserClose = "closed_by_user"
	// EndReasonFinalized — LEGACY: the finalize_session tool and the
	// pending_finalize_at / sweep-finalize machinery that stamped this
	// were deleted in the chat-scoped rewrite. No current call site sets
	// it; the string is preserved only so old store rows / admin filters
	// carrying "finalized" stay valid.
	EndReasonFinalized = "finalized"
	// EndReasonIterCap — bestEffortFallback exited the loop because
	// iter cap was reached without any TurnExit being set. Maps from
	// Flow35ExitIterCap.
	EndReasonIterCap = "iter_cap"
	// EndReasonStuckLoop — turnlc safety guard: same (tool, args)
	// repeated past SameToolArgsMax. Maps from Flow31ExitStuckLoop.
	EndReasonStuckLoop = "stuck_loop"
	// EndReasonURLExhausted — maps from Flow32ExitURLExhausted. The flow
	// state + salvage routing remain wired, but no turnlc guard currently
	// stamps it (the per-(url,mode) guard was removed; domain-stuck /
	// Flow33 is the live turnlc-side fetch guard).
	EndReasonURLExhausted = "url_exhausted"
	// EndReasonDomainStuck — turnlc safety guard: same host failed
	// past SameDomainFailMax across modes. Maps from Flow33ExitDomainStuck.
	EndReasonDomainStuck = "domain_stuck"
	// EndReasonDegenerate — the streamed final reply degenerated into a
	// repetition loop (a wall of one repeated rune, or a tiny-alphabet
	// short cycle). streamFinalReply detects it mid-stream, cancels the
	// model, discards the garbage, and ends the turn with this reason.
	EndReasonDegenerate = "degenerate"
	// EndReasonCtxCancelled — turn ctx was cancelled (gateway shutdown,
	// user /close-session race, parent ctx tree torn down).
	EndReasonCtxCancelled = "ctx_cancelled"
	// EndReasonGenerationRotate — compression rolled the session to a
	// new generation; the prior row gets closed with this. Legacy
	// string in store/sqlite/30-sessions.go:124 + store/postgres/30-sessions.go:122: "new".
	EndReasonGenerationRotate = "new"
	// EndReasonValidationFailed — LEGACY: terminal fail from the removed
	// agora session-validation lifecycle (deleted in the chat-scoped
	// rewrite). No current call site sets it; the string is preserved only
	// so old store rows / admin filters carrying "validation_failed" stay
	// valid.
	EndReasonValidationFailed = "validation_failed"
	// EndReasonValidationSkipped — LEGACY: same removed lifecycle (judge
	// skipped). No current call site; string preserved for old rows.
	EndReasonValidationSkipped = "validation_skipped"
	// EndReasonJudgeUnavailable — LEGACY: the removed resume-watcher parked
	// a session whose judge model was unreachable, then gave up. No current
	// call site sets it (the validation / suspend-resume apparatus was
	// deleted in the chat-scoped rewrite — docs/agora.md §一·五); the string
	// is preserved only so old store rows carrying "judge_unavailable" keep
	// a stable filter vocabulary.
	EndReasonJudgeUnavailable = "judge_unavailable"
	// EndReasonToolRequested — a tool returned ExitRequest with
	// State=Flow34ExitToolRequested (clarify-style "ask user and stop").
	// Distinct from EndReasonUserClose so admin filters can separate
	// model-driven clarify exits from user-initiated /close-session.
	EndReasonToolRequested = "tool_requested"
	// EndReasonSubTask — a delegated sub-session was closed by its
	// sub-loop driver (product delivered OR honest partial product on
	// iter cap / timeout / panic — docs/subagent.md D3). Orphaned sub
	// rows (process crash mid-loop) fall to the stale-session sweep
	// instead and carry its reason.
	EndReasonSubTask = "sub_task"
)
