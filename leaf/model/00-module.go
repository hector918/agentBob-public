package model

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/model/modelcfg"
	"agentbob/leaf/model/providers"
	"agentbob/trunk"
)

// usageFlushPeriod is how often completed-hour usage buckets are persisted during
// the process lifetime. Without it, FlushUsage runs only at shutdown
// (FlushUsageFinal) and an unclean exit loses every accumulated bucket.
const usageFlushPeriod = 10 * time.Minute

// usageRetention bounds how long persisted hourly usage buckets are kept;
// usagePruneSweepPeriod is how often the retention sweep runs. bob_model_usage is
// telemetry (one row per entry per wall-clock hour) — past the window the old
// buckets are dead weight, so a Housekeeper task prunes them (matches skeleton's
// 90-day PruneModelUsage).
const (
	usageRetention        = 90 * 24 * time.Hour
	usagePruneSweepPeriod = 24 * time.Hour
)

// llmReqLogBudget is the soft cap for the LLM request log. The req/res
// JSONL records (system prompt redacted) are turn-varying tails — 8 MiB
// holds a few thousand turns for prompt-cache analysis before the oldest
// half is trimmed. Matches skeleton's budget.
const llmReqLogBudget int64 = 8 * 1024 * 1024

// Module is the model pool as a trunk module: it loads models.yaml, builds the
// MultiPool (empty when nothing is configured / the config is
// unusable), wires hourly usage persistence, and provides contract.ModelPool.
type Module struct {
	home  string
	pool  contract.ModelPool
	state atomic.Int32 // trunk.State
}

// New returns a model module reading $home/models.yaml.
func New(home string) *Module { return &Module{home: home} }

func (m *Module) Name() string { return "model" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.ModelPool](),
		// The image catalog rides along with the pool because it answers about the
		// same backends and is loaded from the same place. Its consumers are in
		// different leaf modules that may not import each other (the image_create
		// tool and modelgate), so one interface below both is the only shared home
		// the prompt guidance can have.
		trunk.TypeOf[contract.ImageCatalog](),
	}
}

// Needs the shared DB (usage tables), the slash registry (the /model command is
// the pool's own operator/debug surface), and the panel registry (the pool
// describes its webui layer — same publish-into-a-registry pattern as slash).
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.DB](),
		trunk.TypeOf[contract.SlashRegistry](),
		trunk.TypeOf[contract.PanelRegistry](),
	}
}

// Optional is false: the pool is a core capability. A missing / unusable
// models.yaml does NOT fail Start — it degrades to an EMPTY pool whose calls
// error while its mtime watch waits for a fixed config, so turns fail
// gracefully until then; only a DB migration failure aborts.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	// Enable the LLM request log (req/res JSONL, system prompt redacted)
	// for prompt-cache analysis. Best-effort; init failure self-disables
	// and never blocks the request path.
	providers.InitReqLogger(filepath.Join(m.home, "logs", "llm-requests.log"), llmReqLogBudget)

	db := trunk.Require[contract.DB](reg)
	if err := MigrateUsage(ctx, db); err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return fmt.Errorf("model: migrate usage: %w", err)
	}

	m.pool = m.buildPool(reg, db)
	trunk.Provide[contract.ModelPool](reg, m.pool)
	trunk.Provide[contract.ImageCatalog](reg, providers.ImageCatalog{})
	// Periodic usage persistence: drain completed-hour buckets to the store so an
	// unclean exit loses at most the in-progress hour (FlushUsageFinal at Stop
	// catches the rest). Optional — minimal configs may run without a housekeeper;
	// An empty pool's FlushUsage is a no-op.
	if hk, ok := trunk.TryRequire[trunk.Housekeeper](reg); ok {
		hk.Register(trunk.Task{
			Name:   "model-usage-flush",
			Period: usageFlushPeriod,
			Run:    func(ctx context.Context) error { m.pool.FlushUsage(ctx); return nil },
		})
		// Retention sweep for the usage table (persistent state → Housekeeper, not a
		// module timer). The store is stateless over db, so the task builds its own
		// (concrete type — Prune is not on the pool's ModelUsageStore flush interface).
		usage := modelUsageStorePG{db: db}
		hk.Register(trunk.Task{
			Name:   "model-usage-prune",
			Period: usagePruneSweepPeriod,
			Run: func(ctx context.Context) error {
				cutoff := clock.Now().Add(-usageRetention).Unix()
				n, err := usage.PruneModelUsage(ctx, cutoff)
				if err != nil {
					return err
				}
				if n > 0 {
					slog.Info("model: pruned usage rows", "removed", n)
				}
				return nil
			},
		})
	}
	trunk.Require[contract.SlashRegistry](reg).Register(contract.SlashCommand{
		Name:      "model",
		DescKey:   "slash.model.desc",
		AdminOnly: true,
		Handler:   m.slashModel,
	})
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.panel())
	m.state.Store(int32(trunk.StateReady))
	return nil
}

