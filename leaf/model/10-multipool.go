package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// Entry is one input row for NewMultiPool — the model package's own model-pool
// input type, so the package depends on nothing but core + providers. The
// caller (pipeline-startup) translates models.yaml into a []Entry.
type Entry struct {
	Name          string
	Provider      string
	Model         string
	BaseURL       string
	APIKeyEnv     string
	Kind          string
	Priority      int
	ContextWindow int
	Concurrency   int
	// MaxOutputTokens is this entry's per-response output cap, applied on
	// both the streaming and block paths (resolution chain in
	// config.ModelsConfig.EffectiveOutputCap: entry.MaxOutputTokens →
	// defaults.MaxOutputTokens → 0).
	MaxOutputTokens int
	ExtraBody       map[string]any
	Tags            []string
}

// Liveness — passive circuit breaker constants. First take, revisit with
// real-world data (§0.10).
const (
	livenessWindow        = 60 * time.Second // sliding window for fail counting
	livenessFailThreshold = 3                // fails inside the window → mark dead
	livenessCooldown      = 5 * time.Minute  // initial dead duration
	livenessMaxCooldown   = 30 * time.Minute // exponential-backoff ceiling
	// consecutiveFailThreshold marks an entry dead after this many errors
	// in a row with no success between — independent of livenessWindow.
	// The window rule needs enough traffic to land N fails inside 60s; on
	// a low-traffic deployment a backend that fails every call (errors
	// minutes apart) never trips it. This rule catches that case.
	consecutiveFailThreshold = 2
)

// Heartbeat — on-demand active liveness probe. Runs ONLY while ≥1 entry is
// dead; one tick every heartbeatInterval calls Ping on each dead entry. A
// per-Ping timeout keeps a stalled backend from holding up the tick.
const (
	heartbeatInterval    = 10 * time.Second
	heartbeatPingTimeout = 5 * time.Second
)

// configCheckInterval throttles the models.yaml mtime stat done by
// maybeReload — at most one stat per request burst window.
const configCheckInterval = 5 * time.Second

// priorityMax is the high clamp for Entry.Priority — values above it are
// clamped at pool-build time.
const priorityMax = 10000

// minEnabledPriority is the low end of the ENABLED priority range. A value
// strictly below it (i.e. <= -10001) is NOT clamped — it survives as the
// DISABLED marker: pick() skips such entries and Snapshot reports them as
// "disabled". buildEntry also skips the startup probe for them.
const minEnabledPriority = -10000

// entryRow is one MultiPool entry: a Chatter, its declared metadata (name /
// tags / context window), and a concurrency semaphore. Liveness state
// (errTimes / deadUntil / consecutiveDeadEvents / paused) is in-memory and
// intentionally not persisted — it resets on restart.
type entryRow struct {
	info    contract.ModelInfo
	chatter contract.Chatter
	sem     chan struct{} // nil → unlimited concurrency

	// baseURL / apiKey are the resolved backend connection facts (env expanded,
	// preset applied) — kept on the row for the side channels that talk to the
	// same server outside the Chatter (providers.CountTokens → POST /tokenize).
	// Set at build time, immutable, read lock-free.
	baseURL string
	apiKey  string

	mu                    sync.Mutex
	inFlight              int
	errTimes              []time.Time // sliding window of recent backend errors
	consecutiveFails      int         // errors in a row, no success between → consecutiveFailThreshold marks dead
	deadUntil             time.Time   // if non-zero and in the future, entry is dead
	consecutiveDeadEvents int         // exponent for cooldown backoff
	lastErr               string      // text of the most recent counted backend error — explains why the entry is cooling / tentative (surfaced read-only in the admin webui). Set in RecordError; not cleared on recovery, so snapshotInfo only surfaces it for a NON-live entry (a recovered entry's stale error stays hidden).
	paused                bool        // admin pause; never auto-recovers
	// tentative marks an entry the heartbeat re-admitted after a
	// successful Ping of a dead backend. A tentative entry is pick-
	// eligible again, but one real error while tentative re-deads it
	// immediately (a cheap Ping succeeding ≠ the backend can serve
	// completions). recordSuccess clears it — a real call worked.
	tentative bool

	// nextProbeAt gates the heartbeat's Ping for THIS entry: one that just
	// BURNED a tentative recovery (Ping said "reachable", the next real call
	// failed) goes unprobed until its own cooldown elapses.
	//
	// Without it the probe loop is pace-blind — it re-Pings every dead entry
	// every heartbeatInterval (10s) no matter how many tentative recoveries
	// that entry has already wasted, so `tentative` (which IsEligible short-
	// circuits ahead of deadUntil) re-admits a permanently-broken backend
	// every tick and the exponential cooldown NEVER actually elapses.
	//: an nginx 502 — which Ping deliberately counts as reachable
	// (see providers.Ping's B2 note: gating on 2xx would strand a backend
	// whose /models 4xx's) — cycled dead→tentative→dead 8 times in ~6 minutes,
	// consecutiveDeadEvents climbing to 8 while the nominal 30m cooldown held
	// for exactly 0 of them. Every cycle also burned a real request on the
	// dead backend, which is what paged the admin over and over.
	//
	// Zero = probe freely, so a FIRST outage still recovers as fast as before;
	// only a recovery that PROVED false buys silence, on the cooldown curve
	// the entry is already keeping (nextCooldown/consecutiveDeadEvents — no
	// second backoff ladder).
	nextProbeAt time.Time

	// Process-lifetime usage counters, guarded by mu. Reset on restart;
	// these feed the live Snapshot. The hourly accumulator below is the
	// SEPARATE persisted history — both are kept.
	totalCalls        int64
	totalErrors       int64
	totalInputTokens  int64
	totalOutputTokens int64

	// hourly accumulates usage per wall-clock hour (key = hour_start
	// unix seconds), guarded by mu. FlushUsage drains completed hours
	// into the store and removes them; the in-progress hour stays.
	hourly map[int64]*hourBucket
}

// hourBucket is one entry's usage for a single wall-clock hour, pending
// flush to the store.
type hourBucket struct {
	calls  int64
	errors int64
	input  int64
	output int64
}

// ModelUsageStore is the narrow store surface the operational ModelUsageRecorder
// drains through — the additive per-hour UPSERT into model_usage (per-backend
// telemetry for /model stats). The model module satisfies it with its own
// pg-backed impl (85-usagestore.go). This is the OPS ledger, NOT the per-user
// consumption ledger (contract.ConsumptionReporter).
type ModelUsageStore interface {
	AddModelUsage(ctx context.Context, entryName string, hourStart, calls, errs, inputTokens, outputTokens int64) error
}

