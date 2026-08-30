package adminline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"agentbob/contract"
	"agentbob/leaf/adminline/store"
)

// rollupOrigin tags the synthetic rollup line — a delivery, not a counted alert,
// so persisting it never feeds the throttle of the bucket it summarises.
const rollupOrigin = "admin-line"

// bucketKey groups alerts for throttling: (origin, level).
type bucketKey struct {
	origin string
	level  contract.AdminLevel
}

// window is one bucket's fixed-window throttle state. Worker-goroutine-owned.
type window struct {
	start      time.Time
	forwarded  int
	suppressed int
}

// handle runs the per-alert pipeline on the worker goroutine: persist
// (best-effort) → throttle → deliver. Errors are logged and swallowed — one bad
// alert must not wedge the worker.
func (l *Line) handle(ctx context.Context, a store.AdminAlert) {
	// 1. Persist first, best-effort. The store being down is itself an alert this
	//    line must surface, so a persist failure does NOT abort delivery — the row
	//    just keeps id 0 (deliver/mark no-op on id 0).
	id, err := l.persist(ctx, a)
	if err != nil {
		slog.Error("admin-line: persist failed — delivering anyway (no audit row)", "origin", a.Origin, "level", a.Level, "err", err)
	} else {
		a.ID = id
	}

	// 2. Unattached → done; Forwarded stays "none".
	if l.deliverer == nil {
		return
	}

	// 3. Throttle for this (origin, level). Window math uses the LINE's clock, not
	//    a.TS (which is the audit raise-time and may lag in the channel).
	key := bucketKey{origin: a.Origin, level: a.Level}
	now := l.nowTime()
	w := l.throttle[key]
	if w == nil {
		w = &window{start: now}
		l.throttle[key] = w
	}
	if now.Sub(w.start) >= l.window {
		l.flushRollup(ctx, key, w) // owns suppressed-zeroing
		w.start, w.forwarded = now, 0
	}

	// 4. Within budget → forward individually.
	if w.forwarded < l.max {
		w.forwarded++
		l.deliver(ctx, a, store.ForwardSent)
		return
	}

	// 5. Over budget → fold into the rollup (persisted; marked suppressed). The
	//    rollup fires lazily when this window next rolls over (or via the sweep).
	w.suppressed++
	if a.ID != 0 {
		l.mark(ctx, a.ID, store.ForwardSuppressed)
	}
}

// sweepThrottle (periodic, worker goroutine) flushes stranded rollups for
// elapsed windows — so an idle bucket's suppressed count is delivered even if no
// next alert comes — then evicts idle buckets to bound map growth.
func (l *Line) sweepThrottle(ctx context.Context) {
	now := l.nowTime()
	for key, w := range l.throttle {
		if now.Sub(w.start) < l.window {
			continue
		}
		// Aged out of its window (in-window buckets `continue`d above): flush any
		// folded rollup, then drop the bucket UNCONDITIONALLY — a new one is created
		// on the next alert. (Was a fragile "delete iff suppressed==0", true only
		// because flushRollup had just zeroed it; reorder a line and it leaked — S-19.)
		l.flushRollup(ctx, key, w) // no-op when nothing was folded
		delete(l.throttle, key)
	}
}

// flushRollup emits one rollup line for a window that folded alerts, then resets
// its suppressed counter. No-op when nothing was folded.
func (l *Line) flushRollup(ctx context.Context, key bucketKey, w *window) {
	if w.suppressed <= 0 {
		return
	}
	n := w.suppressed
	w.suppressed = 0
	rollup := store.AdminAlert{
		TS:        l.nowTime().Unix(),
		Level:     key.level,
		Origin:    rollupOrigin,
		Text:      fmt.Sprintf("%s: %d more %s alert(s) folded", key.origin, n, key.level),
		Forwarded: store.ForwardNone,
	}
	if id, err := l.persist(ctx, rollup); err != nil {
		slog.Warn("admin-line: rollup persist failed — delivering anyway", "summarised_origin", key.origin, "level", key.level, "err", err)
	} else {
		rollup.ID = id
	}
	l.deliver(ctx, rollup, store.ForwardSent)
}

func (l *Line) persist(ctx context.Context, a store.AdminAlert) (int64, error) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	return l.store.AppendAlert(pctx, a)
}

// deliver forwards one alert and records the outcome (sent / failed), so a failed
// forward is distinguishable from a never-attached row ("none").
func (l *Line) deliver(ctx context.Context, a store.AdminAlert, onOK store.AdminForwardState) {
	// Defensive: handle() gates on attachment before throttling, so today this is
	// unreachable unattached — but any future call path must degrade to
	// persist-only (row stays "none"), not panic.
	if l.deliverer == nil {
		slog.Warn("admin-line: deliver with nil deliverer — persisted only", "origin", a.Origin, "level", a.Level, "id", a.ID)
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliverTimeout)
	err := l.deliverer.Deliver(dctx, a)
	cancel()
	if err != nil {
		slog.Warn("admin-line: delivery failed", "origin", a.Origin, "level", a.Level, "id", a.ID, "err", err)
		if a.ID != 0 {
			l.mark(ctx, a.ID, store.ForwardFailed)
		}
		return
	}
	if a.ID != 0 {
		l.mark(ctx, a.ID, onOK)
	}
}

func (l *Line) mark(ctx context.Context, id int64, state store.AdminForwardState) {
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	if err := l.store.MarkAlertForwarded(mctx, id, state); err != nil {
		slog.Warn("admin-line: mark forwarded failed", "id", id, "state", state, "err", err)
	}
}
