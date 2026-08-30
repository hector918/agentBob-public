package core

import (
	"context"
	"time"
)

// SessionInfo is a summary row for listing sessions.
type SessionInfo struct {
	ID           string
	Key          string // routing key, e.g. "telegram:dm:123"
	Source       string // "local" | "telegram" | ...
	UserID       string
	Model        string
	ParentID     string  // delegating parent sid (kind='sub' rows only post-A5; /new never set it), or ""
	CallerID     string  // agora dispatch caller sid (send_message force_new worker rows), or "" — docs/agora-delegation-return.md D1
	Title        string  // "" if untitled
	StartedAt    float64 // unix seconds
	EndedAt      float64 // 0 if still active
	MessageCount int
	Preview      string // first user message, truncated
}

// PendingChat identifies a chat that still had unanswered (accepted-but-not-yet-
// replied) messages when the gateway stopped — used to send a "please re-send" notice
// on the next startup.
//
// MsgIDs is the snapshot of the chat's pending msg_ids at ListPendingChats
// time. Post-notice cleanup deletes exactly these rows — rows written AFTER
// the snapshot (new messages accepted while the notice was in flight) keep
// their crash-recovery protection.
type PendingChat struct {
	Source   string
	ChatID   string
	ThreadID string
	MsgIDs   []string
}

