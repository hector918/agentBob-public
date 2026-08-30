package store

import (
	"context"
	"errors"

	"agentbob/contract"
)

// Item is one work item sitting in a role bucket.
type Item struct {
	ID            string
	ArrangementID string
	Role          string // current bucket
	Priority      int
	Payload       string
	Status        string // queued | in_flight | unmet | blocked | done | cancelled | …
	ClaimedBy     string
	ClaimedAt     int64
	CreatedAt     int64
	UpdatedAt     int64
}

// InsertItem adds a queued item to (arrangement, role) and returns its id.
func (s *PG) InsertItem(ctx context.Context, arrangementID, role string, priority int, payload string, now int64) (string, error) {
	id := newID("ari")
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_arrangement_items (id, arrangement_id, role, priority, payload, status, claimed_by, claimed_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, 'queued', '', 0, $6, $6)`,
		id, arrangementID, role, priority, payload, now)
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetItem reads one item by id (contract.ErrNoRows when absent).
func (s *PG) GetItem(ctx context.Context, id string) (Item, error) {
	var it Item
	err := s.db.QueryRow(ctx,
		`SELECT id, arrangement_id, role, priority, payload, status, claimed_by, claimed_at, created_at, updated_at
		 FROM bob_arrangement_items WHERE id = $1`, id,
	).Scan(&it.ID, &it.ArrangementID, &it.Role, &it.Priority, &it.Payload, &it.Status, &it.ClaimedBy, &it.ClaimedAt, &it.CreatedAt, &it.UpdatedAt)
	if err != nil {
		return Item{}, err
	}
	return it, nil
}

// BucketCount is one non-empty queued bucket (the selector's one-query scan).
type BucketCount struct {
	ArrangementID string
	Role          string
	N             int
}

// QueuedBuckets returns every (arrangement, role) bucket with queued items, in one
// GROUP BY query — the selector costs one round-trip instead of one per bucket.
func (s *PG) QueuedBuckets(ctx context.Context) ([]BucketCount, error) {
	return s.bucketsWithStatus(ctx, "queued")
}

// UnmetBuckets returns every (arrangement, role) bucket holding unmet items — the
// re-arm scan (a role that regains members → its unmet items go back to queued).
func (s *PG) UnmetBuckets(ctx context.Context) ([]BucketCount, error) {
	return s.bucketsWithStatus(ctx, "unmet")
}

func (s *PG) bucketsWithStatus(ctx context.Context, status string) ([]BucketCount, error) {
	rows, err := s.db.Query(ctx,
		`SELECT arrangement_id, role, COUNT(*) FROM bob_arrangement_items
		 WHERE status = $1 GROUP BY arrangement_id, role`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BucketCount
	for rows.Next() {
		var b BucketCount
		if err := rows.Scan(&b.ArrangementID, &b.Role, &b.N); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RequeueUnmet flips a bucket's unmet items back to queued — called when the role
// regains members (self-heal a transient staffing gap). Returns rows re-queued.
func (s *PG) RequeueUnmet(ctx context.Context, arrangementID, role string, now int64) (int64, error) {
	res, err := s.db.Exec(ctx,
		`UPDATE bob_arrangement_items SET status = 'queued', updated_at = $1
		 WHERE arrangement_id = $2 AND role = $3 AND status = 'unmet'`,
		now, arrangementID, role)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// StaleInFlight returns in_flight items of STARTED arrangements whose claim is older than
// cutoff — candidates for lease-timeout reclaim. The started-def filter avoids touching a
// just-cancelled def's items (closes the Cancel↔reclaim race). The caller gates each on
// liveness (claimer not running) before requeuing via RequeueInFlight, so a slow-but-alive
// worker is never reclaimed (which would double-execute its side effects).
func (s *PG) StaleInFlight(ctx context.Context, cutoff int64) ([]Item, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, arrangement_id, role, priority, payload, status, claimed_by, claimed_at, created_at, updated_at
		 FROM bob_arrangement_items
		 WHERE status = 'in_flight' AND claimed_at > 0 AND claimed_at < $1
		   AND arrangement_id IN (SELECT id FROM bob_arrangement_defs WHERE status = 'started')`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.ArrangementID, &it.Role, &it.Priority, &it.Payload, &it.Status, &it.ClaimedBy, &it.ClaimedAt, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// RequeueInFlight flips ONE orphaned in_flight item back to queued (lease-timeout recovery
// for a crashed/restarted worker — its turn is gone, and the selector only dispatches
// queued). Guarded by status='in_flight' so it no-ops if the holder submitted/cancel raced.
// RouteItem's `claimed_by == holder AND in_flight` guard then blocks a late submit from the
// original holder, so a reclaim can't double-route. ok=false when nothing matched.
func (s *PG) RequeueInFlight(ctx context.Context, id string, now int64) (bool, error) {
	res, err := s.db.Exec(ctx,
		`UPDATE bob_arrangement_items SET status = 'queued', claimed_by = '', claimed_at = 0, updated_at = $1
		 WHERE id = $2 AND status = 'in_flight'`, now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountQueued reports how many items are queued at (arrangement, role).
func (s *PG) CountQueued(ctx context.Context, arrangementID, role string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM bob_arrangement_items WHERE arrangement_id = $1 AND role = $2 AND status = 'queued'`,
		arrangementID, role,
	).Scan(&n)
	return n, err
}

// CountLiveForDef counts ONE def's NON-terminal items (anything not done/cancelled).
// Used to decide whether a terminal-item completion closes the WHOLE def: with
// arrangement_inject fan-out a def can have several concurrent items, so the def is done
// only when none remain live (parked items — blocked/unmet — also count, not abandoned).
func (s *PG) CountLiveForDef(ctx context.Context, arrangementID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM bob_arrangement_items
		 WHERE arrangement_id = $1 AND status NOT IN ('done', 'cancelled')`,
		arrangementID,
	).Scan(&n)
	return n, err
}

// ClaimTop atomically claims the top queued item of (arrangement, role) for claimedBy:
// pick the highest-priority (then oldest) queued id, then a single-row conditional
// UPDATE flips it in_flight only if still queued. A lost race retries the next
// candidate. ok=false (no error) when the bucket is empty.
func (s *PG) ClaimTop(ctx context.Context, arrangementID, role, claimedBy string, now int64) (Item, bool, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var id string
		err := s.db.QueryRow(ctx,
			`SELECT id FROM bob_arrangement_items
			 WHERE arrangement_id = $1 AND role = $2 AND status = 'queued'
			 ORDER BY priority DESC, created_at ASC LIMIT 1`,
			arrangementID, role,
		).Scan(&id)
		if errors.Is(err, contract.ErrNoRows) {
			return Item{}, false, nil
		}
		if err != nil {
			return Item{}, false, err
		}
		res, err := s.db.Exec(ctx,
			`UPDATE bob_arrangement_items SET status = 'in_flight', claimed_by = $1, claimed_at = $2, updated_at = $2
			 WHERE id = $3 AND status = 'queued'`,
			claimedBy, now, id)
		if err != nil {
			return Item{}, false, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			it, err := s.GetItem(ctx, id)
			if err != nil {
				return Item{}, false, err
			}
			return it, true, nil
		}
		// lost the race for this id — try the next candidate
	}
	return Item{}, false, nil
}

// RouteItem records a submit outcome on an in_flight item the caller holds: move it to
// newRole (forward/back) re-queued, or park it (newRole stays, status=parked). Guarded
// by claimed_by == expectClaimedBy AND status='in_flight' → a non-holder / duplicate
// submit affects 0 rows (ok=false). claim is cleared on success.
func (s *PG) RouteItem(ctx context.Context, itemID, expectClaimedBy, newRole, newStatus, newPayload string, now int64) (bool, error) {
	res, err := s.db.Exec(ctx,
		`UPDATE bob_arrangement_items
		 SET role = $1, status = $2, payload = $3, claimed_by = '', claimed_at = 0, updated_at = $4
		 WHERE id = $5 AND claimed_by = $6 AND status = 'in_flight'`,
		newRole, newStatus, newPayload, now, itemID, expectClaimedBy)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkUnmet parks every queued item at (arrangement, role) as unmet — the dispatcher
// calls it when a role has no live member to drain it. Returns rows parked.
func (s *PG) MarkUnmet(ctx context.Context, arrangementID, role string, now int64) (int64, error) {
	res, err := s.db.Exec(ctx,
		`UPDATE bob_arrangement_items SET status = 'unmet', updated_at = $1
		 WHERE arrangement_id = $2 AND role = $3 AND status = 'queued'`,
		now, arrangementID, role)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CancelLive parks an arrangement's LIVE items (queued AND in_flight) as cancelled
// (Cancel path). in_flight is included so a worker already running the item can't leave it
// stuck forever — its eventual Submit finds the item already 'cancelled', so RouteItem
// matches nothing and the submit is rejected ("状态已变化"); the worker's product survives
// only in its session history, not re-persisted onto the cancelled item.
func (s *PG) CancelLive(ctx context.Context, arrangementID string, now int64) error {
	_, err := s.db.Exec(ctx,
		`UPDATE bob_arrangement_items SET status = 'cancelled', updated_at = $1
		 WHERE arrangement_id = $2 AND status IN ('queued', 'in_flight')`,
		now, arrangementID)
	return err
}

// ListLive returns every non-terminal item (anything not done/cancelled),
// for the webui status table.
func (s *PG) ListLive(ctx context.Context) ([]Item, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, arrangement_id, role, priority, payload, status, claimed_by, claimed_at, created_at, updated_at
		 FROM bob_arrangement_items WHERE status NOT IN ('done', 'cancelled')
		 ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.ArrangementID, &it.Role, &it.Priority, &it.Payload, &it.Status, &it.ClaimedBy, &it.ClaimedAt, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CountByStatus returns the non-terminal item counts keyed by status, in ONE
// GROUP BY — the panel's queue readout. Terminal rows (done/cancelled) are
// excluded: they are history, and mixing them in would make the queue look
// like it grows forever. A status with no rows is simply absent from the map.
func (s *PG) CountByStatus(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.Query(ctx,
		`SELECT status, COUNT(*) FROM bob_arrangement_items
		 WHERE status NOT IN ('done', 'cancelled') GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// ListLiveForArrangement returns ONE arrangement's non-terminal items in the webui's
// DISPLAY order (in_flight first, longest-held first, then oldest) — the arrangement detail
// page. Unpaged: a single arrangement's live set is bounded (the create-time + runtime gates
// prevent runaway buckets).
func (s *PG) ListLiveForArrangement(ctx context.Context, arrangementID string) ([]Item, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, arrangement_id, role, priority, payload, status, claimed_by, claimed_at, created_at, updated_at
		 FROM bob_arrangement_items WHERE arrangement_id = $1 AND status NOT IN ('done', 'cancelled')
		 ORDER BY (status = 'in_flight') DESC,
		          CASE WHEN status = 'in_flight' THEN claimed_at ELSE 0 END ASC,
		          created_at ASC`, arrangementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.ArrangementID, &it.Role, &it.Priority, &it.Payload, &it.Status, &it.ClaimedBy, &it.ClaimedAt, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// PruneTerminal deletes terminal items (done/cancelled) older than cutoff — the
// Housekeeper retention sweep. Returns rows removed.
func (s *PG) PruneTerminal(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.Exec(ctx,
		`DELETE FROM bob_arrangement_items WHERE status IN ('done', 'cancelled') AND updated_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
