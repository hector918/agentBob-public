// Package accounts is the identity & per-account policy authority. It resolves an
// inbound event's sender to a flow assignment (the router's per-account routing)
// and books each turn's model-token usage to the sender's billing handle. It
// provides contract.Accounts; the flow layer consumes it (router for routing,
// flow tail for billing). The turn core never sees it — usage accounting is a
// Ring-2 host concern, not loop mechanism.
//
// It also owns identity binding: it registers the /accounts + /bindaccount management
// commands, including /accounts bindto — the binding action a mint token's command batch
// invokes (the gate authenticates the token, then runs its batch through the slash
// registry). Binding's auto-allowlist side effect goes through the optional
// contract.AccessGranter (gate).
//
// MINIMAL SLICE (trunk-rebuild): identity resolution + usage + binding +
// management. The webui roster, account merge, status (pause/disable) and
// language are deferred. With no account bound, AccountFor returns ok=false
// (router self-selects) and usage records onto the raw handle.
package accounts

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/leaf/accounts/store"
	"agentbob/trunk"
)

// flushInterval is the usage-buffer drain cadence: one store write per active
// handle per window, instead of a read-modify-write per turn.
const flushInterval = time.Minute

// codeTTL is how long a minted binding code stays redeemable.
const codeTTL = 15 * time.Minute

// Module is the accounts core as a trunk module.
type Module struct {
	mgr   *Manager
	state atomic.Int32 // trunk.State
}

// New returns an accounts module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "accounts" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.Accounts](),
		trunk.TypeOf[contract.ConsumptionReporter](),
		trunk.TypeOf[contract.AccountProvisioner](),
		trunk.TypeOf[contract.APIKeys](),
	}
}

// Needs the shared DB (persistence), the slash registry (the /accounts + /bindaccount
// commands, incl. the /accounts bindto a token's batch invokes), the panel registry
// (webui), and the claim-token facility (binding-code mint). The access granter (gate) is
// consumed optionally/lazily — binding works without it.
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.DB](),
		trunk.TypeOf[contract.SlashRegistry](),
		trunk.TypeOf[contract.PanelRegistry](),
		trunk.TypeOf[contract.ClaimTokens](), // binding-code mint
	}
}

// Optional is true: with no accounts module the flow self-selects and usage is
// dropped — the pipeline still runs. A boot failure here degrades, not aborts.
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	db := trunk.Require[contract.DB](reg)
	if err := store.Migrate(ctx, db); err != nil {
		// F39: the store is the routing authority. If it fails to migrate we must NOT
		// vanish silently — the router would then see contract.Accounts as absent and
		// fall OPEN (an onboarding-source stranger gets the full flow). Provide a
		// FAIL-CLOSED stand-in (every sender reads as unbound) so the router funnels
		// non-admins to intro/refuse while admins still pass the normal floor. Return
		// nil (not the error) so the trunk continues degraded WITHOUT tripping its
		// "Provide-then-error" abort guard; Health() stays Failed for the ops view.
		// Only contract.Accounts is Provided — ConsumptionReporter / AccountProvisioner
		// stay absent (usage dropped, onboarding writes unavailable), which their lazy
		// consumers degrade past. A genuinely no-accounts deployment never registers this
		// module at all, so its documented "unrestricted" mode (acc==nil) is untouched.
		m.state.Store(int32(trunk.StateFailed))
		trunk.Provide[contract.Accounts](reg, degradedAccounts{})
		slog.Error("accounts: store migrate failed — running FAIL-CLOSED (senders gated to intro/refuse until a restart migrates cleanly)", "err", err)
		return nil
	}
	st := store.NewPG(db)
	m.mgr = &Manager{
		store:   st,
		buffer:  newHandleUsageBuffer(st),
		tokens:  trunk.Require[contract.ClaimTokens](reg),
		ttl:     codeTTL,
		granter: lazyAccessGranter(reg),
		agora:   lazyAgora(reg),
	}
	trunk.Provide[contract.Accounts](reg, m.mgr)
	trunk.Provide[contract.ConsumptionReporter](reg, m.mgr)
	trunk.Provide[contract.AccountProvisioner](reg, m.mgr)
	trunk.Provide[contract.APIKeys](reg, m.mgr)

	// Register the management surface (the registry holds the table; the manager
	// holds the logic). /accounts is non-admin so its "new" path is open to any
	// already-admitted sender — a first-contact sender on a closed source is
	// dropped by the gate before slash, so first-contact binding is code-only
	// (the account-link handler). Admin subcommands gate on IsAdmin inside. /bindaccount is
	// self-service.
	slash := trunk.Require[contract.SlashRegistry](reg)
	slash.Register(contract.SlashCommand{Name: "accounts", DescKey: "slash.accounts.desc", Handler: m.mgr.slashAccounts})
	slash.Register(contract.SlashCommand{Name: "bindaccount", DescKey: "slash.bindaccount.desc", Handler: m.mgr.slashBind})
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.panel())

	// Drain the in-memory usage buffer to the store periodically. This is a buffer→DB
	// WRITE (not a prune), but it rides the trunk Housekeeper so every periodic
	// DB-touching job is centrally scheduled/staggered — same choice as the model
	// module's "model-usage-flush". Optional: a minimal config without a housekeeper
	// just relies on Stop's final drain. The final-window drain still happens in Stop.
	if hk, ok := trunk.TryRequire[trunk.Housekeeper](reg); ok {
		hk.Register(trunk.Task{
			Name:   "accounts-usage-flush",
			Period: flushInterval,
			Run:    func(ctx context.Context) error { m.mgr.buffer.Flush(ctx); return nil },
		})
	}

	m.state.Store(int32(trunk.StateReady))
	return nil
}

