package accounts

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"agentbob/contract"
	"agentbob/leaf/accounts/store"
)

// Manager is the accounts module's runtime: it implements contract.Accounts
// (per-account flow routing), contract.ConsumptionReporter (per-user billing),
// and the /accounts bindto binding action (a mint token's batch invokes it) over the store,
// the in-memory consumption buffer, and the claim-token facility. The slash management
// surface (40-binding.go / 50-slash.go) calls the same methods.
type Manager struct {
	store  store.Store
	buffer *handleUsageBuffer

	tokens contract.ClaimTokens // claim-token facility: account-link code mint/verify
	ttl    time.Duration        // minted-code lifetime
	bindMu sync.Mutex           // serializes binds so two never race on one handle
	// granter lazily resolves the optional access writer (contract.AccessGranter,
	// gate). Binding's auto-allowlist side effect goes through it; nil → bind
	// records identity but cannot grant access (operator adds it by hand).
	granter func() contract.AccessGranter
	// agora lazily resolves the optional org authority (contract.Agora). Approve reads
	// ScopeIsAgora(onboard_scope) through it to decide whether to also grant the agora
	// flow; nil (no agora bootstrapped) → approval grants normal only.
	agora func() contract.Agora
}

// agoraAuthority resolves the optional agora authority, or nil when none is wired.
func (m *Manager) agoraAuthority() contract.Agora {
	if m.agora == nil {
		return nil
	}
	return m.agora()
}

// LangFor returns the sender's remembered reply language, ok=false when none /
// unbound-handle-less / store error (the flow then falls back to detection/default).
func (m *Manager) LangFor(ctx context.Context, source, uid string) (string, bool) {
	if source == "" || uid == "" {
		return "", false
	}
	lang, err := m.store.GetHandleLang(ctx, source, uid)
	if err != nil {
		slog.Warn("accounts: lang lookup failed", "source", source, "uid", uid, "err", err)
		return "", false
	}
	if lang == "" {
		return "", false
	}
	return lang, true
}

// RememberLang seeds the sender's reply language (seed-once, real languages only —
// "default"/English is the natural fallback so it is never stored). Best-effort.
func (m *Manager) RememberLang(ctx context.Context, source, uid, lang string) {
	if source == "" || uid == "" || lang == "" || lang == "default" {
		return
	}
	if err := m.store.SetHandleLangIfEmpty(ctx, source, uid, lang); err != nil {
		slog.Warn("accounts: remember lang failed", "source", source, "uid", uid, "err", err)
	}
}

// ReportConsumption books one model call's tokens to the consuming user's handle
// (read from ctx, stamped by the flow before the turn). Buffered and best-effort;
// a no-op when no handle is on ctx (internal / unattributed calls). Handle-keyed,
// so an unbound user's spend is still recorded — the account is the read-time
// rollup.
func (m *Manager) ReportConsumption(ctx context.Context, kind string, in, out int64) {
	src, uid, ok := contract.ConsumerFrom(ctx)
	if !ok || (in == 0 && out == 0) {
		return
	}
	m.buffer.Add(src, uid, 0, 0, map[string]store.KindTokens{kind: {In: in, Out: out}}, nil)
}

var (
	_ contract.Accounts            = (*Manager)(nil)
	_ contract.ConsumptionReporter = (*Manager)(nil)
)
