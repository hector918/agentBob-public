package core

import "context"

// Admin line: the system-level "summon the admin" funnel.
//
// Design: docs/admin-line-design.md.
//
// What lives here:
//   - AdminLevel / AdminForwardState — the small enum types, stored as
//     their lowercase string form.
//   - AdminAlert — one persisted bob_admin_line row.
//   - AdminLine — the system-wide entry point every subsystem calls to
//     summon the admin. Implemented by the adminline module (Phase 2).
//   - AdminLineStore — persists the append-only alert stream; satisfied
//     by the generic dual-backend store (store/{sqlite,postgres} +
//     fallback wrapper).

// AdminLevel is the severity of an admin-line alert. Stored as the
// lowercase string.
type AdminLevel string

const (
	AdminInfo  AdminLevel = "info"
	AdminWarn  AdminLevel = "warn"
	AdminError AdminLevel = "error"
)

// AdminForwardState records whether a persisted alert reached its
// attached delivery target.
type AdminForwardState string

const (
	AdminForwardNone       AdminForwardState = "none"       // no target attached when persisted
	AdminForwardSent       AdminForwardState = "sent"       // forwarded to the attached target
	AdminForwardSuppressed AdminForwardState = "suppressed" // throttled — folded into a rollup, not forwarded individually
	AdminForwardFailed     AdminForwardState = "failed"     // a target was attached but Deliver returned an error
)

// AdminAlert is one persisted admin-line alert (a bob_admin_line row).
type AdminAlert struct {
	ID        int64
	TS        int64 // unix seconds
	Level     AdminLevel
	Origin    string // the calling subsystem's short tag
	Text      string
	Forwarded AdminForwardState
}

// AdminLine is the system-wide entry point every subsystem calls to
// summon the admin. The adminline module (Phase 2) implements it.
type AdminLine interface {
	// Notify forwards one alert to the admin line. Callers typically
	// hold a *adminline.Line which IS safe to call on a typed-nil
	// pointer (the concrete type has an l==nil guard). A BARE /
	// UNTYPED nil interface value, however, will panic on this method
	// call — never pass `var x core.AdminLine; x.Notify(...)`. Wire
	// concrete-or-nothing: either inject a *adminline.Line (which may
	// itself be the typed-nil value) or guard before delegating.
	//
	// Previous doc claimed "nil-safe" without that distinction; BUG
	// 1.2 tightens the contract.
	Notify(ctx context.Context, level AdminLevel, origin, format string, args ...any)
}

// AdminLineStore persists the admin-line alert stream (bob_admin_line).
// Append-only at write time; old rows are age-pruned by housekeeping
// (PruneAdminLine) so the log does not grow unbounded.
type AdminLineStore interface {
	AppendAlert(ctx context.Context, a AdminAlert) (int64, error) // returns the new row id
	MarkAlertForwarded(ctx context.Context, id int64, state AdminForwardState) error
	// PruneAdminLine deletes alert rows with ts < olderThanUnix and
	// returns the number of rows deleted. Uses idx_bob_admin_line_ts.
	PruneAdminLine(ctx context.Context, olderThanUnix int64) (int64, error)
}