// poolState is the swappable set of entries. MultiPool holds it behind an
// atomic.Pointer so a models.yaml reload can replace the whole table in one
// store: in-flight calls keep referencing the old *entryRow values (Go keeps
// them alive), new requests Load() the fresh state.
type poolState struct {
	entries []*entryRow
	byName  map[string]*entryRow
	// fallback is the tag-fallback ruleset (defaults.fallback), carried in
	// the swappable state so a models.yaml reload updates it atomically with
	// the entries pick() matches against. nil → no tag-fallback. See
	// 30-pick.go::fallbackChain.
	fallback []FallbackRule
}

// MultiPool routes model requests across multiple entries from $BOB_HOME/
// models.yaml. Selection is **strict tag-match**: an entry qualifies only if
// it has every tag in req.Requires; ties among qualifiers are broken by how
// many req.Prefer tags they hit. req.PinnedEntry forces a specific entry by
// Name.
type MultiPool struct {
	// state is the live entry table — swapped atomically by Reload.
	// Every method that reads entries does one state.Load().
	state atomic.Pointer[poolState]

	// usage drains per-entry hourly buckets to the configured ModelUsageStore.
	// Always non-nil after NewMultiPool; the underlying store may be nil
	// (no-store / test paths) — Flush then no-ops.
	usage *ModelUsageRecorder

	// heartbeat is the on-demand liveness probe goroutine + state.
	// Always non-nil after NewMultiPool. Close() forwards.
	heartbeat *HeartbeatRunner

	// adminLine lazily resolves the system-level admin-summon funnel (the pool is
	// built and may Start BEFORE the adminline module, so this is a getter, not a
	// value). nil getter / nil result → the all-dead alert is a no-op (tests).
	adminLine func() contract.AdminLine

	// allDeadMu guards allDeadKinds — the PER-KIND latch set for the "model
	// pool fully unavailable" admin alert. A kind's alert fires ONCE on its
	// usable→0-usable transition and is re-armed when that kind recovers,
	// independently of other kinds (a single bool would swallow a second
	// kind's outage that overlaps a first kind's — B7-followup). Without the
	// latch a pool that fails every request would page on every call.
	// exhaustedKinds (same mutex) is the SECOND, independent latch: a kind
	// whose every candidate errored inside one request, i.e. it had no
	// fallback left. Kept separate from allDeadKinds on purpose —
	// checkPoolLiveness recomputes that map wholesale from cooling state and
	// deletes anything it does not currently see as dead, which would wipe an
	// exhaustion latch (the entries behind it are typically NOT marked dead)
	// and log a bogus recovery.
	allDeadMu      sync.Mutex
	allDeadKinds   map[string]bool
	exhaustedKinds map[string]bool

	// queueMu guards the per-kind WAIT QUEUE accounting: queueDepth counts the
	// requests currently blocked waiting for a concurrency slot of that kind
	// (never the ones being served — those hold a slot), and queueFullKinds is
	// the per-kind latch for the "queue full" admin page so a sustained flood
	// pages once rather than per rejected request. Cleared by clearQueueFull
	// when a call of that kind completes.
	queueMu        sync.Mutex
	queueDepth     map[string]int
	queueFullKinds map[string]bool

	// reload owns the models.yaml re-read + mtime-watch + atomic table
	// swap. Always non-nil after NewMultiPool; the underlying closure
	// may be nil (Reload / MaybeReload then no-op).
	reload *ConfigReloader

	// consumption lazily resolves the optional per-USER billing reporter
	// (contract.ConsumptionReporter, implemented by accounts). Set
	// post-construction: accounts may start after the model module, so this is a
	// getter, not a value. nil getter / nil result → spend is not booked. This is
	// the per-user BILLING ledger, distinct from the pool's own per-backend
	// operational ModelUsage above.
	consumption func() contract.ConsumptionReporter

	// tokCache is the exact-token-count cache behind CountTokens
	// (42-count-tokens.go), keyed by (entry, model, content) hash so a count
	// can never be served across tokenizers. Lazily allocated under its mutex.
	tokCacheMu sync.Mutex
	tokCache   map[uint64]int

	// affinity is the soft prompt-cache affinity ledger: AffinityKey → which
	// entry last served it (31-affinity.go). In-memory and process-lifetime by
	// design — it mirrors the backends' KV caches, which don't survive a restart
	// either. Lazily allocated under its mutex; reload doesn't touch it (a
	// renamed/removed entry simply stops matching any candidate).
	affinityMu sync.Mutex
	affinity   map[string]affinityRec
}

// WindowFor returns the declared context window of the entry a request like
// req ranks FIRST on configuration alone (pickStatic: Prefer → priority →
// Name; health- and load-BLIND). This is the sizing half of the window-blind
// picker split (hector): routing never looks at windows, so the
// sizing question is simply "how big is the winner's mouth" — the turn
// compacts its replay and sizes its compression segments to exactly that.
// Deliberately static: a cooling blip or in-flight tie must not flap the
// budget (a smaller transient reading would proactively and IRREVERSIBLY
// over-compact healthy history). Serving may land elsewhere mid-failover;
// the context-400 → compact-and-retry net covers that direction. 0 = nothing
// configured for the request (caller skips compaction / uses its default).
func (p *MultiPool) WindowFor(req contract.ModelRequest) int {
	row := p.pickStatic(req, nil)
	if row == nil {
		return 0
	}
	return row.info.ContextWindow
}

// Fallback values when YAML doesn't set them and the probe yields nothing.
// context: 128K is the modern default when probe + yaml both went silent;
// a model that turns out smaller and overflows simply fails over to another
// entry (see failover in 40-chat.go).
// concurrency: per the project decision — moderate parallelism by default;
// users override per-entry if their server can't handle it.
const (
	fallbackContextWindow = 131072 // 128K
	fallbackConcurrency   = 2
)

