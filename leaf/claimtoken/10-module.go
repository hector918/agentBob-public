// Package claimtoken is the project-wide token AUTHENTICATION facility
// (docs/claim-token.md). It owns ONLY token lifecycle — mint a random secret, freeze
// (kind, payload), store with a TTL, hand it back via Verify, burn it via Consume. It
// is channel- and post-flow-agnostic: who may redeem, from which channel (a chat
// event, an HTTP request), and what a valid token does (allowlist / account-bind /
// inbox-wire / open a takeover session) all live in the owning module. browserd's API
// key is NOT a claim token (browserd is a service authed by a static key).
package claimtoken

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/trunk"
)

// sweepPeriod bounds stale-token memory: expired tokens are dropped lazily on Verify,
// and swept periodically so an unredeemed mint can't accumulate.
const sweepPeriod = 10 * time.Minute

type entry struct {
	kind    string
	payload any
	expUnix int64
}

// Module is the claim-token facility.
type Module struct {
	mu     sync.Mutex
	toks   map[string]entry
	state  atomic.Int32 // trunk.State
	cancel context.CancelFunc
	done   chan struct{}
}

// New returns the facility.
func New() *Module { return &Module{toks: make(map[string]entry), done: make(chan struct{})} }

func (m *Module) Name() string { return "claimtoken" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.ClaimTokens]()}
}

func (m *Module) Needs() []reflect.Type { return nil }

// Optional is false: gate/accounts/agora/webui all redeem through it — without it the
// whole bind/wire/admit/takeover surface silently breaks, so a Start failure should abort.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	trunk.Provide[contract.ClaimTokens](reg, m)
	// Bounded growth: an IN-MODULE ticker sweeps expired tokens. The token store is a
	// PURE in-memory map, so its hygiene runs on its own ticker (like webui-auth and
	// the tools channel-pool) rather than the trunk Housekeeper, which is reserved for
	// persistent (DB/file) sweeps. Correctness does not depend on the cadence — lazy
	// expiry on Verify already drops expired tokens; the sweep only reclaims the memory
	// of never-redeemed mints.
	runCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go m.sweepLoop(runCtx)
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// Mint freezes (kind, payload) behind a random token expiring after ttl.
func (m *Module) Mint(kind string, payload any, ttl time.Duration) string {
	tok := randToken()
	exp := clock.Now().Add(ttl).Unix()
	m.mu.Lock()
	m.toks[tok] = entry{kind: kind, payload: payload, expUnix: exp}
	m.mu.Unlock()
	return tok
}

// Verify authenticates a token WITHOUT consuming it. Expired → drop + ok=false.
func (m *Module) Verify(token string) (string, any, bool) {
	if token == "" {
		return "", nil, false
	}
	now := clock.UnixEpoch()
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.toks[token]
	if !ok {
		return "", nil, false
	}
	if e.expUnix <= now {
		delete(m.toks, token) // lazily reap the expired entry
		return "", nil, false
	}
	return e.kind, e.payload, true
}

// Consume burns a token (idempotent).
func (m *Module) Consume(token string) {
	if token == "" {
		return
	}
	m.mu.Lock()
	delete(m.toks, token)
	m.mu.Unlock()
}

// sweepLoop runs the expired-token sweep on its own ticker until Stop cancels ctx.
func (m *Module) sweepLoop(ctx context.Context) {
	defer close(m.done)
	t := time.NewTicker(sweepPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweep()
		}
	}
}

// sweep drops every expired token.
func (m *Module) sweep() {
	now := clock.UnixEpoch()
	m.mu.Lock()
	for tok, e := range m.toks {
		if e.expUnix <= now {
			delete(m.toks, tok)
		}
	}
	m.mu.Unlock()
}

// randToken returns a 128-bit crypto-random hex token.
func randToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var _ contract.ClaimTokens = (*Module)(nil)
var _ trunk.Module = (*Module)(nil)
