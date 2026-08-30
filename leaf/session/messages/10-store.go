// Package messages is the session module's conversation-history store
// (bob_messages). Per the message-ownership move (docs/wip-message-ownership.md):
// the conversation's continuity is the session, and history is the carrier of that
// continuity — so history belongs to SESSION, not turn. The turn core is the
// short-lived "read the tail + append" executor; it reaches this store ONLY
// through contract.MessageStore (session provides it on the trunk). The store
// lives on the shared contract.DB pool.
//
// Rows: plain user/assistant rows + tool-round rows (assistant-with-calls carries
// tool_calls JSON; a tool result carries tool_call_id + tool_name). Compaction
// (row_kind=summary/commit) / agora-inbox columns land with the Ring-1 hardening.
//
// NOTE the migrate namespace stays "turn" (NOT renamed to "session"): a deployed
// DB already ran these steps under that namespace, so keeping it avoids a
// re-migration (the agora "纯加表不 bump" discipline applied here).
package messages

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/migrate"
)

const schemaV1 = `
CREATE TABLE IF NOT EXISTS bob_messages (
    id          BIGSERIAL PRIMARY KEY,
    session_id  TEXT NOT NULL,
    role        TEXT NOT NULL,
    content     TEXT NOT NULL DEFAULT '',
    token_count INTEGER NOT NULL DEFAULT 0,
    timestamp   DOUBLE PRECISION NOT NULL,
    row_kind    TEXT NOT NULL DEFAULT 'msg'
);
CREATE INDEX IF NOT EXISTS idx_bob_messages_session ON bob_messages(session_id, id);
`

// schemaV2 adds the tool-round columns: assistant-with-calls stores its calls in
// tool_calls (JSON); a tool result stores the call it answers (tool_call_id) and
// the tool's name. All nullable/defaulted so v1 rows read back as plain messages.
const schemaV2 = `
ALTER TABLE bob_messages ADD COLUMN IF NOT EXISTS tool_calls   TEXT NOT NULL DEFAULT '';
ALTER TABLE bob_messages ADD COLUMN IF NOT EXISTS tool_call_id TEXT NOT NULL DEFAULT '';
ALTER TABLE bob_messages ADD COLUMN IF NOT EXISTS tool_name    TEXT NOT NULL DEFAULT '';
`

// schemaV3 reclaims legacy delegated sub-turn rows. Before the in-memory-scratch
// change (wip-message-ownership §5), a sub-turn wrote bob_messages rows under a
// synthetic child sid "<parent>:sub:<n>" and deleted them on exit via defer — which
// LEAKED on a hard crash/kill (the defer never ran). Those orphans have no
// bob_sessions row, so neither the cold-archive sweep nor anything else can reach
// them. Real sids are "s_<base36>_<hex>" (newID) and never contain ":sub:", so this
// LIKE matches ONLY sub-turn scratch. One-time on existing trunk-rebuild DBs; a
// no-op everywhere else (fresh DBs never wrote such rows).
const schemaV3 = `
DELETE FROM bob_messages WHERE session_id LIKE '%:sub:%';
`

// schemaV4 adds per-row structured attribution (who produced the row) as a JSONB blob
// — `meta.author = {kind,id,name}`. A blob (not columns) because the author shape varies
// by row kind (human=id+name, agent=principal, tool=name) and is read inline with the
// row (every access is session-keyed, never by sender), so columns would be sparse for no
// query benefit. Additive; legacy rows read back as '{}' (unattributed).
const schemaV4 = `
ALTER TABLE bob_messages ADD COLUMN IF NOT EXISTS meta JSONB NOT NULL DEFAULT '{}'::jsonb;
`