// SessionStore persists sessions and their message history, the message↔session
// index, and the pending-inbound list. Built as a backend-agnostic interface:
// implementations live under store/sqlite/ (single-process), store/postgres/
// (shared/HA), and store/fallback/ (transparent pg→sqlite failover wrapper).
// Selection happens once at startup via store.OpenFromConfig — see
// docs/store-dual-backend.md.
//
// Sibling interfaces (added when each feature lands, all satisfied by the
// concrete impl):
//   - ChatStateStore  — active mode/model/skills per session_scope (schema v4)
//   - ConsentStore    — pending consent_requests (schema v5)
//   - MemoryStore     — read/write of $BOB_HOME/memory/* layer files
//   - ModelStateStore — model_usage + model_runtime_state (schema v7)
//
// SessionArchiver moves cold sessions (last message older than cutoff) out of
// the live tables into a single blob archive table, keeping the hot path small.
// Optional sibling of SessionStore — the turn-lifecycle sweep type-asserts for
// it, so backends that don't implement it simply skip archival. See
// docs/group-session-archive.md (Part 2). The archive payload holds the
// conversation (messages + light session meta), NOT turn_events telemetry.
type SessionArchiver interface {
	// ArchiveSessionsBefore archives every session (dead OR alive) whose
	// most-recent message (COALESCE(MAX(messages.timestamp), started_at)) is <
	// cutoff: serialize → insert into the archive table → delete the live
	// session, its messages and its turn_events. message_index is KEPT (so a
	// reply can locate & cold-revive a was-alive archived session). Also evicts
	// each archived session's per-sid todo lock. Idempotent and crash-safe
	// (insert-then-delete; a re-run re-archives harmlessly). Returns the count.
	ArchiveSessionsBefore(ctx context.Context, cutoff time.Time) (int, error)

	// RestoreArchivedSession cold-revives a was-alive archived session (one
	// archived while ended_at was NULL): rebuilds its live session row (active)
	// + messages from the archive payload and drops the archive row. Returns
	// (true, nil) on restore, (false, nil) when sid isn't archived or was
	// archived while ended (superseded — caller opens a fresh session). Triggered
	// only by a reply to an archived bot message (docs/group-session-archive.md §4).
	RestoreArchivedSession(ctx context.Context, sid string) (bool, error)

	// PurgeArchivedBefore hard-deletes archive rows whose archived_at < cutoff
	// AND their kept message_index rows — the second-tier cleanup that bounds
	// both the archive blob table and message_index (which is otherwise retained
	// indefinitely for reply-revive). Past this horizon a reply to such a message
	// just opens a fresh session. Returns the number of archive rows purged.
	PurgeArchivedBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// InboxChatRow is one row of an agora inbox's read-only webui transcript: the
// message's role + content plus the chat scope (sessions.session_key) it came
// in on, so the UI can label / filter the transcript by chat location ("聊天
// 框"). Webui-only shape — the only consumer is the inbox chat-history modal.
type InboxChatRow struct {
	Role    string
	Content string
	Scope   string // originating chat scope = sessions.session_key (来路)
}

// InboxChatScope is one distinct chat scope (session_key) that has messages in
// an inbox, with its message count — backs the chat-history "filter by chat
// box" chips. One inbox fronting N chats yields N scopes (chat-scoped model:
// the session key is always the real chat scope, docs/agora.md §一·五).
type InboxChatScope struct {
	Scope string
	Count int64
}

// Each consumer takes only the interface it needs (interface-segregation; §0.2).
type SessionStore interface {
	// EnsureSession returns the active (non-ended, latest) session id for key,
	// creating one if there is none. isNew is true when this call created the
	// session (lets the caller seed per-session state — e.g., apply
	// TodoConfig.DefaultMode — only on first creation).
	EnsureSession(ctx context.Context, key, source, userID, model string) (sessionID string, isNew bool, err error)
	// CurrentSession returns the active session id for key, without creating one.
	// v0.2: "active" means the one most recently made active via the cursor —
	// /new, /switch, or close-session re-election (see SetCursor). If no cursor
	// has been set yet for this scope, falls back to the latest-started non-
	// ended session (legacy behaviour, backward-compatible with pre-cursor rows).
	CurrentSession(ctx context.Context, key string) (sessionID string, ok bool, err error)
	// SetCursor marks sessionID as the active session for its scope by
	// stamping cursor_set_at = NOW(). Used by /new (right after OpenSession),
	// /switch (move active to a different sid), and the close-session
	// re-election path (after closing the previous active, promote another
	// alive sid). The cursor is per-row, not per-scope: the scope's "active"
	// is whichever non-ended row in the scope has the latest cursor_set_at
	// (CurrentSession returns that). Replaces the old in-memory
	// Hub.activeSid map, so the cursor survives bob restarts (audit D1).
	SetCursor(ctx context.Context, sessionID string) error
	// NewGeneration ends the current session for key (if any) and starts a fresh
	// one. A5: compaction no longer rolls generations — it
	// rewrites in place on the same sid (AppendCompactionBatch + the agent's
	// compactReplay rule) — and review #4 removed both the lineage write
	// (parent_session_id stays empty; the column is single-meaning, the
	// delegating parent of a sub-session) and the dead Agent.Reset, so the
	// SOLE remaining consumer is the corruption escape hatch
	// (recoverSessionIfBroken: roll a fresh sid under the same scope when
	// the old one can't be read). NOT used by /new under the v0.2
	// multi-session model — see OpenSession for that.
	NewGeneration(ctx context.Context, key, source, userID, model string) (sessionID string, err error)
	// OpenSession creates a fresh session under key WITHOUT ending any
	// existing one. Used by /new under the v0.2 multi-session model
	// (`docs/session-design.md`): the caller wants a parallel new
	// session in the same scope; old sessions stay alive until
	// /close-session. parentSid="" — the new session is independent
	// (no compression-rotate lineage).
	OpenSession(ctx context.Context, key, source, userID, model string) (sessionID string, err error)
	// OpenSessionWithCaller is OpenSession plus the agora dispatch caller
	// frame: it stamps caller_session_id = callerSid on the new row
	// (docs/agora-delegation-return.md D1, B3b). Used by the
	// send_message(force_new) dispatch path so the worker session knows
	// which session to return its reply to (and so the anti-cycle / abort
	// walks can climb the caller chain). callerSid="" behaves exactly like
	// OpenSession. The new row is an ordinary routable session (kind=''):
	// caller_session_id is independent of parent_session_id (A5's delegate
	// sub-loop link), so the two chains never mix.
	OpenSessionWithCaller(ctx context.Context, key, source, userID, model, callerSid string) (sessionID string, err error)
	// EndSession marks a session ended.
	EndSession(ctx context.Context, sessionID, reason string) error
	// SetTitle sets (or clears, if title=="") a session's title.
	SetTitle(ctx context.Context, sessionID, title string) error
	// SetSessionModel records which pool-entry name most recently served a
	// chat call for this session, so `bob sessions` can show it. Lightweight
	// UPDATE; called per turn.
	SetSessionModel(ctx context.Context, sessionID, model string) error
	// ListSessions returns recent sessions, newest first. source=="" → all sources.
	ListSessions(ctx context.Context, source string, limit int) ([]SessionInfo, error)

	// AliveSessionsByKey returns the non-ended sessions for one scope,
	// newest first. Used by /session (display all alive sids in the
	// scope), /close-session all (cancel + end every alive sid), and
	// /todos (render per-sid sections). v0.2 multi-session model.
	AliveSessionsByKey(ctx context.Context, key string) ([]SessionInfo, error)

	// AliveSessionsByCaller returns the non-ended sessions whose
	// caller_session_id == callerSid — the DIRECT children one dispatch
	// hop down the agora caller tree (docs/agora-async-orchestration.md §2,
	// docs/agora-delegation-return.md §B6). Backs `/close-session`'s
	// abort-cascade: repeatedly applied from a closed sid down it walks the
	// whole worker subtree to preempt a runaway orchestration. Returns []
	// (not nil) when callerSid has no live children. Empty callerSid → nil.
	AliveSessionsByCaller(ctx context.Context, callerSid string) ([]SessionInfo, error)

	// AppendMessage adds a plain user/assistant/system message (no tool involvement)
	// to a session and bumps its message_count. For tool-related rows use
	// AppendAssistantWithToolCalls (assistant turn that called tools) or
	// AppendToolResult (one tool's result).
	AppendMessage(ctx context.Context, sessionID, role, content string, tokenCount int) error
	// AppendCompactionBatch performs one A5 in-place compaction rewrite
	// ATOMICALLY (docs/loop-core-spec.md §6.9; review #4 finding: the
	// row-by-row version let a partial tail land in the LIVE replay window,
	// splitting calls/result pairs in a way orphan repair cannot heal
	// mid-history). In a single transaction it appends: the summary marker
	// row (role=system, RowKindSummary, content = the model-visible summary
	// text, tokenCount as given) → every tail message re-appended through
	// the row-shape-matched form (assistant rows keep ToolCalls, tool rows
	// keep ToolCallID/ToolName — strict chat templates reject broken
	// pairings) → the commit marker row (RowKindCommit). All-or-nothing:
	// on error nothing landed and the replay view is untouched, so the
	// caller's "uncommitted marker = free retry" contract is true by
	// construction for transient errors, cancellation and crashes alike.
	AppendCompactionBatch(ctx context.Context, sessionID, summary string, summaryTokens int, tail []Message) error
	// AppendAssistantWithToolCalls records an assistant turn that emitted tool_calls.
	// content may be "" (the model called tools without prose). Caller must pass
	// a non-empty toolCalls slice; for a plain assistant reply, use AppendMessage.
	AppendAssistantWithToolCalls(ctx context.Context, sessionID, content string, toolCalls []ToolCall, tokenCount int) error
	// AppendToolResult records one tool call's result (role="tool"). toolCallID
	// must match the assistant's earlier tool_calls entry.
	AppendToolResult(ctx context.Context, sessionID, toolCallID, toolName, content string) error
	// GetMessages returns a session's messages in order, with all tool-call
	// fields populated. limit<=0 → all; else the most recent `limit` messages
	// (still in chronological order).
	GetMessages(ctx context.Context, sessionID string, limit int) ([]Message, error)
	// RepairOrphanedToolCalls scans the session for assistant rows whose
	// tool_calls have no matching tool result row, and synthesizes an error
	// result row for each missing one. Called before appending a new user
	// message so a crash-interrupted turn doesn't poison replay. Returns the
	// count of synthesized rows.
	RepairOrphanedToolCalls(ctx context.Context, sessionID string) (int, error)

	// StampMessagesInbox writes inboxID onto this session's messages that
	// aren't stamped yet (agora_inbox_id IS NULL). Called once per turn at
	// turn-end with the turn's resolved agora inbox (chat-scoped model,
	// docs/agora.md §一·五); a no-op when inboxID is "" (non-agora turn).
	// The IS-NULL filter makes it re-wire immune: a later turn under a
	// different inbox only stamps ITS freshly-written rows, never the
	// already-stamped earlier ones.
	StampMessagesInbox(ctx context.Context, sessionID, inboxID string) error
	// InboxForSession returns the agora inbox this session's messages were
	// stamped to (the first non-null agora_inbox_id), or "" for a non-agora
	// session. Admin display helper.
	InboxForSession(ctx context.Context, sessionID string) (string, error)
	// SessionIDsForInbox returns the distinct session ids that have any
	// message stamped to inboxID, most-recently-active first, capped at
	// limit (limit<=0 → all). Backs "view inbox X's chat history".
	SessionIDsForInbox(ctx context.Context, inboxID string, limit int) ([]string, error)
	// ActiveSessionCountForInbox counts the distinct NON-ended sessions
	// with any message stamped to inboxID — the agora dashboard's per-inbox
	// "live sessions" number. 0 for a non-agora / unknown inbox.
	ActiveSessionCountForInbox(ctx context.Context, inboxID string) (int, error)
	// MessagesForInbox returns one page of an agora inbox's user/assistant chat
	// messages (newest window first, chronological within the page) + the total
	// match count, for the read-only webui transcript. q!="" filters by
	// case-insensitive substring of content; scope!="" restricts to one chat box
	// (sessions.session_key) so a multi-source inbox can be read one chat at a
	// time. Each row carries its originating scope. Live messages only (archived
	// sessions' rows have moved to the archive blob — out of scope).
	MessagesForInbox(ctx context.Context, inboxID, q, scope string, limit, offset int) (rows []InboxChatRow, total int64, err error)
	// InboxChatScopes returns the distinct chat scopes (session_key) that have any
	// user/assistant message in inboxID, each with its message count, busiest
	// first. Backs the chat-history "filter by chat box" chips. Empty for a
	// non-agora / unknown inbox.
	InboxChatScopes(ctx context.Context, inboxID string) ([]InboxChatScope, error)

	// RecordMessage notes that msgID (chat chatID, platform) belongs to a
	// specific session: both the rolling sessionScope and the concrete
	// sessionID at recording time. Storing sessionID lets reply-to-bot
	// resume the *exact* session a message was part of, even after /new
	// has rolled the key to a fresh generation (audit A-Q2).
	RecordMessage(ctx context.Context, platform, chatID, msgID, sessionScope, sessionID string) error
	// SessionForMessage returns the session_scope, the specific session_id,
	// and the message-row high-water mark (msgRowID) a previously-recorded
	// message belongs to. session_id may be empty for pre-A-Q2 (schema v3)
	// rows — callers fall back to session_scope + latest-active for those.
	// msgRowID is the messages-table id at the moment the bot reply was
	// indexed (0 for rows recorded before the column existed); a reply to
	// this message truncates the session history to id <= msgRowID
	// (reply-anchored context window, see docs/reply-context-window.md).
	SessionForMessage(ctx context.Context, platform, chatID, msgID string) (sessionScope, sessionID string, msgRowID int64, ok bool, err error)
	// ReviveSession clears ended_at on a session that was previously
	// ended via /new or shutdown. Used when a user replies to a bot
	// message belonging to an old generation: the session re-enters the
	// active set so the turn can continue from its original history.
	// Idempotent: a no-op on already-active sessions.
	ReviveSession(ctx context.Context, sessionID string) error

	// GetTodos returns the session's todo list (the JSON-decoded
	// sessions.todos_json column). Empty list when never set or session
	// missing. Stable order — preserved across reads (the writer chooses).
	GetTodos(ctx context.Context, sessionID string) ([]Todo, error)
	// GetTodoMode returns the session's verification regime (manual /
	// auto). Both backends return TodoModeNone (not manual) on a missing or
	// empty value — a corruption-safety fallback; the real default mode is
	// seeded at session creation, so a stored-empty mode means the row is
	// damaged, not "default to manual".
	GetTodoMode(ctx context.Context, sessionID string) (TodoMode, error)
	// SetTodoMode updates the session's verification regime. The todo
	// tool gates this with a user-confirmation flow when the existing
	// list is non-empty (#97); SetTodoMode itself does no validation
	// beyond accepting any TodoMode value.
	SetTodoMode(ctx context.Context, sessionID string, mode TodoMode) error
	// UpdateTodos performs an atomic read-modify-write on the session's
	// todo list. fn receives the current decoded slice and returns the
	// new slice; the store handles serialization (JSON encode/decode)
	// and concurrency (per-sid mutex) so multiple writers — the tool's
	// main path AND async review-button handlers — can't lose updates
	// to whole-list-overwrite races. fn returning a non-nil error
	// aborts the write; an unchanged-list write is allowed but
	// pointless (caller should detect no-op).
	UpdateTodos(ctx context.Context, sessionID string, fn func(current []Todo) ([]Todo, error)) error

	// AddRubricSkillLoaded marks `skillName` as engaged ("染色") in this
	// session's loaded-skill set. Idempotent (calling with the same skill
	// twice is a no-op); irreversible — see docs/rubric-design.md §5. Two
	// callers, both in coretools/40-skills.go (skill_view): (1) a RUBRIC
	// skill when todo_mode=="auto" (paired with AddRubricSnapshot — the
	// auto-mode verification regime); (2) a force_ocr skill in ANY mode
	// (the orchestrator's forced-OCR pass reads this set; it must arm on
	// `none`-mode human sessions too, so it is NOT auto-gated). The
	// todo mode-lock keys on snapshots (GetRubricSnapshots), not this set,
	// so a force_ocr-only entry never locks the session to auto.
	AddRubricSkillLoaded(ctx context.Context, sessionID, skillName string) error

	// GetRubricSkillsLoaded returns the set of engaged skill names for this
	// session (RUBRIC-染色 skills AND force_ocr skills — see
	// AddRubricSkillLoaded). Returns empty slice (not nil) when none. Read
	// by the orchestrator forced-OCR pass; the RUBRIC judge / mode-lock use
	// GetRubricSnapshots instead.
	GetRubricSkillsLoaded(ctx context.Context, sessionID string) ([]string, error)

	// AddRubricSnapshot stores the full RUBRIC.md body alongside the
	// skill name for this session. Idempotent (same name re-snapshot
	// is a no-op — preserves the original snapshot to honour染色
	// frozen-at-time semantics; admin edits to RUBRIC.md mid-session
	// don't propagate). See docs/rubric-design.md §5.
	//
	// Phase 7: replaces the file-read-at-judge-time
	// pattern. Callers invoke this from skill_view immediately after
	// successful RUBRIC.md read so the judge can later assemble its
	// prompt purely from in-DB state. Stored alongside (and at the
	// same call site as) AddRubricSkillLoaded — the two surfaces
	// coexist during the grace period for in-flight sessions; new
	// reads should prefer snapshots.
	AddRubricSnapshot(ctx context.Context, sessionID, skillName, rubricBody string) error

	// GetRubricSnapshots returns all RUBRIC snapshots染色'd for this
	// session. Returns empty slice (not nil) when none.
	GetRubricSnapshots(ctx context.Context, sessionID string) ([]RubricSnapshot, error)

	// SetRubricBlockingFlag sets/clears the "this session is blocked
	// due to unresolved RUBRIC verification failure" flag. Read by
	// reply emit / next-iter decision points to
	// refuse forward progress until cleared. See
	// docs/rubric-design.md §7.
	SetRubricBlockingFlag(ctx context.Context, sessionID string, blocked bool) error

	// GetRubricBlockingFlag returns the current blocking state.
	// Default false (cleared) for sessions that have never been
	// blocked.
	GetRubricBlockingFlag(ctx context.Context, sessionID string) (bool, error)

	// SetExpertSalvageDisabled sets/clears the per-session "don't attempt
	// an expert-tagged degenerate salvage for this session again" flag.
	// Set when an expert salvage was tried (the Flow36 degenerate path —
	// see agent-orchestrator salvageChat) and failed to produce a usable
	// completion, so the session stops burning repeated expensive expert
	// calls on later degenerate turns. Mirrors SetRubricBlockingFlag; /new
	// (a fresh session row) resets it.
	SetExpertSalvageDisabled(ctx context.Context, sessionID string, disabled bool) error

	// GetExpertSalvageDisabled returns the current flag. Default false
	// (expert still enabled) for sessions that never had a failed expert
	// salvage.
	GetExpertSalvageDisabled(ctx context.Context, sessionID string) (bool, error)

	// AddPending records an inbound message accepted for processing but not yet
	// answered (so a restart can ask the chat to re-send). extras carries
	// optional per-event JSON metadata (today: the merged Telegram
	// album's media_group_id, kept for debug / audit traceability —
	// nothing reads it). Pass nil or an empty map when there is
	// nothing to attach. Each backend stores extras natively (pg =
	// JSONB column; sqlite = TEXT + JSON1) so callers stay
	// backend-agnostic.
	AddPending(ctx context.Context, sessionScope, source, chatID, threadID, msgID string, extras map[string]any) error
	// LookupPendingByExtra / LookupPendingByMediaGroupID removed
	//. The Telegram media-group caption-share that needed
	// them moved into the telegram source (album N-into-1 merge
	// happens before the gateway sees the event), so no caller
	// queries pending_inbound.extras any more. extras column +
	// partial index intentionally retained so a future caller (or a
	// debugger correlating album events) still has the trace data.
	// Restore both methods if a cross-event metadata lookup is ever
	// needed again.

	// RemovePending drops the pending records for (sessionScope, msgIDs) — called once
	// their turn has been answered.
	RemovePending(ctx context.Context, sessionScope string, msgIDs []string) error
	// RemovePendingForChat drops the listed pending_inbound rows for one chat
	// (source, chatID, threadID, msgIDs). Used at startup after the restart
	// notice for that chat has been successfully delivered; msgIDs is the
	// ListPendingChats snapshot, so rows recorded after the snapshot survive.
	RemovePendingForChat(ctx context.Context, source, chatID, threadID string, msgIDs []string) error
	// ListPendingChats returns the distinct chats with leftover pending
	// messages, each carrying the snapshot of its pending msg_ids.
	ListPendingChats(ctx context.Context) ([]PendingChat, error)
	// ClearStalePending deletes pending records older than olderThanSeconds. A
	// safety net for rows leaked by an aborted turn (e.g. panic) — normal rows live
	// milliseconds to minutes. Returns the number of rows deleted.
	ClearStalePending(ctx context.Context, olderThanSeconds float64) (int, error)

	// AppendTurnEvent records one lifecycle event from the turn module
	// to the turn_events table. Events are read by the sweep-time
	// learnings analysis. Best-effort: errors are logged by callers but
	// don't abort the turn.
	AppendTurnEvent(ctx context.Context, sessionID, turnID string, iter int, kind, payload string) error

	// TurnsForLearning returns turns whose OWN last event settled at least
	// minAgeSeconds ago AND that aren't already in turn_learnings — anchored on
	// the turn's age, NOT the containing session's ended state, so alive
	// long-running conversations get distilled incrementally (	// docs/group-session-archive.md §2). Granularity is per-turn.
	//
	// Order: oldest turn first (MAX(ts) ASC). Cap: `limit` rows so a single
	// sweep can't blow the LLM token budget.
	TurnsForLearning(ctx context.Context, minAgeSeconds float64, limit int) ([]TurnPending, error)

	// MarkTurnLearned records that sweep distilled this turn.
	// llmTokens / outputChars are for telemetry, attributed to this
	// turn's slice of the analyser call. inBatch is the number of
	// turns the analyser saw in the same call (so admin can later
	// `SELECT AVG(in_batch) ...` for batching efficiency).
	MarkTurnLearned(ctx context.Context, turnID, sessionID string, llmTokens, outputChars, inBatch int, summary string) error

	// UnlearnedTurnsForSession returns the unlearned turns of this
	// session as TurnPending rows (joined with session metadata for
	// insight routing). Order: by min(event.ts) of each turn. Used by
	// /learn slash to pick what's left to distill in the current
	// chat's session.
	UnlearnedTurnsForSession(ctx context.Context, sessionID string) ([]TurnPending, error)

	// GetTurnEvents returns all events for a single session, ordered by
	// ts. Used by sweep to assemble the analyzer prompt.
	GetTurnEvents(ctx context.Context, sessionID string) ([]TurnEvent, error)

	// AddModelUsage adds the given deltas onto the per-model, per-hour
	// usage row keyed by (entryName, hourStart). hourStart is unix
	// seconds truncated to the hour. Implemented as an additive UPSERT
	// (INSERT ... ON CONFLICT DO UPDATE SET col = col + excluded.col) so
	// it is idempotent under flush retry. The model pool calls this from
	// the housekeeping tick for each completed hour it accumulated.
	AddModelUsage(ctx context.Context, entryName string, hourStart, calls, errs, inputTokens, outputTokens int64) error

	// ModelUsageSince returns every model_usage row with hour_start >=
	// since, ordered by entry_name then hour_start. Lets an operator see
	// how much each model was used per hour.
	ModelUsageSince(ctx context.Context, since int64) ([]ModelUsageRow, error)

	// PruneModelUsage deletes usage rows with hour_start < olderThanUnix
	// and returns the number deleted. ModelUsageSince only ever reads a
	// recent window, so rows below the cutoff are dead weight; without
	// this the per-(model,hour) table grows ~24×N rows/day forever.
	// Mirrors PruneAdminLine / PruneEmailOutbound; called from the
	// housekeeping Sweep.
	PruneModelUsage(ctx context.Context, olderThanUnix int64) (int64, error)

	// PruneMessageIndexBefore deletes message_index rows with ts <
	// olderThanUnix and returns the number deleted. message_index grows by
	// one row per bot-sent message; PurgeArchivedBefore only reaps rows of
	// ARCHIVED sessions, so a continuously-active (never-cold) session's rows
	// would never be collected. This ts-based prune bounds those live-session
	// rows. The cutoff must sit far past the reply-reconnect window (a reply
	// to a bot message older than the cutoff loses its exact-session routing
	// and falls back to scope+latest), so callers use the archive hard-TTL
	// horizon. Called from the housekeeping Sweep.
	PruneMessageIndexBefore(ctx context.Context, olderThanUnix int64) (int64, error)

	// PruneTurnEventsLearnedBefore deletes turn_events rows whose turn
	// already has a row in turn_learnings AND whose ts < olderThanUnix,
	// returning the number deleted. A5 forever-sids removed the
	// generation-roll rotation that used to bound turn_events (a heavy
	// scope's sid rolled on every context overflow, the old sid went
	// idle and its events metabolised through the archive); a
	// continuously-active scope now keeps one sid forever, so its event
	// log grows without bound. Learned events are exhaust — the sweep
	// never re-reads a turn once it is in turn_learnings — so deleting
	// them is safe at any cutoff; unlearned events are kept regardless
	// of age (they are the sweep's pending input). Mirrors
	// PruneModelUsage / PruneMessageIndexBefore; called from the
	// housekeeping Sweep.
	PruneTurnEventsLearnedBefore(ctx context.Context, olderThanUnix int64) (int64, error)

	// PruneTurnLearningsBefore deletes turn_learnings rows with
	// learned_at < olderThanUnix whose turn no longer has any
	// turn_events row, returning the number deleted. Companion to
	// PruneTurnEventsLearnedBefore: the learnings row's only job after
	// distillation is to keep the learned turn out of
	// SessionsForLearning, and that nomination is anchored on
	// turn_events — once the events prune has collected the turn, the
	// marker row carries no dedup duty and is pure exhaust. Callers
	// pass a cutoff at or past the events-prune horizon so a marker is
	// never deleted while its events still exist (which would re-open
	// the turn for learning). Otherwise (FK cascade at archival aside)
	// the table grows one row per turn forever on a never-cold scope.
	// Called from the housekeeping Sweep.
	PruneTurnLearningsBefore(ctx context.Context, olderThanUnix int64) (int64, error)

	// LoadHistory is the canonical "give me this session's messages
	// for model replay" reader (F3). Tries GetMessages
	// first (fast happy path); on any error falls back to a tolerant
	// per-row recovery scan (corrupt rows are skipped with a WARN) to
	// keep partial history flowing. Caller never sees a
	// partial-corruption error — it gets messages or a real fatal
	// store error.
	//
	// Use this from buildMessages / maybeCompress / todo-judge / any
	// other path that wants to feed history to the model. Use
	// GetMessages directly only for diagnostic / admin paths where
	// strict failure surfacing is preferred (e.g. /session slash).
	LoadHistory(ctx context.Context, sessionID string, limit int) ([]Message, error)

	// LoadHistoryReplyWindow is the reply-anchored window that ALSO keeps the
	// current turn. It returns messages with `id <= upToRowID OR id >=
	// keepFromRowID` — i.e. history up to the replied-to anchor M PLUS the
	// current turn's own messages (the reply and its tool calls, whose rows
	// are >= keepFromRowID), dropping only the abandoned branch strictly
	// between them. keepFromRowID is the row of the turn's just-appended user
	// message. keepFromRowID <= 0 → plain `id <= upToRowID` bound (no current-
	// turn floor known). Without this, a reply-to-bot turn would drop the very
	// message it is answering (its row is > M). Same fault-tolerant contract.
	LoadHistoryReplyWindow(ctx context.Context, sessionID string, upToRowID, keepFromRowID int64, limit int) ([]Message, error)

	// LatestMessageRowID returns the highest messages.id for the session (0 if
	// none). Called right after appending the current user turn so the caller
	// captures that row as the reply-window floor (keepFromRowID above).
	LatestMessageRowID(ctx context.Context, sessionID string) (int64, error)

	// SessionByID looks up a single session row by its DB id. Used by
	// admin / debug paths (e.g. `bob debug prompt <sid>`) that have
	// a sid in hand but need the routing key / source / user for
	// downstream wiring.
	SessionByID(ctx context.Context, sessionID string) (SessionInfo, error)

	// ProbeSession is a cheap health probe — runs SELECT 1 FROM
	// messages WHERE session_id=? LIMIT 1 and returns the SQL
	// error if any. Returns nil for both "session has rows" and
	// "session has zero rows" — only true SQL failures (table
	// missing, FK broken, ctx cancelled, schema mismatch) surface.
	//
	// Called at Respond() entry to decide whether the session needs
	// healing (NewGeneration + notice) without paying the cost of a
	// full message scan every turn. B2/F2 fix: replaces
	// the per-turn full-table read that recoverSessionIfBroken used
	// to do.
	ProbeSession(ctx context.Context, sessionID string) error

	// RegisterCallback persists one button-callback registration. id is
	// the opaque wire-id the source plumbs back on click; groupID ties
	// siblings (one logical button group) together so resolving any one
	// deletes the whole set atomically. kind identifies which gateway-
	// side handler should dispatch on click; payload is the kind-specific
	// JSON the handler decodes. expiresAt is unix seconds.
	//
	// Replaces the in-memory pendingRow map of the pre-design
	// — restart-safe: a bob restart between Post-time and click-time no
	// longer loses live buttons.
	//
	// IMPL CONTRACT (R5C-17): on a UNIQUE-constraint conflict
	// (id PK collision) impls MUST wrap the native error so that
	// errors.Is(err, ErrStoreUniqueViolation) is true. The callbacks
	// layer relies on that sentinel to retry id generation on the rare
	// PK collision (callbacks.Manager.Register / idCollisionRetries).
	// A non-classifying impl would surface the collision as a generic
	// error and abort the registration instead of retrying.
	RegisterCallback(ctx context.Context, row CallbackRow) error

	// GetCallback returns the row for callbackID. (false, nil) when not
	// found — used by Resolve to distinguish unknown / expired / already-
	// consumed (all collapse to "id not in table") from real DB errors.
	GetCallback(ctx context.Context, callbackID string) (CallbackRow, bool, error)

	// DeleteCallbackGroup deletes every row with this group_id. Called
	// by Resolve on a click (drops the clicked id + its siblings so the
	// "exactly once" semantic is preserved). Returns the count deleted.
	DeleteCallbackGroup(ctx context.Context, groupID string) (int, error)

	// DeleteExpiredCallbacks deletes rows whose expires_at < now and
	// returns the (group_id, kind) pairs of the deleted rows for the
	// caller's slog accounting. Sweep calls this on a ticker; rows are
	// silently dropped — expiry is NOT a "user rejected" signal (the
	// pre-design that fired handlers on expiry was the
	// source of a manual-mode todo bug).
	DeleteExpiredCallbacks(ctx context.Context, now int64) ([]ExpiredCallback, error)

	// Ping is a cheap liveness probe. Used by FallbackStore's health
	// checker and by doctor. SHOULD complete in < 1s; longer = backend
	// is sick. Returns errors wrapped with the appropriate sentinel
	// (typically core.ErrStoreConnLost on connection failure) so the
	// caller can errors.Is-check.
	Ping(ctx context.Context) error

	// HealthReport returns impl-self-described health for diagnostic
	// surfaces (bob doctor). Each impl decides what notes to include;
	// doctor just prints them without knowing backend specifics.
	HealthReport(ctx context.Context) StoreHealth

	// ServerTime returns the backend's idea of NOW as a unix-second
	// integer (PD2/X fix). The clock package uses this at
	// startup + periodically to compute an offset against the local
	// `time.Now()` so all writes pass through a single time reference
	// regardless of which backend is active (matters most under
	// fallback — flipping primary↔secondary won't shift timestamps).
	//
	// Why int64 sec, not time.Time: cross-backend wire format stays
	// uniform; sub-second precision is lost in the SELECT round-trip
	// anyway. Callers that want time.Time use time.Unix(n, 0).
	//
	// SHOULD complete in < 100ms; longer is fine but indicates a sick
	// backend. Failure wraps to ErrStoreConnLost so fallback's health
	// checker can treat repeated ServerTime failures as "primary
	// down".
	ServerTime(ctx context.Context) (int64, error)

	Close() error
}

// StoreHealth is what HealthReport returns. Fields are impl-agnostic;
// per-backend detail goes in Notes (free-form lines for doctor to
// print). FallbackStore sets BackendInUse to the currently active
// impl's name.
type StoreHealth struct {
	// Backend label as appears in cfg.Store.Backend: "sqlite" /
	// "postgres" / "fallback".
	Backend string

	// BackendInUse — for FallbackStore: which underlying impl is
	// currently serving. Same as Backend for single-impl stores.
	BackendInUse string

	// ImplInUse is the concrete backend impl currently serving —
	// "postgres" / "sqlite". For a single-impl store it equals
	// Backend; for a FallbackStore it is the active side's impl name,
	// so a dashboard can show "pg" / "sqlite" even when
	// Backend == "fallback" (where BackendInUse is only
	// "primary" / "secondary").
	ImplInUse string

	// Healthy = backend currently reachable + queries succeed.
	Healthy bool

	// MessageWrites is a cumulative, in-memory counter of message-history
	// write operations (AppendMessage / AppendAssistantWithToolCalls /
	// AppendToolResult). A dashboard diffs it between polls to derive a
	// store write-throughput rate. Best-effort; resets to 0 on restart.
	// For a FallbackStore it is the sum across both underlying impls.
	MessageWrites int64

	// LastFailover / LastFailback — only meaningful for
	// FallbackStore; zero value otherwise.
	LastFailover time.Time
	LastFailback time.Time

	// Notes are impl-specific diagnostic lines (e.g. for sqlite:
	// "integrity_check=ok"; for pg: "server_version=18.0.3 pool=3/10").
	Notes []string
}

// TurnPending is one row returned by TurnsForLearning — a turn that
// sweep should distill. Carries enough session context for the
// analyser to route insights without an extra session lookup.
type TurnPending struct {
	TurnID       string
	SessionID    string
	SessionScope string
	Source       string
	UserID       string
}

// TurnEvent is one row from turn_events. payload is JSON; callers
// know the schema by `kind` (see docs/turn-lifecycle-design.md §3.4).
type TurnEvent struct {
	SessionID string
	TurnID    string
	Iter      int
	Kind      string
	Payload   string
	Ts        float64
}

// ModelUsageRow is one row from the model_usage table — a single
// model's accumulated usage for one wall-clock hour. HourStart is unix
// seconds truncated to the hour.
type ModelUsageRow struct {
	EntryName    string
	HourStart    int64
	Calls        int64
	Errors       int64
	InputTokens  int64
	OutputTokens int64
}

// CallbackRow is one persisted inline-button-callback registration —
// the table-shaped twin of the in-memory pendingRow callbacks.Manager
// used to keep. Payload is kind-specific JSON the gateway-side
// kind-handler decodes on click. ExpiresAt is unix seconds.
//
// PayloadJSON byte-equality is NOT guaranteed across backends. pg's
// JSONB canonicalizes whitespace + key ordering on store; sqlite's
// TEXT round-trips bytes exactly. Handlers MUST decode via
// json.Unmarshal — they MUST NOT depend on bytes.Equal / sha256 /
// any byte-level comparison.
type CallbackRow struct {
	ID           string // opaque wire id (also PK)
	GroupID      string // siblings in one button group share this
	ChoiceIdx    int    // 0-based index within the group
	SessionScope string
	SessionID    string // may be empty for session-key-only registrations
	Kind         string // routes to handler (e.g. "todo_review")
	PayloadJSON  []byte // handler-specific JSON; see byte-equality caveat above
	ExpiresAt    int64  // unix seconds
	CreatedAt    int64  // unix seconds — useful for debug / TTL audits
}

// ExpiredCallback is one row deleted by DeleteExpiredCallbacks, returned
// so the sweep caller can slog.Info each expiry for observability.
// Carries kind + group_id so logs say what kind of button expired
// without a second query.
type ExpiredCallback struct {
	GroupID      string
	Kind         string
	SessionScope string
	SessionID    string
}