// buildPool loads models.yaml and constructs the pool. A missing / empty /
// unparseable config yields an EMPTY MultiPool, not a dead sentinel: every chat
// call errors, but the pool's mtime watch is alive — fixing models.yaml
// hot-loads on the next pick, so the webui editor's "reload" promise (and
// /model reload) work from a bad boot without a restart.
func (m *Module) buildPool(reg *trunk.Registry, db contract.DB) contract.ModelPool {
	configPath := filepath.Join(m.home, "models.yaml")
	// Image capability declarations: built-ins ship embedded; this directory lets an
	// operator retune a workflow, or add a style this build never heard of, without a
	// rebuild (mirrors the skills module's external dir). Absent = built-ins only.
	//
	// Re-read on every reload, INSIDE reloadFn: a workflow tweak and a models.yaml
	// tweak are the same operational gesture, and loading it only at Start would make
	// "drop the file, /model reload" silently do nothing (docs/image-create-tool.md §5.4).
	imageCapsDir := filepath.Join(m.home, "imagecaps")
	providers.LoadImageCapabilityOverrides(imageCapsDir)
	reloadFn := func() ([]Entry, []FallbackRule, error) {
		providers.LoadImageCapabilityOverrides(imageCapsDir)
		rc, rerr := modelcfg.LoadModels(m.home)
		if rerr != nil {
			return nil, nil, rerr
		}
		return entriesFromConfig(rc), fallbackFromConfig(rc), nil
	}

	entries, fallback, err := reloadFn()
	if err != nil {
		slog.Error("model: models.yaml is unusable — starting an empty pool (fix the file; it hot-loads)", "err", err)
		entries, fallback = nil, nil
	} else if len(entries) == 0 {
		slog.Warn("model: no models.yaml entries — starting an empty pool (add entries; the file hot-loads)", "home", m.home)
	}
	mp, err := NewMultiPool(entries, fallback, reloadFn, configPath)
	if err != nil {
		// Entries parsed but failed to build (a bad individual entry). Same
		// posture: empty pool with the watch alive beats a dead sentinel.
		slog.Error("model: pool build failed — starting an empty pool (fix models.yaml; it hot-loads)", "err", err)
		mp, err = NewMultiPool(nil, nil, reloadFn, configPath)
		if err != nil { // zero-entry construction has no failure path; defensive only
			panic(fmt.Sprintf("model: empty pool construction failed: %v", err))
		}
	}
	mp.SetModelUsageStore(NewModelUsageStorePG(db))         // per-backend ops telemetry
	mp.SetConsumptionReporter(lazyConsumptionReporter(reg)) // per-user billing (optional)
	mp.SetAdminLineResolver(lazyAdminLine(reg))             // all-dead alert (optional)
	return mp
}

// lazyAdminLine resolves the optional admin-summon line on first use. Lazy
// because the adminline module may Start after the model module; absent → the
// all-dead alert is a no-op.
func lazyAdminLine(reg *trunk.Registry) func() contract.AdminLine {
	var once sync.Once
	var l contract.AdminLine
	return func() contract.AdminLine {
		once.Do(func() { l, _ = trunk.TryRequire[contract.AdminLine](reg) })
		return l
	}
}

// lazyConsumptionReporter resolves the optional per-user billing reporter on
// first use. Lazy because the accounts module (which provides it) only Needs the
// DB, so the trunk may start it AFTER the model module; by the first model call
// it has registered if present. Absent → spend is not booked.
func lazyConsumptionReporter(reg *trunk.Registry) func() contract.ConsumptionReporter {
	var once sync.Once
	var r contract.ConsumptionReporter
	return func() contract.ConsumptionReporter {
		once.Do(func() { r, _ = trunk.TryRequire[contract.ConsumptionReporter](reg) })
		return r
	}
}

func (m *Module) Stop(ctx context.Context) error {
	if m.pool != nil {
		m.pool.FlushUsageFinal(ctx)
		m.pool.Close()
	}
	providers.CloseReqLogger()
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// entriesFromConfig bridges parsed models.yaml entries into pool entries — the
// ONE place config and the pool meet.
func entriesFromConfig(mc *modelcfg.ModelsConfig) []Entry {
	if mc == nil {
		return nil
	}
	out := make([]Entry, 0, len(mc.Entries))
	for _, e := range mc.Entries {
		out = append(out, Entry{
			Name:            e.Name,
			Provider:        e.Provider,
			Model:           e.Model,
			BaseURL:         e.BaseURL,
			APIKeyEnv:       e.APIKeyEnv,
			Kind:            e.Kind,
			Priority:        e.Priority,
			ContextWindow:   e.ContextWindow,
			Concurrency:     e.Concurrency,
			MaxOutputTokens: mc.EffectiveOutputCap(e),
			ExtraBody:       e.ExtraBody,
			Tags:            e.Tags,
		})
	}
	return out
}

func fallbackFromConfig(mc *modelcfg.ModelsConfig) []FallbackRule {
	if mc == nil || len(mc.Defaults.Fallback) == 0 {
		return nil
	}
	out := make([]FallbackRule, 0, len(mc.Defaults.Fallback))
	for _, r := range mc.Defaults.Fallback {
		// Lower-case in step with entry Tags (buildEntry): rule.To becomes the next
		// round's required tags, matched via hasAll against the (lowered) entry tags.
		out = append(out, FallbackRule{Tags: lowerStrings(r.Tags), To: lowerStrings(r.To)})
	}
	return out
}

// lowerStrings returns a fresh lower-cased copy of ss (nil stays nil). Tag sets are
// normalised to lower case so matching is case-insensitive; the required/prefer tags
// on the request side are code constants already in lower case.
func lowerStrings(ss []string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}
	return out
}

var _ trunk.Module = (*Module)(nil)
