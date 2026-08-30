package contract

import "context"

// MessageStore is the conversation-history persistence the turn core reads from
// and writes to — the ONLY coupling face between turn (the short-lived "read the
// tail + append" executor) and session (the owner of the conversation's
// continuity, hence its history).
//
// Ownership: the bob_messages table belongs to the SESSION module — "对话的连续性
// = session;历史 = 连续性的载体;所以历史归 session" (docs/loop-core-spec.md
// §1.2 / docs/wip-message-ownership.md). session provides the implementation; the
// turn core consumes it through this interface and never builds its own store.
//
// What a new session implementation MAY freely change is HOW messages are stored
// (schema / index / backend). What it must NOT change unilaterally is the shared
// data model (contract.Message) nor the replay/compaction SEMANTICS — GetReplay
// anchors on the last summary marker (§6.9), AppendCompactionBatch is an atomic
// batch. Those promises are part of the interface, not leakage through the seam.
type MessageStore interface {
	// GetReplay returns the replay window (§6.9): everything FROM the last summary
	// marker (a compaction's start), so the marker is always present and the recent
	// tail is whole. With no summary yet it falls back to the last cap rows.
	GetReplay(ctx context.Context, sessionID string, cap int) ([]Message, error)
	// GetMessages returns a session's history in chronological order; limit<=0 → all.
	GetMessages(ctx context.Context, sessionID string, limit int) ([]Message, error)
	// AppendMessage appends one plain conversation row (user / assistant / tool-less).
	// author = the row's attribution (the human sender for a user row; the agent for an
	// assistant row); zero MsgAuthor = unattributed.
	AppendMessage(ctx context.Context, sessionID, role, content string, author MsgAuthor) error
	// AppendUserMsgs appends a turn's per-speaker user rows ATOMICALLY (one per UserMsg,
	// each tagged with its sender) — all-or-nothing so a mid-batch failure + retry can't
	// duplicate an already-committed speaker row. Empty slice = no-op.
	AppendUserMsgs(ctx context.Context, sessionID string, msgs []UserMsg) error
	// AppendAssistantCalls appends the assistant row that emitted tool calls (I2:
	// persisted BEFORE dispatch, so a crash mid-dispatch leaves a recoverable trail).
	// author = the agent that produced it.
	AppendAssistantCalls(ctx context.Context, sessionID, content string, calls []ToolCall, author MsgAuthor) error
	// AppendToolResult appends one tool result row (I3: one per dispatched call).
	AppendToolResult(ctx context.Context, sessionID, toolCallID, toolName, content string) error
	// AppendCompactionBatch performs an in-place compaction (§6.9) atomically: a
	// summary marker row followed by the recent tail re-appended by shape.
	AppendCompactionBatch(ctx context.Context, sessionID, summary string, recent []Message) error
	// RecentAttachments returns the session's structured attachment set (deduped by Path,
	// newest wins) reconstructed from the per-row attachments — independent of text
	// compaction, so a file the user sent in an EARLIER turn stays reachable after the
	// summary drops its inbox path. limit<=0 → a sane default. Empty (not an error) when
	// the session carries no placed attachments.
	RecentAttachments(ctx context.Context, sessionID string, limit int) ([]Attachment, error)
	// DeleteSessionMessages removes all rows for a session id (real-session archival;
	// per §5 a sub-turn no longer uses it).
	DeleteSessionMessages(ctx context.Context, sessionID string) error
}

// ChatHistory is the read-only, browsing-oriented view of conversation history,
// addressed by chat SCOPE rather than session id — the webui's agora chat-log
// reader (an admin browses an inbox's conversation). Provided by the session
// module (it owns bob_messages AND the scope→session mapping, so the join stays
// inside leaf/session); consumed by flow/agora, which resolves an inbox to its
// scopes and pages the merged history. Kept SEPARATE from MessageStore so the
// turn-hot write path and this admin read path don't share a widening interface.
type ChatHistory interface {
	// MessagesForScopes returns conversation rows across every session whose
	// session_scope is in scopes, NEWEST FIRST, paged by limit/offset, plus the
	// grand total (for the page footer). Empty scopes → (nil, 0, nil).
	MessagesForScopes(ctx context.Context, scopes []string, limit, offset int) (msgs []Message, total int64, err error)
}
