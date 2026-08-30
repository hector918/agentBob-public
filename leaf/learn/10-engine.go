package learn

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// engine is the contract.LearnRegistry + the sweep driver. Sources are registered
// at startup by the owning modules; runCycle walks them and distills each target.
type engine struct {
	model func() contract.ModelPool

	// minFail / backstop tune the failure gate (docs/learn.md §5): a target distills
	// only once it has accumulated minFail failures, OR has ≥1 failure older than
	// backstop (so a thin-but-stale batch still learns eventually). 0 → package
	// defaults (defaultMinFailures / defaultBackstop). Fields, not consts, so tests
	// can lower the threshold.
	minFail  int
	backstop time.Duration

	mu      sync.Mutex
	sources []contract.LearnSource
	lastRun map[string]time.Time // per-target key → last distill time (Traces "since")
}

// AddSource registers a target source (contract.LearnRegistry).
func (e *engine) AddSource(s contract.LearnSource) {
	if s == nil {
		return
	}
	e.mu.Lock()
	e.sources = append(e.sources, s)
	e.mu.Unlock()
}

// runCycle is the housekeeping sweep: enumerate every source's current targets and
// distill each. Fresh enumeration per cycle covers dynamic targets (per-company/
// role). Idempotent — a skipped or back-to-back cycle is harmless.
func (e *engine) runCycle(ctx context.Context) error {
	e.mu.Lock()
	srcs := append([]contract.LearnSource(nil), e.sources...)
	e.mu.Unlock()

	pool := e.model()
	if pool == nil {
		return nil // no distiller wired — nothing to do this cycle
	}
	live := map[string]bool{}
	for _, src := range srcs {
		for _, l := range src.Targets(ctx) {
			if ctx.Err() != nil {
				return ctx.Err() // partial cycle — skip prune (live set is incomplete)
			}
			live[l.Key()] = true
			e.distill(ctx, pool, l)
		}
	}
	e.pruneWatermarks(live)
	return nil
}

// pruneWatermarks drops lastRun entries for targets absent this cycle. A dynamic
// target set — agora's member:<co>:<role> keys churn as roles/companies come and go
// (agora self-prunes the disappearing roles) — would otherwise grow the map without
// bound. Called after a COMPLETE cycle only, so a not-yet-seen target is never pruned.
//
// Not free of duplicate spend: consumed rows are deleted only on the NEXT since() read,
// so they outlive their watermark by one cycle. Pruning a watermark here — or losing
// lastRun entirely on restart, since it's memory-only — makes a reappearing target
// restart from the zero watermark and re-distill an already-consumed batch. Accepted:
// the integral rewrite is idempotent-ish (same failures → similar text), so the cost is
// duplicate model spend on a stale batch, not corruption.
func (e *engine) pruneWatermarks(live map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k := range e.lastRun {
		if !live[k] {
			delete(e.lastRun, k)
		}
	}
}

