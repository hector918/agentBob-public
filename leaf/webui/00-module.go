// Package webui is bob's browser admin surface as a trunk module. It is a
// GENERIC renderer: it knows nothing about any subsystem. Every module describes
// itself by registering a contract.Panel (runtime state + settings) into the
// PanelRegistry this module provides; the http layer collects the panels and the
// frontend renders them with a fixed field vocabulary. See docs/webui-trunk.md.
//
// The module imports only contract + trunk + stdlib — never another leaf/flow
// module. All subsystem data arrives through the closures each module puts in its
// own Panel.
package webui

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/trunk"
)

// lazyTakeover / lazyResume defer TryRequire of these OPTIONAL capabilities to first
// use. webui Starts BEFORE the modules that provide them (leaf/tools, leaf/session both
// Need the PanelRegistry webui provides, so the topo order puts them later); resolving
// at Start sees nil and silently kills takeover. The first request / slash invocation
// runs long after boot, when the provider is in the registry. Concrete (not generic)
// so the arch optional-edge scanner still attributes each TryRequire site to webui.
func lazyTakeover(reg *trunk.Registry) func() contract.BrowserTakeover {
	var once sync.Once
	var v contract.BrowserTakeover
	return func() contract.BrowserTakeover {
		once.Do(func() { v, _ = trunk.TryRequire[contract.BrowserTakeover](reg) })
		return v
	}
}

func lazyControlHold(reg *trunk.Registry) func() contract.BrowserControlHold {
	var once sync.Once
	var v contract.BrowserControlHold
	return func() contract.BrowserControlHold {
		once.Do(func() { v, _ = trunk.TryRequire[contract.BrowserControlHold](reg) })
		return v
	}
}

func lazyResume(reg *trunk.Registry) func() contract.SessionResume {
	var once sync.Once
	var v contract.SessionResume
	return func() contract.SessionResume {
		once.Do(func() { v, _ = trunk.TryRequire[contract.SessionResume](reg) })
		return v
	}
}

// Module serves the dashboard and provides the PanelRegistry. It is critical
// (Optional false) so panel-owning modules can hard-Need the registry — but its
// Start NEVER fails on an http bind problem (it logs and serves nothing), so a
// port conflict degrades the UI without aborting the agent.
type Module struct {
	addr        string
	reg         *registry
	auth        *Auth
	slash       contract.SlashRegistry
	takeover    func() contract.BrowserTakeover    // lazy: leaf/tools provides it, but Starts AFTER webui
	controlHold func() contract.BrowserControlHold // lazy: leaf/tools (browser yield gate)
	resume      func() contract.SessionResume      // lazy: leaf/session (takeover hand-back wake)
	srv         *http.Server
	cancel      context.CancelFunc
	state       atomic.Int32 // trunk.State
}

// New returns a webui module bound to addr ("" or "off" disables the http server
// but still provides the registry).
func New(addr string) *Module { return &Module{addr: addr, reg: newRegistry()} }

func (m *Module) Name() string { return "webui" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.PanelRegistry](),
		trunk.TypeOf[contract.TakeoverMinter](),
	}
}

// Needs the slash registry: webui registers /webui (mints the one-time admin
// unlock token) into the shared command table — same publish pattern as model.
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.SlashRegistry](),
		trunk.TypeOf[contract.ClaimTokens](), // one-time webui unlock tokens
	}
}

func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

