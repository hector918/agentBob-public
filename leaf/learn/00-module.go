// Package learn is the text-space experience optimizer (参考 microsoft/SkillOpt):
// ONE engine improves N "trainable text" targets — a tool's description tail, a
// skill doc, a role playbook — from observed run traces. The owning modules expose
// their targets through a contract.LearnSource; learn owns the single model call
// (the distiller) and the anti-rot policy, so those modules stay dumb and
// model-free. See docs/learn.md.
//
// SLICE 1 (trunk-rebuild): the engine + the contract.Learnable/LearnSource seam +
// the integral-rewrite guard (no replay gate). The only registered source is
// leaf/tools (the tool-description target). skill (replay-gated via contract.Scorer)
// and agora role land in later slices by registering their own source — additive.
package learn

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/trunk"
)

// distillPeriod is how often the engine sweeps every target. Learning is a slow
// accretion; a long period keeps it off the hot path and bounds model spend.
const distillPeriod = 30 * time.Minute

// Module is the learn engine as a trunk module: it provides contract.LearnRegistry
// (the owning modules register their sources into it) and registers one
// housekeeping sweep that distills every target.
type Module struct {
	eng   *engine
	state atomic.Int32 // trunk.State
}

// New returns a learn module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "learn" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.LearnRegistry]()}
}

// Needs the webui PanelRegistry so it can register its dashboard panel at Start.
// Provided by the critical webui module (always present), so this hard Need can't
// skip-trap an Optional module.
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.PanelRegistry]()}
}

// Optional is true: without learn, every target simply keeps its authored text —
// the pipeline is unaffected, nothing learns.
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	m.eng = &engine{
		model:   lazyModelPool(reg),
		lastRun: map[string]time.Time{},
	}
	// Provided BEFORE the owning modules' Start so they can register their source
	// (lifecycle preserves registration order among no-dep modules; learn is
	// registered before tools in cmd/bob).
	trunk.Provide[contract.LearnRegistry](reg, m.eng)

	// Self-describe to the webui (the engine is built). Hard-Need, so the registry
	// is guaranteed present.
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.panel())

	// The distill sweep rides the trunk Housekeeper (mechanism 4). It runs at the same
	// default Priority (0) as every other registered task — there is no yield-to-
	// persistence ordering wired today (all tasks share Priority 0, so the due-batch
	// tie-break never differentiates).
	if hk, ok := trunk.TryRequire[trunk.Housekeeper](reg); ok {
		hk.Register(trunk.Task{
			Name:   "learn-distill",
			Period: distillPeriod,
			Run:    m.eng.runCycle,
		})
	}

	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// lazyModelPool resolves the optional distiller pool, caching the first NON-nil
// result for process life. The model module registers before learn (no dep edge
// from learn to model), so it is normally present on the first call; but if the
// pool is ever absent we RE-RESOLVE on each call rather than caching nil, so a
// later-arriving pool self-heals. Absent → distillation is skipped.
func lazyModelPool(reg *trunk.Registry) func() contract.ModelPool {
	var mu sync.Mutex
	var cached contract.ModelPool
	return func() contract.ModelPool {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil {
			return cached
		}
		p, _ := trunk.TryRequire[contract.ModelPool](reg)
		if p != nil {
			cached = p // freeze only a real pool; keep re-resolving while still nil
		}
		return p
	}
}

var _ trunk.Module = (*Module)(nil)
