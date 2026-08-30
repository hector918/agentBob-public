package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/leaf/model/providers"
)

// ReloaderHooks are the pool-level callbacks ConfigReloader fires
// during a swap. Each is optional; nil means skip. Kept narrow on
// purpose — the reloader stays unaware of MultiPool.
type ReloaderHooks struct {
	// RetireOld is called once with the OLD poolState AFTER the atomic
	// swap. In-flight calls hold references to the old *entryRow values
	// (poolState doc) and keep recording usage onto them until they
	// finish, so the rows must stay flush-reachable: MultiPool wires this
	// to p.usage.Retire(old.entries), which keeps them on the recorder's
	// retired list through the next housekeeping flushes instead of a
	// single pre-swap drain — that drain silently lost any usage recorded
	// after it ran (B25).
	RetireOld func(old *poolState)
	// PostSwap is called once AFTER the atomic swap; MultiPool wires
	// this to p.checkPoolLiveness so a reload that adds a healthy entry
	// clears the all-dead admin latch.
	PostSwap func()
	// NotifyAsyncError surfaces an mtime-watch reload failure to admin.
	// MultiPool wires this to notifyAdminAsync(p.adminLine, ...).
	NotifyAsyncError func(err error)
}

// ConfigReloader is the model-package sub-component that re-reads the
// models.yaml config + rebuilds the entries table + atomically swaps
// it in. Owns its own reload + config locks and the mtime baseline.
//
// Lifecycle: built once by NewMultiPool and never replaced. Concurrency:
// Reload and MaybeReload are safe to call from any goroutine; reloadMu
// serialises the two so a manual /model reload can't race the mtime
// watcher.
type ConfigReloader struct {
	state      *atomic.Pointer[poolState]
	reloadFn   func() ([]Entry, []FallbackRule, error)
	configPath string
	hooks      ReloaderHooks

	// reloadMu serialises Reload + MaybeReload so two concurrent triggers
	// (mtime watch + manual /model reload) can't rebuild the table at the
	// same time.
	reloadMu sync.Mutex
	// configMu guards the throttled mtime watch (MaybeReload): lastCheck
	// is the last stat time, lastMtime the last-seen file mtime.
	configMu  sync.Mutex
	lastCheck time.Time
	lastMtime time.Time
	// servedMtime is the mtime of the config the pool is ACTUALLY serving —
	// it advances only after a swap succeeds. lastMtime cannot answer that:
	// it is the watcher's dedup baseline and advances the moment a change is
	// SEEN, so a config that fails to load leaves the two apart (and must,
	// or a broken file would be retried + admin-paged every check interval).
	// This is the one an operator is owed ("did my edit take effect"), so it
	// is what SnapshotInfo exposes; the divergence itself is announced by the
	// NotifyAsyncError admin page, not by a second panel stat.
	servedMtime time.Time
}

// NewConfigReloader wires a fresh reloader. reloadFn nil → Reload /
// MaybeReload are disabled (return / no-op). configPath empty → the
// mtime watch is disabled (MaybeReload is a no-op even if reloadFn
// is set).
func NewConfigReloader(state *atomic.Pointer[poolState], reloadFn func() ([]Entry, []FallbackRule, error), configPath string, hooks ReloaderHooks) *ConfigReloader {
	r := &ConfigReloader{
		state:      state,
		reloadFn:   reloadFn,
		configPath: configPath,
		hooks:      hooks,
	}
	// Capture the config file's mtime as the initial baseline so an edit
	// between this startup config read and the first MaybeReload isn't
	// silently baselined away. configPath == "" → mtime watch disabled,
	// no stat. A stat error leaves lastMtime zero — MaybeReload then
	// treats the first non-zero mtime it sees as a change and reloads
	// (safe default).
	if configPath != "" {
		if fi, err := os.Stat(configPath); err == nil {
			r.lastMtime = fi.ModTime()
		} else {
			slog.Debug("model pool: initial models.yaml stat failed — no mtime baseline", "err", err)
		}
	}
	return r
}

// Reload is the explicit operator path (/model reload). Liveness state
// is NOT carried over — fresh entries start live (an operator reload
// deliberately wants a clean slate; a paused/cooling entry the operator
// just edited gets a fresh start). Serialised via reloadMu so two
// triggers can't race.
//
// On any parse / build error the current pool is left untouched and the
// error is returned.
func (r *ConfigReloader) Reload() error {
	return r.reload(false)
}

