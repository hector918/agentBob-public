package trunk

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// idleCap bounds how long the scheduler sleeps when nothing is sooner — it still
// wakes on Register/Stop, this just re-evaluates periodically.
const idleCap = 24 * time.Hour

// firstRunDelay caps how long after boot a task's FIRST run is seeded. nextDue is
// in-memory only, so "first run = one full Period out" would let any Period longer
// than the deploy's restart cadence starve forever (a weekly pull+restart resets a
// 7d prune's clock every time — it never fires). Tasks are idempotent by contract,
// so an extra early pass per boot is harmless; boot itself still isn't a
// maintenance moment (the short grace keeps startup I/O clean).
const firstRunDelay = 10 * time.Minute

// failBackoffCap bounds the exponential backoff applied to a persistently
// failing task. A task whose Run keeps erroring/panicking doubles its interval
// each consecutive failure (Period, 2×Period, 4×Period, …) up to this cap, so a
// broken sweep doesn't burn a run + log line every Period forever; one success
// resets it to its normal Period. Tasks with a Period at or above the cap are
// unaffected (backoff never schedules SOONER than the task's own Period).
const failBackoffCap = 6 * time.Hour

// firstDue seeds a task's first run: the sooner of one Period or firstRunDelay.
func firstDue(period time.Duration) time.Duration {
	if period < firstRunDelay {
		return period
	}
	return firstRunDelay
}

// Task is a periodic, idempotent persistence-maintenance job — a DB prune, a
// blob/file sweep — a module registers with the Housekeeper instead of spinning
// its own timer. Priority breaks ties when several tasks come due at once (higher
// runs first). Run MUST be idempotent: a skipped or back-to-back cycle is
// harmless (the scheduler never queues a second copy of a task while it waits).
type Task struct {
	Name     string
	Period   time.Duration
	Priority int // higher runs first among simultaneously-due tasks
	Run      func(ctx context.Context) error
}

// Housekeeper is the trunk's central scheduler for module persistence sweeps: a
// single worker drains a PRIORITY QUEUE of due tasks (highest Priority first), so
// periodic DB/disk maintenance runs in one coordinated place rather than each
// module pounding shared storage from its own timer. Serial execution gives the
// vision's resource-class staggering for free in this first cut; per-resource
// concurrency + pressure-escalation (architecture-vision §5) land when there are
// enough tasks to need them.
//
// Provided into the registry before modules Start; a module resolves it with
// TryRequire[Housekeeper] (OPTIONAL — not a declared Need, so it stays out of the
// topo-sort and the arch graph) and Registers its tasks at Start.
type Housekeeper interface {
	Register(t Task)
}

// scheduled is a Task plus its next-due time (the scheduler's bookkeeping).
type scheduled struct {
	Task
	nextDue time.Time
	fails   int // consecutive Run failures (error or panic); drives backoff, reset on success
}

type houseKeeper struct {
	mu      sync.Mutex
	tasks   []*scheduled
	started bool
	stopped bool // set in stop(); a Register after this is a wiring bug, not silently dropped
	ctx     context.Context
	cancel  context.CancelFunc
	wake    chan struct{}
	done    chan struct{}
}

