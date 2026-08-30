// Package stoma is the source bus — the system's stoma (the leaf pore through
// which it exchanges messages with the outside). It owns every Source's
// lifecycle, fans their inbound events into one stream (Events), looks a source
// up by name for reply delivery (SourceByName), and probes source health. It
// registers a contract.Gateway on the trunk.
//
// The bus deliberately knows NOTHING about sessions or screening — those live
// above it in the inbound flow. It DOES report its own domain (source health /
// lifecycle) to the optional admin line, the same way the model pool reports its
// own health — a subsystem owns alerting on what only it can observe. Its whole
// job is "fan N sources in, hand replies back out, report who's alive".
package stoma

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/trunk"
)

// eventBuffer is the fan-in channel depth. One slot per source is plenty for
// the steady state; the buffer only absorbs a burst while the flow is mid-turn.
const eventBuffer = 64

// Module is the source bus.
type Module struct {
	sources map[string]contract.Source
	events  chan contract.MessageEvent

	cancel context.CancelFunc // cancels the run context (set in Start)
	wg     sync.WaitGroup     // source-run + health goroutines
	bootAt time.Time          // stamped at Start (kept if pre-set) — bounds the source boot-retry window

	health *healthTable
	state  atomic.Int32 // trunk.State

	// healthInterval is the per-source probe cadence. ≤0 disables the loop.
	healthInterval time.Duration

	// adminLine lazily resolves the optional admin-summon funnel for source
	// health/lifecycle alerts (adminline Needs the Gateway this bus provides, so
	// it Starts after us → resolve on first transition). nil → alerts are no-ops.
	adminLine func() contract.AdminLine
}

// New builds a source bus around the given sources.
func New(sources []contract.Source) *Module {
	srcMap := make(map[string]contract.Source, len(sources))
	for _, s := range sources {
		if _, dup := srcMap[s.Name()]; dup {
			// Two sources sharing a Name() would silently shadow one (only the
			// last is fanned in / looked up) — a wiring bug, surfaced loudly.
			panic(fmt.Sprintf("stoma: duplicate source name %q", s.Name()))
		}
		srcMap[s.Name()] = s
	}
	return &Module{
		sources:        srcMap,
		events:         make(chan contract.MessageEvent, eventBuffer),
		health:         newHealthTable(srcMap),
		healthInterval: 60 * time.Second,
	}
}

func (m *Module) Name() string { return "stoma" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.Gateway]()}
}

// Needs the panel registry: the bus describes its webui health layer (same
// publish-into-a-registry pattern as model/slash).
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.PanelRegistry]()}
}

// Optional is false: with no source bus, bob can receive nothing.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

// Events is the fanned-in inbound stream. Closed after every source stops.
func (m *Module) Events() <-chan contract.MessageEvent { return m.events }

// SourceByName returns the registered source, or nil. Lock-free — the map is
// fixed at construction.
func (m *Module) SourceByName(name string) contract.Source { return m.sources[name] }

// SourceNames implements contract.Gateway: every registered source name, sorted.
func (m *Module) SourceNames() []string {
	names := make([]string, 0, len(m.sources))
	for n := range m.sources {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Snapshot is the read-only source-health view for the admin webui
// (StateReporter). Read-only — the bus reports, it does not accept edits here.
func (m *Module) Snapshot() []SourceStatus { return m.health.snapshot() }

// Start spawns one goroutine per source (each Run fans into the shared event
// channel), starts the health probe, and publishes contract.Gateway.
func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	// The run context lives for the bus's serving lifetime — independent of the
	// (transient) Start context — and is cancelled in Stop.
	runCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	// Lazily resolve the admin line (it Starts after the bus). Resolved once on
	// the first health/lifecycle transition.
	var once sync.Once
	var line contract.AdminLine
	m.adminLine = func() contract.AdminLine {
		once.Do(func() { line, _ = trunk.TryRequire[contract.AdminLine](reg) })
		return line
	}

	// Hand the lazy admin line to any source that reports its OWN delivery
	// faults (email's undeliverable-outbound alerts). Generic opt-in — the bus
	// stays source-agnostic; it doesn't learn which platform cares.
	for _, s := range m.sources {
		if n, ok := s.(interface {
			SetAdminLine(func() contract.AdminLine)
		}); ok {
			n.SetAdminLine(m.adminLine)
		}
	}

	// The boot window starts when the sources actually launch — earlier modules
	// (a slow DB migration) must not eat it. Kept if pre-set (tests backdate it).
	if m.bootAt.IsZero() {
		m.bootAt = time.Now()
	}
	for _, s := range m.sources {
		m.wg.Add(1)
		go m.runSource(runCtx, s)
	}
	if m.healthInterval > 0 {
		m.wg.Add(1)
		go m.runHealthChecks(runCtx)
	}

	trunk.Provide[contract.Gateway](reg, m)
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.panel())
	m.state.Store(int32(trunk.StateReady))
	return nil
}

