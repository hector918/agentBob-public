package adminline

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/adminline/store"
	"agentbob/trunk"
)

// retention is how long persisted alerts are kept before the housekeeping prune
// deletes them; prunePeriod is how often that sweep runs.
const (
	retention   = 30 * 24 * time.Hour
	prunePeriod = 6 * time.Hour
)

// Module is the admin-line as a trunk module: it migrates bob_admin_line, builds
// the delivery outlet from its config, runs the worker, registers a prune sweep
// with the housekeeper, and provides contract.AdminLine.
type Module struct {
	outlet string // "<source>:<chat_id>" | "" (unattached)
	line   *Line
	cancel context.CancelFunc
	done   chan struct{}
	state  atomic.Int32 // trunk.State
}

// New returns an admin-line module. outlet is the static attach spec (env / config).
func New(outlet string) *Module { return &Module{outlet: outlet, done: make(chan struct{})} }

func (m *Module) Name() string { return "adminline" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.AdminLine]()}
}

// Needs the DB (audit persistence) and the source bus (the human outlet forwards
// via a source's Send). Optional/lazy consumers (model, stoma) call Notify.
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.DB](),
		trunk.TypeOf[contract.Gateway](),
	}
}

// Optional is true: subsystems hold contract.AdminLine as an optional collaborator
// (a nil line is a no-op), so the pipeline runs without it; a boot failure here
// degrades rather than aborts.
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	db := trunk.Require[contract.DB](reg)
	if err := store.Migrate(ctx, db); err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return fmt.Errorf("adminline: migrate: %w", err)
	}
	gw := trunk.Require[contract.Gateway](reg)

	// A bad/unknown outlet must NOT block boot: log + degrade to unattached.
	deliverer, err := NewDeliverer(m.outlet, gw.SourceByName)
	if err != nil {
		slog.Warn("adminline: outlet unusable — running unattached (alerts persisted, not forwarded)", "outlet", m.outlet, "err", err)
		deliverer = nil
	}
	st := store.NewPG(db)
	m.line = newLine(st, deliverer)
	trunk.Provide[contract.AdminLine](reg, m.line)

	runCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go func() {
		defer close(m.done)
		m.line.Run(runCtx)
	}()

	// Age-prune is a persistence sweep — register it with the trunk's
	// housekeeper (optional: tests / minimal configs may run without one).
	if hk, ok := trunk.TryRequire[trunk.Housekeeper](reg); ok {
		hk.Register(trunk.Task{
			Name:   "adminline-prune",
			Period: prunePeriod,
			Run: func(ctx context.Context) error {
				// clock.Now (DB-calibrated) — alert ts are written on the same scale.
				n, err := st.PruneAdminLine(ctx, clock.Now().Add(-retention).Unix())
				if err == nil && n > 0 {
					slog.Info("adminline: pruned old alerts", "count", n)
				}
				return err
			},
		})
	}

	m.state.Store(int32(trunk.StateReady))
	return nil
}

// Stop cancels the worker (which drains the queue) and waits for it. If Start
// failed before launching the worker (e.g. a migrate error), m.cancel is nil and
// m.done was never wired — return early rather than block forever on <-m.done.
func (m *Module) Stop(context.Context) error {
	if m.cancel == nil {
		m.state.Store(int32(trunk.StateStopped))
		return nil
	}
	m.cancel()
	<-m.done
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

var _ trunk.Module = (*Module)(nil)
