package warrant

import (
	"context"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/trunk"
)

// lazyAdminLine resolves the optional admin-summon line on first use. Lazy
// because the adminline module may Start AFTER warrant (it isn't a declared
// Need); by the first turn it has registered if present. Absent → no alert.
func lazyAdminLine(reg *trunk.Registry) func() contract.AdminLine {
	var once sync.Once
	var l contract.AdminLine
	return func() contract.AdminLine {
		once.Do(func() { l, _ = trunk.TryRequire[contract.AdminLine](reg) })
		return l
	}
}

// warrantLogBudget is the soft cap for the warrant decision log. Records
// are ~200 bytes each, so 10 MiB holds weeks of audit before the oldest
// half is trimmed.
const warrantLogBudget int64 = 10 * 1024 * 1024

// Module is the capability authority as a trunk module: it loads the policy
// matrix, reconciles it against the registered tools, and provides
// contract.Warrant. The flow consumes it to project the turn's authorized tool
// bag; with no warrant the flow projects ZERO tools/skills (fail-CLOSED — see
// flow/normal toolBag/skillBag), not the whole catalog.
type Module struct {
	home  string
	mgr   *Manager
	state atomic.Int32 // trunk.State
}

// New returns a warrant module reading $home/permissions.yaml.
func New(home string) *Module { return &Module{home: home} }

func (m *Module) Name() string { return "warrant" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.Warrant]()}
}

// Needs the tool catalog (reconcile grant rows + look tools up when projecting
// the bag), the channel pool (keeps the stateful exec sessions it vends), and the
// webui PanelRegistry (registers its permissions panel at Start; provided by the
// critical webui module, so the hard Need can't skip-trap this Optional module).
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.ToolCatalog](),
		trunk.TypeOf[contract.ChannelPool](),
		trunk.TypeOf[contract.PanelRegistry](),
		trunk.TypeOf[contract.SlashRegistry](),
	}
}