// MaybeReload does a throttled mtime check on the config file. At most
// one stat per configCheckInterval; if the file's mtime changed since
// the last seen value it kicks off Reload asynchronously (the caller
// that detected the change still serves on the current state — the
// NEXT request gets the new one). Best-effort: a stat error is logged
// at debug and otherwise ignored.
func (r *ConfigReloader) MaybeReload() {
	if r == nil || r.configPath == "" || r.reloadFn == nil {
		return
	}
	r.configMu.Lock()
	now := time.Now()
	if !r.lastCheck.IsZero() && now.Sub(r.lastCheck) < configCheckInterval {
		r.configMu.Unlock()
		return
	}
	r.lastCheck = now
	fi, err := os.Stat(r.configPath)
	if err != nil {
		// File missing / unreadable — nothing to reload against. Don't
		// touch lastMtime so a later re-appearance is still detected.
		r.configMu.Unlock()
		slog.Debug("model pool: models.yaml stat failed — skipping reload check", "err", err)
		return
	}
	mtime := fi.ModTime()
	// No first-call-baseline special case: NewConfigReloader captured
	// the startup mtime baseline, so any edit after the startup config
	// read is caught here. (If the stat at construction failed,
	// lastMtime is zero and this first non-zero mtime triggers a reload
	// — the safe default.)
	if mtime.Equal(r.lastMtime) {
		r.configMu.Unlock()
		return
	}
	r.lastMtime = mtime
	r.configMu.Unlock()
	slog.Info("model pool: models.yaml changed — reloading asynchronously")
	go func() {
		// preserveLiveness=true: an mtime-triggered reload must not
		// resurrect entries mid-cooldown — an unrelated config edit
		// should leave per-entry liveness intact.
		if err := r.reload(true); err != nil {
			slog.Warn("model pool: async reload failed — keeping current pool", "err", err)
			if r.hooks.NotifyAsyncError != nil {
				r.hooks.NotifyAsyncError(err)
			}
		}
	}()
}

// SnapshotInfo is the read-side view of the reloader — ConfigPath +
// LastMtime, the mtime of the config the pool is SERVING (zero before
// any config has been loaded, or when the watch is disabled). Not the
// watcher's dedup baseline: a reload that failed leaves this at the
// config still in service, which is what the reader is asking about.
// Cheap; safe to call concurrently with Reload / MaybeReload.
func (r *ConfigReloader) SnapshotInfo() contract.ReloadInfo {
	if r == nil {
		return contract.ReloadInfo{}
	}
	r.configMu.Lock()
	mt := r.servedMtime
	r.configMu.Unlock()
	return contract.ReloadInfo{ConfigPath: r.configPath, LastMtime: mt}
}

// noteBootConfigServed records the startup config as the one in service.
// Called by NewMultiPool once the boot entries are built and stored — the
// reloader itself can't know whether that succeeded, and an empty pool
// (unparseable / empty models.yaml) is serving nothing, so its stamp must
// stay zero rather than claim the file it failed to load.
func (r *ConfigReloader) noteBootConfigServed() {
	if r == nil {
		return
	}
	r.configMu.Lock()
	r.servedMtime = r.lastMtime
	r.configMu.Unlock()
}

