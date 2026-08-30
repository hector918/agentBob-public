// Package modelgate exposes the model pool as an OpenAI-compatible HTTP API so an
// external client (open-webui, a custom agent, any OpenAI SDK) can use the entries
// in bob's pool. It is a thin STRATEGY over two contracts: contract.ModelPool
// (the routing target) and contract.APIKeys (bearer-token auth + per-key model
// allowlist + billing account, provided by leaf/accounts). It is NOT a chat entry
// point — it never touches flow / turn / session; it forwards a ModelRequest and
// translates the OpenAI JSON shape at the edge (40-wire.go).
//
// Two endpoints: GET /v1/models (the entries a key may select) and POST
// /v1/chat/completions (blocking or SSE streaming, with tools passed through). A
// key bills its owning account through the pool's existing per-handle ledger — the
// consumer stamp (source "apikey", uid = key-id) meets a bound handle accounts
// created at mint time. See docs/modelgate-apikey.md.
//
// Like webui, its http bind is best-effort: a listen failure degrades the API
// without aborting the agent, and BOB_MODELGATE_ADDR="off"/"" disables it.
package modelgate

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

// Module serves the OpenAI-compatible API. Optional: absent or addr-off, it just
// doesn't serve — the agent runs unaffected.
type Module struct {
	addr   string
	srv    *http.Server
	cancel context.CancelFunc
	pool   contract.ModelPool
	state  atomic.Int32 // trunk.State
}

// New returns a modelgate module bound to addr ("" or "off" disables the server).
func New(addr string) *Module { return &Module{addr: addr} }

func (m *Module) Name() string { return "modelgate" }

func (m *Module) Provides() []reflect.Type { return nil }

// Needs the model pool (the routing target — a hard dependency; without it there
// is nothing to expose). The API-key authority (accounts) and the panel registry
// (webui) are resolved LAZILY/optionally so a degraded-accounts or headless
// deployment still starts modelgate (it then 401s every request, or shows no panel).
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.ModelPool](),
	}
}

func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	m.pool = trunk.Require[contract.ModelPool](reg)

	// Self-describe to the webui when present (optional — headless deploys skip it).
	if pr, ok := trunk.TryRequire[contract.PanelRegistry](reg); ok && pr != nil {
		pr.RegisterPanel(m.panel())
	}

	if m.addr == "" || m.addr == "off" {
		slog.Info("modelgate: http server disabled (addr off)")
		m.state.Store(int32(trunk.StateReady))
		return nil
	}

	// The image catalog is OPTIONAL and lazy, like the key authority: it lives in the
	// model module, which may start after this one, and its absence only means the
	// images endpoints have nothing to offer.
	var imgOnce sync.Once
	var imgCat contract.ImageCatalog
	srv := &server{
		images: func() contract.ImageCatalog {
			imgOnce.Do(func() { imgCat, _ = trunk.TryRequire[contract.ImageCatalog](reg) })
			return imgCat
		},
		pool: m.pool,
		keys: lazyAPIKeys(reg), // accounts may Start after modelgate; absent → 401s
	}

	ln, err := net.Listen("tcp", m.addr)
	if err != nil {
		slog.Warn("modelgate: listen failed; API disabled (agent continues)", "addr", m.addr, "err", err)
		m.state.Store(int32(trunk.StateReady))
		return nil
	}
	if !isLoopback(m.addr) {
		slog.Warn("modelgate: bound to a non-loopback address — the OpenAI API is reachable beyond localhost (expected behind a tunnel / on a trusted LAN; every request needs a valid bearer key, but the key travels plaintext on bare HTTP — prefer an HTTPS tunnel)", "addr", m.addr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.srv = &http.Server{
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a streaming completion holds the connection open for the
		// length of the generation.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if err := m.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Warn("modelgate: server exited", "err", err)
		}
	}()
	slog.Info("modelgate serving OpenAI-compatible API", "addr", m.addr)
	m.state.Store(int32(trunk.StateReady))
	return nil
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

// lazyAPIKeys resolves the OPTIONAL contract.APIKeys on first use — accounts may
// Start after modelgate, and is absent on a degraded/no-accounts deployment. A nil
// result makes the server fail every request closed (401), never open. Concrete
// (not generic) so the arch optional-edge scanner attributes the TryRequire to
// modelgate.
func lazyAPIKeys(reg *trunk.Registry) func() contract.APIKeys {
	var once sync.Once
	var v contract.APIKeys
	return func() contract.APIKeys {
		once.Do(func() { v, _ = trunk.TryRequire[contract.APIKeys](reg) })
		return v
	}
}

// isLoopback reports whether addr's host is a loopback address. A non-loopback
// bind exposes the API to the network — worth a warning (mirrors webui).
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var _ trunk.Module = (*Module)(nil)
