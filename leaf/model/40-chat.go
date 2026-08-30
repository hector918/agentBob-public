package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"agentbob/contract"
	"agentbob/leaf/model/providers"
)

// firstContentTimeout bounds how long a STREAMING attempt may go
// without producing its first content token before the pool gives up
// on it and fails over to the next candidate. It catches a backend
// that accepts the request then stalls (OOM, wedged KV cache) — a
// failure mode the passive liveness breaker is too slow to notice
// within a single request.
//
// 300s, not 90s (hector): from the outside "no first token" is
// indistinguishable from a HEALTHY local backend chewing a near-window
// prefill, which legitimately takes minutes on modest hardware (实弹: a 75K
// prompt's prefill exceeded 90s, the watchdog cut the primary every round and
// its errStreamStall strikes would cool a perfectly healthy entry). One flat
// constant, deliberately no size-scaled deadline — the cost is a genuinely
// wedged backend burns up to 5min before failover, accepted for the simpler
// mental model. streamHardTimeout (10min, provider side) stays the ceiling.
//
// Applied only while a fallback candidate still exists: the LAST
// candidate runs to the provider's streamHardTimeout instead, since a
// slow-but-live model still beats failing the turn outright.
// Post-content stalls are streamHardTimeout's job, not this one's.
//
// There is no wall-clock cap on the failover FANOUT itself: the loop
// terminates naturally when the (finite) candidate set is exhausted —
// each candidate is excluded after one attempt — so the worst case is
// bounded by candidate-count × per-attempt timeout.
//
// A var (not const) only so tests can lower it; production never
// reassigns.
var firstContentTimeout = 300 * time.Second

// busyRetryBudget bounds how long ONE request keeps re-trying an entry that
// answers "busy / momentarily unreachable" (isTransientBackendErr) before the
// pool concedes that nothing can serve it. 60s (hector): the
// observed sidecar restart windows resolve inside a minute, and a media tool
// call the user is waiting on is far better served by a slow answer than by a
// "backend unavailable" the model then papers over with a different tool.
//
// A var (not const) only so tests can lower it; production never reassigns.
var busyRetryBudget = 60 * time.Second

// busyRetryBackoff is the wait before each successive retry; the last step
// repeats until the budget runs out. Starts short — a restarting container is
// usually back within a second or two — and stretches so a longer outage is
// not hammered.
var busyRetryBackoff = []time.Duration{time.Second, 3 * time.Second, 6 * time.Second, 12 * time.Second, 24 * time.Second}

// withBusyRetry runs call, and on a TRANSIENT backend failure (the backend or
// its proxy said "busy / not here right now") waits and runs it again against
// the SAME entry until it succeeds, fails for another reason, the caller's ctx
// ends, or busyRetryBudget elapses. `allow` is false whenever the request still
// has somewhere else to go: failing over to a healthy peer beats queueing on a
// sick one, so only the last-resort candidate queues.
//
// The entry's concurrency slot is HELD across the waits by design — the retrying
// request keeps its place in line, and other callers pile up behind it in the
// kind's wait queue (bounded by enterQueue), which is exactly what "the backend
// is busy" should look like from the outside. Health accounting sees ONE error
// for the whole sequence: the caller records only what this function finally
// returns, so a blip absorbed here never touches the cooling breaker, while an
// outage that outlives the budget still cools the entry normally.
func withBusyRetry[T any](ctx context.Context, row *entryRow, allow bool, call func() (T, error)) (T, error) {
	out, err := call()
	if !allow || err == nil || !isTransientBackendErr(err) {
		return out, err
	}
	deadline := time.Now().Add(busyRetryBudget)
	for i := 0; ; i++ {
		wait := busyRetryBackoff[min(i, len(busyRetryBackoff)-1)]
		if time.Now().Add(wait).After(deadline) {
			slog.Warn("model entry still busy when the retry budget ran out — giving up",
				"entry", row.info.Name, "budget", busyRetryBudget, "err", err)
			return out, err
		}
		slog.Info("model entry busy — waiting in queue and retrying",
			"entry", row.info.Name, "wait", wait, "attempt", i+2, "err", err)
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			var zero T
			return zero, ctx.Err()
		}
		timer.Stop()
		out, err = call()
		if err == nil {
			slog.Info("model entry recovered after busy-retry", "entry", row.info.Name, "attempts", i+2)
			return out, nil
		}
		if !isTransientBackendErr(err) {
			return out, err
		}
	}
}