// NewMultiPool builds a pool from a []Entry. Each entry gets a Chatter
// (OpenAI-compatible client over base_url + api_key_env). For every enabled
// entry whose context_window or concurrency is unset, we probe the backend
// (provider-specific endpoint, 2 s timeout); YAML always wins when set, so
// operators can force a smaller context (e.g. the real per-slot capacity of a
// llama-cpp server, which /props does not report) even if the server reports a
// larger one. Probe failures fall through to the constants above and are noted
// via ContextSource / ConcurrencySource on ModelInfo.
//
// `fallback` is the tag-fallback ruleset (defaults.fallback); nil/empty
// disables it. `reload` is the injected re-load closure (re-read + re-translate
// the config, including the fallback rules); nil disables Reload. `configPath`
// is the file maybeReload stats for mtime changes; empty disables the mtime watch.
func NewMultiPool(entries []Entry, fallback []FallbackRule, reload func() ([]Entry, []FallbackRule, error), configPath string) (*MultiPool, error) {
	// ZERO entries is a legal boot state (models.yaml missing/empty/unparseable):
	// the pool serves errors but its mtime watch lives, so fixing the file
	// hot-loads without a restart — the webui editor's "reload" promise.
	p := &MultiPool{
		usage: NewModelUsageRecorder(),
	}
	// HeartbeatRunner / ConfigReloader both need the pool-level
	// checkPoolLiveness as a callback — wired post-struct since the
	// closures capture `p`. ConfigReloader's NotifyAsyncError closes
	// over p.adminLine, which is set later via SetAdminLineResolver; the
	// closure reads it at call time, so the lazy wiring is fine.
	p.heartbeat = NewHeartbeatRunner(&p.state, func() { p.checkPoolLiveness() })
	p.reload = NewConfigReloader(&p.state, reload, configPath, ReloaderHooks{
		RetireOld: func(old *poolState) { p.usage.Retire(old.entries) },
		PostSwap: func() {
			p.checkPoolLiveness()
			// A preserve-liveness reload can carry a dead row into the
			// fresh table while the heartbeat goroutine is exiting (its
			// no-dead re-check ran against the OLD table) — re-ensure the
			// probe whenever the swapped-in table still has a dead entry
			// (B28).
			if p.heartbeat.hasDeadEntry() {
				p.heartbeat.Ensure()
			}
		},
		NotifyAsyncError: func(err error) {
			notifyAdminAsync(p.admin(), contract.AdminError, "model-pool",
				"models.yaml hot-reload failed — keeping the previous pool; the edited config is NOT live: %v", err)
		},
	})
	st := &poolState{byName: map[string]*entryRow{}, fallback: fallback}
	for i, e := range entries {
		row, err := buildEntry(i, e, true)
		if err != nil {
			return nil, err
		}
		st.entries = append(st.entries, row)
		st.byName[e.Name] = row
	}
	p.state.Store(st)
	// The boot table built — the startup config is the one in service. An
	// empty pool (unparseable / empty models.yaml, see buildPool) skips this
	// and keeps a zero stamp: it is serving nothing.
	if len(entries) > 0 {
		p.reload.noteBootConfigServed()
	}
	return p, nil
}

// Close implements contract.ModelPool — stops the on-demand heartbeat
// goroutine (if running). Idempotent.
func (p *MultiPool) Close() {
	if p == nil || p.heartbeat == nil {
		return
	}
	p.heartbeat.Close()
}

// IsEligible reports whether the entry is pick-eligible right now: not
// paused, and either live, tentative (heartbeat-readmitted), or
// cooled-down (deadUntil elapsed). It also auto-recovers an entry whose
// deadUntil has passed — that recovery is the one place this mutates the
// row, so the caller MUST NOT hold e.mu.
//
// Cooldown re-admission is a passive backstop (no Ping evidence that the
// backend recovered), so it deliberately keeps consecutiveFails intact:
// the entry comes back on probation, and the FIRST error after re-
// admission re-deads it immediately (consecutiveFails is already at the
// threshold) with the next backoff step — same one-strike posture as a
// failed tentative recovery. A genuinely-recovered backend confirms via
// RecordSuccess, which clears consecutiveFails. (The heartbeat's tentative
// path is the primary, Ping-evidenced recovery route.)
func (e *entryRow) IsEligible() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.paused {
		return false
	}
	if e.tentative {
		return true
	}
	if e.deadUntil.IsZero() {
		return true
	}
	if time.Now().After(e.deadUntil) {
		e.deadUntil = time.Time{}
		// consecutiveFails intentionally NOT reset here — see the doc above:
		// passive cooldown re-admission is probationary (one-strike).
		slog.Info("model entry recovered from cooldown (probationary — one error re-deads it)", "name", e.info.Name)
		return true
	}
	return false
}

// recordError logs a backend error for the entry. The entry is marked dead
// when EITHER livenessFailThreshold errors land inside livenessWindow OR
// consecutiveFailThreshold errors occur in a row with no success between
// (the latter catches a backend that fails every call on a low-traffic
// deployment, where errors are minutes apart and never fill the window).
// The dead cooldown is exponentially backed off (capped at
// livenessMaxCooldown). Cancellation errors don't count — those are
// user-initiated, not the backend's fault.
//
// Tentative path: if the entry is `tentative` (heartbeat re-admitted it after
// a successful Ping), one real error means the tentative recovery failed — the
// entry is re-deaded immediately, NO 3-in-window threshold. A cheap Ping
// succeeding does not prove the backend can serve completions.
//
// Returns newlyDead=true if this call freshly marked the entry dead — the
// caller (MultiPool.recordError thin wrapper) uses that to (a) start the
// on-demand heartbeat if it isn't already running, and (b) recompute the
// pool-level all-dead admin alert.
func (e *entryRow) RecordError(err error) (newlyDead bool) {
	if err == nil {
		return false
	}
	// (#all-models-tentative): tool-args-truncated is a
	// PROMPT-level failure — the conversation produced output that
	// blew the model's per-response token budget mid tool-call JSON.
	// Every candidate sees the same conversation history + same fine-
	// tune behaviour, so a single such cascade would otherwise bump
	// every candidate's consecutiveFails and drag the whole pool dead
	// for 5 min of cooldown + tentative-recovery flap. Same posture as
	// "ctx cancel / deadline exceeded" filters below: not the entry's
	// fault, don't count. See IsToolArgsTruncatedErr's doc for the
	// false-positive / false-negative trade-offs.
	if IsToolArgsTruncatedErr(err) {
		return false
	}
	// B3 (narrowed): a CONTENT-level 4xx (400/413/422
	// — context overflow, bad params, payload too large) is a request fault
	// every candidate would hit identically (no ContextWindow pre-routing
	// exists), so counting it toward the entry's cooling breaker can falsely
	// kill a healthy backend for 5 min and push even small requests to
	// fallback. Entry-HEALTH 4xx (401/403/404/405/429) and 5xx DO count —
	// those are this entry's own problem and SHOULD cool it. See
	// isPromptLevel4xx.
	if isPromptLevel4xx(err) {
		return false
	}
	// B1 (supersedes D2 round 3's transport-first ordering):
	// caller-initiated termination is filtered BEFORE counting the error.
	// http.Client.Do wraps every mid-roundtrip failure in *url.Error —
	// including the caller's own ctx being cancelled or its deadline
	// elapsing — so a transport-gated filter would miss the common shape
	// (a user /stop or a turn-budget deadline firing while the request is
	// in flight) and cool a healthy entry. context.Canceled is always the
	// caller's doing (no pool sentinel wraps it). DeadlineExceeded counts
	// only when it rides one of the pool's own sentinels: errStreamStall
	// (D2 — the pool actively timed the attempt out) or errHardTimeout
	// (the provider's per-call hard cap elapsed while the caller's ctx was
	// still alive — a wedged backend; recordError classifies, since only
	// the attempt's call site still holds the caller ctx). Kernel
	// ETIMEDOUT does not satisfy errors.Is(_, context.DeadlineExceeded),
	// so genuine TCP-level timeouts still count.
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, errStreamStall) && !errors.Is(err, errHardTimeout) {
		return false
	}
	e.mu.Lock()
	e.totalErrors++
	// Retain the most recent counted error so the admin webui can explain why
	// this entry is cooling / tentative. Capped — a backend can return a long
	// body, and this is a glance-level diagnostic, not a log.
	e.lastErr = truncateErr(err.Error())
	addUsage(e, clock.Now(), 0, 1, 0, 0) // persisted hour bucket — DB-calibrated
	now := time.Now()                    // operational liveness window — local
	cutoff := now.Add(-livenessWindow)
	pruned := e.errTimes[:0]
	for _, t := range e.errTimes {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	pruned = append(pruned, now)
	e.errTimes = pruned
	e.consecutiveFails++

	switch {
	case e.tentative:
		// Failed tentative recovery — one strike re-deads it.
		cooldown := nextCooldown(e.consecutiveDeadEvents)
		e.deadUntil = now.Add(cooldown)
		// This recovery was false — hold the probe off for the same cooldown
		// so the heartbeat stops re-admitting it every 10s (see nextProbeAt).
		e.nextProbeAt = now.Add(cooldown)
		e.consecutiveDeadEvents++
		e.tentative = false
		newlyDead = true
		slog.Warn("model entry re-marked dead (tentative recovery failed)",
			"name", e.info.Name,
			"cooldown", cooldown,
			"backoff_event", e.consecutiveDeadEvents,
			"err", err,
		)
	default:
		// Unified threshold for every entry kind.
		if (len(pruned) >= livenessFailThreshold || e.consecutiveFails >= consecutiveFailThreshold) &&
			(e.deadUntil.IsZero() || e.deadUntil.Before(now)) {
			cooldown := nextCooldown(e.consecutiveDeadEvents)
			e.deadUntil = now.Add(cooldown)
			e.consecutiveDeadEvents++
			newlyDead = true
			slog.Warn("model entry marked dead",
				"name", e.info.Name,
				"fails_in_window", len(pruned),
				"consecutive_fails", e.consecutiveFails,
				"cooldown", cooldown,
				"backoff_event", e.consecutiveDeadEvents,
				"err", err,
			)
		}
	}
	e.mu.Unlock()
	return newlyDead
}