// schemaV5 persists a user row's structured attachments as a JSONB blob (the same
// shape carried transiently in TurnSpec.Attachments): each entry's Kind/MIME/FileName/
// Path/Size, NOT the bytes (those live in the space) and NOT LocalPath (json:"-",
// transient). A blob (not a side table) because access is always session-keyed and
// flattened in memory — never queried by an attachment field — exactly like meta and
// tool_calls. NULL on rows with no attachments (most rows). This makes the
// conversation's attachment set recoverable from history instead of only via a space
// rescan; the model never sees it (it reads the attachment list from the row content).
const schemaV5 = `
ALTER TABLE bob_messages ADD COLUMN IF NOT EXISTS attachments JSONB;
`

// Migrate brings the turn module's tables to the current version.
func Migrate(ctx context.Context, db contract.DB) error {
	return migrate.Run(ctx, db, "turn", []migrate.Step{
		{Version: 1, SQL: schemaV1},
		{Version: 2, SQL: schemaV2},
		{Version: 3, SQL: schemaV3},
		{Version: 4, SQL: schemaV4},
		{Version: 5, SQL: schemaV5},
	})
}

// attachmentsJSON marshals a user row's attachments for the JSONB column, returning nil
// (→ SQL NULL) when there are none, so a row without attachments stays cheap. Bytes are
// never included; LocalPath is json:"-".
func attachmentsJSON(atts []contract.Attachment) any {
	if len(atts) == 0 {
		return nil
	}
	b, err := json.Marshal(atts)
	if err != nil {
		return nil
	}
	return string(b)
}

// dedupAttachmentsByPath keeps the FIRST occurrence of each Path and drops empties —
// applied to a newest-first list so the newest record of a re-used path wins. This is
// the "current attachment set" semantics for RecentAttachments.
func dedupAttachmentsByPath(atts []contract.Attachment) []contract.Attachment {
	seen := make(map[string]bool, len(atts))
	var out []contract.Attachment
	for _, a := range atts {
		if a.Path == "" || seen[a.Path] {
			continue
		}
		seen[a.Path] = true
		out = append(out, a)
	}
	return out
}

// authorJSON marshals a MsgAuthor to the row's meta JSON ("{}" when unattributed, so the
// stored blob is always valid JSONB). metaAuthor is the inverse for scans.
func authorJSON(a contract.MsgAuthor) string {
	if a.IsZero() {
		return "{}"
	}
	b, err := json.Marshal(struct {
		Author contract.MsgAuthor `json:"author"`
	}{a})
	if err != nil {
		return "{}"
	}
	return string(b)
}

func metaAuthor(metaRaw string) contract.MsgAuthor {
	if metaRaw == "" || metaRaw == "{}" {
		return contract.MsgAuthor{}
	}
	var m struct {
		Author contract.MsgAuthor `json:"author"`
	}
	_ = json.Unmarshal([]byte(metaRaw), &m)
	return m.Author
}

// PG is the Postgres history store.
type PG struct{ db contract.DB }

// NewPG returns a history store over db.
func NewPG(db contract.DB) *PG { return &PG{db: db} }

// PG satisfies the turn↔session coupling face: session provides it as
// contract.MessageStore (the turn core never builds its own store).
var _ contract.MessageStore = (*PG)(nil)
var _ contract.ChatHistory = (*PG)(nil)

// now returns epoch-float seconds on the DB-calibrated clock (message timestamps).
func now() float64 { return clock.UnixSeconds() }

// DeleteSessionMessages removes all bob_messages rows for a session id — the delete
// half of cold-session archival (docs/wip-message-ownership.md §6: archive the rows
// to the blob table, then drop them here). Sub-turns no longer use it: their scratch
// transcript lives in an in-memory store (turn/96-substore.go), never in this table.
func (s *PG) DeleteSessionMessages(ctx context.Context, sessionID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM bob_messages WHERE session_id = $1`, sessionID)
	return err
}

// AppendMessage appends one plain conversation row. The bob_messages.token_count
// column is dead data (written historically, never read — compaction budgets
// re-estimate from content via estTokens), so every INSERT here leaves it at its
// DEFAULT 0. The column itself stays (dropping it would be a destructive migration
// for zero benefit).
func (s *PG) AppendMessage(ctx context.Context, sessionID, role, content string, author contract.MsgAuthor) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_messages (session_id, role, content, timestamp, row_kind, meta)
		 VALUES ($1, $2, $3, $4, 'msg', $5::jsonb)`,
		sessionID, role, content, now(), authorJSON(author),
	)
	return err
}

