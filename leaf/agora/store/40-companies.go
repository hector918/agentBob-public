package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"agentbob/contract"
)

// Companies CRUD over bob_agora_companies.

const companySelect = `
SELECT id, name, config::text, permissions_yaml, playbook,
       COALESCE(report_inbox_id, ''), status, created_at, updated_at
  FROM bob_agora_companies`

// CreateCompany inserts a new Company (id + timestamps assigned here; status
// defaults to "active").
func (s *PG) CreateCompany(ctx context.Context, c Company, nowUnix int64) (Company, error) {
	if c.Name == "" {
		return Company{}, fmt.Errorf("agora: Company.Name is required")
	}
	c.ID = newID("co")
	c.CreatedAt = nowUnix
	c.UpdatedAt = nowUnix
	if c.Status == "" {
		c.Status = "active"
	}
	cfg, err := marshalAnyMap(c.Config)
	if err != nil {
		return Company{}, err
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO bob_agora_companies
    (id, name, config, permissions_yaml, playbook, report_inbox_id, status, created_at, updated_at)
VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9)`,
		c.ID, c.Name, cfg, c.PermissionsYAML, c.Playbook, nullStr(c.ReportInboxID),
		c.Status, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return Company{}, fmt.Errorf("agora: create company: %w", err)
	}
	return c, nil
}

// GetCompany returns one Company by id (contract.ErrNoRows when absent).
func (s *PG) GetCompany(ctx context.Context, id string) (Company, error) {
	return scanCompany(s.db.QueryRow(ctx, companySelect+` WHERE id = $1`, id))
}

// GetCompanyByName returns one Company by name (contract.ErrNoRows when absent).
func (s *PG) GetCompanyByName(ctx context.Context, name string) (Company, error) {
	return scanCompany(s.db.QueryRow(ctx, companySelect+` WHERE name = $1`, name))
}

// ListCompanies returns every Company, ordered by name.
func (s *PG) ListCompanies(ctx context.Context) ([]Company, error) {
	rows, err := s.db.Query(ctx, companySelect+` ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Company
	for rows.Next() {
		c, err := scanCompany(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCompanyStatus flips the company kill switch. Only "active"/"disabled"
// are accepted. Returns contract.ErrNoRows when companyID has no row.
func (s *PG) UpdateCompanyStatus(ctx context.Context, companyID, status string, nowUnix int64) error {
	switch status {
	case "active", "disabled":
	default:
		return fmt.Errorf("agora: UpdateCompanyStatus only supports active|disabled, got %q", status)
	}
	if companyID == "" {
		return fmt.Errorf("agora: UpdateCompanyStatus requires non-empty companyID")
	}
	return s.affectOne(ctx,
		`UPDATE bob_agora_companies SET status = $1, updated_at = $2 WHERE id = $3`,
		status, nowUnix, companyID)
}

// SetReportInbox points the Company at its bridge inbox ("" clears it).
func (s *PG) SetReportInbox(ctx context.Context, companyID, inboxID string, nowUnix int64) error {
	if companyID == "" {
		return fmt.Errorf("agora: SetReportInbox requires non-empty companyID")
	}
	return s.affectOne(ctx,
		`UPDATE bob_agora_companies SET report_inbox_id = $1, updated_at = $2 WHERE id = $3`,
		nullStr(inboxID), nowUnix, companyID)
}

// GetPermissionsYAML returns the Company's raw permissions_yaml document.
func (s *PG) GetPermissionsYAML(ctx context.Context, companyID string) (string, error) {
	var body string
	err := s.db.QueryRow(ctx,
		`SELECT permissions_yaml FROM bob_agora_companies WHERE id = $1`, companyID,
	).Scan(&body)
	if err != nil {
		return "", err
	}
	return body, nil
}

// SetPermissionsYAML replaces the Company's permissions_yaml document. Returns
// contract.ErrNoRows when companyID has no row.
func (s *PG) SetPermissionsYAML(ctx context.Context, companyID, yaml string, nowUnix int64) error {
	if companyID == "" {
		return fmt.Errorf("agora: SetPermissionsYAML requires non-empty companyID")
	}
	return s.affectOne(ctx,
		`UPDATE bob_agora_companies SET permissions_yaml = $1, updated_at = $2 WHERE id = $3`,
		yaml, nowUnix, companyID)
}

// GetCompanyPlaybook returns the Company's raw playbook prose.
func (s *PG) GetCompanyPlaybook(ctx context.Context, companyID string) (string, error) {
	var body string
	err := s.db.QueryRow(ctx,
		`SELECT playbook FROM bob_agora_companies WHERE id = $1`, companyID,
	).Scan(&body)
	if err != nil {
		return "", err
	}
	return body, nil
}

// SetCompanyPlaybook stores the raw playbook prose as-is (never parsed /
// validated / reconciled). Returns contract.ErrNoRows when companyID has no row.
func (s *PG) SetCompanyPlaybook(ctx context.Context, companyID, playbook string, nowUnix int64) error {
	if companyID == "" {
		return fmt.Errorf("agora: SetCompanyPlaybook requires non-empty companyID")
	}
	return s.affectOne(ctx,
		`UPDATE bob_agora_companies SET playbook = $1, updated_at = $2 WHERE id = $3`,
		playbook, nowUnix, companyID)
}

// DeleteCompanyCascade HARD-deletes a Company and the given orphan Members in ONE
// transaction (atomic: either the whole cut lands or nothing does).
//
// Deleting the company row drives the FK ON DELETE CASCADE: its employments, its
// bridge inboxes, and those inboxes' sources all go, and permissions_yaml goes
// with the row. Each orphan member is then deleted, cascading that member's own
// inboxes + their sources. orphanMemberIDs MUST be the members whose only active
// employment was at this company (computed by the caller — the store does not
// re-derive the policy); a member still employed elsewhere must NOT be in the list
// (deleting them would orphan their other company's directory).
//
// Returns contract.ErrNoRows when companyID has no row (nothing deleted).
func (s *PG) DeleteCompanyCascade(ctx context.Context, companyID string, orphanMemberIDs []string) error {
	if companyID == "" {
		return fmt.Errorf("agora: DeleteCompanyCascade requires non-empty companyID")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() // no-op once committed; rolls back on any early return
		}
	}()
	res, err := tx.Exec(ctx, `DELETE FROM bob_agora_companies WHERE id = $1`, companyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return contract.ErrNoRows
	}
	for _, mid := range orphanMemberIDs {
		if mid == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM bob_agora_members WHERE id = $1`, mid); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func scanCompany(r contract.Row) (Company, error) {
	var (
		out     Company
		cfgJSON string
	)
	err := r.Scan(&out.ID, &out.Name, &cfgJSON, &out.PermissionsYAML, &out.Playbook,
		&out.ReportInboxID, &out.Status, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, contract.ErrNoRows) {
		return Company{}, err
	}
	if err != nil {
		return Company{}, fmt.Errorf("agora: scan company: %w", err)
	}
	if cfgJSON != "" {
		if err := json.Unmarshal([]byte(cfgJSON), &out.Config); err != nil {
			return Company{}, fmt.Errorf("agora company %q: config JSON: %w", out.Name, err)
		}
	}
	return out, nil
}

// affectOne runs an UPDATE/DELETE expected to touch exactly one row and maps a
// zero-row outcome to contract.ErrNoRows so callers can errors.Is-check a missing
// target instead of a silent no-op.
func (s *PG) affectOne(ctx context.Context, query string, args ...any) error {
	res, err := s.db.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return contract.ErrNoRows
	}
	return nil
}