func newHousekeeper() *houseKeeper {
	return &houseKeeper{wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// Register adds a task. Safe before or after start; a post-start Register wakes
// the scheduler to pick it up. A task with no Run or a non-positive Period is
// rejected (a wiring mistake) rather than silently scheduled.
func (h *houseKeeper) Register(t Task) {
	if t.Run == nil || t.Period <= 0 {
		slog.Warn("housekeeper: ignoring task with nil Run or non-positive Period", "task", t.Name)
		return
	}
	h.mu.Lock()
	if h.stopped {
		// The scheduler is torn down; appending here would queue a task onto a dead
		// worker that never runs it. A Register after stop is a shutdown-ordering bug.
		h.mu.Unlock()
		slog.Warn("housekeeper: ignoring Register after stop — scheduler is shut down", "task", t.Name)
		return
	}
	st := &scheduled{Task: t}
	if h.started {
		st.nextDue = time.Now().Add(firstDue(t.Period))
	}
	h.tasks = append(h.tasks, st)
	h.mu.Unlock()
	h.signal()
}

// signal nudges the scheduler loop without blocking (the wake chan is buffered 1).
func (h *houseKeeper) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// start launches the single scheduler worker. Called once by the trunk AFTER all
// modules have started (so every task is registered). Seeds each task's first run
// at min(Period, firstRunDelay) — see firstRunDelay for why long-period tasks must
// not wait a full Period on an unpersisted clock.
func (h *houseKeeper) start() {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return
	}
	h.started = true
	now := time.Now()
	for _, st := range h.tasks {
		if st.nextDue.IsZero() {
			st.nextDue = now.Add(firstDue(st.Period))
		}
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.mu.Unlock()
	go h.loop()
}

// stop cancels the worker and waits for it to drain the in-flight task.
func (h *houseKeeper) stop() {
	h.mu.Lock()
	started, cancel := h.started, h.cancel
	h.stopped = true // set under the same lock Register checks, so a racing Register loses
	h.mu.Unlock()
	if !started || cancel == nil {
		return
	}
	cancel()
	<-h.done
}

func (h *houseKeeper) loop() {
	defer close(h.done)
	for {
		wait, due := h.collect()
		if len(due) > 0 {
			// Priority queue over the due batch: highest Priority first, name as a
			// deterministic tiebreak. One worker runs them serially — so two
			// storage-heavy sweeps never hammer the DB/disk at the same instant.
			sort.SliceStable(due, func(i, j int) bool {
				if due[i].Priority != due[j].Priority {
					return due[i].Priority > due[j].Priority
				}
				return due[i].Name < due[j].Name
			})
			for _, st := range due {
				if h.ctx.Err() != nil {
					return
				}
				h.runOnce(st)
			}
			continue // re-collect immediately (more may have come due meanwhile)
		}
		timer := time.NewTimer(wait)
		select {
		case <-h.ctx.Done():
			timer.Stop()
			return
		case <-h.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// collect returns the tasks due now (advancing their nextDue so the same task is
// never queued twice while it waits) plus, when none are due, how long to sleep
// until the soonest.
func (h *houseKeeper) collect() (wait time.Duration, due []*scheduled) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	wait = idleCap
	for _, st := range h.tasks {
		if !st.nextDue.After(now) {
			due = append(due, st)
			st.nextDue = now.Add(st.Period)
		} else if d := time.Until(st.nextDue); d < wait {
			wait = d
		}
	}
	return wait, due
}

// runOnce runs one task, isolating a panic and logging a returned error — a
// misbehaving sweep must not take down the scheduler or its siblings. A failed
// run (error or panic) pushes the task's nextDue out exponentially (see
// failBackoffCap); a successful run resets it to its normal Period.
func (h *houseKeeper) runOnce(st *scheduled) {
	failed := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				failed = true
				slog.Error("housekeeper: task panicked", "task", st.Name, "panic", r)
			}
		}()
		if err := st.Run(h.ctx); err != nil {
			// A shutdown (stop() cancels h.ctx) makes a well-behaved in-flight Run
			// return context.Canceled — that's an orderly stop, not a task failure, so
			// don't count it or back it off. h.ctx is only cancelled by stop(), so this
			// can't mask a task's own internal cancellation.
			if errors.Is(err, context.Canceled) && h.ctx.Err() != nil {
				return
			}
			failed = true
			slog.Warn("housekeeper: task failed", "task", st.Name, "err", err)
		}
	}()

	h.mu.Lock()
	defer h.mu.Unlock()
	if !failed {
		st.fails = 0
		return
	}
	st.fails++
	// Doubling loop instead of a shift: bounded by the cap, so a long failure
	// streak can never overflow the Duration.
	limit := failBackoffCap
	if st.Period > limit {
		limit = st.Period // never schedule sooner than the task's own Period
	}
	backoff := st.Period
	for i := 0; i < st.fails && backoff < limit; i++ {
		backoff *= 2
	}
	if backoff > limit {
		backoff = limit
	}
	if backoff > st.Period {
		st.nextDue = time.Now().Add(backoff)
		slog.Warn("housekeeper: backing off failing task", "task", st.Name, "fails", st.fails, "next_in", backoff)
	}
}
