package contract

import (
	"context"
	"errors"
)

// ErrNoRows is returned by Row.Scan when a QueryRow matched no rows. The pgpool
// adapter translates the driver's no-rows sentinel into this, so a store impl
// can distinguish "not found" from a real error with errors.Is(err,
// contract.ErrNoRows) — WITHOUT importing database/sql, keeping the "it is SQL /
// Postgres" fact inside pgpool.
var ErrNoRows = errors.New("contract: no rows in result set")

// ErrStoreLocked marks a transient lock-contention failure (serialization
// failure / deadlock / lock-not-available) the pgpool adapter classifies from the
// driver's SQLSTATE. It is the ONLY store error a caller may retry (spec I15: one
// retry @50ms); every other store error must not be retried (rewriting a bad write
// turns one bad row into two). Matched with errors.Is(err, contract.ErrStoreLocked).
var ErrStoreLocked = errors.New("contract: store locked (retryable)")

// DB is the minimal SQL-execution surface shared by every module's persistence
// layer. It is provided by the pgpool foundation (the single place that knows
// the database is Postgres) and consumed ONLY by modules' store implementations
// — never by business logic, which speaks the domain store interfaces
// (SessionStore, AccountStore, …).
//
// The surface is SQL-generic — no driver types leak. A module's persistence
// file writes its own SQL against DB and owns its own tables; the "it is SQL /
// Postgres" fact stops at that file and at pgpool, and reaches nothing else.
type DB interface {
	Exec(ctx context.Context, query string, args ...any) (Result, error)
	Query(ctx context.Context, query string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) Row
	// Begin starts a transaction. Nested transactions are unsupported: Begin on
	// a Tx returns an error.
	Begin(ctx context.Context) (Tx, error)
}

// Result reports the outcome of an Exec.
type Result interface {
	RowsAffected() (int64, error)
}

// Row is a single-row query result; Scan copies the columns into dest.
type Row interface {
	Scan(dest ...any) error
}

// Rows is a multi-row query result. Iterate with Next, read with Scan, check
// Err after the loop, and always Close.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// Tx is a transaction: the DB surface plus Commit/Rollback.
type Tx interface {
	DB
	Commit() error
	Rollback() error
}
