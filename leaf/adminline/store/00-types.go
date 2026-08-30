// Package store is the admin-line module's persistence: the append-only
// bob_admin_line alert audit stream. AdminAlert / AdminForwardState / the Store
// interface stay HERE (not in contract) — the Line and this impl are the only
// users; the only cross-module surface is contract.AdminLine (the Notify funnel).
package store

import (
	"context"

	"agentbob/contract"
)

// AdminForwardState records whether a persisted alert reached its delivery
// target. Stored as the lowercase string.
type AdminForwardState string

const (
	ForwardNone       AdminForwardState = "none"       // no target attached when persisted
	ForwardSent       AdminForwardState = "sent"       // forwarded to the attached target
	ForwardSuppressed AdminForwardState = "suppressed" // throttled — folded into a rollup
	ForwardFailed     AdminForwardState = "failed"     // a target was attached but Deliver errored
)

// AdminAlert is one persisted admin-line alert (a bob_admin_line row).
type AdminAlert struct {
	ID        int64
	TS        int64 // unix seconds
	Level     contract.AdminLevel
	Origin    string // the calling subsystem's short tag
	Text      string
	Forwarded AdminForwardState
}

// Store persists the append-only admin-line alert stream. Age-pruned by a
// housekeeping task so the log does not grow unbounded.
type Store interface {
	AppendAlert(ctx context.Context, a AdminAlert) (int64, error) // returns the new row id
	MarkAlertForwarded(ctx context.Context, id int64, state AdminForwardState) error
	// PruneAdminLine deletes rows with ts < olderThanUnix, returning the count.
	PruneAdminLine(ctx context.Context, olderThanUnix int64) (int64, error)
}
