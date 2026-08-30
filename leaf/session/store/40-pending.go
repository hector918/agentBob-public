package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// AddPending records an unanswered inbound message (crash recovery). extras is
// marshalled to JSONB; nil/empty round-trips as '{}'. Today extras only carries
// a merged Telegram album's media_group_id (debug/audit trace).
func (s *PG) AddPending(ctx context.Context, sessionScope, source, chatID, threadID, msgID string, extras map[string]any) error {
	extrasJSON, err := encodeExtras(extras)
	if err != nil {
		return fmt.Errorf("AddPending: encode extras: %w", err)
	}
	_, err = s.db.Exec(ctx,
		`INSERT INTO bob_pending_inbound (session_scope, source, chat_id, thread_id, msg_id, queued_at, extras)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		 ON CONFLICT (session_scope, msg_id) DO NOTHING`,
		sessionScope, source, chatID, threadID, msgID, now(), extrasJSON,
	)
	return err
}

func encodeExtras(extras map[string]any) (string, error) {
	if len(extras) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(extras)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// RemovePending deletes pending rows by (scope, msg ids).
func (s *PG) RemovePending(ctx context.Context, sessionScope string, msgIDs []string) error {
	if len(msgIDs) == 0 {
		return nil
	}
	ph := make([]string, len(msgIDs))
	args := make([]any, 0, len(msgIDs)+1)
	args = append(args, sessionScope)
	for i, id := range msgIDs {
		ph[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	_, err := s.db.Exec(ctx,
		`DELETE FROM bob_pending_inbound WHERE session_scope = $1 AND msg_id IN (`+strings.Join(ph, ",")+`)`,
		args...,
	)
	return err
}

// RemovePendingForChat deletes pending rows for one chat, bounded to the
// caller's ListPendingChats snapshot so rows added after it keep their
// crash-recovery protection.
func (s *PG) RemovePendingForChat(ctx context.Context, source, chatID, threadID string, msgIDs []string) error {
	if len(msgIDs) == 0 {
		return nil
	}
	ph := make([]string, len(msgIDs))
	args := make([]any, 0, len(msgIDs)+3)
	args = append(args, source, chatID, threadID)
	for i, id := range msgIDs {
		ph[i] = fmt.Sprintf("$%d", i+4)
		args = append(args, id)
	}
	_, err := s.db.Exec(ctx,
		`DELETE FROM bob_pending_inbound WHERE source = $1 AND chat_id = $2 AND thread_id = $3 AND msg_id IN (`+strings.Join(ph, ",")+`)`,
		args...,
	)
	return err
}

// ListPendingChats groups pending rows by chat, snapshotting each chat's msg_ids
// so the post-notice cleanup can delete exactly the rows it saw.
func (s *PG) ListPendingChats(ctx context.Context) ([]PendingChat, error) {
	rows, err := s.db.Query(ctx,
		`SELECT source, chat_id, thread_id, msg_id FROM bob_pending_inbound ORDER BY source, chat_id, thread_id, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingChat
	idx := map[[3]string]int{}
	for rows.Next() {
		var source, chatID, threadID, msgID string
		if err := rows.Scan(&source, &chatID, &threadID, &msgID); err != nil {
			return nil, err
		}
		key := [3]string{source, chatID, threadID}
		i, ok := idx[key]
		if !ok {
			i = len(out)
			idx[key] = i
			out = append(out, PendingChat{Source: source, ChatID: chatID, ThreadID: threadID})
		}
		out[i].MsgIDs = append(out[i].MsgIDs, msgID)
	}
	return out, rows.Err()
}

// CountPending returns how many in-flight rows the WAL holds. Depth only — the
// rows themselves are ListPendingChats' business; this is the operator's "is the
// crash-recovery queue draining" number, cheap enough for a panel poll.
func (s *PG) CountPending(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM bob_pending_inbound`).Scan(&n)
	return n, err
}

// ClearStalePending deletes pending rows older than olderThanSeconds.
func (s *PG) ClearStalePending(ctx context.Context, olderThanSeconds float64) (int, error) {
	if olderThanSeconds <= 0 {
		return 0, nil
	}
	cutoff := now() - olderThanSeconds
	res, err := s.db.Exec(ctx, `DELETE FROM bob_pending_inbound WHERE queued_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
