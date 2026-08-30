package store

import (
	"context"

	"agentbob/contract"
	"agentbob/migrate"
)

// schemaV1 creates the session module's tables: lifecycle-only bob_sessions, the
// crash-safe pending-inbound queue, and the message-index for reply routing.
// Columns the turn module needs (todos/rubric/tokens, the messages table it
// references) are added by that module's own migration step.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS bob_sessions (
    id                TEXT PRIMARY KEY,
    session_scope     TEXT NOT NULL,
    source            TEXT NOT NULL DEFAULT '',
    user_id           TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    parent_session_id TEXT,
    caller_session_id TEXT,
    title             TEXT,
    started_at        DOUBLE PRECISION NOT NULL,
    ended_at          DOUBLE PRECISION,
    end_reason        TEXT,
    message_count     INTEGER NOT NULL DEFAULT 0,
    cursor_set_at     DOUBLE PRECISION,
    kind              TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_bob_sessions_scope   ON bob_sessions(session_scope);
CREATE INDEX IF NOT EXISTS idx_bob_sessions_started ON bob_sessions(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_bob_sessions_parent  ON bob_sessions(parent_session_id);
CREATE INDEX IF NOT EXISTS idx_bob_sessions_caller  ON bob_sessions(caller_session_id);

CREATE TABLE IF NOT EXISTS bob_pending_inbound (
    id            BIGSERIAL PRIMARY KEY,
    session_scope TEXT NOT NULL,
    source        TEXT NOT NULL,
    chat_id       TEXT NOT NULL,
    thread_id     TEXT NOT NULL DEFAULT '',
    msg_id        TEXT NOT NULL,
    queued_at     DOUBLE PRECISION NOT NULL,
    extras        JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS idx_bob_pending_session ON bob_pending_inbound(session_scope);
CREATE INDEX IF NOT EXISTS idx_bob_pending_extras_gid
    ON bob_pending_inbound ((extras->>'media_group_id'))
    WHERE extras->>'media_group_id' IS NOT NULL;

CREATE TABLE IF NOT EXISTS bob_message_index (
    platform      TEXT NOT NULL,
    chat_id       TEXT NOT NULL,
    msg_id        TEXT NOT NULL,
    session_scope TEXT NOT NULL,
    session_id    TEXT NOT NULL DEFAULT '',
    ts            DOUBLE PRECISION NOT NULL,
    msg_row_id    BIGINT,
    PRIMARY KEY (platform, chat_id, msg_id)
);
CREATE INDEX IF NOT EXISTS idx_bob_msgidx_ts         ON bob_message_index(ts);
CREATE INDEX IF NOT EXISTS idx_bob_msgidx_session_id ON bob_message_index(session_id);
`

// schemaV2 adds the per-scope sink prefs (the /trace + /stream toggles). Keyed by
// session_scope (NOT session id): a toggle follows the conversation scope across
// its session generations. A scope with no row uses DefaultSinkPrefs (both ON).
const schemaV2 = `
CREATE TABLE IF NOT EXISTS bob_scope_prefs (
    session_scope TEXT PRIMARY KEY,
    trace         BOOLEAN NOT NULL DEFAULT TRUE,
    stream        BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at    DOUBLE PRECISION NOT NULL
);
`

// schemaV3 adds the session mode column — the flow-declared verification /
// acknowledgement regime (auto/manual/none). DEFAULT 'none' = the plain flow's
// value, so existing rows are valid without a backfill (a defaulted ADD COLUMN
// fills old rows, avoiding the NULL-cast / NOT-NULL ALTER trap). Read by the
// busy-queue (notice gating) + the nudge fast-lane; the todo verification axis
// reuses it when it lands.
const schemaV3 = `
ALTER TABLE bob_sessions ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT 'none';
`

// schemaV4 re-keys the reply-routing index on (session_scope, msg_id) → session_id
// (B-4). The session SCOPE already encodes source+chat, so the old platform/chat_id
// columns were redundant — the index now carries only the three coordinates that
// matter (scope, msg_id, session_id), all session-native (the session module never
// sees platform/source objects). Safe to drop+recreate: the WRITE side was never
// wired, so the old table is always empty.
const schemaV4 = `
DROP TABLE IF EXISTS bob_message_index;
CREATE TABLE bob_message_index (
    session_scope TEXT NOT NULL,
    msg_id        TEXT NOT NULL,
    session_id    TEXT NOT NULL DEFAULT '',
    ts            DOUBLE PRECISION NOT NULL,
    msg_row_id    BIGINT,
    PRIMARY KEY (session_scope, msg_id)
);
CREATE INDEX IF NOT EXISTS idx_bob_msgidx_ts         ON bob_message_index(ts);
CREATE INDEX IF NOT EXISTS idx_bob_msgidx_session_id ON bob_message_index(session_id);
`

// schemaV5 drops the dead message_count column: it was never written (always the
// DEFAULT 0), so the only "count" it ever reported was 0. Removed rather than wired
// up — the live message count is derivable from bob_messages when actually needed.
const schemaV5 = `
ALTER TABLE bob_sessions DROP COLUMN IF EXISTS message_count;
`

// schemaV6 adds the cold-session archive (docs/wip-message-ownership.md §6 /
// loop-core-spec §11.8). A cold session's meta + full message transcript are
// serialized into payload and the hot bob_sessions / bob_messages rows dropped;
// the reply index (bob_message_index) is kept so a later reply can locate and
// revive it. ended_at IS NULL = the session was alive when archived = revivable;
// a non-NULL ended_at (closed / finalized / a rotated generation) means the
// context was deliberately retired and a reply opens a fresh session instead.
const schemaV6 = `
CREATE TABLE IF NOT EXISTS bob_sessions_archive (
    session_id    TEXT PRIMARY KEY,
    session_scope TEXT NOT NULL,
    source        TEXT NOT NULL DEFAULT '',
    started_at    DOUBLE PRECISION,
    last_msg_at   DOUBLE PRECISION,
    ended_at      DOUBLE PRECISION,
    archived_at   DOUBLE PRECISION NOT NULL,
    payload       JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bob_sessions_archive_archived ON bob_sessions_archive(archived_at);
`

// schemaV7 adds the per-session admin-ownership bit. Set when an admin addresses
// the session; read by flow/normal ONLY for a pure-internal turn (a dispatch
// return / cron wake with no human event in the batch) so the headless
// continuation of an admin's session keeps admin authority. DEFAULT FALSE = the
// safe (user) value, so existing rows need no backfill (a defaulted ADD COLUMN
// fills old rows, avoiding the NULL-cast / NOT-NULL ALTER trap — same as schemaV3).
const schemaV7 = `
ALTER TABLE bob_sessions ADD COLUMN IF NOT EXISTS admin_owned BOOLEAN NOT NULL DEFAULT FALSE;
`

// schemaV8 makes the crash-recovery pending queue idempotent on (session_scope,
// msg_id): the same inbound replayed (a source redelivery / a double-submit) must
// not stack duplicate rows. AddPending pairs this with ON CONFLICT DO NOTHING. The
// existing media_group-merge dedups before AddPending, so a real second message in
// the same scope always carries a distinct msg_id — the index never collapses two
// genuine messages.
const schemaV8 = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_bob_pending_scope_msg
    ON bob_pending_inbound (session_scope, msg_id);
`

// schemaV9 drops the idx_bob_pending_extras_gid partial index: nothing ever
// queried by extras->>'media_group_id', so every album WAL write paid index
// maintenance for a dead index. The extras column itself stays — it is a
// debug/audit trace, and the pending table is transient and small enough for
// ad-hoc seq scans. (schemaV1 still creates the index on a fresh DB; this step
// drops it right after — append-only discipline over editing a shipped step.)
const schemaV9 = `
DROP INDEX IF EXISTS idx_bob_pending_extras_gid;
`

// schemaV10 adds the per-session sticky SOFT model-tag preference (armed by
// /uncensored, stamped onto the session by the turn that consumes the arm; every
// later turn on the session feeds it to ModelRequest.Prefer). Comma-joined tag
// list; DEFAULT ” = no preference, so existing rows need no backfill (a defaulted
// ADD COLUMN fills old rows, avoiding the NULL-cast / NOT-NULL ALTER trap — same
// as schemaV3/V7). Distinct from the `model` column, which merely RECORDS the
// entry that last served the session.
const schemaV10 = `
ALTER TABLE bob_sessions ADD COLUMN IF NOT EXISTS prefer_model_tags TEXT NOT NULL DEFAULT '';
`

// schemaV11 adds the turn-mode wiring (docs/turn-driver-split.md): the scope's
// DEFAULT mode (the /looping toggle, on bob_scope_prefs) and the session's
// BIRTH STAMP (copied from the scope default at creation, immutable for the
// session's life). DEFAULT ” = regular on both — existing rows need no
// backfill (defaulted ADD COLUMN, same NULL-cast-trap avoidance as schemaV10).
const schemaV11 = `
ALTER TABLE bob_scope_prefs ADD COLUMN IF NOT EXISTS turn_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE bob_sessions ADD COLUMN IF NOT EXISTS turn_mode TEXT NOT NULL DEFAULT '';
`

// Migrate brings the session module's tables to the current version.
func Migrate(ctx context.Context, db contract.DB) error {
	return migrate.Run(ctx, db, "session", []migrate.Step{
		{Version: 1, SQL: schemaV1},
		{Version: 2, SQL: schemaV2},
		{Version: 3, SQL: schemaV3},
		{Version: 4, SQL: schemaV4},
		{Version: 5, SQL: schemaV5},
		{Version: 6, SQL: schemaV6},
		{Version: 7, SQL: schemaV7},
		{Version: 8, SQL: schemaV8},
		{Version: 9, SQL: schemaV9},
		{Version: 10, SQL: schemaV10},
		{Version: 11, SQL: schemaV11},
	})
}
