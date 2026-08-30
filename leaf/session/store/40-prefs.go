package store

import (
	"context"
	"errors"
	"fmt"

	"agentbob/contract"
)

// GetScopePrefs returns the scope's persisted sink prefs, or DefaultSinkPrefs
// (both ON) when the scope has no row — so a never-toggled scope behaves exactly
// as it did before prefs were persisted.
func (s *PG) GetScopePrefs(ctx context.Context, scope string) (contract.SinkPrefs, error) {
	var p contract.SinkPrefs
	err := s.db.QueryRow(ctx,
		`SELECT trace, stream FROM bob_scope_prefs WHERE session_scope = $1`, scope,
	).Scan(&p.Trace, &p.Stream)
	if errors.Is(err, contract.ErrNoRows) {
		return contract.DefaultSinkPrefs(), nil
	}
	if err != nil {
		return contract.DefaultSinkPrefs(), fmt.Errorf("GetScopePrefs %s: %w", scope, err)
	}
	return p, nil
}

// SetScopePrefs upserts the scope's sink prefs.
func (s *PG) SetScopePrefs(ctx context.Context, scope string, prefs contract.SinkPrefs) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_scope_prefs (session_scope, trace, stream, updated_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (session_scope)
		 DO UPDATE SET trace = EXCLUDED.trace, stream = EXCLUDED.stream, updated_at = EXCLUDED.updated_at`,
		scope, prefs.Trace, prefs.Stream, now())
	if err != nil {
		return fmt.Errorf("SetScopePrefs %s: %w", scope, err)
	}
	return nil
}

// GetScopeTurnMode returns the scope's DEFAULT turn mode (the /looping toggle) —
// the value birth-stamped onto sessions created in this scope. Deliberately
// separate from SinkPrefs: that type is the Source.NewSink rendering contract,
// and turn semantics must not ride it. No row / error → "" (regular).
func (s *PG) GetScopeTurnMode(ctx context.Context, scope string) (contract.TurnMode, error) {
	var mode string
	err := s.db.QueryRow(ctx,
		`SELECT COALESCE(turn_mode,'') FROM bob_scope_prefs WHERE session_scope = $1`, scope,
	).Scan(&mode)
	if errors.Is(err, contract.ErrNoRows) {
		return contract.TurnModeRegular, nil
	}
	if err != nil {
		return contract.TurnModeRegular, fmt.Errorf("GetScopeTurnMode %s: %w", scope, err)
	}
	return contract.TurnMode(mode), nil
}

// SetScopeTurnMode upserts the scope's default turn mode without touching the
// sink-prefs columns (a fresh row seeds DefaultSinkPrefs values, so a later
// GetScopePrefs read of this row behaves like the never-toggled default).
func (s *PG) SetScopeTurnMode(ctx context.Context, scope string, mode contract.TurnMode) error {
	def := contract.DefaultSinkPrefs()
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_scope_prefs (session_scope, trace, stream, turn_mode, updated_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (session_scope)
		 DO UPDATE SET turn_mode = EXCLUDED.turn_mode, updated_at = EXCLUDED.updated_at`,
		scope, def.Trace, def.Stream, string(mode), now())
	if err != nil {
		return fmt.Errorf("SetScopeTurnMode %s: %w", scope, err)
	}
	return nil
}