// Stop does a final drain so a graceful shutdown doesn't lose the last window's
// usage. (The Housekeeper is already stopped by the trunk before module Stop, so the
// periodic flush task won't run again — this final Flush captures the tail.)
func (m *Module) Stop(ctx context.Context) error {
	if m.mgr != nil {
		m.mgr.buffer.Flush(ctx)
	}
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// lazyAccessGranter resolves the optional gate access writer on first use. Lazy
// because the gate may Start before or after accounts; by the first bind both
// have registered. Absent → binding records identity but cannot auto-allowlist.
func lazyAccessGranter(reg *trunk.Registry) func() contract.AccessGranter {
	var once sync.Once
	var g contract.AccessGranter
	return func() contract.AccessGranter {
		once.Do(func() { g, _ = trunk.TryRequire[contract.AccessGranter](reg) })
		return g
	}
}

// lazyAgora resolves the OPTIONAL contract.Agora (org authority) on first use — agora
// may start after accounts and is absent on a no-agora deployment. Approve reads
// ScopeIsAgora through it; nil → approval grants normal only.
func lazyAgora(reg *trunk.Registry) func() contract.Agora {
	var once sync.Once
	var a contract.Agora
	return func() contract.Agora {
		once.Do(func() { a, _ = trunk.TryRequire[contract.Agora](reg) })
		return a
	}
}

// degradedAccounts is the fail-CLOSED stand-in the module Provides when its store
// Migrate fails at boot (F39). It answers every routing query as "unbound / unknown"
// so the router funnels non-admin senders to intro/refuse instead of granting them the
// full flow on an authority that isn't actually live — while a genuinely no-accounts
// deployment (this module never registered → contract.Accounts absent) keeps its
// documented unrestricted behavior. Admins are platform-known and still pass the
// router's normal floor. LangFor / RememberLang degrade to no-ops.
type degradedAccounts struct{}

func (degradedAccounts) AccountFor(context.Context, contract.MessageEvent) (contract.AccountInfo, bool) {
	return contract.AccountInfo{}, false
}
func (degradedAccounts) LangFor(context.Context, string, string) (string, bool) { return "", false }
func (degradedAccounts) RememberLang(context.Context, string, string, string)   {}

var (
	_ trunk.Module      = (*Module)(nil)
	_ contract.Accounts = degradedAccounts{}
)