// lastErrMaxLen caps the retained last-error text (a glance-level diagnostic).
const lastErrMaxLen = 600

// truncateErr bounds an error string for the retained lastErr field so a
// pathological backend body can't grow per-entry state without limit. Rune-aware
// so a multi-byte (e.g. CJK) error never gets cut mid-rune into invalid bytes.
func truncateErr(s string) string {
	if len(s) <= lastErrMaxLen {
		return s
	}
	r := []rune(s)
	if len(r) <= lastErrMaxLen {
		return s // long in bytes but within the rune budget — keep whole
	}
	return string(r[:lastErrMaxLen]) + "…"
}

// recordError is the thin pool-side wrapper: forwards to the row's method
// and (on newlyDead) starts the on-demand heartbeat + recomputes the
// pool-level all-dead admin alert. Keeps Chat / ChatStreamWatch callers
// blissfully unaware of either follow-up.
//
// ctx MUST be the CALLER's ctx for the attempt (the one Chat /
// ChatStreamWatch received), never a provider-derived one. It is the only
// signal that disambiguates a DeadlineExceeded's origin: the providers cap
// every call with their own hard timeout (streamHardTimeout), and
// http.Client.Do wraps that expiry in the same *url.Error shape as a
// caller deadline firing mid-roundtrip. A caller ctx that is still alive
// means the elapsed deadline was the provider's own cap — the backend
// accepted the request and never completed it, which is the entry's fault
// — so the error is stamped with errHardTimeout to pass RecordError's
// caller-deadline filter and count toward cooling. A dead caller ctx
// leaves the error unstamped and the filter drops it as caller-initiated.
// The classification must happen before any attempt-local cancel kills
// the derived ctx — irrelevant here, since ctx is the caller's, which no
// attempt-local cancel touches.
func (p *MultiPool) recordError(ctx context.Context, row *entryRow, err error) {
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		err = fmt.Errorf("%w: %w", errHardTimeout, err)
	}
	if row.RecordError(err) {
		p.heartbeat.Ensure()
		p.checkPoolLiveness()
	}
}

// notifyAdminAsync fires an admin-line Notify on a fresh goroutine with
// a Background ctx, so a slow / blocked deliverer cannot stall the
// caller (D8). nil-safe — an unset adminLine no-ops. We
// snapshot the line pointer into the goroutine's closure so a
// concurrent SetAdminLineResolver swap (rare; setter is called once at boot)
// cannot race the call.
func notifyAdminAsync(line contract.AdminLine, level contract.AdminLevel, origin, format string, args ...any) {
	if line == nil {
		return
	}
	go func() {
		// Panics in the deliverer (nil deref, format misuse, third-party
		// adapter bug) would otherwise crash the whole bob process — the
		// goroutine boundary means the caller's recover can't catch them.
		// Log + swallow so a misbehaving admin sink can't take down the
		// pool (BUG-4 round 2 — symmetric with runHeartbeat).
		defer func() {
			if r := recover(); r != nil {
				slog.Error("notifyAdminAsync: adminLine.Notify panicked",
					"panic", r, "origin", origin)
			}
		}()
		line.Notify(context.Background(), level, origin, format, args...)
	}()
}

// nextCooldown computes the dead-cooldown for the given backoff exponent:
// livenessCooldown doubled per consecutive dead event, capped (and overflow-
// guarded) at livenessMaxCooldown.
func nextCooldown(consecutiveDeadEvents int) time.Duration {
	cooldown := livenessCooldown << consecutiveDeadEvents
	if cooldown > livenessMaxCooldown || cooldown < livenessCooldown {
		cooldown = livenessMaxCooldown
	}
	return cooldown
}

// RecordSuccess clears the consecutive-dead counter on a successful call so
// the next failure starts at the base cooldown again. If the entry was
// `tentative` (heartbeat re-admitted it), a real call succeeding confirms
// the recovery — clear the flag, the entry is fully live again.
func (e *entryRow) RecordSuccess() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.consecutiveDeadEvents = 0
	e.consecutiveFails = 0
	e.nextProbeAt = time.Time{} // a real call worked — no probe debt to serve
	if e.tentative {
		e.tentative = false
		slog.Info("model entry confirmed recovered (tentative call succeeded)", "name", e.info.Name)
	}
}

