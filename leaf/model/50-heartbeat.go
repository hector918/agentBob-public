package model

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"agentbob/contract"
)

// HeartbeatRunner is the on-demand liveness loop: a single goroutine
// that Pings every currently-dead entry every heartbeatInterval and
// tentatively re-admits one whose Ping succeeds. Spawned (CAS) by
// Ensure on the false→true dead-transition; stops itself once a tick
// finds no entry still dead.
//
// State ownership:
//   - poolCtx / poolCancel — heartbeat lifetime; cancelled by Close
//   - running — at-most-one goroutine
//
// Concurrency: Ensure is safe to call from any goroutine (Chat /
// ChatStreamWatch hot path); the CAS guarantees at most one active goroutine.
// Close is idempotent. SnapshotInfo does no IO.
type HeartbeatRunner struct {
	state      *atomic.Pointer[poolState]
	onRecovery func() // pool-level hook: checkPoolLiveness (entryRow-unaware)

	poolCtx    context.Context
	poolCancel context.CancelFunc

	running atomic.Bool
}

// NewHeartbeatRunner wires a fresh runner against the shared state
// pointer + recovery callback. The runner doesn't start a goroutine
// until Ensure() is called on a fresh dead-transition.
func NewHeartbeatRunner(state *atomic.Pointer[poolState], onRecovery func()) *HeartbeatRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &HeartbeatRunner{
		state:      state,
		onRecovery: onRecovery,
		poolCtx:    ctx,
		poolCancel: cancel,
	}
}

// Close cancels the runner's lifetime context — a running goroutine
// notices on the next select and exits. Idempotent.
func (h *HeartbeatRunner) Close() {
	if h == nil || h.poolCancel == nil {
		return
	}
	h.poolCancel()
}

// Ensure starts the liveness goroutine if it isn't already running.
// Called by recordError on a fresh dead-transition — the heartbeat
// exists ONLY while ≥1 entry is dead, and stops itself once none are.
func (h *HeartbeatRunner) Ensure() {
	if h == nil {
		return
	}
	if !h.running.CompareAndSwap(false, true) {
		return // already running
	}
	go h.run()
}

// run is the on-demand liveness loop. running is managed EXPLICITLY
// (not a blanket defer): the no-dead-entries exit clears the flag then
// re-checks for a dead entry that recordError may have marked in the
// race window. A blanket `defer Store(false)` would clobber a
// concurrent re-arm.
func (h *HeartbeatRunner) run() {
	// Panic safety net only — release the flag so a later dead-transition
	// can restart the heartbeat. Normal exits clear it explicitly.
	defer func() {
		if r := recover(); r != nil {
			h.running.Store(false)
			slog.Error("model pool heartbeat panicked — flag released", "panic", r)
		}
	}()
	slog.Info("model pool heartbeat started (≥1 entry dead)")
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.poolCtx.Done():
			h.running.Store(false)
			slog.Info("model pool heartbeat stopped (pool closing)")
			return
		case <-ticker.C:
			if h.tick() {
				continue // still ≥1 dead entry — keep probing
			}
			// No dead entry this tick. Release the flag, THEN re-check:
			// a recordError that newly-deaded an entry in the gap
			// between the tick and now would have seen running still
			// true and skipped Ensure — leaving its dead entry with no
			// active probe. Clearing the flag before the re-scan closes
			// that window.
			h.running.Store(false)
			if !h.hasDeadEntry() {
				slog.Info("model pool heartbeat stopped (no dead entries)")
				return
			}
			// A dead entry slipped into the gap. Re-arm — unless a
			// concurrent Ensure already grabbed it (CAS lost).
			if !h.running.CompareAndSwap(false, true) {
				return // another Ensure took over
			}
			// re-acquired — keep looping
		}
	}
}

