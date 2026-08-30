package pgpool

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the database/sql "pgx" driver

	"agentbob/contract"
	"agentbob/trunk"
)

// Module owns the shared Postgres connection pool and provides contract.DB.
type Module struct {
	dsn   string
	pool  *sql.DB
	state atomic.Int32 // trunk.State
}

// New returns a pgpool module that connects to dsn on Start.
func New(dsn string) *Module { return &Module{dsn: dsn} }

func (m *Module) Name() string { return "pgpool" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.DB]()}
}

func (m *Module) Needs() []reflect.Type { return nil }

// Optional is false: nothing persists without the database, so a failed
// connection must abort startup rather than degrade.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

// Start opens the pool, verifies connectivity, and registers contract.DB.
// Fail-fast: pgx defers DSN parsing, so a malformed DSN and an unreachable server
// both surface at PingContext (not sql.Open); the sql.Open guard remains only as
// defense for an unregistered driver.
func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	if m.dsn == "" {
		m.state.Store(int32(trunk.StateFailed))
		return fmt.Errorf("pgpool: empty DSN")
	}
	db, err := sql.Open("pgx", m.dsn)
	if err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return fmt.Errorf("pgpool: open: %w", err)
	}
	// Small pool — Synology pg is modest. Tunable once config lands.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	// Recycle proactively: on a multi-day daemon an idle conn through a
	// NAT/firewall gets silently dropped, and the pool would hand out a dead one.
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		m.state.Store(int32(trunk.StateFailed))
		return fmt.Errorf("pgpool: ping: %w", err)
	}

	m.pool = db
	trunk.Provide[contract.DB](reg, poolDB{execer{db}, db})
	m.state.Store(int32(trunk.StateReady))
	return nil
}

// Stop closes the pool.
func (m *Module) Stop(context.Context) error {
	if m.pool == nil {
		return nil
	}
	if err := m.pool.Close(); err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return err
	}
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// compile-time: Module is a trunk.Module.
var _ trunk.Module = (*Module)(nil)