// recordSuccess is the thin pool-side wrapper kept for hot-path
// readability — Chat / ChatStreamWatch both flow through it.
func (p *MultiPool) recordSuccess(row *entryRow) { row.RecordSuccess() }

// recordChatSuccess is the shared post-success bookkeeping for every chat
// path (chatOne / streamOneWatch): reset the entry's
// liveness/cooling counters (recordSuccess) and fold one successful call's
// usage into the per-entry totals + the hourly bob_model_usage buckets.
// kind labels the cache-visibility log line. Centralised so the three call
// sites can't drift — streamOneWatch silently skipped all of this before
//, starving /model + bob_model_usage of the main tool-round
// traffic and letting healthy backends accumulate consecutiveFails to a
// false death.
func (p *MultiPool) recordChatSuccess(ctx context.Context, row *entryRow, usage contract.Usage, kind string) {
	p.recordSuccess(row)
	// A real call served — re-arm this kind's exhaustion page. Keyed off the
	// ENTRY's kind (row.info.Kind), not the caller-supplied kind label above,
	// which is only a cache-visibility log tag.
	p.clearKindExhausted(row.info.Kind)
	// Same posture for the queue-full page: the kind is draining again.
	p.clearQueueFull(row.info.Kind)
	p.recordChatTokens(ctx, row, usage, kind, 0)
}

// recordChatTokens books ONE call's token cost — the per-backend stats behind
// /model, and the per-user consumption ledger — with no liveness meaning at all.
// errs is what the hour bucket should count for it (0 for a served call, 1 for a
// call that produced tokens and then failed).
//
// Split out of recordChatSuccess for the thinking-only path: an entry
// that burns its whole output cap in the thinking channel really did spend those
// tokens, and it is the ONE failure class guaranteed to have a bill (its own
// trigger condition is "output tokens > 0"). Every other failure path leaves usage
// at zero, which is why booking and liveness could share a function until now.
// Routing it through recordChatSuccess instead would have been wrong the other way
// — that would clear the exhaustion latch and reset the entry's fail counter on a
// call that answered nobody.
func (p *MultiPool) recordChatTokens(ctx context.Context, row *entryRow, usage contract.Usage, kind string, errs int64) {
	row.mu.Lock()
	row.totalCalls++
	row.totalInputTokens += int64(usage.InputTokens)
	row.totalOutputTokens += int64(usage.OutputTokens)
	addUsage(row, clock.Now(), 1, errs, int64(usage.InputTokens), int64(usage.OutputTokens))
	row.mu.Unlock()
	logCacheUsage(row.info.Name, kind, usage)
	// Per-user billing (the consumption ledger, NOT the per-backend ModelUsage
	// stats above): report this call's real API tokens against the consuming
	// user's handle (read from ctx, stamped by the flow), tagged by the KIND of
	// the entry that ACTUALLY served it. Reported per call (not at turn end), so
	// side-LLM calls deep in the turn are attributed for free. row.info.Kind ==
	// req.Kind on the normal tag-matched pick (the picker gates on Kind —
	// pick.go); the pinned path skips that gate, but row.info.Kind still reflects
	// what ran, so attribution stays correct either way.
	if p.consumption != nil {
		if r := p.consumption(); r != nil {
			r.ReportConsumption(ctx, row.info.Kind, int64(usage.InputTokens), int64(usage.OutputTokens))
		}
	}
}

// MarkProbed reconciles entry liveness after a heartbeat Ping. The
// caller (HeartbeatRunner.tick) does NOT hold e.mu — this method
// re-checks state under the lock so a concurrent RecordSuccess /
// Resume / reload between Ping launch and result can't double-update.
//
// ok=true ("tentative recovery"): the dead entry's Ping succeeded —
// re-admit it to pick (clear deadUntil + set tentative).
//
// ok=false ("still unreachable"): re-arm deadUntil to the current
// backoff level so consecutiveDeadEvents stays consistent. A finite
// deadUntil that lapsed during the outage would silently flip a
// down entry to "live" and let the heartbeat stop probing.
//
// Returns (recovered, stillDead). Both false means the re-check found
// the entry no longer eligible for the recovery accounting (paused /
// already tentative / no longer cooling) — caller skips logging.
//
// Replaces the heartbeat's pre-direct-field-write block;
// see docs/model-pool-design.md §3 (entryRow ownership invariant).
func (e *entryRow) MarkProbed(ok bool) (recovered, stillDead bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.paused || e.tentative || e.deadUntil.IsZero() || !e.deadUntil.After(time.Now()) {
		return false, false
	}
	if ok {
		e.tentative = true
		e.deadUntil = time.Time{}
		return true, false
	}
	e.deadUntil = time.Now().Add(nextCooldown(e.consecutiveDeadEvents))
	return false, true
}

// Pause marks the entry as administratively paused (skipped by pick until
// Resume). In-memory only — restart clears it.
func (e *entryRow) Pause() {
	e.mu.Lock()
	e.paused = true
	e.mu.Unlock()
	slog.Info("model entry paused", "name", e.info.Name)
}

// Pause is the slash-facing wrapper: name lookup + dispatch to entryRow.
// Returns an error when name is unknown.
func (p *MultiPool) Pause(name string) error {
	row, err := p.findRow(name)
	if err != nil {
		return err
	}
	row.Pause()
	// Pausing can remove the LAST usable entry — recompute the all-dead
	// latch so the "fully unavailable" alert (whose wording already covers
	// paused) fires, symmetric with Resume's recompute (D17).
	// Without it the pool can sit dark silently: paused entries are never
	// picked, so no recordError ever triggers a later recompute.
	p.checkPoolLiveness()
	return nil
}

// findRow is the shared name-lookup the admin wrappers (Pause / Resume)
// use to turn a slash arg into an
// *entryRow. Uniform error wording so slash output stays consistent.
func (p *MultiPool) findRow(name string) (*entryRow, error) {
	st := p.state.Load()
	if st == nil {
		return nil, fmt.Errorf("no such pool entry: %s", name)
	}
	row, ok := st.byName[name]
	if !ok {
		return nil, fmt.Errorf("no such pool entry: %s", name)
	}
	return row, nil
}

// Resume clears an admin pause and nothing else. The cooling state
// (deadUntil / consecutiveDeadEvents) and tentative flag are left
// intact — they auto-recover via the heartbeat / cooldown expiry without
// admin intervention.
//
// (no return value — name lookup lives in the pool-side wrapper).
func (e *entryRow) Resume() {
	e.mu.Lock()
	e.paused = false
	e.mu.Unlock()
	slog.Info("model entry resumed (pause cleared)", "name", e.info.Name)
}

