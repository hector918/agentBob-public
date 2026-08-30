package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agentbob/contract"
)

// activeSessionID returns the cursor-elected active sid for key, or ("", false,
// nil) if none. Cursor-then-started_at ordering; delegated sub-sessions
// (kind='sub') are excluded — they are not a scope's conversations.
func (s *PG) activeSessionID(ctx context.Context, key string) (string, bool, error) {
	var id string
	err := s.db.QueryRow(ctx,
		`SELECT id FROM bob_sessions
		 WHERE session_scope = $1 AND ended_at IS NULL AND kind <> 'sub'
		 ORDER BY COALESCE(cursor_set_at, 0) DESC, started_at DESC
		 LIMIT 1`,
		key,
	).Scan(&id)
	if errors.Is(err, contract.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// MarkSessionAdmin records that an admin addressed this session (promote-only,
// idempotent — the conditional WHERE makes it a no-op once already set, so it
// doesn't amplify into a write per admin turn).
func (s *PG) MarkSessionAdmin(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	_, err := s.db.Exec(ctx,
		`UPDATE bob_sessions SET admin_owned = TRUE WHERE id = $1 AND NOT admin_owned`, sessionID)
	if err != nil {
		return fmt.Errorf("MarkSessionAdmin %s: %w", sessionID, err)
	}
	return nil
}

// SetCursor marks sessionID as the scope's active session.
func (s *PG) SetCursor(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	_, err := s.db.Exec(ctx,
		`UPDATE bob_sessions SET cursor_set_at = $1 WHERE id = $2`, now(), sessionID)
	if err != nil {
		return fmt.Errorf("SetCursor %s: %w", sessionID, err)
	}
	return nil
}

func (s *PG) createSession(ctx context.Context, key, source, userID, model, parentID, callerID string) (string, error) {
	id := newID("s")
	var parent any
	if parentID != "" {
		parent = parentID
	}
	var caller any
	if callerID != "" {
		caller = callerID
	}
	// turn_mode is the BIRTH STAMP (docs/turn-driver-split.md): copied from the
	// scope's default (bob_scope_prefs.turn_mode, the /looping toggle) atomically
	// with the INSERT, so a session's mode is fixed at creation and a concurrent
	// toggle can never tear it. No prefs row → '' (regular).
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_sessions (id, session_scope, source, user_id, model, parent_session_id, caller_session_id, started_at, turn_mode)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
		         COALESCE((SELECT turn_mode FROM bob_scope_prefs WHERE session_scope = $2), ''))`,
		id, key, source, userID, model, parent, caller, now(),
	)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return id, nil
}

// EnsureSession returns the scope's active sid, creating one if absent.
func (s *PG) EnsureSession(ctx context.Context, key, source, userID, model string) (string, bool, error) {
	if id, ok, err := s.activeSessionID(ctx, key); err != nil {
		return "", false, err
	} else if ok {
		return id, false, nil
	}
	sid, err := s.createSession(ctx, key, source, userID, model, "", "")
	return sid, err == nil, err
}

// CurrentSession returns the scope's active sid without creating one.
func (s *PG) CurrentSession(ctx context.Context, key string) (string, bool, error) {
	return s.activeSessionID(ctx, key)
}

// OpenSession creates a new session without ending existing ones (multi-session).
func (s *PG) OpenSession(ctx context.Context, key, source, userID, model string) (string, error) {
	return s.createSession(ctx, key, source, userID, model, "", "")
}

// OpenSessionWithCaller is OpenSession plus the agora dispatch caller frame
// (callerSid → caller_session_id; parent stays empty, kind=”).
func (s *PG) OpenSessionWithCaller(ctx context.Context, key, source, userID, model, callerSid string) (string, error) {
	return s.createSession(ctx, key, source, userID, model, "", callerSid)
}

// NewGeneration ends the scope's active session (if any) and opens a fresh one —
// the corruption escape hatch. No parent linkage (single-meaning column).
func (s *PG) NewGeneration(ctx context.Context, key, source, userID, model string) (string, error) {
	if id, ok, err := s.activeSessionID(ctx, key); err != nil {
		return "", err
	} else if ok {
		if source == "" || userID == "" {
			var prevSrc, prevUser string
			if scanErr := s.db.QueryRow(ctx, `SELECT source, user_id FROM bob_sessions WHERE id = $1`, id).Scan(&prevSrc, &prevUser); scanErr == nil {
				if source == "" {
					source = prevSrc
				}
				if userID == "" {
					userID = prevUser
				}
			}
		}
		if err := s.EndSession(ctx, id, EndReasonGenerationRotate); err != nil {
			return "", err
		}
	}
	return s.createSession(ctx, key, source, userID, model, "", "")
}

// SessionByID returns one session. Preserves the not-found signal
// (contract.ErrNoRows) so callers can errors.Is-check it.
func (s *PG) SessionByID(ctx context.Context, sessionID string) (SessionInfo, error) {
	if sessionID == "" {
		return SessionInfo{}, fmt.Errorf("SessionByID: empty sessionID")
	}
	var si SessionInfo
	var mode, preferTags, turnMode string
	err := s.db.QueryRow(ctx, `
		SELECT id, session_scope, source, user_id, COALESCE(model,''),
		       COALESCE(parent_session_id,''), COALESCE(caller_session_id,''),
		       COALESCE(title,''),
		       started_at, COALESCE(ended_at, 0), COALESCE(admin_owned, FALSE),
		       COALESCE(mode,''), COALESCE(prefer_model_tags,''), COALESCE(turn_mode,'')
		FROM bob_sessions WHERE id = $1`, sessionID,
	).Scan(&si.ID, &si.Key, &si.Source, &si.UserID, &si.Model,
		&si.ParentID, &si.CallerID, &si.Title, &si.StartedAt, &si.EndedAt, &si.AdminOwned, &mode, &preferTags, &turnMode)
	if err != nil {
		return SessionInfo{}, fmt.Errorf("SessionByID %s: %w", sessionID, err)
	}
	si.Mode = contract.SessionMode(mode)
	si.PreferModelTags = splitTags(preferTags)
	si.TurnMode = contract.TurnMode(turnMode)
	return si, nil
}

// EndSession marks a session ended (idempotent on already-ended).
func (s *PG) EndSession(ctx context.Context, sessionID, reason string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE bob_sessions SET ended_at = $1, end_reason = $2 WHERE id = $3 AND ended_at IS NULL`,
		now(), reason, sessionID)
	return err
}

