// Package urllib is the URL-memory layer as a trunk module: a single pg-backed
// table (bob_urls) recording URLs the agent has successfully fetched, plus the
// configured search engines. It provides contract.URLLibrary; the web tools
// (web_search / scrapling / browser) Record successful fetches and Recall on
// search, and the normal flow MarkSatisfied-s the last URL when a turn ends with a
// final answer — all OPTIONAL (TryRequire) consumers, so the library is purely
// additive: absent, every web tool keeps its current behaviour.
//
// Trunk-native rebuild of skeleton's urllib (which carried three backends + two
// tables behind a factory). Simpler here: ONE pg backend, ONE table, no factory —
// urllib hard-needs contract.DB (it IS the database), so it has no in-memory mode.
package urllib

import (
	"context"
	"log/slog"
	"reflect"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/trunk"
)

// urlRetention is how long a never-useful (satisfied_count = 0) URL is kept after
// its last fetch before the retention sweep drops it; urlPruneSweepPeriod is how
// often that sweep runs. URLs that ever proved useful are kept regardless of age.
const (
	urlRetention        = 90 * 24 * time.Hour
	urlPruneSweepPeriod = 24 * time.Hour
)

// Module is the URL library as a trunk module: it migrates its one table + seeds
// the default engines at Start and provides contract.URLLibrary.
type Module struct {
	lib   *library
	state atomic.Int32 // trunk.State
}

// New returns a urllib module. It hard-needs contract.DB (provided by pgpool).
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "urllib" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.URLLibrary]()}
}

// Needs contract.DB — the library is the database; without it there is nothing to
// provide, so this is a HARD dependency (unlike tools/skills which TryRequire DB
// only for an optional persistence side-channel).
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.DB]()}
}

// Optional is true: without the URL library the web tools simply don't recall /
// record — the pipeline is unaffected.
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	db := trunk.Require[contract.DB](reg)
	if err := migrateURLs(ctx, db); err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return err
	}
	m.lib = &library{db: db, lastBySid: map[string]string{}}
	m.lib.seed(ctx, DefaultSeeds)
	trunk.Provide[contract.URLLibrary](reg, m.lib)
	// Retention sweep for bob_urls (persistent state → Housekeeper, not a module
	// timer). Optional — a minimal config runs without a housekeeper. Prune is
	// value-aware (keeps ever-useful / recent URLs), so this only ages out noise.
	lib := m.lib
	if hk, ok := trunk.TryRequire[trunk.Housekeeper](reg); ok {
		hk.Register(trunk.Task{
			Name:   "urllib-prune",
			Period: urlPruneSweepPeriod,
			Run: func(ctx context.Context) error {
				cutoff := clock.Now().Add(-urlRetention).Unix()
				n, err := lib.Prune(ctx, float64(cutoff))
				if err != nil {
					return err
				}
				if n > 0 {
					slog.Info("urllib: pruned stale urls", "removed", n)
				}
				return nil
			},
		})
	}
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

var _ trunk.Module = (*Module)(nil)