// Resume is the slash-facing wrapper. After flipping the pause off it
// recomputes the pool-level all-dead latch (that's a pool concern, not a
// row concern — kept here in the wrapper).
func (p *MultiPool) Resume(name string) error {
	row, err := p.findRow(name)
	if err != nil {
		return err
	}
	row.Resume()
	// A resumed entry is usable again — recompute + clear the all-dead
	// latch so a later full outage pages the admin afresh.
	p.checkPoolLiveness()
	// The heartbeat ignores paused entries, so pausing the only dead entry
	// stops its goroutine; a Resume that re-exposes a still-cooling entry
	// must restart the active probe, or recovery degrades to passive
	// cooldown expiry (B28). Ensure is CAS-guarded — a
	// redundant call no-ops.
	if p.heartbeat.hasDeadEntry() {
		p.heartbeat.Ensure()
	}
	return nil
}

// SetModelUsageStore injects the store the per-backend operational ModelUsage
// (model_usage / /model stats) flushes through. Called once at startup after the
// store opens. nil leaves FlushUsage a no-op (buckets accumulate harmlessly in
// memory).
func (p *MultiPool) SetModelUsageStore(s ModelUsageStore) { p.usage.SetStore(s) }

// SetConsumptionReporter injects a lazy resolver for the optional per-USER
// billing reporter (contract.ConsumptionReporter, implemented by accounts). Lazy
// because accounts may start after the model module. This is the consumption
// ledger seam, distinct from SetModelUsageStore (backend ops telemetry).
func (p *MultiPool) SetConsumptionReporter(fn func() contract.ConsumptionReporter) {
	p.consumption = fn
}

// SetAdminLineResolver wires a lazy resolver for the admin-summon funnel so the
// pool can page the admin on the "every entry is dead/paused" transition. Lazy
// because the pool is built before the adminline module — the resolver is invoked
// on first alert, by which time adminline has registered if present. Call once at
// startup.
func (p *MultiPool) SetAdminLineResolver(fn func() contract.AdminLine) {
	if p == nil {
		return
	}
	p.adminLine = fn
}

// admin resolves the admin line nil-safely (unset getter → nil → no-op alert).
func (p *MultiPool) admin() contract.AdminLine {
	if p.adminLine == nil {
		return nil
	}
	return p.adminLine()
}

// allEntriesUnusable reports whether the pool is in the all-dead state — i.e.
// some configured kind has zero usable entries (deadKind != ""). Thin bool view
// over deadKind for callers that only need the yes/no.
func (p *MultiPool) allEntriesUnusable() bool {
	return p.deadKind() != ""
}

// deadKind returns the single headline dead kind (KindLLM first) or "" — kept
// for callers that only want the chat-outage headline. deadKinds is the full set.
func (p *MultiPool) deadKind() string {
	if dk := p.deadKinds(); len(dk) > 0 {
		return dk[0]
	}
	return ""
}

// deadKind scans the pool PER Kind and returns the name of a configured kind
// (one with ≥1 enabled entry) that currently has zero usable entries — the
// all-dead condition, kind-scoped. Kind isolation matters: a request only
// routes to its own Kind (pickReady), so a healthy ocr/asr entry must NOT keep
// the pool "alive" while every llm entry is dead — every chat turn would still
// fail with ErrNoModelAvailable and the admin alert would never fire (B7).
// Returns nil when every configured kind has at least one usable entry. A pool
// with zero entries returns nil (the "no model configured" condition, surfaced
// separately as the empty pool state, not an all-dead transition). KindLLM is
// reported first (a chat outage is the operator's headline), the rest sorted.
// The full set (not just the first) is returned so checkPoolLiveness can latch
// each kind's outage independently. Cheap state scan, one row.mu per entry,
// no network IO.
func (p *MultiPool) deadKinds() []string {
	st := p.state.Load()
	if st == nil || len(st.entries) == 0 {
		return nil
	}
	now := time.Now()
	// A kind is alive when ANY enabled entry of it is usable. (The old F75
	// demoted/full-service dual accounting left with the small-window demotion
	// mechanism; routing is fully window-blind since,
	// so liveness has no window dimension to special-case — an oversized
	// prompt on a small winner 400s and the turn compacts to that window.)
	type kindState struct {
		hasEnabled, usable bool
	}
	byKind := map[string]*kindState{}
	for _, row := range st.entries {
		if row.info.Priority < minEnabledPriority {
			continue // disabled — never routable, doesn't make the pool usable
		}
		ks := byKind[row.info.Kind]
		if ks == nil {
			ks = &kindState{}
			byKind[row.info.Kind] = ks
		}
		ks.hasEnabled = true
		row.mu.Lock()
		// Usable = not paused AND (live, OR tentative, OR cooldown elapsed).
		if !row.paused &&
			(row.tentative || row.deadUntil.IsZero() || !row.deadUntil.After(now)) {
			ks.usable = true
		}
		row.mu.Unlock()
	}
	unusable := func(ks *kindState) bool { return ks.hasEnabled && !ks.usable }
	// KindLLM headlines when chat is down; then any other configured kind found
	// dead, in deterministic (sorted) order.
	var dead []string
	if ks := byKind[contract.KindLLM]; ks != nil && unusable(ks) {
		dead = append(dead, contract.KindLLM)
	}
	others := make([]string, 0, len(byKind))
	for k := range byKind {
		if k != contract.KindLLM {
			others = append(others, k)
		}
	}
	sort.Strings(others)
	for _, k := range others {
		if unusable(byKind[k]) {
			dead = append(dead, k)
		}
	}
	return dead
}