// AppendUserMsgs appends a turn's per-speaker user rows in ONE transaction — atomic so a
// mid-batch failure rolls the whole batch back (a retry then re-persists cleanly instead
// of duplicating an already-committed speaker row). One shared timestamp; id order keeps
// speaker order.
func (s *PG) AppendUserMsgs(ctx context.Context, sessionID string, msgs []contract.UserMsg) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	ts := now()
	for _, m := range msgs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO bob_messages (session_id, role, content, timestamp, row_kind, meta, attachments)
			 VALUES ($1, 'user', $2, $3, 'msg', $4::jsonb, $5::jsonb)`,
			sessionID, m.Text, ts, authorJSON(m.Author), attachmentsJSON(m.Attachments)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AppendAssistantCalls appends the assistant row that emitted tool calls (I2:
// persisted BEFORE dispatch, so a crash mid-dispatch leaves a recoverable trail).
// Nothing is written to token_count (a dead column, see AppendMessage).
func (s *PG) AppendAssistantCalls(ctx context.Context, sessionID, content string, calls []contract.ToolCall, author contract.MsgAuthor) error {
	raw, err := json.Marshal(calls)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx,
		`INSERT INTO bob_messages (session_id, role, content, timestamp, row_kind, tool_calls, meta)
		 VALUES ($1, 'assistant', $2, $3, 'msg', $4, $5::jsonb)`,
		sessionID, content, now(), string(raw), authorJSON(author),
	)
	return err
}

// AppendToolResult appends one tool result row (I3: one per dispatched call).
func (s *PG) AppendToolResult(ctx context.Context, sessionID, toolCallID, toolName, content string) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_messages (session_id, role, content, timestamp, row_kind, tool_call_id, tool_name, meta)
		 VALUES ($1, 'tool', $2, $3, 'msg', $4, $5, $6::jsonb)`,
		sessionID, content, now(), toolCallID, toolName, authorJSON(contract.MsgAuthor{Kind: "tool", Name: toolName}),
	)
	return err
}

