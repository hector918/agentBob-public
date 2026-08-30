package model

import (
	"sort"
	"time"

	"agentbob/contract"
)

// Snapshot implements contract.ModelPool. It is the CQRS read-side
// aggregator: a single state.Load() + one snapshotInfo per entry +
// SnapshotInfo() from each sub-component (heartbeat / reload / usage).
// Cheap; takes one row.mu per entry, no IO.
//
// The sub-component readouts are additive (see contract.PoolSnapshot) — a
// downstream reader that only looks at Entries / InFlight is unaffected.
func (p *MultiPool) Snapshot() contract.PoolSnapshot {
	out := contract.PoolSnapshot{}
	now := time.Now()
	st := p.state.Load()
	if st != nil {
		for _, row := range st.entries {
			info := row.snapshotInfo(now)
			out.Entries = append(out.Entries, info)
			out.InFlight += info.InFlight
		}
	}
	out.Queues = p.queueSnapshot()
	out.Heartbeat = p.heartbeat.SnapshotInfo()
	out.Reload = p.reload.SnapshotInfo()
	out.Usage = p.usage.SnapshotInfo()
	return out
}

// queueSnapshot reports the per-kind wait queue: the callers blocked on a
// concurrency slot, which no other snapshot field can show (InFlight counts the
// ones already being served). Kinds at rest are omitted — the depth map only
// holds kinds with a live waiter, and reporting a row of zeros for every
// configured kind would bury the one that matters.
func (p *MultiPool) queueSnapshot() []contract.QueueInfo {
	p.queueMu.Lock()
	depth := make(map[string]int, len(p.queueDepth))
	full := make(map[string]bool, len(p.queueDepth))
	for kind, n := range p.queueDepth {
		if n <= 0 {
			continue
		}
		depth[kind] = n
		full[kind] = p.queueFullKinds[kind]
	}
	p.queueMu.Unlock()
	if len(depth) == 0 {
		return nil
	}
	kinds := make([]string, 0, len(depth))
	for kind := range depth {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds) // stable order: the panel redraws every tick
	out := make([]contract.QueueInfo, 0, len(kinds))
	for _, kind := range kinds {
		// queueCapacity reads the entry table (state.Load), not queueMu — taken
		// outside the lock above so the two are never held together.
		out = append(out, contract.QueueInfo{
			Kind:     kind,
			Waiting:  depth[kind],
			Capacity: p.queueCapacity(kind),
			Full:     full[kind],
		})
	}
	return out
}

// snapshotInfo collects the per-entry portion of a Snapshot under one
// row.mu acquisition: the row's static info merged with its dynamic
// liveness / usage counters, plus the State label derived from the
// current flag combination.
//
// State priority (highest first):
//
//	disabled > paused > cooling > tentative > live
func (e *entryRow) snapshotInfo(now time.Time) contract.ModelInfo {
	e.mu.Lock()
	ifl := e.inFlight
	paused := e.paused
	dead := !e.deadUntil.IsZero() && e.deadUntil.After(now)
	tentative := e.tentative
	info := e.info
	info.TotalCalls = e.totalCalls
	info.TotalErrors = e.totalErrors
	info.TotalInputTokens = e.totalInputTokens
	info.TotalOutputTokens = e.totalOutputTokens
	lastErr := e.lastErr
	if dead {
		info.DeadUntilUnix = e.deadUntil.Unix()
	}
	e.mu.Unlock()
	info.InFlight = ifl
	switch {
	case info.Priority < minEnabledPriority:
		info.State = "disabled"
	case paused:
		info.State = "paused"
	case dead:
		info.State = "cooling"
	case tentative:
		// Heartbeat re-admitted it after a cheap Ping — pick-eligible
		// again but NOT proven (one real error re-deads it). Surfaced
		// distinctly so a flapping backend never shows as healthy "live".
		info.State = "tentative"
	default:
		info.State = "live"
	}
	// lastErr is a diagnostic for WHY an entry is non-live (it cooled / is flapping).
	// RecordSuccess deliberately does NOT clear it, so a recovered-to-live entry still
	// holds its stale error — suppress it here so the panel doesn't render it
	// warn-colored forever (the field's "only shown for non-live entries" contract).
	if info.State != "live" {
		info.LastError = lastErr
	}
	return info
}