// checkPoolLiveness recomputes the per-kind all-dead state and drives the
// admin-alert latch. Each kind's alert fires ONCE on its usable→0-usable
// transition (one contract.AdminError per newly-dead kind); when a latched
// kind recovers, its latch clears so its next outage pages again — all
// independently per kind. Called after any liveness change (recordError once
// an entry is newly dead, heartbeatTick after a recovery). Safe to call
// redundantly — the latch makes repeat calls in the same state no-ops.
func (p *MultiPool) checkPoolLiveness() {
	// Compute + apply under the latch lock: with the scan outside it, two
	// concurrent callers (recordError vs heartbeat onRecovery) can apply
	// stale results in inverted order — a spurious all-dead page that then
	// leaves the latch set and swallows the next genuine outage (B27,
	//). Lock order is safe: deadKinds takes each row.mu and
	// nothing acquires allDeadMu while holding a row.mu; notifyAdminAsync
	// delivers on its own goroutine, so the alert cannot stall the lock.
	p.allDeadMu.Lock()
	dead := p.deadKinds()
	fresh := make(map[string]bool, len(dead))
	var newlyDead, recovered []string
	for _, k := range dead {
		fresh[k] = true
		if !p.allDeadKinds[k] {
			if p.allDeadKinds == nil {
				p.allDeadKinds = map[string]bool{}
			}
			p.allDeadKinds[k] = true
			// Suppress the page if the exhaustion latch already covers this
			// kind: both latches then describe ONE outage (the attempt that
			// exhausts a kind is often the same one that tips its last entry
			// into cooling), and reportKindExhausted applies the mirror-image
			// check. Still latch above, so this kind's own liveness
			// bookkeeping stays correct. Note the asymmetry in clearing is
			// deliberate: liveness recovery (cooldown expiry / a heartbeat
			// Ping) does NOT clear exhaustedKinds, because answering /v1/models
			// is not evidence the backend can serve — only a real served call
			// re-arms the page.
			if !p.exhaustedKinds[k] {
				newlyDead = append(newlyDead, k)
			}
		}
	}
	for k := range p.allDeadKinds {
		if !fresh[k] {
			delete(p.allDeadKinds, k)
			recovered = append(recovered, k)
		}
	}
	p.allDeadMu.Unlock()

	for _, k := range newlyDead {
		msg := fmt.Sprintf("model pool fully unavailable for kind %q — every %s entry is dead/paused", k, k)
		slog.Error(msg)
		notifyAdminAsync(p.admin(), contract.AdminError, "model-pool", msg)
	}
	for _, k := range recovered {
		slog.Info("model pool recovered — at least one entry is usable again", "kind", k)
	}
}

// reportKindExhausted pages the admin when one request burned through EVERY
// candidate of a kind and all of them errored: the kind had no fallback left,
// so the caller just ate a hard failure. Latched per kind (fires ONCE per
// outage), cleared by clearKindExhausted when a call of that kind succeeds.
//
// Why alerting cannot keep riding checkPoolLiveness: that path runs only when
// RecordError marks an entry NEWLY DEAD, and the cooling breaker counts CALLS
// (consecutiveFailThreshold=2, or livenessFailThreshold=3 inside a 60s window).
// A single-entry kind that is called rarely — asr and ocr both are — can fail
// every call it ever receives and still never reach either threshold, so the
// entry stays "usable", no kind ever goes 0-usable, and nothing pages. Observed
//: the sole kind:asr entry had a 100% failure rate across its entire
// history (one call, one failure, the backend could not load its model at all)
// and the pool still considered it healthy; no alert was ever emitted.
//
// So alerting is decoupled from cooling. Cooling still owns ROUTING — counting
// calls is right there, and lowering its thresholds would cool healthy busy
// backends on a transient blip. It simply no longer gates the page.
//
// requires carries the request's tag filter (ModelRequest.Requires) so the page
// names the constraint that actually ran dry. Without it the message reads
// "kind llm has no fallback left" even when the kind is perfectly healthy and
// only ONE tag's sole provider is down — pick filters candidates by Requires,
// so an orphan tag exhausts the candidate set while smart is serving the main
// conversation in the same second. That text sent a real outage
// diagnosis down the wrong path; the latch stays kind-scoped (a tag-scoped one
// buys little once a dead entry is skipped by pick and never yields a lastErr).
func (p *MultiPool) reportKindExhausted(kind string, requires []string, err error) {
	// A request fault is not an outage. RecordError deliberately keeps these
	// two classes off the entry's health breaker — content-level 4xx (B3,
	//) and the tool-args-truncated cascade — precisely
	// because EVERY candidate sees the same bad request and fails identically.
	// That is exactly the shape that reaches this branch, so without the same
	// filter one oversized or malformed request pages "no fallback left" while
	// the pool is perfectly healthy. (Caller cancellation and context overflow
	// return earlier in failover and never get here; Chat's failoverOpts sets
	// no nonRetryable, so nothing else screens them out for us.)
	// Thinking-only joins them for the same reason read one level
	// down: the backend is ALIVE — it streamed tokens — so paging "no fallback
	// left for kind X" describes an outage that isn't happening. It reaches here
	// only on a single-candidate lane (a hard Requires with one provider, e.g.
	// judge), where one long think would otherwise page an admin, be cleared by
	// the next success, and page again on the next long think. The user's turn
	// still gets an honest failure notice — that path is unchanged.
	if IsToolArgsTruncatedErr(err) || isPromptLevel4xx(err) || errors.Is(err, ErrThinkingOnly) {
		return
	}
	if kind == "" {
		kind = contract.KindLLM
	}
	p.allDeadMu.Lock()
	// Already latched — either by this path, or by the liveness alert for the
	// same outage (the last candidate can go newly-dead on the very attempt
	// that exhausts the kind). One outage, one page.
	if p.allDeadKinds[kind] || p.exhaustedKinds[kind] {
		p.allDeadMu.Unlock()
		return
	}
	if p.exhaustedKinds == nil {
		p.exhaustedKinds = map[string]bool{}
	}
	p.exhaustedKinds[kind] = true
	p.allDeadMu.Unlock()

	msg := fmt.Sprintf("model pool has no fallback left for kind %q — every candidate errored", kind)
	if len(requires) > 0 {
		msg = fmt.Sprintf("model pool has no fallback left for kind %q with required tags %v — every candidate errored (other tags of this kind may be fine)", kind, requires)
	}
	slog.Error(msg, "kind", kind, "requires", requires, "err", err)
	notifyAdminAsync(p.admin(), contract.AdminError, "model-pool", "%s (last error: %v)", msg, err)
}

// clearKindExhausted drops a kind's exhaustion latch once a real call of that
// kind succeeds, so its next outage pages afresh. Symmetric with the
// recovered-kind branch of checkPoolLiveness. Success is the only thing that
// clears it: a backend answering /v1/models proves nothing about whether it can
// serve (an ASR/OCR shim lazy-loads its model and probes green until the first
// real request), which is why the heartbeat Ping deliberately plays no part
// here.
func (p *MultiPool) clearKindExhausted(kind string) {
	if kind == "" {
		kind = contract.KindLLM
	}
	p.allDeadMu.Lock()
	latched := p.exhaustedKinds[kind]
	delete(p.exhaustedKinds, kind)
	p.allDeadMu.Unlock()
	if latched {
		slog.Info("model pool: kind served again — exhaustion alert re-armed", "kind", kind)
	}
}

// queueDepthFactor sets how deep a kind's wait queue is, as a multiple of that
// kind's total configured concurrency: a kind with one 1-slot entry queues 2
// waiters, a kind with two 4-slot entries queues 16. Deep enough that a normal
// burst lines up and is served (hector: "排满队应该可以继续排队"),
// shallow enough that a backend which stops draining is reported as full
// instead of silently accumulating callers who are all going to time out.
const queueDepthFactor = 2

