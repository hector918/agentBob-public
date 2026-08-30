package store

import (
	"context"
	"errors"
	"fmt"

	"agentbob/contract"
)

// Employments CRUD over bob_agora_employments.

const employmentSelect = `
SELECT id, member_id, company_id, role_name, status, started_at, COALESCE(ended_at, 0)
  FROM bob_agora_employments`

// Hire inserts an active Employment linking memberID to (companyID, roleName).
func (s *PG) Hire(ctx context.Context, memberID, companyID, roleName string, nowUnix int64) (Employment, error) {
	if memberID == "" || companyID == "" || roleName == "" {
		return Employment{}, fmt.Errorf("agora: Hire requires non-empty memberID + companyID + roleName")
	}
	e := Employment{
		ID:        newID("emp"),
		MemberID:  memberID,
		CompanyID: companyID,
		RoleName:  roleName,
		Status:    "active",
		StartedAt: nowUnix,
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO bob_agora_employments
    (id, member_id, company_id, role_name, status, started_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
		e.ID, e.MemberID, e.CompanyID, e.RoleName, e.Status, e.StartedAt,
	)
	if err != nil {
		return Employment{}, fmt.Errorf("agora: hire: %w", err)
	}
	return e, nil
}

// GetEmployment returns one Employment by id (contract.ErrNoRows when absent).
func (s *PG) GetEmployment(ctx context.Context, id string) (Employment, error) {
	return scanEmployment(s.db.QueryRow(ctx, employmentSelect+` WHERE id = $1`, id))
}

// ListEmploymentsByMember returns a Member's employments (any status).
func (s *PG) ListEmploymentsByMember(ctx context.Context, memberID string) ([]Employment, error) {
	rows, err := s.db.Query(ctx, employmentSelect+` WHERE member_id = $1 ORDER BY started_at ASC`, memberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEmployments(rows)
}

// ListEmployments returns every Employment (or active-only when activeOnly).
func (s *PG) ListEmployments(ctx context.Context, activeOnly bool) ([]Employment, error) {
	q := employmentSelect + ` ORDER BY started_at ASC`
	if activeOnly {
		q = employmentSelect + ` WHERE status = 'active' ORDER BY started_at ASC`
	}
	rows, err := s.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEmployments(rows)
}

// Terminate marks an Employment terminated. Returns contract.ErrNoRows when id
// has no row.
func (s *PG) Terminate(ctx context.Context, id string, nowUnix int64) error {
	return s.affectOne(ctx, `
UPDATE bob_agora_employments
   SET status = 'terminated', ended_at = $1
 WHERE id = $2`, nowUnix, id)
}

// SetEmploymentRole changes an Employment's role_name. Returns contract.ErrNoRows
// when id has no row. nowUnix is accepted for signature parity with the other
// single-row updaters; the table has no role-change timestamp column so it is
// unused here.
func (s *PG) SetEmploymentRole(ctx context.Context, employmentID, roleName string, nowUnix int64) error {
	if employmentID == "" || roleName == "" {
		return fmt.Errorf("agora: SetEmploymentRole requires non-empty employmentID + roleName")
	}
	return s.affectOne(ctx, `
UPDATE bob_agora_employments
   SET role_name = $1
 WHERE id = $2`, roleName, employmentID)
}

// DeleteEmployment removes an Employment row. Member inboxes are NOT affected —
// they belong to the member (member_id), not the employment, so they persist.
func (s *PG) DeleteEmployment(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM bob_agora_employments WHERE id = $1`, id)
	return err
}

func scanEmployment(r contract.Row) (Employment, error) {
	var out Employment
	err := r.Scan(&out.ID, &out.MemberID, &out.CompanyID, &out.RoleName, &out.Status, &out.StartedAt, &out.EndedAt)
	if errors.Is(err, contract.ErrNoRows) {
		return Employment{}, err
	}
	if err != nil {
		return Employment{}, fmt.Errorf("agora: scan employment: %w", err)
	}
	return out, nil
}

func scanEmployments(rows contract.Rows) ([]Employment, error) {
	var out []Employment
	for rows.Next() {
		e, err := scanEmployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
