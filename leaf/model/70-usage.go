package model

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// ModelUsageRecorder is the model-package sub-component responsible for
// persisting per-entry hourly usage buckets to the configured
// ModelUsageStore. It's intentionally a tiny struct — the "real" state lives
// on each entryRow's hourly map; ModelUsageRecorder is just the drain.
//
// Lifecycle:
//   - Flush(ctx, entries, includeCurrentHour=false) is the housekeeping
//     path (called from pipeline-startup's flush tick).
//   - Flush(ctx, entries, includeCurrentHour=true) is the shutdown path —
//     the in-progress hour is included because the process is exiting.
//   - Retire(rows) queues rows displaced by a config reload; they drain
//     through the next housekeeping flushes (see retiredFlushes).
//
// Goroutine model: synchronous; called from whoever drives the flush
// tick. No background loop of its own.
type ModelUsageRecorder struct {
	store ModelUsageStore // nil → Flush and Retire are silent no-ops (test / no-store paths)

	// retiredMu guards retired: rows displaced by a pool-state swap.
	// In-flight calls hold the old *entryRow values and keep recording
	// usage onto them after the swap (B25), so each displaced
	// batch stays flush-reachable here until it ages out.
	retiredMu sync.Mutex
	retired   []retiredBatch
}

// retiredBatch is one displaced poolState's rows plus the number of
// flushes that must still include them before the batch is dropped.
type retiredBatch struct {
	rows        []*entryRow
	flushesLeft int
}

// retiredFlushes is how many Flush passes a retired batch survives. Two
// passes (one housekeeping tick apart) cover every in-flight call that
// can still write usage to a displaced row — a call is bounded by the
// provider hard timeout, far below one tick.
const retiredFlushes = 2

// NewModelUsageRecorder builds a recorder with no store wired — call
// SetStore later (the pool is constructed BEFORE the store opens, so
// post-construction wiring avoids a construction-order cycle).
func NewModelUsageRecorder() *ModelUsageRecorder { return &ModelUsageRecorder{} }

// SetStore wires the store handle. Calling SetStore(nil) reverts the
// recorder to no-op mode. Safe to call from a single goroutine at boot;
// concurrent flushes vs SetStore are not protected (the store wiring
// happens once before flush ticks start).
func (u *ModelUsageRecorder) SetStore(s ModelUsageStore) { u.store = s }

// SnapshotInfo is the read-side view of the recorder — HasStore
// reflects whether the store handle has been wired. Cheap.
func (u *ModelUsageRecorder) SnapshotInfo() contract.UsageInfo {
	if u == nil {
		return contract.UsageInfo{}
	}
	return contract.UsageInfo{HasStore: u.store != nil}
}

// Retire queues rows displaced by a pool-state swap for the next
// housekeeping flushes. Their buckets (including the in-progress hour —
// the rows take no new traffic beyond in-flight stragglers) are drained
// on each pass until the batch ages out after retiredFlushes passes.
//
// No-op when no store is wired — same guard as Flush. Flush is the only
// drain, so on a store-less recorder a queued batch would pin every
// displaced poolState's rows in memory forever, one batch per reload;
// skipping the queue lets the old rows GC as usual. Unlocked store read:
// same posture as Flush (SetStore is wired once at boot, before reloads
// or flush ticks run).
func (u *ModelUsageRecorder) Retire(rows []*entryRow) {
	if u == nil || u.store == nil || len(rows) == 0 {
		return
	}
	u.retiredMu.Lock()
	u.retired = append(u.retired, retiredBatch{rows: rows, flushesLeft: retiredFlushes})
	u.retiredMu.Unlock()
}

// Flush drains every COMPLETED hour's accumulated usage from `entries`
// into the store and removes those buckets, then drains the retired
// batches. When includeCurrentHour is false the in-progress hour is left
// in memory; when true (the shutdown path) ALL hours including the
// current partial one are flushed — and the retired batches drain
// terminally regardless of remaining passes. The store's AddModelUsage
// is an additive UPSERT, so a partial-hour row is correct: a fresh entry
// flushes onto the same (entry, hour) row later and the counts add up.
//
// No-op when no store is wired.
func (u *ModelUsageRecorder) Flush(ctx context.Context, entries []*entryRow, includeCurrentHour bool) {
	if u == nil || u.store == nil {
		return
	}
	u.flushRows(ctx, entries, includeCurrentHour, includeCurrentHour)
	u.flushRetired(ctx, includeCurrentHour)
}