// GetSessionMode returns the session's persisted flow-declared mode; a missing
// row / empty / invalid column reads as SessionModeNone.
func (s *PG) GetSessionMode(ctx context.Context, sessionID string) (contract.SessionMode, error) {
	var mode string
	err := s.db.QueryRow(ctx, `SELECT mode FROM bob_sessions WHERE id = $1`, sessionID).Scan(&mode)
	if errors.Is(err, contract.ErrNoRows) {
		return contract.SessionModeNone, nil
	}
	if err != nil {
		return contract.SessionModeNone, err
	}
	m := contract.SessionMode(mode)
	if !m.IsValid() {
		return contract.SessionModeNone, nil
	}
	return m, nil
}

// SetSessionMode persists the session's flow-declared mode (clamped to none if
// invalid).
func (s *PG) SetSessionMode(ctx context.Context, sessionID string, mode contract.SessionMode) error {
	if !mode.IsValid() {
		mode = contract.SessionModeNone
	}
	_, err := s.db.Exec(ctx, `UPDATE bob_sessions SET mode = $1 WHERE id = $2`, string(mode), sessionID)
	return err
}

// SetTitle sets (or clears, on empty) a session's title.
func (s *PG) SetTitle(ctx context.Context, sessionID, title string) error {
	title = strings.TrimSpace(title)
	var t any
	if title != "" {
		t = title
	}
	_, err := s.db.Exec(ctx, `UPDATE bob_sessions SET title = $1 WHERE id = $2`, t, sessionID)
	return err
}

// SetSessionModel sets a session's model (no-op on empty).
func (s *PG) SetSessionModel(ctx context.Context, sessionID, model string) error {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	_, err := s.db.Exec(ctx, `UPDATE bob_sessions SET model = $1 WHERE id = $2`, model, sessionID)
	return err
}

// SetSessionPreferModelTags persists (or clears, on empty) the session's sticky
// soft model-tag preference (comma-joined in prefer_model_tags).
func (s *PG) SetSessionPreferModelTags(ctx context.Context, sessionID string, tags []string) error {
	_, err := s.db.Exec(ctx, `UPDATE bob_sessions SET prefer_model_tags = $1 WHERE id = $2`,
		joinTags(tags), sessionID)
	return err
}

// joinTags / splitTags map the []string tag list to/from the comma-joined column.
// splitTags drops empty segments so ” round-trips to nil (no preference).
func joinTags(tags []string) string { return strings.Join(tags, ",") }

func splitTags(col string) []string {
	if col == "" {
		return nil
	}
	var out []string
	for _, t := range strings.Split(col, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// AliveSessionsByKey returns a scope's alive (non-sub) sessions, newest first.
// Preview is empty until the messages table lands with the turn module.
func (s *PG) AliveSessionsByKey(ctx context.Context, key string) ([]SessionInfo, error) {
	if key == "" {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT s.id, s.session_scope, s.source, s.user_id, s.model,
		       COALESCE(s.parent_session_id, ''), COALESCE(s.caller_session_id, ''),
		       COALESCE(s.title, ''),
		       s.started_at, COALESCE(s.ended_at, 0), ''
		FROM bob_sessions s
		WHERE s.session_scope = $1 AND s.ended_at IS NULL AND s.kind <> 'sub'
		ORDER BY s.started_at DESC`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSessions(rows)
}

// ListSessions returns recent sessions, optionally filtered by source.
func (s *PG) ListSessions(ctx context.Context, source string, limit int) ([]SessionInfo, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT s.id, s.session_scope, s.source, s.user_id, s.model,
	             COALESCE(s.parent_session_id, ''), COALESCE(s.caller_session_id, ''),
	             COALESCE(s.title, ''),
	             s.started_at, COALESCE(s.ended_at, 0), ''
	      FROM bob_sessions s`
	args := []any{}
	pos := 1
	if source != "" {
		q += fmt.Sprintf(` WHERE s.source = $%d`, pos)
		args = append(args, source)
		pos++
	}
	q += fmt.Sprintf(` ORDER BY s.started_at DESC LIMIT $%d`, pos)
	args = append(args, limit)
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSessions(rows)
}

// scanSessions decodes an (11-column) session result set.
func scanSessions(rows contract.Rows) ([]SessionInfo, error) {
	var out []SessionInfo
	for rows.Next() {
		var si SessionInfo
		if err := rows.Scan(&si.ID, &si.Key, &si.Source, &si.UserID, &si.Model,
			&si.ParentID, &si.CallerID, &si.Title, &si.StartedAt, &si.EndedAt, &si.Preview); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}