// acquire blocks on the entry's semaphore (if any) until a slot is
// free or ctx is cancelled. release returns the slot. D4 — logs at
// debug when a call has to wait for a slot (concurrency contention is
// invisible otherwise). Blocking means QUEUEING, so the wait is admitted
// through the kind's bounded wait queue (enterQueue) — past its depth the
// caller is told the queue is full rather than joining an unbounded line.
func (p *MultiPool) acquire(ctx context.Context, row *entryRow) error {
	if row.sem != nil {
		// Try non-blocking first so we don't log on every call.
		select {
		case row.sem <- struct{}{}:
		default:
			leave, qerr := p.enterQueue(row.info.Kind)
			if qerr != nil {
				return qerr
			}
			slog.Debug("model entry concurrency saturated — waiting", "entry", row.info.Name, "limit", cap(row.sem))
			select {
			case row.sem <- struct{}{}:
				leave()
			case <-ctx.Done():
				leave()
				return ctx.Err()
			}
		}
	}
	row.mu.Lock()
	row.inFlight++
	row.mu.Unlock()
	return nil
}

func (p *MultiPool) release(row *entryRow) {
	row.mu.Lock()
	row.inFlight--
	row.mu.Unlock()
	if row.sem != nil {
		<-row.sem
	}
}

// tryAcquire takes a concurrency slot WITHOUT blocking. An unlimited
// entry (row.sem == nil) always succeeds. A bounded entry succeeds
// only if a slot is free right now — a full entry returns false so the
// caller (Chat / ChatStreamWatch) can fail over to the next same-tag entry
// instead of queueing. On success it does the same inFlight
// bookkeeping as acquire.
func (p *MultiPool) tryAcquire(row *entryRow) bool {
	if row.sem != nil {
		select {
		case row.sem <- struct{}{}:
		default:
			return false
		}
	}
	row.mu.Lock()
	row.inFlight++
	row.mu.Unlock()
	return true
}

// acquireAnyOf is the all-saturated last resort: every same-tag entry
// was found full, so wait on ALL their semaphores at once and take
// whichever frees first. Returns the row that got a slot (inFlight
// already bumped) or ctx.Err() if the context is cancelled while
// waiting. Every row passed here has a non-nil sem (a nil-sem /
// unlimited entry is never saturated); an empty slice is guarded
// defensively. This is a queueing wait, so it is admitted through the kind's
// bounded wait queue exactly like acquire's blocking path.
func (p *MultiPool) acquireAnyOf(ctx context.Context, rows []*entryRow) (*entryRow, error) {
	if len(rows) == 0 {
		return nil, errors.New("model pool: acquireAnyOf called with no entries")
	}
	leave, qerr := p.enterQueue(rows[0].info.Kind)
	if qerr != nil {
		return nil, qerr
	}
	defer leave()
	cases := make([]reflect.SelectCase, 0, len(rows)+1)
	for _, row := range rows {
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectSend,
			Chan: reflect.ValueOf(row.sem),
			Send: reflect.ValueOf(struct{}{}),
		})
	}
	cases = append(cases, reflect.SelectCase{
		Dir:  reflect.SelectRecv,
		Chan: reflect.ValueOf(ctx.Done()),
	})
	chosen, _, _ := reflect.Select(cases)
	if chosen == len(rows) {
		// the trailing ctx.Done() case fired
		return nil, ctx.Err()
	}
	row := rows[chosen]
	row.mu.Lock()
	row.inFlight++
	row.mu.Unlock()
	return row, nil
}

// eligibleSaturatedForPrimary returns the still-eligible entries in
// `saturated` that satisfy the request's PRIMARY required-tag set
// (req.Requires) — i.e. the busy-but-healthy entries the request actually
// wanted before any tag-fallback degradation.
//
// B1: saturation is "busy", not "unavailable". When the primary
// tag set is empty in pick ONLY because its entries are saturated (vs dead /
// paused / errored), the caller must WAIT on those primary entries rather than
// silently degrade to a tag-fallback hop (e.g. smart → small). This collects
// the rows the all-saturated wait path can block on; an empty result means the
// primary set is genuinely unavailable (degradation is then legitimate).
func eligibleSaturatedForPrimary(requires []string, saturated []*entryRow) []*entryRow {
	var out []*entryRow
	for _, r := range saturated {
		if hasAll(r.info.Tags, requires) && r.IsEligible() {
			out = append(out, r)
		}
	}
	return out
}