// reload is the shared body for both Reload and MaybeReload. It
// re-reads the config via the injected closure, rebuilds every entry,
// swaps the pool state atomically, retires the OLD state's rows to the
// usage recorder (their buckets — incl. usage still being recorded by
// in-flight calls — drain on the next housekeeping flushes; the new
// entries' buckets start empty), and re-baselines the mtime watch.
//
// preserveLiveness controls whether per-entry liveness (dead / cooldown
// / error window / pause / tentative) survives the swap. When true,
// each new row inherits the liveness of the OLD row with the same Name
// — so an unrelated config edit can't resurrect an entry that is
// mid-cooldown. When false the new rows start live (clean slate).
//
// The concurrency semaphore and in-flight counters are NEVER carried
// over — every new row gets a fresh semaphore and a zero in-flight
// count.
func (r *ConfigReloader) reload(preserveLiveness bool) error {
	if r == nil {
		return errors.New("model pool: nil")
	}
	if r.reloadFn == nil {
		return errors.New("model pool: reload closure not set — reload unavailable")
	}
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	// Stat BEFORE the config read: on success this becomes the watcher's
	// new mtime baseline (applied after the swap below). Without the
	// re-baseline a manual /model reload after an edit leaves lastMtime
	// stale, so the next request's mtime check fires a guaranteed
	// duplicate async reload of the very config the pool already serves
	// (B26). Stat-then-read means an edit racing the read at
	// worst triggers one redundant reload (mtime newer than baseline) —
	// never a missed one, which stat-after-read would allow.
	var baselineMtime time.Time
	if r.configPath != "" {
		if fi, serr := os.Stat(r.configPath); serr == nil {
			baselineMtime = fi.ModTime()
		}
	}

	entries, fallback, err := r.reloadFn()
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("reload: model config is absent or empty")
	}
	old := r.state.Load()
	st := &poolState{byName: map[string]*entryRow{}, fallback: fallback}
	for i, e := range entries {
		// allowProbe=true: a reload re-probes so context windows stay
		// fresh (a deliberate trade of reload speed — probes are bounded).
		row, berr := buildEntry(i, e, true)
		if berr != nil {
			return fmt.Errorf("reload: %w", berr)
		}
		// mtime-watch path: inherit liveness from the same-named old row
		// so a cooling / paused entry isn't resurrected by an unrelated
		// edit. The new row is not yet published — it needs no lock; the
		// old row is read under its own mutex.
		if preserveLiveness && old != nil {
			if oldRow, ok := old.byName[e.Name]; ok {
				copyLiveness(oldRow, row)
			}
		}
		st.entries = append(st.entries, row)
		st.byName[e.Name] = row
	}

	r.state.Store(st)
	// Hand the displaced rows to the usage recorder AFTER the swap: in-
	// flight calls still hold them and keep recording usage until they
	// finish, so they are drained by the next housekeeping flushes rather
	// than one pre-swap pass (B25 — see ReloaderHooks.RetireOld).
	if old != nil && r.hooks.RetireOld != nil {
		r.hooks.RetireOld(old)
	}
	// Re-baseline the mtime watch to the config just served (see the stat
	// above NewConfigReloader-style ordering rationale). This is the ONLY
	// place servedMtime advances — everything above it (parse, Validate,
	// buildEntry, the atomic swap) has to have succeeded to get here, which
	// is what makes it an acknowledgement rather than a sighting.
	if !baselineMtime.IsZero() {
		r.configMu.Lock()
		r.lastMtime = baselineMtime
		r.servedMtime = baselineMtime
		r.lastCheck = time.Now()
		r.configMu.Unlock()
	}
	// Recompute the all-dead latch against the freshly swapped table: a
	// reload can change pool liveness (e.g. add a healthy model to a
	// previously all-dead pool), and a stale latch would otherwise skip
	// the next genuine outage page. Safe here — only reloadMu is held;
	// the hook ultimately takes each row.mu, none of which is held at
	// this point (copyLiveness released the old rows).
	if r.hooks.PostSwap != nil {
		r.hooks.PostSwap()
	}
	names := make([]string, 0, len(st.entries))
	for _, row := range st.entries {
		names = append(names, row.info.Name)
	}
	slog.Info("model pool reloaded from models.yaml",
		"entries", len(st.entries), "names", names, "preserve_liveness", preserveLiveness)
	return nil
}

