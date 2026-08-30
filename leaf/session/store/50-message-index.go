package store

import (
	"context"
	"database/sql"
	"errors"

	"agentbob/contract"
)

// RecordSent indexes a delivered bot message so a later reply to it routes to the
// same session. Keyed by (session_scope, msg_id) → session_id — the three
// coordinates that matter; the scope already encodes source+chat, so no separate
// platform/chat columns (B-4). Called by a flow (which holds the sink's sent
// msg_id + the wire scope) through contract.MessageIndexer.
//
// MINIMAL SLICE: msg_row_id is recorded NULL — the reply-anchored context-window
// bound (the bob_messages high-water id) lands with a later turn-module change.
func (s *PG) RecordSent(ctx context.Context, sessionScope, msgID, sessionID string) error {
	if sessionScope == "" || msgID == "" {
		return nil
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_message_index (session_scope, msg_id, session_id, ts, msg_row_id)
		 VALUES ($1, $2, $3, $4, NULL)
		 ON CONFLICT (session_scope, msg_id) DO UPDATE SET session_id = EXCLUDED.session_id, ts = EXCLUDED.ts`,
		sessionScope, msgID, sessionID, now(),
	)
	return err
}

// SessionForMessage looks up the session a (scope, msg) maps to — the reply-
// routing read the resolver runs on a reply-to-bot inbound (the reply arrives in
// the same scope the message was sent in).
func (s *PG) SessionForMessage(ctx context.Context, sessionScope, msgID string) (sessionID string, msgRowID int64, ok bool, err error) {
	if sessionScope == "" || msgID == "" {
		return "", 0, false, nil
	}
	var rowID sql.NullInt64 // NULL until the messages high-water bound lands
	err = s.db.QueryRow(ctx,
		`SELECT session_id, msg_row_id FROM bob_message_index WHERE session_scope = $1 AND msg_id = $2`,
		sessionScope, msgID,
	).Scan(&sessionID, &rowID)
	if errors.Is(err, contract.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return sessionID, rowID.Int64, true, nil
}

// PruneMessageIndex deletes index rows older than olderThanSeconds. Registered as
// session housekeeping (S-3) so the table — which grows one row per delivered bot
// message once RecordSent is wired — doesn't grow without bound.
func (s *PG) PruneMessageIndex(ctx context.Context, olderThanSeconds float64) (int, error) {
	if olderThanSeconds <= 0 {
		return 0, nil
	}
	res, err := s.db.Exec(ctx, `DELETE FROM bob_message_index WHERE ts < $1`, now()-olderThanSeconds)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