// AppendCompactionBatch performs an in-place compaction (spec §6.9) in ONE
// transaction: it writes a summary marker row (role=user, row_kind=summary — the
// marker's identity is the row_kind column, store-stamped; the user role makes the
// post-compaction window open with a user message, so strict templates that reject
// user-less conversations are satisfied by construction) then
// re-appends the recent tail rows by SHAPE (assistant-with-calls keeps its calls; a
// tool row keeps its pairing). Atomic — the whole batch commits or rolls back, so a
// replay (from the last summary marker) never sees a half-written compaction; on
// failure the prior replay start is still valid. sid is unchanged (A5): no state
// rebind, no task-list migration.
func (s *PG) AppendCompactionBatch(ctx context.Context, sessionID, summary string, recent []contract.Message) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	ts := now()

	if _, err := tx.Exec(ctx,
		`INSERT INTO bob_messages (session_id, role, content, timestamp, row_kind)
		 VALUES ($1, 'user', $2, $3, 'summary')`,
		sessionID, summary, ts,
	); err != nil {
		return err
	}
	// The re-appended tail is tagged row_kind=contract.RowKindReplay, NOT 'msg': the same rows
	// still live before the marker as their 'msg' originals, so a full-table viewer
	// (MessagesForScopes) drops 'replay' to show the tail once. The replay window is
	// unaffected — GetReplay anchors on the marker id and includes every later row
	// regardless of kind.
	//
	// Every branch re-writes the attachments bag (m.Atts, populated by the GetReplay
	// the caller split the tail from) — the F92 twin: without it a bag-bearing user
	// row re-inserted here is a NULL-bag replay copy, and once PruneCompacted drops
	// the pre-marker original, RecentAttachments (which scans attachments IS NOT
	// NULL) loses that turn's files forever.
	for _, m := range recent {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			raw, merr := json.Marshal(m.ToolCalls)
			if merr != nil {
				return merr
			}
			_, err = tx.Exec(ctx,
				`INSERT INTO bob_messages (session_id, role, content, timestamp, row_kind, tool_calls, meta, attachments)
				 VALUES ($1, 'assistant', $2, $3, '`+contract.RowKindReplay+`', $4, $5::jsonb, $6::jsonb)`,
				sessionID, m.Content, ts, string(raw), authorJSON(m.Author), attachmentsJSON(m.Atts))
		case m.Role == "tool":
			_, err = tx.Exec(ctx,
				`INSERT INTO bob_messages (session_id, role, content, timestamp, row_kind, tool_call_id, tool_name, meta, attachments)
				 VALUES ($1, 'tool', $2, $3, '`+contract.RowKindReplay+`', $4, $5, $6::jsonb, $7::jsonb)`,
				sessionID, m.Content, ts, m.ToolCallID, m.ToolName, authorJSON(m.Author), attachmentsJSON(m.Atts))
		default:
			_, err = tx.Exec(ctx,
				`INSERT INTO bob_messages (session_id, role, content, timestamp, row_kind, meta, attachments)
				 VALUES ($1, $2, $3, $4, '`+contract.RowKindReplay+`', $5::jsonb, $6::jsonb)`,
				sessionID, m.Role, m.Content, ts, authorJSON(m.Author), attachmentsJSON(m.Atts))
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ImportMessages re-inserts an archived transcript for sid, preserving each row's
// RowKind (incl. summary markers), tool shape (assistant-with-calls keeps its
// calls; a tool row keeps its pairing) and attachments bag (contract.Message.Atts
// — the former session-local ArchivedMessage envelope, folded into the contract
// shape when the compaction path needed the same bag through the turn seam), so
// GetReplay anchoring and RecentAttachments survive a restore. Timestamps are
// reset to now() but rows are inserted in order, so the id ordering
// GetReplay/GetMessages rely on is preserved. The export side is GetReplay (the
// window since the last summary marker, NOT full history — see 40-archive.go).
//
// exec is the executor: Restore passes its shared tx so the transcript import and
// the lifecycle-row recreate commit atomically (contract.Tx embeds contract.DB) —
// no live-but-empty session can ever be observed mid-restore, and a crash rolls the
// whole thing back rather than half-importing.
func (s *PG) ImportMessages(ctx context.Context, exec contract.DB, sessionID string, msgs []contract.Message) error {
	ts := now()
	for _, m := range msgs {
		rk := m.RowKind
		if rk == "" {
			rk = contract.RowKindMsg
		}
		// D10: a cold-archived session exported only its GetReplay window and deleted the
		// 'msg' originals, so on restore the RowKindReplay rows are the ONLY surviving copy
		// of the pre-compaction tail. Left as 'replay' they'd be filtered out of the webui
		// chat browser (MessagesForScopes drops summary/replay), making the conversation
		// look like it starts at the last compaction point. Rewrite them to 'msg' — the
		// originals they once duplicated are gone, so there is no double-render risk.
		if rk == contract.RowKindReplay {
			rk = contract.RowKindMsg
		}
		var err error
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			raw, merr := json.Marshal(m.ToolCalls)
			if merr != nil {
				return merr
			}
			_, err = exec.Exec(ctx,
				`INSERT INTO bob_messages (session_id, role, content, timestamp, row_kind, tool_calls, meta, attachments)
				 VALUES ($1, 'assistant', $2, $3, $4, $5, $6::jsonb, $7::jsonb)`,
				sessionID, m.Content, ts, rk, string(raw), authorJSON(m.Author), attachmentsJSON(m.Atts))
		case m.Role == "tool":
			_, err = exec.Exec(ctx,
				`INSERT INTO bob_messages (session_id, role, content, timestamp, row_kind, tool_call_id, tool_name, meta, attachments)
				 VALUES ($1, 'tool', $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb)`,
				sessionID, m.Content, ts, rk, m.ToolCallID, m.ToolName, authorJSON(m.Author), attachmentsJSON(m.Atts))
		default:
			_, err = exec.Exec(ctx,
				`INSERT INTO bob_messages (session_id, role, content, timestamp, row_kind, meta, attachments)
				 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb)`,
				sessionID, m.Role, m.Content, ts, rk, authorJSON(m.Author), attachmentsJSON(m.Atts))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

const msgCols = `role, content, tool_calls, tool_call_id, tool_name, row_kind, meta, attachments`

// GetMessages returns a session's history in chronological order. limit<=0 → all.
// Reconstructs tool_calls / tool_call_id / tool_name so the model sees a complete
// assistant-calls → tool-results pairing.
func (s *PG) GetMessages(ctx context.Context, sessionID string, limit int) ([]contract.Message, error) {
	q := `SELECT ` + msgCols + ` FROM bob_messages WHERE session_id = $1 ORDER BY id`
	args := []any{sessionID}
	if limit > 0 {
		q = `SELECT ` + msgCols + ` FROM (
		         SELECT id, ` + msgCols + ` FROM bob_messages WHERE session_id = $1 ORDER BY id DESC LIMIT $2
		     ) t ORDER BY id`
		args = append(args, limit)
	}
	return s.scanMessages(ctx, q, args...)
}

// MessagesForScopes implements contract.ChatHistory: conversation rows across every
// session whose session_scope is in scopes, NEWEST FIRST, paged by limit/offset,
// plus the grand total. The join to bob_sessions (a sibling table in the same
// session subsystem) maps scope→session_id, so the message store needs no agora /
// inbox knowledge — the caller (flow/agora) resolves inbox→scopes. Empty scopes,
// or scopes that match no session, → (nil, 0, nil).
func (s *PG) MessagesForScopes(ctx context.Context, scopes []string, limit, offset int) ([]contract.Message, int64, error) {
	if len(scopes) == 0 {
		return nil, 0, nil
	}
	if limit <= 0 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}
	// Hand-rolled IN-list (the leaf/session/store convention — see 40-pending /
	// 60-archive) rather than `= ANY($1)`: keeps array-param encoding out of the
	// driver path entirely. row_kind NOT IN ('summary','replay') drops both the
	// compaction summary markers (role=system) AND the re-appended replay-tail copies
	// (whose 'msg' originals still render before the marker) so the browser shows the
	// real conversation ONCE — applied to BOTH the count and the rows so the page
	// total matches what renders.
	ph := make([]string, len(scopes))
	args := make([]any, 0, len(scopes)+2)
	for i, sc := range scopes {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args = append(args, sc)
	}
	in := strings.Join(ph, ",")
	var total int64
	if err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM bob_messages m JOIN bob_sessions sess ON m.session_id = sess.id
		 WHERE m.row_kind NOT IN ('summary','replay') AND sess.session_scope IN (`+in+`)`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}
	// Custom scan (not the shared scanMessages) because the chat reader also surfaces
	// the wall-clock — selects m.timestamp as the 7th column into Message.TimeUnix.
	q := `SELECT m.role, m.content, m.tool_calls, m.tool_call_id, m.tool_name, m.row_kind, m.timestamp, m.meta
	      FROM bob_messages m JOIN bob_sessions sess ON m.session_id = sess.id
	      WHERE m.row_kind NOT IN ('summary','replay') AND sess.session_scope IN (` + in + `)
	      ORDER BY m.timestamp DESC, m.id DESC
	      LIMIT ` + fmt.Sprintf("$%d", len(scopes)+1) + ` OFFSET ` + fmt.Sprintf("$%d", len(scopes)+2)
	args = append(args, limit, offset)
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var msgs []contract.Message
	for rows.Next() {
		var m contract.Message
		var rawCalls, rawMeta string
		if err := rows.Scan(&m.Role, &m.Content, &rawCalls, &m.ToolCallID, &m.ToolName, &m.RowKind, &m.TimeUnix, &rawMeta); err != nil {
			return nil, 0, err
		}
		if rawCalls != "" {
			if err := json.Unmarshal([]byte(rawCalls), &m.ToolCalls); err != nil {
				return nil, 0, err
			}
		}
		m.Author = metaAuthor(rawMeta)
		msgs = append(msgs, m)
	}
	return msgs, total, rows.Err()
}

// GetReplay returns the replay window (spec §6.9): everything FROM the last summary
// marker (a compaction's start) — so the marker is ALWAYS present and the recent
// tail is whole, however many rows it holds. With no summary yet, it falls back to
// the last `cap` rows (the pre-compaction backstop). Anchoring on the marker (not a
// fixed row count) is what keeps a compaction visible after a long recent tail.
func (s *PG) GetReplay(ctx context.Context, sessionID string, cap int) ([]contract.Message, error) {
	var summaryID int64
	if err := s.db.QueryRow(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM bob_messages WHERE session_id = $1 AND row_kind = 'summary'`,
		sessionID,
	).Scan(&summaryID); err != nil {
		return nil, err
	}
	if summaryID > 0 {
		return s.scanMessages(ctx,
			`SELECT `+msgCols+` FROM bob_messages WHERE session_id = $1 AND id >= $2 ORDER BY id`,
			sessionID, summaryID)
	}
	return s.GetMessages(ctx, sessionID, cap) // no summary → last cap rows
}

func (s *PG) scanMessages(ctx context.Context, q string, args ...any) ([]contract.Message, error) {
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contract.Message
	for rows.Next() {
		var m contract.Message
		var rawCalls, rawMeta string
		var rawAtts []byte
		if err := rows.Scan(&m.Role, &m.Content, &rawCalls, &m.ToolCallID, &m.ToolName, &m.RowKind, &rawMeta, &rawAtts); err != nil {
			return nil, err
		}
		if rawCalls != "" {
			if err := json.Unmarshal([]byte(rawCalls), &m.ToolCalls); err != nil {
				return nil, err
			}
		}
		m.Author = metaAuthor(rawMeta)
		if len(rawAtts) > 0 {
			_ = json.Unmarshal(rawAtts, &m.Atts) // a corrupt bag loses only the bag, not the row
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecentAttachments reconstructs the session's attachment set from the per-row
// attachments JSONB: the most recent `limit` attachment-bearing user rows are scanned
// newest-first and flattened, then deduped by Path (newest wins). This is the durable
// "bag" an attachment-consuming tool selects from — the structured form the model-blind
// path can't read from the prompt text. limit <= 0 applies a sane default. Empty (not an
// error) for a session with no placed attachments.
func (s *PG) RecentAttachments(ctx context.Context, sessionID string, limit int) ([]contract.Attachment, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(ctx,
		`SELECT attachments FROM bob_messages
		  WHERE session_id = $1 AND attachments IS NOT NULL
		  ORDER BY id DESC LIMIT $2`,
		sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []contract.Attachment
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var atts []contract.Attachment
		if err := json.Unmarshal(raw, &atts); err != nil {
			continue // skip a corrupt blob rather than fail the whole read
		}
		all = append(all, atts...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dedupAttachmentsByPath(all), nil
}