// queueCapacity is how many requests of `kind` may WAIT for a slot at once:
// queueDepthFactor × the summed concurrency of that kind's ENABLED entries.
// Health is deliberately not consulted — capacity is a property of the
// configuration, so a cooling blip must not shrink the queue under the callers
// already standing in it. 0 means "no cap": some entry of this kind declares
// unlimited concurrency (sem == nil), so nothing ever queues on it anyway.
func (p *MultiPool) queueCapacity(kind string) int {
	st := p.state.Load()
	if st == nil {
		return 0
	}
	total := 0
	for _, row := range st.entries {
		if row.info.Kind != kind || row.info.Priority < minEnabledPriority {
			continue
		}
		if row.sem == nil {
			return 0
		}
		total += cap(row.sem)
	}
	return total * queueDepthFactor
}

// enterQueue admits this request to `kind`'s wait queue and returns the leave
// func the caller MUST call once it stops waiting (slot acquired, or gave up).
// Over capacity it returns contract.ErrModelQueueFull and pages the admin
// (latched per kind) — that is the "nobody can take you" answer, as distinct
// from the "everything is dead" page reportKindExhausted owns.
//
// Only the BLOCKING acquire paths call this. A request that takes a free slot
// immediately never enters the queue, and a saturated entry that has a peer to
// spill to fails over instead of queueing (tryAcquire), so the queue only ever
// holds callers with nowhere else to go.
func (p *MultiPool) enterQueue(kind string) (func(), error) {
	if kind == "" {
		kind = contract.KindLLM
	}
	capacity := p.queueCapacity(kind)
	p.queueMu.Lock()
	if capacity > 0 && p.queueDepth[kind] >= capacity {
		depth := p.queueDepth[kind]
		latched := p.queueFullKinds[kind]
		if p.queueFullKinds == nil {
			p.queueFullKinds = map[string]bool{}
		}
		p.queueFullKinds[kind] = true
		p.queueMu.Unlock()
		slog.Warn("model pool: wait queue full — rejecting", "kind", kind, "waiting", depth, "cap", capacity)
		if !latched {
			notifyAdminAsync(p.admin(), contract.AdminError, "model-pool",
				"model pool wait queue full for kind %q — %d request(s) already waiting (cap %d = %d× total concurrency); the backend is not draining fast enough",
				kind, depth, capacity, queueDepthFactor)
		}
		return nil, fmt.Errorf("model pool: kind %q wait queue full (%d waiting, cap %d): %w",
			kind, depth, capacity, contract.ErrModelQueueFull)
	}
	if p.queueDepth == nil {
		p.queueDepth = map[string]int{}
	}
	p.queueDepth[kind]++
	p.queueMu.Unlock()
	return func() {
		p.queueMu.Lock()
		if p.queueDepth[kind] <= 1 {
			delete(p.queueDepth, kind)
		} else {
			p.queueDepth[kind]--
		}
		p.queueMu.Unlock()
	}, nil
}

// clearQueueFull drops a kind's queue-full latch once a call of that kind
// completes, so the next time the queue backs up it pages afresh. Same
// success-only posture as clearKindExhausted, and called from the same seam.
func (p *MultiPool) clearQueueFull(kind string) {
	if kind == "" {
		kind = contract.KindLLM
	}
	p.queueMu.Lock()
	latched := p.queueFullKinds[kind]
	delete(p.queueFullKinds, kind)
	p.queueMu.Unlock()
	if latched {
		slog.Info("model pool: kind drained — queue-full alert re-armed", "kind", kind)
	}
}

// addUsage adds one call's deltas into the entry's current-hour bucket.
// Caller MUST hold row.mu.
func addUsage(row *entryRow, now time.Time, calls, errs, input, output int64) {
	hour := now.Truncate(time.Hour).Unix()
	if row.hourly == nil {
		row.hourly = map[int64]*hourBucket{}
	}
	b := row.hourly[hour]
	if b == nil {
		b = &hourBucket{}
		row.hourly[hour] = b
		if dropped := dropOldestHourly(row); dropped > 0 {
			slog.Warn("model usage hourly buckets at cap — oldest dropped",
				"entry", row.info.Name, "dropped", dropped)
		}
	}
	b.calls += calls
	b.errors += errs
	b.input += input
	b.output += output
}

// hourlyBucketCap bounds entryRow.hourly when buckets cannot drain — the
// store is not wired, or it keeps failing and Flush keeps re-adding (D21,
//). One week of wall-clock hours; oldest buckets go first, so
// a prolonged outage costs the stalest history, never recent usage.
const hourlyBucketCap = 168

// dropOldestHourly evicts the oldest hour buckets until row.hourly is
// within hourlyBucketCap. Caller MUST hold row.mu. Returns the count
// dropped so callers can surface the loss.
func dropOldestHourly(row *entryRow) int {
	dropped := 0
	for len(row.hourly) > hourlyBucketCap {
		oldest := int64(math.MaxInt64)
		for h := range row.hourly {
			if h < oldest {
				oldest = h
			}
		}
		delete(row.hourly, oldest)
		dropped++
	}
	return dropped
}

// FlushUsage persists every COMPLETED hour's accumulated usage to the
// store and removes those buckets. The in-progress hour stays in
// memory. Thin wrapper — see ModelUsageRecorder.Flush.
func (p *MultiPool) FlushUsage(ctx context.Context) {
	if p == nil {
		return
	}
	st := p.state.Load()
	if st == nil {
		return
	}
	p.usage.Flush(ctx, st.entries, false)
}

// FlushUsageFinal persists ALL accumulated usage including the in-progress
// hour. Called once on a graceful shutdown — unlike the per-tick FlushUsage
// (which leaves the current hour to keep accumulating), a clean exit should
// not lose the partial hour the way a crash does.
func (p *MultiPool) FlushUsageFinal(ctx context.Context) {
	if p == nil {
		return
	}
	st := p.state.Load()
	if st == nil {
		return
	}
	p.usage.Flush(ctx, st.entries, true)
}

// ----- dynamic models.yaml reload ------------------------------------------

// Reload is the slash-facing wrapper around ConfigReloader.Reload (the
// explicit operator path, /model reload). Liveness state is NOT carried
// over — fresh entries start live.
func (p *MultiPool) Reload() error { return p.reload.Reload() }

// maybeReload is the hot-path thin wrapper around
// ConfigReloader.MaybeReload — called from pick() per request.
func (p *MultiPool) maybeReload() { p.reload.MaybeReload() }

var _ contract.ModelPool = (*MultiPool)(nil)
