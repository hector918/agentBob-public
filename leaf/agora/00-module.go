package agora

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/leaf/agora/store"
	"agentbob/trunk"
)

// Module is agora as a trunk module: it owns the bob_agora_* pg tables, holds
// the in-process org mirror (OrgCache) + the InboxRouter, and provides
// contract.Agora. It is the OWN authorization authority for agora turns (it does
// NOT route through warrant — docs/agora-port.md §5.3).
//
// Stage 2c adds the data-creation command surface: the operation layer
// (60-operations.go) + the /agora slash adapter (70-slash.go) registered at
// Start. The dispatcher (④b 去程), bridge outbound, and role-skill-fork are
// still NOT here — agora has no goroutine in this stage (Start builds RAM state
// + registers slash, Stop just marks stopped).
type Module struct {
	home    string // $BOB_HOME — roots the role-learning files (80-learn.go)
	org     *OrgCache
	router  *InboxRouter
	store   *store.PG
	impl    *Impl
	learn   *memberLearn                  // failure-driven role-guidance learning (80-learn.go)
	granter func() contract.AccessGranter // lazy/optional gate granter — /agora inbox wirehere allowlists the wiring sender
	tokens  contract.ClaimTokens          // claim-token facility: wire-token mint
	// live catalog getters (lazy/optional) — the webui permissions editor enumerates
	// them to merge the current tools/skills into a company's grant matrix.
	toolCat  func() contract.ToolCatalog
	skillCat func() contract.SkillCatalog
	// sessionMgr lazily resolves the OPTIONAL contract.SessionManager — the
	// in-flight probe DeleteCompany consults to refuse a delete that would yank a
	// live turn's inbox. Lazy/optional (mirrors lazyAccessGranter): agora must
	// start without it. Absent → DeleteCompany fails closed (can't verify).
	sessionMgr func() contract.SessionManager
	state      atomic.Int32 // trunk.State
}

// New returns an agora module. It self-manages its pg tables (Needs only
// contract.DB); the tool/skill catalogs are LAZY/optional (TryRequire thunks),
// so agora starts without them.
func New(home string) *Module { return &Module{home: home} }

func (m *Module) Name() string { return "agora" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.Agora](),
		trunk.TypeOf[contract.AgoraSend](),
		trunk.TypeOf[contract.MemberFailureSink](), // the agora flow's role-failure sink (learn)
	}
}

// Needs the DB + the SlashRegistry (the /agora command surface is a HARD
// dependency — agora's data-creation commands register at Start). The tool +
// skill catalogs are SOFT dependencies (lazyToolCatalog / lazySkillCatalog via
// TryRequire), NOT declared Needs: agora must start without them, and
// AuthorizeTools/Skills then return an empty (fail-closed) set. See
// docs/agora-port.md §5.3.
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.DB](),
		trunk.TypeOf[contract.PanelRegistry](),
		trunk.TypeOf[contract.SlashRegistry](),
		trunk.TypeOf[contract.ClaimTokens](), // wire-token mint
	}
}

// Optional is true: a deployment with no agora bootstrapped simply runs without
// it (the agora flow's Accepts never fires) — fail-open.
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	db := trunk.Require[contract.DB](reg)
	if err := store.Migrate(ctx, db); err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return err
	}
	m.store = store.NewPG(db)

	m.org = NewOrgCache()
	if err := m.org.Load(ctx, m.store); err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return err
	}
	m.router = NewInboxRouter(m.org, m.store)
	if err := m.router.Reload(ctx); err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return err
	}

	m.toolCat = lazyToolCatalog(reg)
	m.skillCat = lazySkillCatalog(reg)
	m.sessionMgr = lazySessionManager(reg)
	// Role-guidance learning (docs/wip-member-learn.md): built before the Impl so
	// TurnContext can read a role's guidance to inject; only when a writable home exists.
	if m.home != "" {
		m.learn = newMemberLearn(m.home, m.org, m.router)
		trunk.Provide[contract.MemberFailureSink](reg, m.learn)
		if lr, ok := trunk.TryRequire[contract.LearnRegistry](reg); ok {
			lr.AddSource(memberLearnSource{fail: m.learn.fail, ins: m.learn.ins})
		}
	}
	m.impl = newImpl(m.org, m.router, m.learn)
	trunk.Provide[contract.Agora](reg, m.impl)
	// AgoraSend (send_message Stage B seam) is the SAME impl: it shares the OrgCache +
	// InboxRouter + buildDirectory, so target resolution / channel auth stays the same
	// answer as the directory the turn sees. send_message resolves it LAZILY.
	trunk.Provide[contract.AgoraSend](reg, m.impl)

	// Register the /agora data-creation command surface (the registry holds the
	// table; the operation layer holds the logic).
	m.registerSlash(trunk.Require[contract.SlashRegistry](reg))

	// Inbox wire-tokens: mint via /agora inbox wire (a batch carrying /agora inbox
	// wirehere <id>); redeemed at the target chat, where wirehere binds THAT chat to the
	// inbox + allowlists the sender via the granter. The granter is lazy/optional.
	m.tokens = trunk.Require[contract.ClaimTokens](reg)
	m.granter = lazyAccessGranter(reg)

	// Self-describe to the webui (the org is loaded). Hard-Need PanelRegistry, so
	// the registry is guaranteed present.
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.panel())

	m.state.Store(int32(trunk.StateReady))
	return nil
}

