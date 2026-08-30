package retrieval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/migrate"
)

// ErrPermanent marks a leaf rejection retrying will never fix (a content-fault
// 4xx). The feeder dead-letters a head batch that hits it so one poison batch
// can't block the FIFO outbox forever; transient errors leave rows for next tick.
var ErrPermanent = errors.New("retrieval: leaf permanently rejected batch")

// outboxCap is the hard row ceiling (safety bound, not a tuning knob). A leaf that
// stays down would otherwise grow the outbox unbounded (each row carries full
// content) and blow the main DB — better to drop the OLDEST cold-memory than wedge
// the DB. The prune sweep trims to this. See docs/cold-memory-trunk.md §3.4.
const outboxCap = 50000

// outboxRow is one pending corpus message: a turn's user/assistant text plus the
// flow-stamped identity, awaiting push to the leaf.
type outboxRow struct {
	ID        int64
	MessageID string // deterministic: sid:<eventID> / sid:<anchorID>:a
	CompanyID string
	SessionID string
	UserID    string // source_handle (or __bob__)
	ChannelID string // room (group scope); "" for a private DM
	Role      string
	Content   string
	Ts        float64
	Lang      string
}

const outboxSchemaV1 = `
CREATE TABLE IF NOT EXISTS bob_retrieval_outbox (
  id          BIGSERIAL PRIMARY KEY,
  message_id  TEXT NOT NULL,
  company_id  TEXT NOT NULL,
  session_id  TEXT NOT NULL,
  user_id     TEXT NOT NULL,
  channel_id  TEXT NOT NULL DEFAULT '',
  role        TEXT NOT NULL,
  content     TEXT NOT NULL,
  ts          DOUBLE PRECISION NOT NULL,
  lang        TEXT NOT NULL DEFAULT '',
  created_at  DOUBLE PRECISION NOT NULL
);
`

// migrateOutbox brings the outbox table to current (own "retrieval" namespace).
func migrateOutbox(ctx context.Context, db contract.DB) error {
	return migrate.Run(ctx, db, "retrieval", []migrate.Step{
		{Version: 1, SQL: outboxSchemaV1},
	})
}

// outbox is the pg-backed pending queue.
type outbox struct{ db contract.DB }

// insert appends a turn's rows in ONE multi-row INSERT — all-or-nothing per turn
// (a single failing row aborts the whole turn's enqueue, vs the old per-row loop's
// partial commit), and one round-trip instead of N. created_at is stamped for
// diagnostics/auditing only; FIFO ordering is solely by the BIGSERIAL id.
func (o *outbox) insert(ctx context.Context, rows []outboxRow) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 10
	ts := now()
	vals := make([]string, len(rows))
	args := make([]any, 0, len(rows)*cols)
	for i, r := range rows {
		b := i * cols
		vals[i] = fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			b+1, b+2, b+3, b+4, b+5, b+6, b+7, b+8, b+9, b+10)
		args = append(args, r.MessageID, r.CompanyID, r.SessionID, r.UserID, r.ChannelID,
			r.Role, r.Content, r.Ts, r.Lang, ts)
	}
	_, err := o.db.Exec(ctx,
		`INSERT INTO bob_retrieval_outbox
		   (message_id, company_id, session_id, user_id, channel_id, role, content, ts, lang, created_at)
		 VALUES `+strings.Join(vals, ","), args...)
	if err != nil {
		return fmt.Errorf("retrieval: outbox insert: %w", err)
	}
	return nil
}

// pop reads the oldest limit rows (does NOT delete — del() removes them after a
// successful push; a failed push leaves them for the next tick). The caller passes
// a clamped positive limit (BatchSize → [1,200]).
func (o *outbox) pop(ctx context.Context, limit int) ([]outboxRow, error) {
	rows, err := o.db.Query(ctx,
		`SELECT id, message_id, company_id, session_id, user_id, channel_id, role, content, ts, lang
		   FROM bob_retrieval_outbox ORDER BY id ASC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("retrieval: outbox pop: %w", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.ID, &r.MessageID, &r.CompanyID, &r.SessionID, &r.UserID,
			&r.ChannelID, &r.Role, &r.Content, &r.Ts, &r.Lang); err != nil {
			return nil, fmt.Errorf("retrieval: outbox scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// del removes rows by id (after a successful push). Empty → no-op. Builds an
// IN-list (database/sql + pgx stdlib driver doesn't take a slice for ANY($1)).
func (o *outbox) del(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	_, err := o.db.Exec(ctx,
		"DELETE FROM bob_retrieval_outbox WHERE id IN ("+strings.Join(ph, ",")+")", args...)
	if err != nil {
		return fmt.Errorf("retrieval: outbox delete: %w", err)
	}
	return nil
}

// outboxStats is the panel readout: how deep the queue is and how long its head
// has been waiting. Depth alone can't tell a busy minute from a dead leaf — a
// steady 30 rows that keeps turning over is healthy, 30 rows whose oldest is
// three weeks old is the outage.
type outboxStats struct {
	Rows      int64
	OldestAge time.Duration // 0 when empty
}

// stats reads depth + head age in one round-trip. MIN over an empty table is
// NULL, so it is COALESCEd here rather than scanned into a null wrapper — an
// empty outbox has no head to be old.
func (o *outbox) stats(ctx context.Context) (outboxStats, error) {
	var (
		rows   int64
		oldest float64
	)
	err := o.db.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(MIN(created_at), 0) FROM bob_retrieval_outbox`).Scan(&rows, &oldest)
	if err != nil {
		return outboxStats{}, fmt.Errorf("retrieval: outbox stats: %w", err)
	}
	out := outboxStats{Rows: rows}
	if rows > 0 {
		// created_at is stamped from the same calibrated clock now() reads, so this
		// is a like-for-like subtraction; clamp because the two are read at
		// different moments and a calibration resync can move the offset backwards.
		if age := now() - oldest; age > 0 {
			out.OldestAge = time.Duration(age * float64(time.Second))
		}
	}
	return out, nil
}

// prune trims the outbox to the newest cap rows (FIFO drop-oldest), for the
// leaf-down-forever case. Returns rows removed.
func (o *outbox) prune(ctx context.Context, keep int) (int64, error) {
	res, err := o.db.Exec(ctx,
		`DELETE FROM bob_retrieval_outbox
		  WHERE id NOT IN (SELECT id FROM bob_retrieval_outbox ORDER BY id DESC LIMIT $1)`, keep)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
