// Package adminline is the system-level "summon the admin" funnel — the single
// module every subsystem calls (contract.AdminLine.Notify) when it must reach the
// operator. A single worker goroutine drains a buffered channel and runs the
// per-alert pipeline: persist (best-effort audit) → throttle → deliver. Notify
// returns immediately (调用方无感); a full queue spills to a synchronous persist
// so an alert is never lost. Delivery goes through the Deliverer seam, so this
// package imports only contract (+ its own store) — never the source packages.
//
// Outlets: the human outlet ("<source>:<chat_id>", via contract.Source.Send) or
// unattached (persist only). Pruning runs as a trunk housekeeping task.
package adminline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/adminline/store"
)

const (
	throttleWindow = 60 * time.Second
	throttleMax    = 5
	// queueDepth buffers Notify→worker. An ordinary burst is absorbed without a
	// caller touching the synchronous fallback; a sustained flood spills there.
	queueDepth = 256
	// persistTimeout caps a worker/shutdown store call so a wedged backend can't
	// freeze the loop. syncFallbackPersistTimeout is shorter because it runs in
	// the CALLER's goroutine (source-health etc.) — a storm must not hold them up.
	persistTimeout             = 10 * time.Second
	syncFallbackPersistTimeout = 2 * time.Second
	deliverTimeout             = 15 * time.Second
)

// Line is the admin-line body — the single funnel implementing contract.AdminLine.
type Line struct {
	store     store.Store
	deliverer Deliverer // nil = unattached (persist only)
	// window/max are the fixed-window throttle knobs — always the package
	// defaults in production; tests may override before Run starts.
	window time.Duration
	max    int

	// now is the time source — DB-calibrated (clock.Now), injectable so tests drive
	// the fixed-window throttle deterministically. Always non-nil (set by newLine).
	now func() time.Time

	// queue carries rendered alerts from Notify to the worker.
	queue chan store.AdminAlert
	// throttle is per-(origin,level) window state, owned SOLELY by the worker
	// goroutine — never touched from Notify — so it needs no locking.
	throttle map[bucketKey]*window
}

// newLine builds the line. deliverer may be nil (unattached). The worker is NOT
// started — call Run in its own goroutine.
func newLine(st store.Store, deliverer Deliverer) *Line {
	return &Line{
		store:     st,
		deliverer: deliverer,
		window:    throttleWindow,
		max:       throttleMax,
		now:       clock.Now, // persisted alert ts must be DB-calibrated, like warrant/accounts
		queue:     make(chan store.AdminAlert, queueDepth),
		throttle:  make(map[bucketKey]*window),
	}
}

func (l *Line) nowTime() time.Time {
	return l.now() // persisted alert ts (bob_admin_line.ts) — DB-calibrated
}

// Notify is the system-wide entry point. It renders the text, builds the alert,
// and hands it to the worker — returning immediately. A full queue persists
// synchronously instead so the alert is never lost. Typed-nil safe.
func (l *Line) Notify(ctx context.Context, level contract.AdminLevel, origin, format string, args ...any) {
	if l == nil {
		return
	}
	a := store.AdminAlert{
		TS:        l.nowTime().Unix(),
		Level:     level,
		Origin:    origin,
		Text:      fmt.Sprintf(format, args...),
		Forwarded: store.ForwardNone,
	}
	select {
	case l.queue <- a:
	default:
		slog.Warn("admin-line: queue full — persisting alert synchronously", "origin", origin, "level", level)
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncFallbackPersistTimeout)
		if _, err := l.store.AppendAlert(pctx, a); err != nil {
			slog.Error("admin-line: synchronous persist failed — alert lost", "origin", origin, "level", level, "err", err)
		}
		cancel()
	}
}

// Run drains the worker queue until ctx is cancelled — call in its own goroutine.
// It is the sole owner of the throttle state (no locking). On cancel it drains
// the buffer and persists each (the "never lose" invariant across shutdown).
func (l *Line) Run(ctx context.Context) {
	if l == nil {
		return
	}
	slog.Info("admin-line worker started", "throttle_window", l.window, "throttle_max", l.max, "attached", l.deliverer != nil)
	ticker := time.NewTicker(l.window) // throttle-map sweep: flush stranded rollups + evict idle buckets
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.drainOnShutdown()
			slog.Info("admin-line worker stopped")
			return
		case a := <-l.queue:
			l.handle(ctx, a)
		case <-ticker.C:
			l.sweepThrottle(ctx)
		}
	}
}

// drainOnShutdown persists every alert still buffered after cancel (bounded by
// queueDepth), THEN flushes any stranded throttle rollups. Persist-only: the
// never-lose invariant is AppendAlert alone.
func (l *Line) drainOnShutdown() {
	for {
		select {
		case a := <-l.queue:
			pctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), persistTimeout)
			if _, err := l.store.AppendAlert(pctx, a); err != nil {
				slog.Error("admin-line: shutdown-drain persist failed — alert lost", "origin", a.Origin, "level", a.Level, "err", err)
			}
			cancel()
		default:
			// Queue drained — flush STRANDED throttle rollups too: suppressed alerts
			// folded into a window that hasn't hit its sweep would otherwise be lost on
			// shutdown. flushRollup no-ops on suppressed==0 and derives its own
			// WithoutCancel+timeout ctxs, so a background ctx is fine. l.throttle is
			// worker-owned (this runs in the worker goroutine) — no lock needed.
			for key, w := range l.throttle {
				l.flushRollup(context.Background(), key, w)
			}
			return
		}
	}
}

var _ contract.AdminLine = (*Line)(nil)