// flushRetired runs one drain pass over the retired batches and drops a
// batch once it has seen retiredFlushes passes. final (the shutdown
// path) drains everything terminally — the process is exiting, so a
// re-added bucket could never flush again.
func (u *ModelUsageRecorder) flushRetired(ctx context.Context, final bool) {
	u.retiredMu.Lock()
	batches := u.retired
	u.retired = nil
	u.retiredMu.Unlock()
	var keep []retiredBatch
	for _, b := range batches {
		b.flushesLeft--
		terminal := final || b.flushesLeft <= 0
		u.flushRows(ctx, b.rows, true, terminal)
		if !terminal {
			keep = append(keep, b)
		}
	}
	if len(keep) > 0 {
		u.retiredMu.Lock()
		// Prepend: a batch Retire()d during the pass is younger and
		// belongs after the survivors.
		u.retired = append(keep, u.retired...)
		u.retiredMu.Unlock()
	}
}

// flushRows drains `rows`' buckets (completed hours only, or all hours
// when includeCurrentHour) into the store.
//
// terminal selects the failure posture. false — the rows stay flush-
// reachable: a failed bucket is re-added so the next flush retries it
// (usage is never dropped while the row survives), re-add WARNs are
// aggregated into ONE summary line per pass (D21 — a store
// outage of D hours used to produce ~D²/2 per-bucket lines), and the
// re-add is bounded by hourlyBucketCap (oldest dropped). true — the rows
// are leaving flush reach (shutdown, or a retired batch's last pass):
// a persistence failure is accepted as an explicit loss, where a silent
// re-add would only pretend the bucket was kept.
func (u *ModelUsageRecorder) flushRows(ctx context.Context, rows []*entryRow, includeCurrentHour, terminal bool) {
	if len(rows) == 0 {
		return
	}
	currentHour := clock.Now().Truncate(time.Hour).Unix()
	type pending struct {
		row    *entryRow
		hour   int64
		bucket hourBucket
	}
	var todo []pending
	for _, row := range rows {
		row.mu.Lock()
		for hour, b := range row.hourly {
			if !includeCurrentHour && hour >= currentHour {
				continue // in-progress hour — leave it
			}
			todo = append(todo, pending{row: row, hour: hour, bucket: *b})
			delete(row.hourly, hour)
		}
		row.mu.Unlock()
	}
	retained, capDropped := 0, 0
	var lastErr error
	for _, t := range todo {
		err := u.store.AddModelUsage(ctx, t.row.info.Name, t.hour,
			t.bucket.calls, t.bucket.errors, t.bucket.input, t.bucket.output)
		if err == nil {
			continue
		}
		if terminal {
			slog.Warn("model usage flush failed on shutdown/retire — bucket dropped (row is leaving flush reach)",
				"entry", t.row.info.Name, "hour_start", t.hour, "err", err)
			continue
		}
		retained++
		lastErr = err
		// Re-add so the usage isn't lost; a concurrent call may have
		// started a fresh bucket for the same hour in the meantime.
		t.row.mu.Lock()
		if t.row.hourly == nil {
			t.row.hourly = map[int64]*hourBucket{}
		}
		b := t.row.hourly[t.hour]
		if b == nil {
			b = &hourBucket{}
			t.row.hourly[t.hour] = b
		}
		b.calls += t.bucket.calls
		b.errors += t.bucket.errors
		b.input += t.bucket.input
		b.output += t.bucket.output
		capDropped += dropOldestHourly(t.row)
		t.row.mu.Unlock()
	}
	if retained > 0 {
		slog.Warn("model usage flush failed — keeping buckets for next flush",
			"buckets", retained, "dropped_at_cap", capDropped, "err", lastErr)
	}
}
