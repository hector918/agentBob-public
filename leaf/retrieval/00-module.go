// Package retrieval is the cold-memory feed + read client as a trunk module:
// the bob side of the external chat-retrieval leaf (sidecars/chat-retrieval).
// It provides contract.RetrievalClient (read, for the recall_memory tool) and
// contract.RetrievalFeed (write, for the flow's post-turn enqueue), backed by a
// durable outbox table + a drain goroutine. OFF by default — Start registers
// nothing until retrieval.enabled + base_url are set (consumers degrade: the
// recall tool reports unavailable, the flow skips). See docs/cold-memory-trunk.md.
package retrieval

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/trunk"
)

// outboxPrunePeriod is how often the cap-trim sweep runs (the leaf-down-forever
// safety; normal operation drains far faster than this).
const outboxPrunePeriod = time.Hour

// Module is the cold-memory feed as a trunk module.
type Module struct {
	cfg    Config
	cl     *Client // nil when disabled
	cancel context.CancelFunc
	wg     sync.WaitGroup
	state  atomic.Int32 // trunk.State
	// lastDrainErr is the feeder's current failure, "" while it is draining. The
	// feeder's own logging goes quiet after the first failure (drainFailing), so
	// this is what the panel reads to answer "is the feed moving right now".
	// atomic.Value holds a string; nil before the first tick.
	lastDrainErr atomic.Value
}

// drainErr returns the feeder's current failure ("" = draining fine).
func (m *Module) drainErr() string {
	s, _ := m.lastDrainErr.Load().(string)
	return s
}

// New returns a retrieval module from its config section (config.LoadConfig).
func New(cfg Config) *Module { return &Module{cfg: cfg} }

func (m *Module) Name() string { return "retrieval" }

// Provides the read client + write feed. Declared even when disabled — an Optional
// module that declares but doesn't register just lets consumers degrade (trunk warns).
func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.RetrievalClient](),
		trunk.TypeOf[contract.RetrievalFeed](),
	}
}

// Needs contract.DB — the outbox lives in the DB. Hard need (only consulted when
// the module actually starts its work; disabled Start returns before using it).
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.DB]()}
}

// Optional: without the feed, recall is unavailable and the flow skips — the
// pipeline is otherwise unaffected.
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	if !m.cfg.Configured() {
		slog.Info("retrieval: disabled (set retrieval.enabled + retrieval.base_url to feed/recall cold memory)")
		m.state.Store(int32(trunk.StateReady))
		return nil // declared Provides left unregistered → consumers degrade
	}
	ctx := context.Background()
	db := trunk.Require[contract.DB](reg)
	if err := migrateOutbox(ctx, db); err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return err
	}
	st := &outbox{db: db}
	m.cl = NewClient(m.cfg.BaseURL, m.cfg.Timeout(), m.cfg.APIKey)

	trunk.Provide[contract.RetrievalClient](reg, m.cl)
	trunk.Provide[contract.RetrievalFeed](reg, &feed{st: st})

	// Drain goroutine: a long-lived loop independent of the (transient) Start ctx,
	// cancelled in Stop (mirrors stoma's runCtx pattern).
	runCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(1)
	go m.runFeeder(runCtx, st)

	// Cap-trim sweep (persistent state → Housekeeper). Optional — a minimal config
	// runs without a housekeeper; normal drain keeps the outbox small anyway.
	if hk, ok := trunk.TryRequire[trunk.Housekeeper](reg); ok {
		hk.Register(trunk.Task{
			Name:   "retrieval-outbox-prune",
			Period: outboxPrunePeriod,
			Run: func(ctx context.Context) error {
				n, err := st.prune(ctx, outboxCap)
				if err != nil {
					return err
				}
				if n > 0 {
					slog.Warn("retrieval: outbox over cap — dropped oldest", "removed", n, "cap", outboxCap)
				}
				return nil
			},
		})
	}

	// Panel is optional the same way the housekeeper is: a minimal config (no
	// webui) still feeds and recalls, it just has nobody to show the queue to.
	if pr, ok := trunk.TryRequire[contract.PanelRegistry](reg); ok && pr != nil {
		pr.RegisterPanel(m.panel(st))
	}

	m.state.Store(int32(trunk.StateReady))
	slog.Info("retrieval: enabled", "base_url", m.cfg.BaseURL, "tick", m.cfg.Tick())
	return nil
}

// runFeeder ticks every cfg.Tick(), draining the outbox to the leaf until empty (or
// an error) each tick. A transient leaf outage just leaves rows for the next tick.
func (m *Module) runFeeder(ctx context.Context, st *outbox) {
	defer m.wg.Done()
	t := time.NewTicker(m.cfg.Tick())
	defer t.Stop()
	batch := m.cfg.BatchSize()
	drainFailing := false // latch: loud on the up→down edge, quiet while it stays down
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for {
				n, err := drain(ctx, m.cl, st, batch)
				if err != nil {
					if ctx.Err() == nil { // shutdown cancel is not a drain failure
						// Every tick refreshes this, unlike the log: the latch below
						// silences the WARN after the first failure, so the panel would
						// otherwise show a reason from days ago.
						m.lastDrainErr.Store(err.Error())
						if !drainFailing {
							slog.Warn("retrieval: drain failed (rows kept for retry)", "err", err)
							drainFailing = true
						} else {
							slog.Debug("retrieval: drain still failing (rows kept for retry)", "err", err)
						}
					}
					break
				}
				m.lastDrainErr.Store("")
				if drainFailing { // first success after a run of failures
					slog.Info("retrieval: drain recovered")
					drainFailing = false
				}
				if n == 0 {
					break // outbox empty
				}
			}
		}
	}
}

func (m *Module) Stop(context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

var _ trunk.Module = (*Module)(nil)
