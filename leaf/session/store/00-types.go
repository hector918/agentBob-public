// Package store is the session module's persistence: session lifecycle, the
// crash-safe pending-inbound queue, and the message-index for reply routing. It
// owns its tables on the shared contract.DB pool. SessionInfo / PendingChat /
// the Store interface stay here (not in contract) — the Manager and this impl
// are the only users, the seam is internal to the session module.
//
// MINIMAL SLICE (trunk-rebuild): the monolithic old SessionStore (~70 methods)
// is split by domain. This carries only what the resolver + arbiter + recovery
// need; messages/history, todos, rubric, turn-events, model-usage, callbacks all
// belong to other modules and land with them. The bob_sessions table here is
// lifecycle-only — todos/rubric/token columns are added by the turn module's own
// migration step when it lands.
package store

import (
	"context"

	"agentbob/contract"
)

// EndReasonGenerationRotate is stamped on a session ended by NewGeneration (the
// /new-generation corruption escape hatch).
const EndReasonGenerationRotate = "generation_rotate"

// SessionInfo is one session row. Preview (first user message) is empty until
// the messages table lands with the turn module.
type SessionInfo struct {
	ID        string
	Key       string // routing key / scope, e.g. "telegram:dm:123"
	Source    string
	UserID    string
	Model     string
	ParentID  string  // delegating parent sid (sub-session rows), or ""
	CallerID  string  // agora dispatch caller sid, or ""
	Title     string  // "" if untitled
	StartedAt float64 // unix seconds
	EndedAt   float64 // 0 if still active
	Preview   string  // first user message (empty until messages land)
	// AdminOwned is true once an admin has addressed this session. Read by
	// flow/normal only for a pure-internal continuation (no human in the batch).
	AdminOwned bool
	// Mode is the flow-declared session mode (auto/manual/none). Captured in the
	// archive payload so a cold-restore preserves it — a revived auto session
	// self-resumes immediately instead of re-deriving to none until the next turn's
	// OnMode re-cache. Empty reads as SessionModeNone.
	Mode contract.SessionMode
	// PreferModelTags is the session's sticky SOFT model-tag preference
	// (prefer_model_tags, comma-joined in the column). Stamped when a turn consumes
	// a /uncensored arm; every later turn on the session feeds it to
	// ModelRequest.Prefer. Rides the archive payload (this struct is the payload
	// meta), so a revived session keeps the preference. Populated by SessionByID
	// only — the list views (AliveSessionsByKey/ListSessions) don't carry it,
	// mirroring AdminOwned/Mode.
	PreferModelTags []string
	// TurnMode is the session's birth-stamped loop-driver mode (turn_mode column):
	// copied from the scope's default at creation, immutable for the session's
	// life (docs/turn-driver-split.md). Rides the archive payload so a revived
	// session keeps its stamp. Populated by SessionByID only, like the two above.
	// "" = regular.
	TurnMode contract.TurnMode
}

// PendingChat is one chat's worth of unanswered inbound messages, recovered on
// restart to send a "please resend" notice.
type PendingChat struct {
	Source   string
	ChatID   string
	ThreadID string
	MsgIDs   []string
}

// Store is the session module's persistence interface. The Manager depends on
// it; the pg impl (*PG) is the only implementation (no dual-backend).
type Store interface {
	// session lifecycle
	EnsureSession(ctx context.Context, key, source, userID, model string) (sessionID string, isNew bool, err error)
	CurrentSession(ctx context.Context, key string) (sessionID string, ok bool, err error)
	SetCursor(ctx context.Context, sessionID string) error
	// MarkSessionAdmin promote-only records that an admin addressed this session
	// (idempotent). flow/normal reads admin_owned to keep admin authority on a
	// pure-internal (no human in the batch) continuation of an admin's session.
	MarkSessionAdmin(ctx context.Context, sessionID string) error
	OpenSession(ctx context.Context, key, source, userID, model string) (sessionID string, err error)
	OpenSessionWithCaller(ctx context.Context, key, source, userID, model, callerSid string) (sessionID string, err error)
	NewGeneration(ctx context.Context, key, source, userID, model string) (sessionID string, err error)
	SessionByID(ctx context.Context, sessionID string) (SessionInfo, error)
	EndSession(ctx context.Context, sessionID, reason string) error
	SetTitle(ctx context.Context, sessionID, title string) error
	SetSessionModel(ctx context.Context, sessionID, model string) error
	// SetSessionPreferModelTags persists (or clears, on empty) the session's sticky
	// soft model-tag preference. The arbiter stamps it when a turn consumes a
	// /uncensored arm; /uncensored clears it to toggle off.
	SetSessionPreferModelTags(ctx context.Context, sessionID string, tags []string) error
	// GetSessionMode returns the session's flow-declared mode (auto/manual/none).
	// A missing row / empty column reads as contract.SessionModeNone.
	GetSessionMode(ctx context.Context, sessionID string) (contract.SessionMode, error)
	// SetSessionMode persists the session's mode. The arbiter writes it (idempotent)
	// at turn start from the selected flow's declared mode.
	SetSessionMode(ctx context.Context, sessionID string, mode contract.SessionMode) error
	AliveSessionsByKey(ctx context.Context, key string) ([]SessionInfo, error)
	ListSessions(ctx context.Context, source string, limit int) ([]SessionInfo, error)
	// ArchivedSessionScope returns a cold-archived session's OWN scope key — for
	// recovering a virtual-group member sub-scope when a reply points at a session that
	// could not be revived (F6). ok=false (no error) when the sid is not archived.
	ArchivedSessionScope(ctx context.Context, sessionID string) (scope string, ok bool, err error)

	// crash-safe pending-inbound queue
	AddPending(ctx context.Context, sessionScope, source, chatID, threadID, msgID string, extras map[string]any) error
	RemovePending(ctx context.Context, sessionScope string, msgIDs []string) error
	RemovePendingForChat(ctx context.Context, source, chatID, threadID string, msgIDs []string) error
	ListPendingChats(ctx context.Context) ([]PendingChat, error)
	CountPending(ctx context.Context) (int, error)
	ClearStalePending(ctx context.Context, olderThanSeconds float64) (int, error)

	// message-index (reply routing) — keyed by (session_scope, msg_id) → session_id
	RecordSent(ctx context.Context, sessionScope, msgID, sessionID string) error
	SessionForMessage(ctx context.Context, sessionScope, msgID string) (sessionID string, msgRowID int64, ok bool, err error)
	PruneMessageIndex(ctx context.Context, olderThanSeconds float64) (int, error)

	// per-scope sink prefs (the /trace + /stream toggles). GetScopePrefs returns
	// contract.DefaultSinkPrefs (both ON) when the scope has no row.
	GetScopePrefs(ctx context.Context, scope string) (contract.SinkPrefs, error)
	SetScopePrefs(ctx context.Context, scope string, prefs contract.SinkPrefs) error

	// per-scope DEFAULT turn mode (the /looping toggle) — birth-stamped onto
	// sessions at creation (docs/turn-driver-split.md). "" = regular.
	GetScopeTurnMode(ctx context.Context, scope string) (contract.TurnMode, error)
	SetScopeTurnMode(ctx context.Context, scope string, mode contract.TurnMode) error

	Ping(ctx context.Context) error
}