// servedByFallbackHop reports whether `row` came from a tag-fallback hop
// rather than the primary required-tag set: it does NOT carry every primary
// required tag. Used to detect (and loudly log) a quality degradation.
func servedByFallbackHop(row *entryRow, requires []string) bool {
	return len(requires) > 0 && !hasAll(row.info.Tags, requires)
}

// failoverOpts parametrises the shared failover driver with the few
// per-path differences the two pool entrypoints (Chat /
// ChatStreamWatch) carry.
type failoverOpts struct {
	// logPrefix namespaces the driver's log lines ("model pool" /
	// "model pool (stream-watch)").
	logPrefix string
	// armFirstContent arms firstContentTimeout on an attempt while a
	// further candidate remains to fail over to (streaming attempts);
	// false always passes 0 — a non-streaming attempt is bounded by the
	// provider's own per-call timeout instead.
	armFirstContent bool
	// nonRetryable, when non-nil, marks errors that must return to the
	// caller immediately WITH the attempt's result (which may carry
	// forensics, e.g. the serving entry name) instead of excluding the
	// entry and failing over — used for conversation-level watcher trips,
	// where every candidate would fail identically.
	nonRetryable func(error) bool
}

// failover is the single candidate-iteration driver behind Chat /
// ChatStreamWatch (D20 — previously near-identical
// copies that had already drifted): pinned short-circuit,
// pick/exclude loop, saturation spill, the B1 saturated-primary wait, the
// all-saturated last resort, first-content deadline arming, per-attempt
// error failover, and the terminal ErrNoModelAvailable wrap.
//
// attempt runs one candidate; the entry's concurrency slot is already
// held, and the attempt callee (chatOne / streamOneWatch)
// owns its release. firstContentDeadline is firstContentTimeout while a
// further candidate remains, 0 on the pinned / last-resort / final-
// candidate paths (let a slow-but-live model run to the provider's
// streamHardTimeout — it beats failing the turn). lastResort says this
// candidate is the end of the line — nothing left to fail over to — which is
// the ONLY condition under which the attempt queues on a busy backend and
// retries it (withBusyRetry); while a peer remains, failing over there is
// both faster and healthier than waiting.
//
// No wall-clock budget on the fanout: the loop terminates when the
// finite candidate set is exhausted (each entry is excluded after one
// attempt), and a single attempt is bounded by firstContentTimeout /
// the provider's hard timeout, so the worst case is candidate-count ×
// per-attempt timeout.
func failover[T any](ctx context.Context, p *MultiPool, req contract.ModelRequest, opts failoverOpts, attempt func(row *entryRow, firstContentDeadline time.Duration, lastResort bool) (T, error)) (T, error) {
	var zero T
	// Pinned-entry path: a pinned request named exactly one entry —
	// honour it and never spill. A blocking acquire here means a
	// saturated pinned entry queues (the desired behaviour for a pin);
	// the spill loop below would instead infinite-loop, since pick
	// keeps returning the same pinned row. No first-content deadline:
	// there is nowhere to fail over to.
	if req.PinnedEntry != "" {
		row, pickErr := p.pick(req, nil)
		if pickErr != nil {
			return zero, pickErr
		}
		if err := p.acquire(ctx, row); err != nil {
			return zero, err
		}
		return attempt(row, 0, true)
	}

	// exclude names entries that errored OR are currently saturated —
	// both are passed to pick so it ranks the next-best alternative.
	exclude := map[string]bool{}
	// saturatedRows collects entries found full this request, in
	// discovery (priority) order — the all-saturated last resort waits
	// on these.
	var saturatedRows []*entryRow
	var lastErr error
	errorAttempts := 0
	for {
		row, pickErr := p.pick(req, exclude)
		if pickErr != nil {
			// No un-excluded entry left. If some were skipped purely
			// for saturation, the last resort is to wait on the
			// still-eligible ones — one may have died / been paused
			// while we failed over.
			var eligible []*entryRow
			for _, r := range saturatedRows {
				if r.IsEligible() {
					eligible = append(eligible, r)
				}
			}
			if len(eligible) > 0 {
				row, aerr := p.acquireAnyOf(ctx, eligible)
				if aerr != nil {
					return zero, aerr
				}
				// Slot held — last resort, no further failover: return
				// whatever this attempt yields (success or error).
				return attempt(row, 0, true)
			}
			if lastErr != nil {
				// Candidates exhausted, every one errored — classify
				// as ErrNoModelAvailable so callers (small-model
				// fallback, agora suspend) can branch on it; keep
				// lastErr's text.
				slog.Warn(opts.logPrefix+": all candidates failed — giving up",
					"error_attempts", errorAttempts, "err", lastErr)
				// This kind had no fallback left. Page the admin here rather
				// than leaving it to the cooling breaker, which counts calls
				// and so stays silent for a rarely-called kind that fails
				// every single request. Latched — one page per outage.
				p.reportKindExhausted(req.Kind, req.Requires, lastErr)
				return zero, fmt.Errorf("model pool: all %d candidate(s) failed (last: %v): %w",
					errorAttempts, lastErr, ErrNoModelAvailable)
			}
			return zero, pickErr // pickErr already wraps ErrNoModelAvailable
		}
		// B1: pick handed back a tag-fallback hop (e.g. small) because the
		// primary set (e.g. smart) yielded nothing. If the ONLY reason the
		// primary set is empty is that its entries are saturated (busy, not
		// dead/paused/errored), WAIT on them instead of silently degrading —
		// the request asked for smart, so a queue beats a quiet downgrade.
		if servedByFallbackHop(row, req.Requires) {
			if eligibleSat := eligibleSaturatedForPrimary(req.Requires, saturatedRows); len(eligibleSat) > 0 {
				slog.Info(opts.logPrefix+": primary entries saturated — waiting rather than degrading to tag-fallback",
					"required", req.Requires, "waiting_on", len(eligibleSat))
				waited, aerr := p.acquireAnyOf(ctx, eligibleSat)
				switch {
				case aerr == nil:
					// Slot held — last resort, no further failover.
					return attempt(waited, 0, true)
				case errors.Is(aerr, contract.ErrModelQueueFull):
					// The primary's queue is full, but a usable (degraded) entry
					// is right here — serving `small` beats failing a request
					// that `smart` has no room for. Falls through to the
					// degradation path below, which logs it.
					slog.Warn(opts.logPrefix+": primary queue full — degrading to tag-fallback instead of failing",
						"required", req.Requires, "served_by", row.info.Name)
				default:
					return zero, aerr
				}
			}
			// Degradation is legitimate (primary genuinely unavailable). It
			// MUST stay visible: pick logs the first-pick degradation at INFO
			// but SUPPRESSES it mid-failover (tried non-empty), which is
			// exactly when a saturation spill degrades quietly — cover that
			// gap here.
			if len(exclude) > 0 {
				slog.Warn(opts.logPrefix+": degraded to tag-fallback mid-failover — primary unavailable",
					"required", req.Requires, "served_by", row.info.Name)
			}
		}
		// pick returned a row — try to take a slot without blocking. A
		// full entry spills to the next same-tag entry instead of
		// queueing.
		if !p.tryAcquire(row) {
			slog.Debug(opts.logPrefix+": entry saturated — failing over",
				"entry", row.info.Name, "limit", cap(row.sem))
			saturatedRows = append(saturatedRows, row)
			exclude[row.info.Name] = true
			continue
		}
		// Is this the end of the line? It decides two things: whether a
		// stalled stream is cut at firstContentTimeout (only while another
		// candidate remains — the last one runs to the provider's
		// streamHardTimeout, since a slow-but-live model beats failing the
		// turn), and whether a BUSY backend is queued on and retried rather
		// than abandoned for a peer.
		lastResort := !p.hasFallbackCandidate(req, exclude, row, saturatedRows)
		var fcDeadline time.Duration
		if opts.armFirstContent && !lastResort {
			fcDeadline = firstContentTimeout
		}
		out, err := attempt(row, fcDeadline, lastResort)
		if err == nil {
			return out, nil
		}
		if opts.nonRetryable != nil && opts.nonRetryable(err) {
			return out, err
		}
		// Context overflow is a prompt-level fault, not the entry's: surface it
		// straight to the caller as ErrContextExceeded — the turn compacts to
		// the serving entry's window and retries on the same winner (pick is
		// window-blind; churning peers would just re-collect the same 400).
		// isPromptLevel4xx already keeps it off the entry's health breaker; the
		// request is rejected pre-generation so nothing was streamed.
		if IsContextExceededErr(err) {
			return zero, fmt.Errorf("model pool: %w", ErrContextExceeded)
		}
		lastErr = err
		// ctx cancellation (e.g. /close-session) is intentional —
		// don't fail over.
		if ctx.Err() != nil {
			return zero, err
		}
		errorAttempts++
		exclude[row.info.Name] = true
		// logPrefix already distinguishes the two entrypoints in the log line.
		slog.Warn(opts.logPrefix+": chat failed — retrying on another entry", "failed", row.info.Name, "err", err)
	}
}

