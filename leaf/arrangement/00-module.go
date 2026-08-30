// Package arrangement is the arrangement table as a trunk module (docs/arrangement.md):
// a flexible, per-company orchestration of role-owned priority buckets. An
// "arrangement" is one orchestration (a company has many); its spec is a flexible JSON
// (ordered role buckets + free-form content), validated at Define and frozen once
// started. The FIXED engine = Inject (input) + a single ticker that nudges idle members
// to Pull (dispatch) + the rule "one priority bucket per role". Members process in a
// normal agora turn and Submit, which routes the item forward / back / parked.
//
// Storage = two row tables (leaf/arrangement/store): defs (frozen) + items (the live
// queue, per-item atomic claim). Couples to agora (role→member scopes), the gateway
// (deliver the nudge) and session (per-scope idle gate) ONLY through contract, lazily.
package arrangement

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/arrangement/store"
	"agentbob/trunk"
)

const (
	defaultTick     = 30 * time.Second
	minTick         = 1 * time.Second
	maxTick         = 1 * time.Hour
	termRetention   = 30 * 24 * time.Hour // terminal items kept this long, then swept
	termPrunePeriod = 6 * time.Hour
	// terminal arrangements (defs: cancelled OR done) are swept daily, after a 7-day
	// grace (the cutoff arg carries the retention; the period is pure cadence). 'done'
	// is included so completed defs don't accumulate forever. Daily cadence keeps the
	// sweep responsive; the Housekeeper's firstRunDelay seeding means even a longer
	// period would survive frequent restarts now, but a day between passes costs
	// nothing (the sweep is idempotent and cheap).
	cancelledRetention   = 7 * 24 * time.Hour
	cancelledPrunePeriod = 24 * time.Hour
	// stalledDefRetention: a 'started' def with no runnable (queued/in_flight) item and
	// no def/item activity for this long is STALLED — only parked items remain
	// (blocked/rejected_no_upstream/long-unmet), so it will never CompleteDef and nobody
	// cancels it (accumulation-inventory #41). The defs-prune sweep closes it (→
	// cancelled), which then rides the normal terminal-defs retention above.
	stalledDefRetention = 30 * 24 * time.Hour
	dispatchTimeout     = 30 * time.Second
	// staleLeaseSeconds is the in_flight lease: an item claimed (in_flight) longer than this
	// is assumed orphaned (worker crashed/restarted mid-item) and requeued by selectRound's
	// ReclaimStale. Generous vs a normal worker turn so a slow-but-alive worker isn't
	// reclaimed prematurely (and even if it is, RouteItem's claim guard blocks a double-route).
	staleLeaseSeconds = int64(30 * 60) // 30 min
	// defaultPushInterval paces the push-queue drainer: it emits at most ONE wake every
	// this long, so a selector tick can't fire a burst of concurrent member turns. The
	// selector only ENQUEUES wakes; this drains them gently. Runtime-adjustable via
	// /arrangement push <时长>.
	defaultPushInterval = 8 * time.Second
	// nudgeCooldownSec: the selector won't re-enqueue a scope within this many seconds of
	// its last emitted nudge — closes the async emit→turn-busy window so a member can't be
	// woken twice into concurrent pull turns. Must exceed worst-case turn-start latency.
	nudgeCooldownSec = 30
	// defaultMaxConcurrentArrangements caps how many arrangements one company may have RUNNING
	// (a started def with a queued/in_flight item) at once — parked-only defs don't count.
	// Define refuses a new one past the cap. Runtime-adjustable via
	// /arrangement maxconcurrent_for_arrangement <n>.
	defaultMaxConcurrentArrangements = 2
)

// Module is arrangement as a trunk module.
type Module struct {
	store                     *store.PG
	tickNanos                 atomic.Int64 // selector cadence (runtime-settable)
	pushNanos                 atomic.Int64 // push-queue drain interval (runtime-settable)
	maxConcurrentArrangements atomic.Int64 // per-company cap on running arrangements (runtime-settable)

	agora func() contract.Agora
	gw    func() contract.Gateway
	sm    func() contract.SessionManager

	// Push queue: the selector enqueues wakes; the drainer pops one per push interval.
	// Shared by the two goroutines → guarded by pushMu. pushSet dedups by pushItem.key()
	// (one pending wake per arrangement+role+scope — per-scope pacing is lastNudged's
	// job, not the dedup's). lastNudged[scope]=unix-sec of the last EMITTED nudge —
	// the selector won't re-enqueue a scope within nudgeCooldownSec of its last nudge,
	// bridging the async emit→turn-busy window (RunningAtScope reads 0 until the woken
	// turn registers busy, which can lag the 8s push interval under load).
	pushMu     sync.Mutex
	pushQ      []pushItem
	pushSet    map[string]bool
	lastNudged map[string]int64

	cancel context.CancelFunc
	wg     sync.WaitGroup
	state  atomic.Int32
}

// New returns an arrangement module.
func New() *Module {
	return &Module{pushSet: map[string]bool{}, lastNudged: map[string]int64{}}
}

func (m *Module) Name() string { return "arrangement" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.Arrangements]()}
}