// bootRetryWindow bounds the only automatic source restart: a Run exit within
// the first minutes after Start is almost always a connect-time transient (host
// rebooted before LAN/DNS is up, IM backend briefly unreachable) — exactly the
// failure an unattended deployment hits, and exactly when sticky-dead would take
// the whole inbound platform offline for the process lifetime. Past the window,
// a Run exit keeps the deliberate sticky-dead semantics (sources reconnect
// internally while Run blocks; a steady-state Run EXIT is a real fault an
// operator should see, not something to hammer forever).
const (
	bootRetryWindow   = 5 * time.Minute
	bootRetryBaseWait = time.Second
	bootRetryMaxWait  = 30 * time.Second
)

// runSource drives one source's Run, fanning its events into the shared
// channel, and classifies how it exited. A clean exit or operator shutdown is
// silent; a Ctrl-D-style self-exit (io.EOF) is info; an exit inside the boot
// window is retried with backoff (see bootRetryWindow); any other error marks
// the source dead so the health view stops reporting a crashed receive loop as
// alive. Routing that to the admin line is the flow's job, not the bus's.
func (m *Module) runSource(ctx context.Context, s contract.Source) {
	defer m.wg.Done()
	wait := bootRetryBaseWait
	for {
		slog.Info("source starting", "source", s.Name())
		err := s.Run(ctx, m.events)
		switch {
		case ctx.Err() != nil:
			return // operator shutdown — silent.
		case errors.Is(err, io.EOF):
			// Clean self-exit (e.g. local stdin closed under a no-tty docker deploy). Not an
			// error → no alert, but the receive loop is gone, so mark it STOPPED (F35) — else
			// the admin panel keeps showing a stopped source as green "ready" forever (its
			// HealthCheck probe still passes).
			slog.Info("source self-exited (EOF)", "source", s.Name())
			m.health.markStopped(s.Name())
			return
		}
		if time.Since(m.bootAt) < bootRetryWindow {
			slog.Warn("source stopped during boot window — retrying", "source", s.Name(), "err", err, "wait", wait)
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			wait = min(wait*2, bootRetryMaxWait)
			continue
		}
		if err == nil {
			// Run returned nil WITHOUT ctx cancellation = an unexpected self-stop: the
			// source is no longer receiving. Mark it dead so the health view stops
			// reporting a no-longer-listening source as alive.
			slog.Warn("source self-stopped without shutdown — marking dead", "source", s.Name())
			if m.health.markDead(s.Name(), errors.New("source stopped receiving without shutdown")) {
				m.alert(contract.AdminError, "source-lifecycle", "source %q stopped receiving without shutdown", s.Name())
			}
			return
		}
		slog.Error("source stopped with error", "source", s.Name(), "err", err)
		if m.health.markDead(s.Name(), err) {
			m.alert(contract.AdminError, "source-lifecycle", "source %q stopped: %v", s.Name(), err)
		}
		return
	}
}

// alert pages the admin line nil-safely (unset/absent line → no-op), on a fresh
// goroutine with a Background ctx + deferred recover — symmetric with the model
// pool's notifyAdminAsync (BUG-4 / D8). A panic in the deliverer (nil deref, format
// misuse, third-party adapter bug) crosses the goroutine boundary where the caller's
// stack can't recover it, so a synchronous Notify would crash the whole process when
// a source-death or health alert fired; here it is logged + swallowed. Going async
// also keeps a slow/blocked deliverer from stalling the source/health goroutines.
// (The adminLine resolver is fixed in Start and never hot-swapped, so there is no
// concurrent swap to race here.)
func (m *Module) alert(level contract.AdminLevel, origin, format string, args ...any) {
	if m.adminLine == nil {
		return
	}
	l := m.adminLine()
	if l == nil {
		return
	}
	// D35 (deliberate-accept): this alert goroutine is NOT in m.wg, so one spawned just as
	// Stop begins can outlive Stop and call Notify after AdminLine (which stops before
	// stoma) has torn down. Benign by design — Notify renders + non-blocking-sends to a
	// never-closed queue (no send-on-closed panic), falls back to a logged AppendAlert, and
	// the recover below contains any panic; worst case the alert is dropped. Tracking it in
	// m.wg would make Stop wait on a best-effort audit ping — not worth it for a dropped alert.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("stoma: adminLine.Notify panicked", "panic", r, "origin", origin)
			}
		}()
		l.Notify(context.Background(), level, origin, format, args...)
	}()
}

// Stop cancels every source's Run, waits for the goroutines to exit, then
// closes the event channel so a `for range Events()` consumer terminates.
//
// The bus only guarantees a clean channel close, not delivery: buffered-but-
// unstarted events are intentionally dropped (the inbound flow no longer drains
// them — the sender re-sends). Only an in-flight turn's message is WAL'd, and boot
// recovery clears that silently.
func (m *Module) Stop(context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	close(m.events)
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// compile-time assertions.
var (
	_ trunk.Module     = (*Module)(nil)
	_ contract.Gateway = (*Module)(nil)
)