// Optional is true: a boot failure (bad permissions.yaml) degrades to an absent
// warrant — which the flow reads as fail-CLOSED (zero tools/skills), not as
// no-enforcement-full-catalog — rather than aborting boot.
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	// Enable the warrant decision log (one JSONL line per Check). Best-
	// effort; init failure self-disables and never blocks the gate.
	InitDecisionLogger(filepath.Join(m.home, "logs", "warrant.log"), warrantLogBudget)

	cat := trunk.Require[contract.ToolCatalog](reg)
	// FAIL CLOSED: a present-but-corrupt permission source yields a deny-all matrix
	// (broken=true), never an allow-all gap — and we must NOT reconcile/overwrite the
	// operator's broken file. A boot error no longer makes warrant go absent (which
	// the flow would read as allow-all). deny-all is the fail-closed engine behaviour
	// (allows() denies unknowns); an admin is summoned on the first turn.
	store := newFileStore(m.home)
	p, policyBroken := LoadOrFailClosed(store)
	skillCat, _ := trunk.TryRequire[contract.SkillCatalog](reg)
	if policyBroken {
		slog.Error("warrant: permissions source unusable — FAILING CLOSED (all tools/skills denied until fixed + restart)")
	} else {
		toolNames := make([]string, 0)
		for _, s := range cat.Specs() {
			toolNames = append(toolNames, s.Name)
		}
		changed := reconcile(p, "tool", toolNames, false)
		// Skills are projected by the same matrix (skill:use:<name>); reconcile their
		// grant rows too. Optional — a deployment may have no skills module. ORDER
		// (S-4): this reconcile only sees skills if the skills module STARTED first;
		// guaranteed by cmd registration order ("skills before warrant") + the
		// optional-edge ledger (arch wantOptional), NOT a hard Need — a hard Need
		// would let an absent (Optional) skills module skip this (Optional) warrant
		// entirely and turn tool enforcement allow-all. A miss here fails CLOSED
		// (skills denied), not open, and surfaces on the first skill use.
		if skillCat != nil {
			skillNames := make([]string, 0)
			for _, info := range skillCat.List() {
				skillNames = append(skillNames, info.Name)
			}
			// A DEGRADED catalog (external scan fell back to builtins-only) lists a
			// known-incomplete set — reconcile add-only so the temporarily-unseen
			// external skills' grant rows (incl. admin-flipped ones) survive the blip.
			changed = reconcile(p, "skill", skillNames, skillCatalogDegraded(skillCat)) || changed
		}
		if changed {
			if werr := store.Save(p); werr != nil {
				// The matrix is active in memory; a write failure just means the new
				// grant rows aren't persisted yet — log, don't abort.
				slog.Warn("warrant: reconcile could not persist permissions — in-memory policy active", "err", werr)
			}
		}
	}
	// FAIL CLOSED, same posture as the permissions matrix above: a PRESENT-but-
	// unparseable spaces.yaml must not silently reroute an ssh space's terminal
	// to a local empty dir ("ran on prod" that ran nowhere). A missing file is
	// the normal all-local default.
	spaces, serr := loadSpaces(m.home)
	spacesBroken := false
	if serr != nil {
		slog.Error("warrant: spaces.yaml unparseable — FAILING CLOSED (space exec/file disabled until fixed + restart)", "err", serr)
		spaces = &spaceConfig{spaces: map[string]spaceBackend{}}
		spacesBroken = true
	}
	broker, _ := trunk.TryRequire[contract.Broker](reg) // remote spaces need it; local don't
	m.mgr = &Manager{
		catalog:      cat,
		skillCat:     skillCat,
		home:         m.home,
		pool:         trunk.Require[contract.ChannelPool](reg),
		broker:       broker,
		spaces:       spaces,
		spacesBroken: spacesBroken,
		adminLine:    lazyAdminLine(reg),
	}
	// policy + policyBroken are atomics (see Manager doc): seed them post-build so
	// /permission reload can later swap a fresh matrix in race-free.
	m.mgr.policy.Store(p)
	m.mgr.policyBroken.Store(policyBroken)
	trunk.Provide[contract.Warrant](reg, m.mgr)
	// exec-home retention (see 45-exechome-sweep.go): persistent disk state, so it
	// rides the trunk Housekeeper rather than a module ticker.
	if hk, ok := trunk.TryRequire[trunk.Housekeeper](reg); ok {
		home := m.home
		hk.Register(trunk.Task{
			Name:   "warrant-exec-home-sweep",
			Period: execHomeSweepPeriod,
			Run: func(ctx context.Context) error {
				if n := sweepExecHome(ctx, home, time.Now().Add(-execHomeRetention)); n > 0 {
					slog.Info("warrant: swept exec-home", "removed", n)
				}
				return nil
			},
		})
	}
	// Self-describe to the webui (the Manager + live matrix are seeded). Hard-Need,
	// so the registry is guaranteed present.
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.mgr.panel())
	// /tools + /skills: list what the SENDER may use — the per-identity projection
	// is warrant's own concern, so it owns these (vs cramming them into /session,
	// which sits below warrant and would have to reach up for the catalogs).
	slashReg := trunk.Require[contract.SlashRegistry](reg)
	slashReg.Register(contract.SlashCommand{
		Name:    "tools",
		DescKey: "slash.tools.desc",
		Handler: m.mgr.slashTools,
	})
	slashReg.Register(contract.SlashCommand{
		Name:    "skills",
		DescKey: "slash.skills.desc",
		Handler: m.mgr.slashSkills,
	})
	// /permission reload: re-read permissions.yaml into the live matrix without a
	// restart (file backend has no watcher). Admin-only — it's an operator action.
	slashReg.Register(contract.SlashCommand{
		Name:    "permission",
		DescKey: "slash.permission.desc",
		Handler: m.mgr.slashPermission,
	})
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	CloseDecisionLogger()
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

var _ trunk.Module = (*Module)(nil)
