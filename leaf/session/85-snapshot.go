package session

import (
	"context"
	"sort"
	"time"
)

// SessionSnapshot is the read-only arbiter view for the webui — counts only, no
// per-sid detail (the arbiter's in-memory maps are the source of truth; a richer
// roster can drill later).
type SessionSnapshot struct {
	Busy          int // sids with a turn in flight
	PendingEvents int // events queued behind busy sids
	Dropped       int // sids whose pending batch overflowed (awaiting announce)
	Tracked       int // sids with live arbiter state
}

// PendingWALCount reports how many rows the crash-recovery WAL
// (bob_pending_inbound) currently holds — the DURABLE side of the queue, as
// opposed to SessionSnapshot.PendingEvents, which counts the in-memory batches
// waiting behind a busy sid. The two answer different questions and neither
// implies the other: a busy-queued message never gets a WAL row, and a WAL row
// outlives the process that made it. Kept off Snapshot because this one costs a
// store round-trip while everything there is a map read.
func (m *Manager) PendingWALCount(ctx context.Context) (int, error) {
	return m.store.CountPending(ctx)
}

// InFlightRow is one currently-running turn for the webui in-flight panel.
type InFlightRow struct {
	Sid       string
	Scope     string
	StartedAt time.Time
}

// InFlight returns the turns running RIGHT NOW (busy sids) with their scope + start time,
// oldest first (the longest-running on top). Powers the webui "running" table so an
// operator sees live activity + elapsed instead of a blank wait.
func (m *Manager) InFlight() []InFlightRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]InFlightRow, 0, len(m.busy))
	for sid, busy := range m.busy {
		if !busy {
			continue
		}
		meta := m.sidMeta[sid]
		out = append(out, InFlightRow{Sid: sid, Scope: meta.Scope, StartedAt: meta.StartedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// Snapshot returns the live arbiter counts under the manager lock.
func (m *Manager) Snapshot() SessionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := 0
	for _, b := range m.busy {
		if b {
			busy++
		}
	}
	pending := 0
	for _, q := range m.pending {
		pending += len(q)
	}
	dropped := 0
	for _, d := range m.dropped {
		if d {
			dropped++
		}
	}
	return SessionSnapshot{Busy: busy, PendingEvents: pending, Dropped: dropped, Tracked: len(m.sidMeta)}
}