// buildEntry resolves one Entry into an *entryRow: env-var auth, context
// window / concurrency, kind default, priority clamp. Shared by
// NewMultiPool and ConfigReloader.reload so the two never diverge.
//
// Context window: the yaml declaration is authoritative when set (声明即真相,
// hector) — a llama-cpp /props probe reports the server's TOTAL
// n_ctx, not the per-slot capacity a request actually gets, so the probe can
// only fill in entries that declare nothing. When both exist and the probe
// reads SMALLER than the declaration, we warn: the declared window won't fit
// the server and every full-size prompt would 400.
//
// A DISABLED entry (priority < minEnabledPriority) skips the probe
// entirely: an operator may import hundreds of disabled models, and
// probing each (2 s timeout) would be unacceptable.
func buildEntry(i int, e Entry, allowProbe bool) (*entryRow, error) {
	// Normalize the provider name once — Profiles keys, Probe's switch and
	// NewChatter's switch all expect lowercase, and yaml authors write
	// "Anthropic" / "anthropic" interchangeably.
	provider := strings.ToLower(strings.TrimSpace(e.Provider))
	baseURL := e.BaseURL
	keyEnv := e.APIKeyEnv
	preset, hasPreset := providers.Profiles[provider]
	if hasPreset {
		if baseURL == "" {
			baseURL = preset.BaseURL
		}
		if keyEnv == "" {
			keyEnv = preset.APIKeyEnv
		}
	}
	if baseURL == "" {
		return nil, fmt.Errorf("entries[%d] (%s): base_url not set and provider %q has no preset", i, e.Name, e.Provider)
	}
	apiKey := ""
	if keyEnv != "" {
		apiKey = os.Getenv(keyEnv)
	}

	disabled := e.Priority < minEnabledPriority

	// Probe every non-disabled entry: a live probe is ground truth for
	// the context window (it overrides the yaml value). Best-effort and
	// silent on failure — startup / reload must not block on a flaky
	// backend. Providers with no probe path (openrouter / openai /
	// custom) return an empty result cheaply. A disabled entry is
	// skipped — no point probing a backend pick() never routes to.
	var probe providers.ProbeResult
	if allowProbe && !disabled {
		probe = providers.Probe(context.Background(), provider, baseURL, apiKey, e.Model)
	}

	// Effective context window: the yaml declaration wins when set (the
	// operator's word is truth — it is the turn's compaction budget and
	// segment capacity via WindowFor), then a live probe, then the provider
	// preset, then the 128K default.
	var ctxWin int
	var ctxSrc string
	switch {
	case e.ContextWindow > 0:
		ctxWin, ctxSrc = e.ContextWindow, "yaml"
		if probe.ContextWindow > 0 && probe.ContextWindow < e.ContextWindow {
			slog.Warn("model entry: declared context_window exceeds the server's probed window — full-size prompts will overflow",
				"name", e.Name, "declared", e.ContextWindow, "probed", probe.ContextWindow)
		}
	case probe.ContextWindow > 0:
		ctxWin, ctxSrc = probe.ContextWindow, "probe:"+probe.Source
	case hasPreset && preset.DefaultContextWindow > 0:
		ctxWin, ctxSrc = preset.DefaultContextWindow, "profile-default"
	default:
		ctxWin, ctxSrc = fallbackContextWindow, "default"
	}
	concur, concSrc := e.Concurrency, "yaml"
	if concur == 0 {
		if probe.Concurrency > 0 {
			concur, concSrc = probe.Concurrency, "probe:"+probe.Source
		} else {
			concur, concSrc = fallbackConcurrency, "default"
		}
	}

	slog.Info("model entry resolved",
		"name", e.Name, "provider", provider, "model", e.Model,
		"context_window", ctxWin, "context_source", ctxSrc,
		"concurrency", concur, "concurrency_source", concSrc)

	chatter := providers.NewChatter(provider, baseURL, apiKey, e.MaxOutputTokens, e.ExtraBody)
	kind := strings.ToLower(strings.TrimSpace(e.Kind)) // 归一大小写:kind 精确匹配闭集常量(同 provider/B10)
	if kind == "" {
		kind = contract.KindLLM
	}
	// High clamp only — the low end is NOT clamped so a value below
	// minEnabledPriority survives as the disabled marker.
	//
	// NO small-window demotion (removed) and NO window-based
	// routing at all: priority is the operator's declaration and
	// the pool never rewrites it; the picker is window-blind — the turn sizes
	// its prompt to the chosen entry's declared window instead.
	priority := e.Priority
	if priority > priorityMax {
		priority = priorityMax
	}
	row := &entryRow{
		info: contract.ModelInfo{
			Name:              e.Name,
			Provider:          provider,
			Model:             e.Model,
			Kind:              kind,
			Priority:          priority,
			Tags:              lowerStrings(e.Tags),
			ContextWindow:     ctxWin,
			ContextSource:     ctxSrc,
			Concurrency:       concur,
			ConcurrencySource: concSrc,
			State:             "live",
		},
		chatter: chatter,
		baseURL: baseURL,
		apiKey:  apiKey,
	}
	if concur > 0 {
		row.sem = make(chan struct{}, concur)
	}
	return row, nil
}

// copyLiveness carries per-entry liveness state from a previous
// entryRow into a freshly-built one during an mtime-triggered reload:
// the error-timestamp sliding window, the consecutive-fail counter, the
// dead/cooldown deadline, the exponential-backoff exponent, the admin
// pause, the tentative (heartbeat-probed) flag, and the last-error text
// (the cooling reason shown read-only in the webui — without it a carried
// cooling entry would show as cooling with no explanation). The old row's
// fields are read under its mutex; `to` is not yet published so it needs
// no lock.
//
// NOT copied: the concurrency semaphore, in-flight counter, and usage
// counters — every new row gets fresh ones.
func copyLiveness(from, to *entryRow) {
	from.mu.Lock()
	errTimes := append([]time.Time(nil), from.errTimes...)
	consecutiveFails := from.consecutiveFails
	deadUntil := from.deadUntil
	consecutiveDeadEvents := from.consecutiveDeadEvents
	paused := from.paused
	tentative := from.tentative
	lastErr := from.lastErr
	from.mu.Unlock()

	to.errTimes = errTimes
	to.consecutiveFails = consecutiveFails
	to.deadUntil = deadUntil
	to.consecutiveDeadEvents = consecutiveDeadEvents
	to.paused = paused
	to.tentative = tentative
	to.lastErr = lastErr
}
