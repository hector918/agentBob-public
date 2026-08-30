package store

import (
	"context"
	"errors"
	"fmt"

	"agentbob/contract"
)

// Members CRUD over bob_agora_members.

const memberSelect = `
SELECT id, name, model_pref_tag, prompt_style, created_at, updated_at
  FROM bob_agora_members`

// CreateMember inserts a new Member (id + timestamps assigned here).
func (s *PG) CreateMember(ctx context.Context, m Member, nowUnix int64) (Member, error) {
	if m.Name == "" {
		return Member{}, fmt.Errorf("agora: Member.Name is required")
	}
	m.ID = newID("p")
	m.CreatedAt = nowUnix
	m.UpdatedAt = nowUnix
	_, err := s.db.Exec(ctx, `
INSERT INTO bob_agora_members
    (id, name, model_pref_tag, prompt_style, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
		m.ID, m.Name, m.ModelPrefTag, m.PromptStyle,
		m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		return Member{}, fmt.Errorf("agora: create member: %w", err)
	}
	return m, nil
}

// GetMember returns one Member by id (contract.ErrNoRows when absent).
func (s *PG) GetMember(ctx context.Context, id string) (Member, error) {
	return scanMember(s.db.QueryRow(ctx, memberSelect+` WHERE id = $1`, id))
}

// GetMemberByName returns one Member by name (contract.ErrNoRows when absent).
func (s *PG) GetMemberByName(ctx context.Context, name string) (Member, error) {
	return scanMember(s.db.QueryRow(ctx, memberSelect+` WHERE name = $1`, name))
}

// ListMembers returns every Member, ordered by name.
func (s *PG) ListMembers(ctx context.Context) ([]Member, error) {
	rows, err := s.db.Query(ctx, memberSelect+` ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMember removes a Member (employments / inboxes CASCADE).
func (s *PG) DeleteMember(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM bob_agora_members WHERE id = $1`, id)
	return err
}

// SetMemberPromptStyle overwrites a member's prompt-style persona (the webui member
// editor). An empty body clears it.
func (s *PG) SetMemberPromptStyle(ctx context.Context, memberID, style string, nowUnix int64) error {
	if memberID == "" {
		return fmt.Errorf("agora: SetMemberPromptStyle requires non-empty memberID")
	}
	return s.affectOne(ctx,
		`UPDATE bob_agora_members SET prompt_style = $1, updated_at = $2 WHERE id = $3`,
		style, nowUnix, memberID)
}

func scanMember(r contract.Row) (Member, error) {
	var out Member
	err := r.Scan(&out.ID, &out.Name, &out.ModelPrefTag, &out.PromptStyle,
		&out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, contract.ErrNoRows) {
		return Member{}, err
	}
	if err != nil {
		return Member{}, fmt.Errorf("agora: scan member: %w", err)
	}
	return out, nil
}
