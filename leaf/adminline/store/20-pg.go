package store

import (
	"context"
	"fmt"

	"agentbob/contract"
)

// PG is the Postgres implementation of Store over the shared contract.DB pool.
type PG struct {
	db contract.DB
}

// NewPG returns an admin-line store over db.
func NewPG(db contract.DB) *PG { return &PG{db: db} }

// AppendAlert INSERTs one row and returns its BIGSERIAL id.
func (s *PG) AppendAlert(ctx context.Context, a AdminAlert) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx,
		`INSERT INTO bob_admin_line (ts, level, origin, text, forwarded)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		a.TS, string(a.Level), a.Origin, a.Text, string(a.Forwarded),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("AppendAlert: %w", err)
	}
	return id, nil
}

// MarkAlertForwarded updates one row's forward state (no-op for an unknown id).
func (s *PG) MarkAlertForwarded(ctx context.Context, id int64, state AdminForwardState) error {
	if _, err := s.db.Exec(ctx,
		`UPDATE bob_admin_line SET forwarded = $1 WHERE id = $2`, string(state), id,
	); err != nil {
		return fmt.Errorf("MarkAlertForwarded: %w", err)
	}
	return nil
}

// PruneAdminLine deletes rows older than olderThanUnix, returning the count.
func (s *PG) PruneAdminLine(ctx context.Context, olderThanUnix int64) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM bob_admin_line WHERE ts < $1`, olderThanUnix)
	if err != nil {
		return 0, fmt.Errorf("PruneAdminLine: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("PruneAdminLine: %w", err)
	}
	return n, nil
}

var _ Store = (*PG)(nil)
