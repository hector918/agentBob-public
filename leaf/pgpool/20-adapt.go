package pgpool

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"agentbob/contract"
)

// lockBusyCodes are the Postgres SQLSTATEs that mean "try again" — a transient
// contention failure where the write did NOT happen, so a retry is safe (spec I15).
var lockBusyCodes = map[string]bool{
	"40001": true, // serialization_failure
	"40P01": true, // deadlock_detected
	"55P03": true, // lock_not_available
	"55006": true, // object_in_use
}

// classifyExec maps a driver lock-contention error to contract.ErrStoreLocked
// (keeping the original in the chain) so callers retry ONLY those; every other
// error passes through untouched (must not be retried).
func classifyExec(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && lockBusyCodes[pgErr.Code] {
		return errors.Join(contract.ErrStoreLocked, err)
	}
	return err
}

// querier is the database/sql query surface shared by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// execer adapts a database/sql querier to contract.DB's query methods. sql.Result
// passes through structurally; *sql.Rows and *sql.Row are thinly wrapped so the
// database/sql error sentinels never escape the package — Row.Scan maps
// sql.ErrNoRows to contract.ErrNoRows, and BOTH Rows.Err and Row.Scan classify lock
// contention as contract.ErrStoreLocked so callers retry it (I15). Failed calls
// return a true nil interface, not a typed-nil.
type execer struct{ q querier }

func (e execer) Exec(ctx context.Context, query string, args ...any) (contract.Result, error) {
	res, err := e.q.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, classifyExec(err)
	}
	return res, nil
}

func (e execer) Query(ctx context.Context, query string, args ...any) (contract.Rows, error) {
	r, err := e.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classifyExec(err)
	}
	return rows{r}, nil
}

// rows wraps *sql.Rows so a lock-contention error surfaced at ITERATION time (via
// Rows.Err(), not the initial Query) is classified as contract.ErrStoreLocked, same
// as Exec/Query/Scan (I15). The embedded *sql.Rows supplies Next/Scan/Close as-is.
type rows struct{ *sql.Rows }

func (rw rows) Err() error { return classifyExec(rw.Rows.Err()) }

func (e execer) QueryRow(ctx context.Context, query string, args ...any) contract.Row {
	// *sql.Row is never nil; any error is deferred to Scan. Wrap it so a no-rows
	// result surfaces as contract.ErrNoRows (the database/sql sentinel never
	// escapes this package).
	return row{e.q.QueryRowContext(ctx, query, args...)}
}

// row wraps *sql.Row to translate sql.ErrNoRows into contract.ErrNoRows.
type row struct{ r *sql.Row }

func (rw row) Scan(dest ...any) error {
	err := rw.r.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.ErrNoRows
	}
	// A write via QueryRow(... RETURNING ...) surfaces its error here — classify
	// lock contention so callers can retry it (I15), same as Exec.
	return classifyExec(err)
}

// poolDB is contract.DB backed by the connection pool; Begin opens a real tx.
type poolDB struct {
	execer
	pool *sql.DB
}

func (d poolDB) Begin(ctx context.Context) (contract.Tx, error) {
	t, err := d.pool.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return txDB{execer{t}, t}, nil
}

// txDB is contract.Tx backed by a database/sql transaction.
type txDB struct {
	execer
	tx *sql.Tx
}

func (txDB) Begin(context.Context) (contract.Tx, error) {
	return nil, errors.New("pgpool: nested transactions not supported")
}

// Commit classifies lock contention (e.g. serialization failure surfacing at
// COMMIT) as contract.ErrStoreLocked, same as Exec/Query/Scan (I15) — the tx
// aborted, the write did not happen, so a retry is safe.
func (t txDB) Commit() error   { return classifyExec(t.tx.Commit()) }
func (t txDB) Rollback() error { return t.tx.Rollback() }

// compile-time: the adapters satisfy the contract.
var (
	_ contract.DB = poolDB{}
	_ contract.Tx = txDB{}
)