// hasDeadEntry reports whether any entry is currently dead (cooldown
// not elapsed, not paused, not tentative). Cheap — a state scan, no
// Ping. Used by run to close the start/stop race.
func (h *HeartbeatRunner) hasDeadEntry() bool {
	st := h.state.Load()
	if st == nil {
		return false
	}
	now := time.Now()
	for _, row := range st.entries {
		row.mu.Lock()
		dead := !row.paused && !row.tentative && !row.deadUntil.IsZero() && row.deadUntil.After(now)
		row.mu.Unlock()
		if dead {
			return true
		}
	}
	return false
}

// tick Pings every currently-dead entry and applies tentative recovery
// on success. Returns true if at least one entry is still dead after the
// tick (the loop should keep running). The Ping is network IO — it runs
// WITHOUT holding entryRow.mu: dead entries are snapshotted under the
// lock, Pinged outside it, results applied back under the lock.
func (h *HeartbeatRunner) tick() bool {
	st := h.state.Load()
	if st == nil {
		return false
	}
	now := time.Now()
	type deadEntry struct {
		row     *entryRow
		chatter contract.Chatter
	}
	var toPing []deadEntry
	backingOff := 0 // dead, but its probe backoff (nextProbeAt) hasn't elapsed
	for _, row := range st.entries {
		row.mu.Lock()
		dead := !row.paused && !row.tentative && !row.deadUntil.IsZero() && row.deadUntil.After(now)
		waiting := dead && now.Before(row.nextProbeAt)
		row.mu.Unlock()
		if !dead {
			continue
		}
		if waiting {
			// Burned a tentative recovery recently — skip this tick (entryRow.
			// nextProbeAt explains why). Still counted as dead below so the
			// loop keeps running.
			backingOff++
			continue
		}
		toPing = append(toPing, deadEntry{row: row, chatter: row.chatter})
	}
	if len(toPing) == 0 {
		// backingOff entries MUST keep the loop alive: returning false stops the
		// heartbeat, and Ensure only re-arms on a FRESH dead-transition — so a
		// backed-off entry would sit dead with nobody left to probe it once its
		// window elapses, until unrelated traffic happened to error again.
		return backingOff > 0
	}
	stillDead := false
	recovered := false
	for _, d := range toPing {
		pingCtx, cancel := context.WithTimeout(h.poolCtx, heartbeatPingTimeout)
		err := d.chatter.Ping(pingCtx)
		cancel()
		// entryRow.MarkProbed re-checks under the row's mu and decides:
		// "tentative recovery" (ok=true → recovered=true) or "still dead"
		// (re-arm deadUntil → stillDead=true). Recheck-skip (the entry
		// changed state mid-Ping) returns both false — no-op silently.
		// Single entryRow-owned writer per §3 (HeartbeatRunner must not
		// fiddle entry fields directly).
		rec, dead := d.row.MarkProbed(err == nil)
		if rec {
			recovered = true
			slog.Info("model pool heartbeat: dead entry Ping ok — tentatively recovered", "name", d.row.info.Name)
		} else if dead {
			stillDead = true
			slog.Debug("model pool heartbeat: dead entry still unreachable", "name", d.row.info.Name, "err", err)
		}
	}
	if recovered {
		// A dead entry was tentatively re-admitted — the pool may no
		// longer be all-dead. Recompute + clear the admin-alert latch so
		// a later full outage pages again. Hook is pool-supplied so the
		// heartbeat stays unaware of MultiPool.
		if h.onRecovery != nil {
			h.onRecovery()
		}
	}
	return stillDead || backingOff > 0
}

// SnapshotInfo is the read-side view of the runner — Running reflects
// whether a probe goroutine is currently active. Cheap; safe to call
// concurrently with Ensure / Close / tick.
func (h *HeartbeatRunner) SnapshotInfo() contract.HeartbeatInfo {
	if h == nil {
		return contract.HeartbeatInfo{}
	}
	return contract.HeartbeatInfo{Running: h.running.Load()}
}
