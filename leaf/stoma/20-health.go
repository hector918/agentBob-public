package stoma

import (
	"context"
	"sort"
	"sync"
	"time"

	"agentbob/contract"
)

const (
	// healthProbeTimeout caps a single source's HealthCheck before the bus gives
	// up and counts the tick as a failure.
	healthProbeTimeout = 5 * time.Second
	// healthFailThreshold is how many consecutive probe failures flip a source
	// to "degraded" — one transient miss shouldn't.
	healthFailThreshold = 3
)

// SourceStatus is the read-only per-source health view (StateReporter). status
// is "ready" | "degraded" (failing its probe) | "dead" (its Run exited with an
// error and nothing restarts it) | "stopped" (its Run self-exited cleanly via
// io.EOF — e.g. local stdin closed under docker; NOT an error).
type SourceStatus struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	FailStreak int    `json:"fail_streak,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

// srcHealth tracks one source's recent probe outcomes. dead/stopped are sticky:
// once a source's Run exits, an API-level probe succeeding afterwards would lie
// "healthy" (the receive loop is gone until a bob restart). dead = error exit
// (alerting); stopped = clean io.EOF self-exit (F35 — no alert, but the panel must
// not keep showing a stopped source as green ready).
type srcHealth struct {
	healthy    bool
	failStreak int
	lastErr    string
	dead       bool
	deadErr    string
	stopped    bool
}

type healthTable struct {
	mu sync.Mutex
	m  map[string]*srcHealth
}

func newHealthTable(srcs map[string]contract.Source) *healthTable {
	m := make(map[string]*srcHealth, len(srcs))
	for name := range srcs {
		m[name] = &srcHealth{healthy: true}
	}
	return &healthTable{m: m}
}

// markDead records that a source's Run exited with an error — permanent for the
// process lifetime, and forces the source unhealthy regardless of later probes.
// Returns true on the FIRST transition to dead (so the caller alerts once).
func (h *healthTable) markDead(name string, err error) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.m[name]
	if e == nil || e.dead {
		return false
	}
	e.dead = true
	e.deadErr = err.Error()
	e.healthy = false
	return true
}

// markStopped records that a source's Run self-exited CLEANLY via io.EOF (F35) — a
// stopped source is not dead (no error, no alert), but its receive loop is gone, so the
// panel must show "stopped", not green "ready". Sticky for the process lifetime; a
// later probe succeeding must not resurrect it. No-op once dead/stopped.
func (h *healthTable) markStopped(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.m[name]
	if e == nil || e.dead || e.stopped {
		return
	}
	e.stopped = true
	e.healthy = false
}

// record folds one probe outcome into a source's health and returns the health
// TRANSITION it caused, for the caller to alert on: "" (no change), "degraded"
// (crossed the fail threshold), or "recovered" (a success after being degraded).
// A dead source stays dead.
func (h *healthTable) record(name string, probeErr error) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.m[name]
	if e == nil || e.dead || e.stopped {
		return "" // a Run that exited (error OR clean EOF) stays exited regardless of probes
	}
	if probeErr == nil {
		wasHealthy := e.healthy
		e.healthy = true
		e.failStreak = 0
		e.lastErr = ""
		if !wasHealthy {
			return "recovered"
		}
		return ""
	}
	wasHealthy := e.healthy
	e.failStreak++
	e.lastErr = probeErr.Error()
	if e.failStreak >= healthFailThreshold {
		e.healthy = false
	}
	if wasHealthy && !e.healthy {
		return "degraded"
	}
	return ""
}

func (h *healthTable) snapshot() []SourceStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]SourceStatus, 0, len(h.m))
	for name, e := range h.m {
		st := SourceStatus{Name: name, FailStreak: e.failStreak}
		switch {
		case e.dead:
			st.Status = "dead"
			st.LastError = e.deadErr
		case e.stopped:
			st.Status = "stopped" // clean io.EOF self-exit (F35) — not an error, but not ready
		case !e.healthy:
			st.Status = "degraded"
			st.LastError = e.lastErr
		default:
			st.Status = "ready"
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// runHealthChecks is the bus's per-source heartbeat: every healthInterval it
// probes each source concurrently and folds the result into the health table,
// alerting the admin line on each health transition (consistent with how the
// model pool reports its own health — a subsystem owns alerting its own domain).
func (m *Module) runHealthChecks(ctx context.Context) {
	defer m.wg.Done()
	ticker := time.NewTicker(m.healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.probeAll(ctx)
		}
	}
}

// probeAll probes every source once, concurrently, with a per-probe timeout.
// Skipped entirely when ctx is already done (a shutdown race would otherwise
// look like a real failure).
func (m *Module) probeAll(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	var wg sync.WaitGroup
	for name, s := range m.sources {
		wg.Add(1)
		go func(name string, s contract.Source) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
			defer cancel()
			err := s.HealthCheck(pctx)
			if ctx.Err() != nil {
				// Shutdown cancelled the probe mid-flight; a cancelled check is not a
				// real source failure — don't record it or fire a spurious alert.
				return
			}
			switch m.health.record(name, err) {
			case "degraded":
				m.alert(contract.AdminError, "source-health", "source %q is failing its health probe", name)
			case "recovered":
				m.alert(contract.AdminInfo, "source-health", "source %q recovered", name)
			}
		}(name, s)
	}
	// Don't let a source whose HealthCheck ignores its ctx pin the lifetime goroutine
	// at shutdown: join, but also release on ctx.Done(). A still-running probe then
	// finishes on its own goroutine — we just stop waiting on it.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
