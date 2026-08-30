// Package clock is bob's single process-wide time reference, calibrated against
// the store 中间层's authoritative time (the DB). It is a heartwood foundation
// utility (like prompt / logging / i18n) so any leaf module may import it — a
// leaf/clock module could not, since a leaf module must never import another leaf
// module (arch boundary).
//
// Why calibrate: bob's host clock and the DB clock can drift (NTP), and an HA /
// multi-host deployment writing to one DB needs a single time scale or audit
// ordering breaks. At startup (and hourly after) we read the DB's now(), compute
// offset = serverNow − localNow, and every persisted timestamp goes through
// clock.Now()/UnixSeconds()/UnixEpoch() = local + offset, landing on the DB scale.
//
// Everything here is UTC: epoch is timezone-agnostic (an absolute instant), and
// Now() returns a .UTC() time.Time. Display-time-zone conversion is a presentation
// concern done at the edge (the now tool / the prompt date), never in storage.
//
// Failure mode: no DB (or a failed calibration) leaves offset 0 → bob uses its
// local clock until the next successful resync, WARN-logged. So an uncalibrated
// process behaves exactly like the old raw time.Now() — never worse.
package clock

import (
	"context"
	"log/slog"
	"reflect"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/trunk"
)

// offsetNanos is serverNow − localNow in nanoseconds; 0 until calibrated.
var offsetNanos atomic.Int64

// Now returns the current instant on the DB-calibrated scale, in UTC.
func Now() time.Time { return time.Now().Add(time.Duration(offsetNanos.Load())).UTC() }

// UnixSeconds returns the calibrated time as fractional epoch seconds (the unit the
// session store persists).
func UnixSeconds() float64 { return float64(Now().UnixNano()) / 1e9 }

// UnixEpoch returns the calibrated time as whole epoch seconds (the unit the
// accounts store persists).
func UnixEpoch() int64 { return Now().Unix() }

const resyncInterval = time.Hour

// Module is the calibration lifecycle owner: at Start it reads the DB's now() once
// and launches an hourly resync. Optional — without a DB it logs and runs on the
// local clock (offset 0).
type Module struct {
	cancel context.CancelFunc
	state  atomic.Int32
}

func New() *Module { return &Module{} }

func (*Module) Name() string             { return "clock" }
func (*Module) Provides() []reflect.Type { return nil }
func (*Module) Needs() []reflect.Type    { return nil } // soft DB dep via TryRequire — see arch wantGraph
func (*Module) Optional() bool           { return true }
func (m *Module) Health() trunk.State    { return trunk.State(m.state.Load()) }

func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	db, ok := trunk.TryRequire[contract.DB](reg)
	if !ok {
		slog.Warn("clock: no DB — running on the local host clock (uncalibrated)")
		m.state.Store(int32(trunk.StateReady))
		return nil
	}
	if err := calibrate(ctx, db); err != nil {
		slog.Warn("clock: initial calibration failed — local clock until next resync", "err", err)
	} else {
		slog.Info("clock: calibrated against the DB", "offset_ms", offsetNanos.Load()/1e6)
	}
	rctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go resyncLoop(rctx, db)
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// calibrate reads the DB server time and sets the offset. The RTT is halved out by
// taking the local midpoint of the query window. The epoch→nanos conversion is done
// server-side in exact numeric arithmetic (integer literal keeps it out of double
// precision, whose ulp at the current epoch is ~0.5µs) and scanned as int64.
func calibrate(ctx context.Context, db contract.DB) error {
	before := time.Now()
	var serverNanos int64
	if err := db.QueryRow(ctx, "SELECT (extract(epoch from now()) * 1000000000)::bigint").Scan(&serverNanos); err != nil {
		return err
	}
	after := time.Now()
	localMid := before.Add(after.Sub(before) / 2)
	offsetNanos.Store(serverNanos - localMid.UnixNano())
	return nil
}

func resyncLoop(ctx context.Context, db contract.DB) {
	t := time.NewTicker(resyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := calibrate(ctx, db); err != nil {
				slog.Warn("clock: resync failed — keeping last offset", "err", err)
			}
		}
	}
}

var _ trunk.Module = (*Module)(nil)