// Needs the DB, the SlashRegistry (/arrangement) and the PanelRegistry (status table).
// agora + gateway + session + Housekeeper are SOFT (TryRequire).
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.DB](),
		trunk.TypeOf[contract.PanelRegistry](),
		trunk.TypeOf[contract.SlashRegistry](),
	}
}

func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	db := trunk.Require[contract.DB](reg)
	if err := store.Migrate(ctx, db); err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return err
	}
	m.store = store.NewPG(db)
	m.tickNanos.Store(int64(defaultTick))
	m.pushNanos.Store(int64(defaultPushInterval))
	m.maxConcurrentArrangements.Store(defaultMaxConcurrentArrangements)

	m.agora = lazyAgora(reg)
	m.gw = lazyGateway(reg)
	m.sm = lazySessionManager(reg)

	trunk.Provide[contract.Arrangements](reg, m)
	m.registerSlash(trunk.Require[contract.SlashRegistry](reg))
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.panel())

	// Two goroutines on one cancellable background ctx (the Start ctx is transient):
	// the SELECTOR picks work + enqueues wakes; the DRAINER paces them out. Both are
	// cancel + wg.Wait()-ed in Stop.
	runCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(2)
	go m.runSelector(runCtx)
	go m.runDrainer(runCtx)

	// terminal-item retention sweep on the central Housekeeper.
	if hk, ok := trunk.TryRequire[trunk.Housekeeper](reg); ok {
		st := m.store
		hk.Register(trunk.Task{
			Name:   "arrangement-term-prune",
			Period: termPrunePeriod,
			Run: func(c context.Context) error {
				_, e := st.PruneTerminal(c, clock.Now().Add(-termRetention).Unix())
				return e
			},
		})
		// Sweep terminal arrangements (defs) daily, after a 7-day grace: 'cancelled'
		// OR 'done' — 'started' is live. Their items go with them. The same pass first
		// closes STALLED defs (started, no runnable item, no activity for 30d — they
		// never complete or cancel on their own, inventory #41); closing stamps
		// updated_at, so they ride the terminal prune after its own grace.
		hk.Register(trunk.Task{
			Name:   "arrangement-terminal-defs-prune",
			Period: cancelledPrunePeriod,
			Run: func(c context.Context) error {
				if n, e := st.CloseStalledDefs(c, clock.Now().Add(-stalledDefRetention).Unix(), clock.UnixEpoch()); e != nil {
					return e
				} else if n > 0 {
					slog.Info("arrangement: closed stalled arrangements", "count", n)
				}
				n, e := st.PruneTerminalDefs(c, clock.Now().Add(-cancelledRetention).Unix())
				if e == nil && n > 0 {
					slog.Info("arrangement: pruned terminal arrangements", "count", n)
				}
				return e
			},
		})
	}

	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	if m.cancel != nil {
		m.cancel()
		m.wg.Wait()
	}
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// setTick updates the runtime ticker interval (clamped). Returns the applied value.
func (m *Module) setTick(d time.Duration) time.Duration {
	d = clampTick(d)
	m.tickNanos.Store(int64(d))
	return d
}

// setPushInterval updates the push-queue drain interval (clamped). Returns the applied value.
func (m *Module) setPushInterval(d time.Duration) time.Duration {
	d = clampPush(d)
	m.pushNanos.Store(int64(d))
	return d
}

// setMaxConcurrentArrangements updates the per-company running-arrangement cap (min 1). Returns
// the applied value.
func (m *Module) setMaxConcurrentArrangements(n int) int64 {
	if n < 1 {
		n = 1
	}
	m.maxConcurrentArrangements.Store(int64(n))
	return int64(n)
}

func clampTick(d time.Duration) time.Duration {
	if d < minTick {
		return minTick
	}
	if d > maxTick {
		return maxTick
	}
	return d
}

// clampPush bounds the push-queue drain interval to the same [minTick, maxTick] range.
func clampPush(d time.Duration) time.Duration { return clampTick(d) }

func lazyAgora(reg *trunk.Registry) func() contract.Agora {
	var once sync.Once
	var a contract.Agora
	return func() contract.Agora {
		once.Do(func() { a, _ = trunk.TryRequire[contract.Agora](reg) })
		return a
	}
}

func lazyGateway(reg *trunk.Registry) func() contract.Gateway {
	var once sync.Once
	var gw contract.Gateway
	return func() contract.Gateway {
		once.Do(func() { gw, _ = trunk.TryRequire[contract.Gateway](reg) })
		return gw
	}
}

func lazySessionManager(reg *trunk.Registry) func() contract.SessionManager {
	var once sync.Once
	var sm contract.SessionManager
	return func() contract.SessionManager {
		once.Do(func() { sm, _ = trunk.TryRequire[contract.SessionManager](reg) })
		return sm
	}
}

// nowUnix reads the DB-calibrated process clock (same scale as accounts/agora/session),
// so arrangement timestamps + ages don't drift vs the rest of bob.
func nowUnix() int64 { return clock.UnixEpoch() }

var _ trunk.Module = (*Module)(nil)
