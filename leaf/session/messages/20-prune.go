package messages

// In-place compaction (A5) rewrites nothing: the summary marker + replay tail are
// APPENDED, and the rows before the marker just stay in bob_messages. GetReplay
// anchors on the LAST summary marker, so pre-marker rows are never replayed again —
// they only feed full-transcript readers (MessagesForScopes / archival export).
// Without a sweep a long-lived, repeatedly-compacting session grows without bound
// (docs/accumulation-inventory.md A5). PruneCompacted is that sweep, run from the
// session module's Housekeeper task.

import "context"

// PruneCompacted deletes rows superseded by an in-place compaction: for each
// session, rows BEFORE its last committed summary marker (id < marker) that are
// also older than olderThanSeconds. The marker itself and everything after it —
// the whole replay window — are untouched, so GetReplay is unaffected. The age
// gate keeps the recent transcript readable by full-history viewers; only rows
// past the retention window go. AppendCompactionBatch commits atomically, so any
// marker row seen here is a committed one. Returns the number of pruned rows.
func (s *PG) PruneCompacted(ctx context.Context, olderThanSeconds float64) (int, error) {
	if olderThanSeconds <= 0 {
		return 0, nil
	}
	cutoff := now() - olderThanSeconds
	res, err := s.db.Exec(ctx, `
		DELETE FROM bob_messages m
		 USING (SELECT session_id, MAX(id) AS marker_id
		          FROM bob_messages
		         WHERE row_kind = 'summary'
		         GROUP BY session_id) mk
		 WHERE m.session_id = mk.session_id
		   AND m.id < mk.marker_id
		   AND m.timestamp < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