// distill improves one target from the FAILURES observed since its last distill
// (docs/learn.md §4: failure-driven). The integral-rewrite guard regenerates the
// WHOLE learned text from (current text + recent failures), capped — no replay
// gate. A later slice adds, right here, a `if sc, ok := l.(contract.Scorer); ok {
// … replay-gated accept … }` branch for the targets that opt into it; the rewrite
// path below stays the default.
func (e *engine) distill(ctx context.Context, pool contract.ModelPool, l contract.Learnable) {
	key := l.Key()
	since := e.since(key)

	traces, err := l.Traces(ctx, since)
	if err != nil {
		slog.Warn("learn: read traces failed", "target", key, "err", err)
		return
	}
	if len(traces) == 0 {
		return // nothing new since last cycle
	}
	// Advance the watermark to the NEWEST consumed trace, NOT time.Now() at
	// completion: the model call below blocks for seconds, and any tool call
	// recorded during it would have At ≤ a wall-clock watermark yet never be in
	// this batch — stamping max(At) instead lets those in-flight rows survive to
	// the next cycle (no silent loss, no double-processing of consumed rows).
	watermark := traces[0].At
	for _, t := range traces {
		if t.At.After(watermark) {
			watermark = t.At
		}
	}

	// Failure-driven gate (docs/learn.md §4/§5): learn only from mistakes, and only
	// once enough have accumulated. Successes carry no corrective signal.
	failures := failuresOnly(traces)
	if !e.failureGateOpen(failures) {
		// Not enough failure signal yet. HOLD the watermark so pending failures keep
		// accumulating across cycles; but when there were ZERO failures, advance past
		// the consumed successes so we don't rescan them every cycle.
		if len(failures) == 0 {
			e.markRun(key, watermark)
		}
		return
	}

	cur, err := l.Current(ctx)
	if err != nil {
		slog.Warn("learn: read current text failed", "target", key, "err", err)
		return
	}

	resp, err := pool.Chat(ctx, distillRequest(), distillPrompt(key, cur, failures))
	if err != nil {
		// No distiller model (e.g. no entry tagged "learn") or a transient backend
		// error — leave the target untouched and retry next cycle (since unchanged).
		slog.Warn("learn: distill model call failed", "target", key, "err", err)
		return
	}

	revised := capText(strings.TrimSpace(resp.Content), maxLearnedChars)
	// An EMPTY distiller response (Content=="" with nil err — a real outcome of the stream
	// path) must NOT erase accumulated text. Only Apply("") when cur was already empty;
	// otherwise consume the failures (markRun) WITHOUT the destructive overwrite.
	if revised == "" && strings.TrimSpace(cur) != "" {
		e.markRun(key, watermark)
		return
	}
	// Compare against the CAPPED current text: cur may already exceed the cap (an
	// older/oversized value) and `revised` is always capped, so comparing the capped
	// output to the UNcapped cur would never match — forcing a needless Apply +
	// truncation every cycle. Capping both sides makes a genuine no-change a no-op.
	if revised == capText(strings.TrimSpace(cur), maxLearnedChars) {
		e.markRun(key, watermark) // consumed these failures, text unchanged
		return
	}
	if err := l.Apply(ctx, revised); err != nil {
		// CONSUME the failures even though Apply failed (advance the watermark). A
		// PERSISTENT Apply fault (full disk / read-only / bad perms under the insights
		// dir) would otherwise hold the watermark forever, re-reading the same failure
		// batch and re-running the PAID distiller model every cycle with zero progress.
		// We accept losing this one signal batch; the next real failure re-arms the gate.
		slog.Warn("learn: apply revised text failed (consuming batch to avoid a paid re-distill loop)", "target", key, "err", err)
		e.markRun(key, watermark)
		return
	}
	e.markRun(key, watermark)
	slog.Info("learn: target updated", "target", key, "failures", len(failures), "chars", len(revised))
}

// failuresOnly keeps the not-OK rollouts — the mistakes the failure-driven engine
// learns from. Order (oldest-first) is preserved.
func failuresOnly(traces []contract.Rollout) []contract.Rollout {
	var out []contract.Rollout
	for _, t := range traces {
		if !t.OK {
			out = append(out, t)
		}
	}
	return out
}

// failureGateOpen decides whether a target has enough failure signal to distill:
// minFail failures accumulated, OR ≥1 failure that has waited longer than backstop
// (so a thin-but-stale batch still learns). 0 fields → package defaults.
func (e *engine) failureGateOpen(failures []contract.Rollout) bool {
	if len(failures) == 0 {
		return false
	}
	n := e.minFail
	if n <= 0 {
		n = defaultMinFailures
	}
	if len(failures) >= n {
		return true
	}
	bs := e.backstop
	if bs <= 0 {
		bs = defaultBackstop
	}
	// Oldest failure waited out the backstop. The Traces contract guarantees
	// oldest-first order (contract/95-learn.go), so the first entry IS the oldest.
	// Guard a zero/unset At: without it a target that forgets to stamp At (a future
	// skill/member source) would read as infinitely old and distill on its very first
	// failure, defeating the count gate.
	oldest := failures[0].At
	return !oldest.IsZero() && clock.Now().Sub(oldest) >= bs
}

// since returns the watermark to read traces from (zero on first sight → all the
// ring holds). markRun advances it to the newest consumed trace's timestamp. Both
// take e.mu: the sweep worker writes lastRun while the webui panel's snapshot()
// reads it on a separate goroutine — without the lock that is a concurrent map
// read+write (a runtime fatal error, not a recoverable panic). distill calls these
// WITHOUT holding e.mu, so the lock is non-reentrant-safe.
func (e *engine) since(key string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastRun[key]
}

func (e *engine) markRun(key string, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastRun[key] = at
}

var _ contract.LearnRegistry = (*engine)(nil)
