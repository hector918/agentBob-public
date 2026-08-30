package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agentbob/contract"
)

// ColdRow is one cold-session candidate: identity + the timestamps the archiver
// records in the archive row. LastMsgAt is COALESCE(MAX(msg.timestamp),
// started_at) — the last-activity age the cutoff is judged against. EndedAt is 0
// when the session was still alive (revivable on restore).
type ColdRow struct {
	SessionID string
	Scope     string
	Source    string
	StartedAt float64
	LastMsgAt float64
	EndedAt   float64 // 0 = was alive when it went cold (revivable)
}

// ColdSessionIDs returns up to `limit` sessions whose last activity is older than
// cutoff (epoch seconds) — dead or alive, both archived (§11.3), oldest first.
// Last activity = COALESCE(last message timestamp, started_at), so a never-used
// session is judged by its start. Delegated sub-sessions (kind='sub') are excluded.
//
// The last-message lookup is a LATERAL "ORDER BY id DESC LIMIT 1" per session,
// served by the existing (session_id, id) index — NOT a full-table MAX(timestamp)
// GROUP BY over all of bob_messages (timestamp is monotonic with the serial id, so
// the newest row carries the newest timestamp). Cost is O(candidate sessions × index
// lookup), driven by the smaller bob_sessions, not by bob_messages size. The `limit`
// caps work per sweep so a large first-run backlog drains over successive sweeps
// rather than one unbounded serial pass. Intra-module (both tables are session
// subpackages over the same contract.DB).
func (s *PG) ColdSessionIDs(ctx context.Context, cutoff float64, limit int) ([]ColdRow, error) {
	rows, err := s.db.Query(ctx, `
		SELECT s.id, s.session_scope, s.source, s.started_at,
		       COALESCE(last.ts, s.started_at) AS last_msg_at,
		       COALESCE(s.ended_at, 0) AS ended_at
		FROM bob_sessions s
		LEFT JOIN LATERAL (
		    SELECT timestamp AS ts FROM bob_messages m
		    WHERE m.session_id = s.id ORDER BY m.id DESC LIMIT 1
		) last ON true
		WHERE COALESCE(last.ts, s.started_at) < $1 AND s.kind <> 'sub'
		ORDER BY COALESCE(last.ts, s.started_at)
		LIMIT $2`,
		cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColdRow
	for rows.Next() {
		var c ColdRow
		if err := rows.Scan(&c.SessionID, &c.Scope, &c.Source, &c.StartedAt, &c.LastMsgAt, &c.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ArchiveRow inserts one archive row (ON CONFLICT DO NOTHING — idempotent on a
// crash-retry: a row archived but not yet dropped from the hot tables is re-seen
// as cold and re-archived to the same primary key, harmlessly). endedAt 0 is
// stored as NULL (= was alive = revivable).
func (s *PG) ArchiveRow(ctx context.Context, sessionID, scope, source string, startedAt, lastMsgAt, endedAt, archivedAt float64, payload string) error {
	var ended any
	if endedAt != 0 {
		ended = endedAt
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO bob_sessions_archive
		    (session_id, session_scope, source, started_at, last_msg_at, ended_at, archived_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (session_id) DO NOTHING`,
		sessionID, scope, source, startedAt, lastMsgAt, ended, archivedAt, payload)
	if err != nil {
		return fmt.Errorf("ArchiveRow %s: %w", sessionID, err)
	}
	return nil
}

// DeleteSession drops one bob_sessions row (the hot-table half of archival).
func (s *PG) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM bob_sessions WHERE id = $1`, sessionID)
	if err != nil {
		return fmt.Errorf("DeleteSession %s: %w", sessionID, err)
	}
	return nil
}

// ArchivedSession returns the archived payload + revivability for a sid. ok=false
// (no error) when the sid is not archived. wasAlive = ended_at IS NULL (the
// session was alive when archived → revivable).
func (s *PG) ArchivedSession(ctx context.Context, sessionID string) (payload string, wasAlive bool, ok bool, err error) {
	var endedAt *float64
	row := s.db.QueryRow(ctx,
		`SELECT payload, ended_at FROM bob_sessions_archive WHERE session_id = $1`, sessionID)
	if err := row.Scan(&payload, &endedAt); err != nil {
		if errors.Is(err, contract.ErrNoRows) {
			return "", false, false, nil
		}
		return "", false, false, fmt.Errorf("ArchivedSession %s: %w", sessionID, err)
	}
	return payload, endedAt == nil, true, nil
}

// ArchivedSessionScope returns a cold-archived session's own scope key
// (bob_sessions_archive.session_scope) — used to recover a virtual-group member
// sub-scope when a reply points at a session that could not be revived (F6).
// ok=false (no error) when the sid is not archived.
func (s *PG) ArchivedSessionScope(ctx context.Context, sessionID string) (scope string, ok bool, err error) {
	row := s.db.QueryRow(ctx,
		`SELECT session_scope FROM bob_sessions_archive WHERE session_id = $1`, sessionID)
	if err := row.Scan(&scope); err != nil {
		if errors.Is(err, contract.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("ArchivedSessionScope %s: %w", sessionID, err)
	}
	return scope, true, nil
}

// DeleteArchived drops one archive row (the restore path, after rehydration). exec
// is the executor — Restore threads its shared tx here so recreate+import+delete
// commit atomically (contract.Tx embeds contract.DB).
func (s *PG) DeleteArchived(ctx context.Context, exec contract.DB, sessionID string) error {
	_, err := exec.Exec(ctx, `DELETE FROM bob_sessions_archive WHERE session_id = $1`, sessionID)
	if err != nil {
		return fmt.Errorf("DeleteArchived %s: %w", sessionID, err)
	}
	return nil
}

// RecreateArchivedSession re-inserts a bob_sessions row with the SAME sid, alive
// (ended_at NULL) — the lifecycle half of restore. parent/caller links are not
// carried (single-meaning columns; a revived conversation re-roots flat). admin_owned,
// mode and prefer_model_tags ARE carried (captured in the archive payload): a revived
// admin session keeps admin authority on a headless continuation rather than silently
// dropping to FALSE, a revived auto session self-resumes instead of re-deriving to
// none, and a revived session keeps its sticky /uncensored model preference. ON
// CONFLICT DO NOTHING guards a racing concurrent restore. exec = the restore tx.
func (s *PG) RecreateArchivedSession(ctx context.Context, exec contract.DB, info SessionInfo) error {
	var title any
	if info.Title != "" {
		title = info.Title
	}
	mode := info.Mode
	if mode == "" {
		mode = contract.SessionModeNone
	}
	_, err := exec.Exec(ctx, `
		INSERT INTO bob_sessions (id, session_scope, source, user_id, model, title, started_at, ended_at, admin_owned, mode, prefer_model_tags, turn_mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULL, $8, $9, $10, $11)
		ON CONFLICT (id) DO NOTHING`,
		info.ID, info.Key, info.Source, info.UserID, info.Model, title, info.StartedAt, info.AdminOwned, string(mode), joinTags(info.PreferModelTags), string(info.TurnMode))
	if err != nil {
		return fmt.Errorf("RecreateArchivedSession %s: %w", info.ID, err)
	}
	return nil
}

// PurgeArchivedBefore hard-deletes archive rows older than cutoff (archived_at <
// cutoff) and returns their sids, so the caller can drop the matching reply-index
// rows too (the second-level cleanup, §11.8). Past this hard TTL a reply opens a
// fresh session.
func (s *PG) PurgeArchivedBefore(ctx context.Context, cutoff float64) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`DELETE FROM bob_sessions_archive WHERE archived_at < $1 RETURNING session_id`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		sids = append(sids, sid)
	}
	return sids, rows.Err()
}

// DeleteMessageIndexForSessions drops reply-routing rows for the given sids (the
// second-level cleanup pairs this with PurgeArchivedBefore). No-op on empty. An
// IN-placeholder list (not a bound array) — the database/sql + pgx-stdlib pool
// can't encode a Go slice as a single parameter (mirrors RemovePending). Chunked at
// ≤1000 bind parameters per statement so a large purge can't exceed PostgreSQL's
// int16 (65535) bind-parameter cap — which would error AFTER the archive rows were
// already deleted.
func (s *PG) DeleteMessageIndexForSessions(ctx context.Context, sids []string) error {
	const chunk = 1000
	for start := 0; start < len(sids); start += chunk {
		end := start + chunk
		if end > len(sids) {
			end = len(sids)
		}
		batch := sids[start:end]
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, sid := range batch {
			ph[i] = fmt.Sprintf("$%d", i+1)
			args[i] = sid
		}
		_, err := s.db.Exec(ctx,
			`DELETE FROM bob_message_index WHERE session_id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return fmt.Errorf("DeleteMessageIndexForSessions: %w", err)
		}
	}
	return nil
}