// Chat implements contract.ModelPool. The response is tagged with the
// entry's Name so the agent can record "which model answered" in the
// session row.
//
// Failover: a non-streaming call is atomic, so on failure Chat
// transparently retries on the next-best entry (the shared failover
// driver) until one succeeds or the candidates are exhausted — the
// caller only ever sees the final result (success, or the last error).
// ctx cancellation and pinned requests are not retried.
//
// Liveness: every attempt's error increments that entry's fail
// counter; success resets its consecutive-dead counter.
func (p *MultiPool) Chat(ctx context.Context, req contract.ModelRequest, msgs []contract.Message) (contract.ChatResponse, error) {
	return failover(ctx, p, req, failoverOpts{
		logPrefix: "model pool",
	}, func(row *entryRow, _ time.Duration, lastResort bool) (contract.ChatResponse, error) {
		return p.chatOne(ctx, req, msgs, row, lastResort)
	})
}

// chatOne runs one non-streaming call against row and records
// liveness + usage. The entry's concurrency slot MUST already be held
// by the caller — chatOne only releases it (the deferred release
// below), never acquires it.
//
// busyRetry (set by the driver on the last-resort candidate) lets a
// "backend busy / momentarily unreachable" answer be waited out on this same
// entry instead of failing the call; only the FINAL error reaches
// recordError, so an absorbed blip never counts against the entry's health.
func (p *MultiPool) chatOne(ctx context.Context, req contract.ModelRequest, msgs []contract.Message, row *entryRow, busyRetry bool) (contract.ChatResponse, error) {
	defer p.release(row)
	slog.Debug("pool dispatched (chat)", "entry", row.info.Name, "model", row.info.Model, "requires", req.Requires, "prefer", req.Prefer, "tools", len(req.Tools), "msgs", len(msgs))
	// Tag ctx with the entry name so the provider's reqlog can record
	// "smart" / "small" / etc. alongside the model name.
	cctx := providers.WithEntryName(ctx, row.info.Name)
	resp, err := withBusyRetry(ctx, row, busyRetry, func() (contract.ChatResponse, error) {
		return row.chatter.Chat(cctx, row.info.Model, msgs, req.Tools)
	})
	if err != nil {
		// Thinking-only is the one failure the provider raises with a real bill
		// attached and with the backend demonstrably alive, so it books its tokens
		// and skips the health breaker — cooling the entry would pull the only
		// thinking-tagged entry out of rotation over a single long think. The driver
		// still excludes it and reaches for the next candidate (that exclusion is
		// error-driven, not health-driven). Mirrors the streaming twin in
		// 45-chat-stream-watch.go; see providers.ErrThinkingOnly for the taxonomy.
		if errors.Is(err, ErrThinkingOnly) {
			p.recordChatTokens(cctx, row, resp.Usage, "chat", 1)
			return contract.ChatResponse{}, err
		}
		p.recordError(ctx, row, err)
		return contract.ChatResponse{}, err
	}
	p.recordChatSuccess(cctx, row, resp.Usage, "chat")
	// Cache affinity rides the same per-success point as the rest of the
	// bookkeeping (recordChatSuccess's centralisation note applies): the
	// conversation's warm KV cache is wherever the serve actually landed —
	// pinned and saturated-last-resort paths included, since every failover
	// branch funnels through here. No-op for a keyless request.
	p.recordAffinity(req.AffinityKey, row.info.Name)
	resp.Model = row.info.Name
	return resp, nil
}

// logCacheUsage emits a per-call cache visibility line when the backend
// reported any cache reuse. Silent when InputTokens is 0 (probably an
// errored / aborted call) or when the backend doesn't expose cache info
// (CacheReadInputTokens stays 0 — log nothing rather than spam zero
// ratios for every backend that doesn't support prompt caching).
//
// `grep "model cache" agent.log | awk '...'` is the dashboard until we
// either ship a real one or aggregate into bob_model_usage's hourly rows.
func logCacheUsage(entry, kind string, u contract.Usage) {
	if u.InputTokens <= 0 {
		return
	}
	cached := u.CacheReadInputTokens + u.CacheCreationInputTokens
	if cached <= 0 {
		return
	}
	pct := 100 * u.CacheReadInputTokens / u.InputTokens
	slog.Info("model cache",
		"entry", entry,
		"kind", kind,
		"input", u.InputTokens,
		"cache_read", u.CacheReadInputTokens,
		"cache_create", u.CacheCreationInputTokens,
		"hit_pct", pct,
	)
}