// Stop has no goroutine to drain in ④a (the dispatcher is ④b) — just mark stopped.
func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// The lazy* thunks resolve OPTIONAL providers on first NON-nil use. They must NOT
// latch a negative: agora starts before its soft deps (a catalog / the session
// module can Start after agora), so a resolve attempt during that boot window
// returns nil. A sync.Once would cache that nil for the whole process lifetime —
// e.g. sessionMgr latching nil would leave DeleteCompany permanently unable to
// verify in-flight turns. Instead retry-while-nil under a small mutex; once
// resolved the value is cached (providers never de-register). TryRequire is a cheap
// RLock map read and these sit only on cold slash/panel paths.

// lazyAccessGranter resolves the OPTIONAL contract.AccessGranter (gate) — the wire
// redeemer allowlists the redeeming chat after a successful bind. Absent → nil
// (allowlist skipped, best-effort side effect).
func lazyAccessGranter(reg *trunk.Registry) func() contract.AccessGranter {
	var mu sync.Mutex
	var g contract.AccessGranter
	return func() contract.AccessGranter {
		mu.Lock()
		defer mu.Unlock()
		if g == nil {
			g, _ = trunk.TryRequire[contract.AccessGranter](reg)
		}
		return g
	}
}

// lazySessionManager resolves the OPTIONAL contract.SessionManager — the in-flight
// probe for DeleteCompany (RunningAtScope). Absent → nil (DeleteCompany fails
// closed: it can't verify in-flight, so it refuses rather than risk yanking a live
// turn's inbox). Session starts after agora via stoma, so the retry-while-nil above
// matters most here: a boot-window resolve must not latch nil.
func lazySessionManager(reg *trunk.Registry) func() contract.SessionManager {
	var mu sync.Mutex
	var mgr contract.SessionManager
	return func() contract.SessionManager {
		mu.Lock()
		defer mu.Unlock()
		if mgr == nil {
			mgr, _ = trunk.TryRequire[contract.SessionManager](reg)
		}
		return mgr
	}
}

// lazyToolCatalog resolves the OPTIONAL contract.ToolCatalog. Agora does NOT
// hard-Need it (it self-authorizes from company grants and must start without a
// catalog); the first lookup after tools started finds it. Absent → nil
// (AuthorizeTools returns an empty fail-closed set).
func lazyToolCatalog(reg *trunk.Registry) func() contract.ToolCatalog {
	var mu sync.Mutex
	var cat contract.ToolCatalog
	return func() contract.ToolCatalog {
		mu.Lock()
		defer mu.Unlock()
		if cat == nil {
			cat, _ = trunk.TryRequire[contract.ToolCatalog](reg)
		}
		return cat
	}
}

// lazySkillCatalog resolves the OPTIONAL contract.SkillCatalog — symmetric with
// lazyToolCatalog. Absent → nil (AuthorizeSkills fail-closed).
func lazySkillCatalog(reg *trunk.Registry) func() contract.SkillCatalog {
	var mu sync.Mutex
	var cat contract.SkillCatalog
	return func() contract.SkillCatalog {
		mu.Lock()
		defer mu.Unlock()
		if cat == nil {
			cat, _ = trunk.TryRequire[contract.SkillCatalog](reg)
		}
		return cat
	}
}

var _ trunk.Module = (*Module)(nil)