// Start provides the registry (always), then best-effort brings up the http
// server. A listen failure is logged, not returned — the registry stays usable
// so panel registration and the rest of the agent proceed.
func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	// Auth's one-time unlock tokens live in the claim-token facility (hard Need →
	// registered before this Start). m.auth is only touched post-Start (provide +
	// server + slash handlers), so building it here is race-free.
	m.auth = NewAuth(trunk.Require[contract.ClaimTokens](reg))
	trunk.Provide[contract.PanelRegistry](reg, m.reg)
	trunk.Provide[contract.TakeoverMinter](reg, m.auth) // escalate_to_coo mints takeover tokens through here
	m.slash = trunk.Require[contract.SlashRegistry](reg)
	// BrowserTakeover (leaf/tools) is provided by modules that
	// Start AFTER webui — they Need the PanelRegistry webui provides, so the topo order
	// puts them later. Resolving them HERE would always see nil and silently kill
	// takeover; resolve them LAZILY instead — the first request / slash invocation runs
	// long after boot, by when the providers are up.
	m.takeover = lazyTakeover(reg)
	m.controlHold = lazyControlHold(reg)
	m.resume = lazyResume(reg)

	m.slash.Register(contract.SlashCommand{
		Name:      "webui",
		DescKey:   "slash.webui.desc",
		AdminOnly: true,
		Handler:   m.slashWebui,
	})
	// /takeover is always registered; its handler reports "unavailable" if no browser
	// backend resolved (can't be gated here — the provider Starts after webui).
	m.slash.Register(contract.SlashCommand{
		Name:      "takeover",
		DescKey:   "slash.takeover.desc",
		AdminOnly: false,
		Handler:   m.slashTakeover,
	})

	if m.addr == "" || m.addr == "off" {
		slog.Info("webui: http server disabled (addr off); registry still provided")
		m.state.Store(int32(trunk.StateReady))
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	s := &server{reg: m.reg, auth: m.auth, slash: m.slash, takeover: m.takeover, controlHold: m.controlHold, resume: m.resume, start: time.Now()}

	ln, err := net.Listen("tcp", m.addr)
	if err != nil {
		slog.Warn("webui: listen failed; dashboard disabled (agent continues)", "addr", m.addr, "err", err)
		m.state.Store(int32(trunk.StateReady))
		return nil
	}
	// Only start the background loops once the listener is up — on a bind failure
	// (degraded boot above) there is no consumer, so launching them earlier would
	// leave them polling for the process lifetime.
	// The poller fills the cache on its first tick (immediately, but OFF the boot
	// path) — handleState serves an empty snapshot until then. So a slow / deadlocked
	// panel degrades the dashboard without ever blocking trunk boot.
	go s.pollLoop(ctx, refreshInterval)
	go s.authSweepLoop(ctx, authSweepInterval) // separate bucket from panel polling
	if !isLoopback(m.addr) {
		slog.Warn("webui: bound to a non-loopback address — reachable beyond localhost (expected behind a tunnel / on a trusted LAN; data panels are admin-only, writes need a /webui token; NOTE the session cookie travels plaintext on bare HTTP — prefer an HTTPS tunnel)", "addr", m.addr)
	}
	// BaseContext ties every request context to the module ctx: Stop's m.cancel()
	// then cancels in-flight handlers (notably the long-lived SSE screencast loop,
	// which otherwise keeps heartbeating and makes Shutdown wait forever).
	m.srv = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if err := m.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Warn("webui: server exited", "err", err)
		}
	}()
	// Boot convenience: push a fresh admin unlock token to the admin line so the
	// operator needn't run /webui after every restart. Only when the server is up.
	go m.announceBootToken(ctx, reg)
	slog.Info("webui serving", "addr", m.addr)
	m.state.Store(int32(trunk.StateReady))
	return nil
}

// announceBootToken mints a one-time admin (coverage "*") unlock token at startup and
// pushes it to the AdminLine outlet — so a restart auto-DMs the operator the token
// instead of making them run /webui. AdminLine is provided by a module that Starts
// AFTER webui, so poll briefly for it (don't cache a nil); skip silently when no admin
// outlet is wired or the run is cancelled. Token TTL is the usual 5 min.
func (m *Module) announceBootToken(ctx context.Context, reg *trunk.Registry) {
	var al contract.AdminLine
	for i := 0; i < 40; i++ { // ~10s, 250ms apart — adminline Starts shortly after webui
		if a, ok := trunk.TryRequire[contract.AdminLine](reg); ok {
			al = a
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
	if al == nil {
		return // no admin line wired — nothing to push to
	}
	// Let the outlet's source finish connecting before pushing — telegram serves ~1-2s
	// after boot, and adminline does not retry a failed delivery (a too-early push is lost).
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}
	tok := m.auth.Mint()
	if tok == "" {
		return
	}
	al.Notify(ctx, contract.AdminInfo, "webui", "🔓 webui admin unlock token (valid 5m): %s  —  paste at %s", tok, m.addr)
}

func (m *Module) Stop(ctx context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	if m.srv != nil {
		_ = m.srv.Shutdown(ctx)
	}
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// isLoopback reports whether addr's host is a loopback address (the default,
// safe bind). A non-loopback bind exposes the unauthenticated dashboard to the
// network — worth a warning.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	// An empty host (the ":port" shorthand) binds ALL interfaces (wildcard /
	// 0.0.0.0) — NOT loopback — so let it fall through to ParseIP("")==nil → false
	// and fire the exposure warning. Only the literal "localhost" is loopback here.
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var _ trunk.Module = (*Module)(nil)
