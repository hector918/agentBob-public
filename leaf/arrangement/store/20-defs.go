package store

import (
	"context"

	"agentbob/contract"
)

// Def is one arrangement definition row.
type Def struct {
	ID        string
	Company   string
	Author    string
	Status    string // started | done | cancelled
	Spec      string // validated JSON, frozen once started
	CreatedAt int64
	UpdatedAt int64
}

// InsertDef stores a new arrangement and returns its id.
func (s *PG) InsertDef(ctx context.Context, company, author, status, spec string, now int64) (string, error) {
	id := newID("arr")
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_arrangement_defs (id, company, author, status, spec, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $6)`,
		id, company, author, status, spec, now)
	if err != nil {
		return "", err
	}
	return id, nil
}

// DeleteDef removes a def and any of its items (there is no FK/cascade). Used to
// roll back a just-created def whose kickoff item insert failed, so a failed Define
// leaves no orphan 'started' zero-item def behind. Items first, then the def.
func (s *PG) DeleteDef(ctx context.Context, id string) error {
	if _, err := s.db.Exec(ctx,
		`DELETE FROM bob_arrangement_items WHERE arrangement_id = $1`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx,
		`DELETE FROM bob_arrangement_defs WHERE id = $1`, id)
	return err
}

// GetDef reads one arrangement (contract.ErrNoRows when absent).
func (s *PG) GetDef(ctx context.Context, id string) (Def, error) {
	var d Def
	err := s.db.QueryRow(ctx,
		`SELECT id, company, author, status, spec, created_at, updated_at FROM bob_arrangement_defs WHERE id = $1`, id,
	).Scan(&d.ID, &d.Company, &d.Author, &d.Status, &d.Spec, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return Def{}, err
	}
	return d, nil
}

// ListDefs returns all arrangements (statusFilter "" = all, else only that status).
func (s *PG) ListDefs(ctx context.Context, statusFilter string) ([]Def, error) {
	q := `SELECT id, company, author, status, spec, created_at, updated_at FROM bob_arrangement_defs`
	var rows contract.Rows
	var err error
	if statusFilter == "" {
		rows, err = s.db.Query(ctx, q+` ORDER BY created_at DESC`)
	} else {
		rows, err = s.db.Query(ctx, q+` WHERE status = $1 ORDER BY created_at DESC`, statusFilter)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Def
	for rows.Next() {
		var d Def
		if err := rows.Scan(&d.ID, &d.Company, &d.Author, &d.Status, &d.Spec, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountRunningDefs counts a company's RUNNING arrangements — started defs that hold
// at least one runnable item (queued or in_flight) — for the per-company concurrency
// cap. It deliberately does NOT count a started def whose only remaining items are
// parked (blocked / rejected_no_upstream / unmet): those consume no worker capacity and
// would otherwise occupy a cap slot forever (a parked item is non-terminal, so its def
// never reaches CompleteDef's done). An unmet def re-counts naturally once its role is
// staffed and self-heal re-queues the item. (L-AG-D1: cap measures running work, not
// open defs; the parked-row accumulation is a separate backlog.)
func (s *PG) CountRunningDefs(ctx context.Context, company string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(DISTINCT d.id)
		   FROM bob_arrangement_defs d
		   JOIN bob_arrangement_items i ON i.arrangement_id = d.id
		  WHERE d.company = $1 AND d.status = 'started'
		    AND i.status IN ('queued', 'in_flight')`, company,
	).Scan(&n)
	return n, err
}

// PruneTerminalDefs deletes TERMINAL arrangements (status 'cancelled' OR 'done')
// whose terminalization landed before cutoff (updated_at, set by CancelDef/
// CompleteDef). 'started' is live and never pruned. 'done' is the common
// terminal status, so without it bob_arrangement_defs (+ each Spec TEXT) would grow
// unbounded with lifetime arrangement count. It first deletes ALL of those defs'
// items (there is no FK/cascade): Cancel only terminalizes queued items — an
// in_flight/unmet/blocked item would otherwise be left dangling at a now-deleted
// arrangement_id (never reaped by the terminal-item sweep, shown as a phantom live
// row, and a late submit would error). Items go with their def.
func (s *PG) PruneTerminalDefs(ctx context.Context, cutoff int64) (int64, error) {
	sub := `(SELECT id FROM bob_arrangement_defs WHERE status IN ('cancelled','done') AND updated_at < $1)`
	if _, err := s.db.Exec(ctx,
		`DELETE FROM bob_arrangement_items WHERE arrangement_id IN `+sub, cutoff); err != nil {
		return 0, err
	}
	ct, err := s.db.Exec(ctx,
		`DELETE FROM bob_arrangement_defs WHERE status IN ('cancelled','done') AND updated_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := ct.RowsAffected()
	return n, nil
}

// CloseStalledDefs terminalizes STALLED defs → cancelled: status='started' with NO
// runnable (queued/in_flight) item AND no def/item activity since cutoff. Such a def —
// only parked items (blocked / rejected_no_upstream / long-unmet) or none at all — never
// reaches CompleteDef (parked items are non-terminal, so CountLiveForDef stays >0) and
// nobody cancels it, so the rows would accumulate forever (accumulation-inventory #41).
// Closing routes them into PruneTerminalDefs' existing sweep (items go with the def)
// after its grace. A def that regains a runnable item (self-heal requeue, inject) or has
// any recent item activity is untouched. Returns rows closed.
func (s *PG) CloseStalledDefs(ctx context.Context, cutoff, now int64) (int64, error) {
	ct, err := s.db.Exec(ctx,
		`UPDATE bob_arrangement_defs SET status = 'cancelled', updated_at = $1
		 WHERE status = 'started' AND updated_at < $2
		   AND NOT EXISTS (
		     SELECT 1 FROM bob_arrangement_items i
		      WHERE i.arrangement_id = bob_arrangement_defs.id
		        AND (i.status IN ('queued', 'in_flight') OR i.updated_at >= $2))`,
		now, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := ct.RowsAffected()
	return n, nil
}

// CancelDef terminalizes a live def → cancelled, guarded on status='started' (the
// only live status) so a concurrent CompleteDef (→ done) or an already-cancelled def
// is never clobbered. Returns the rows affected: 0 means the def was already
// terminal (done/cancelled), which Cancel treats as a no-op.
func (s *PG) CancelDef(ctx context.Context, id string, now int64) (int64, error) {
	ct, err := s.db.Exec(ctx,
		`UPDATE bob_arrangement_defs SET status = 'cancelled', updated_at = $1
		 WHERE id = $2 AND status = 'started'`, now, id)
	if err != nil {
		return 0, err
	}
	n, _ := ct.RowsAffected()
	return n, nil
}

// CompleteDef marks a STARTED def done — guarded on status='started' so a concurrent
// Cancel (which sets 'cancelled') is never overwritten by a late terminal-item completion.
func (s *PG) CompleteDef(ctx context.Context, id string, now int64) error {
	_, err := s.db.Exec(ctx,
		`UPDATE bob_arrangement_defs SET status = 'done', updated_at = $1 WHERE id = $2 AND status = 'started'`,
		now, id)
	return err
}
