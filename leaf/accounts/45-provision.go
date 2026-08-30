package accounts

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/leaf/accounts/store"
)

// This file implements contract.AccountProvisioner — the onboarding WRITE seam,
// separate from the read-only routing surface. Identity (account) and access (flow)
// are layered: EnsureBareAccount mints a flow-less identity; access is granted
// separately by Approve. It keys on the platform-FAMILY handle (ev.AccountHandle),
// so one identity covers every per-bot source in the family.

// EnsureBareAccount binds the event's handle to a NEW account with NO flow when it has
// none — identity only, NOT auto-allowlisted (access is granted later via Approve
// at admin approval). A handle already bound returns its existing account.
//
// Uses CreateBareAccount (flow=” explicitly, NOT the column's "normal" default) so the
// account can't start with access. Serialized with the other binds via bindMu so a
// concurrent self-bind can't double-create the handle.
func (m *Manager) EnsureBareAccount(ctx context.Context, ev contract.MessageEvent) (string, bool, error) {
	m.bindMu.Lock()
	defer m.bindMu.Unlock()
	src, uid := ev.AccountHandle()
	if h, ok, err := m.store.HandleBySourceUID(ctx, src, uid); err != nil {
		return "", false, err
	} else if ok {
		return h.AccountID, false, nil
	}
	// Bare = flow-less identity, recording the scope it first appeared in (so admin
	// approval can grant agora-vs-normal by ScopeIsAgora(onboard_scope)).
	a, err := m.store.CreateBareAccount(ctx, ev.UserName, ev.RoutingScope(), nowUnix())
	if err != nil {
		return "", false, err
	}
	if _, err := m.store.BindHandle(ctx, store.SourceHandle{
		AccountID:   a.ID,
		Source:      src,       // identity key = platform-family
		PlatformUID: uid,       // identity key = stable uid
		BoundSource: ev.Source, // REAL source-name (what the gate yaml is named)
		AccessUID:   ev.UserID, // REAL gate-uid
		DisplayName: ev.UserName,
		BoundVia:    "intro-bare",
	}, nowUnix()); err != nil {
		m.cleanupOrphan(ctx, a.ID)
		return "", false, err
	}
	return a.ID, true, nil
}

// Approve grants a pending (bare) account access in ONE admin action: it sets the
// account's flow to "normal" — plus "agora" iff its onboard_scope is an agora scope
// (ScopeIsAgora) — and allowlists each of its handles on their source (the roster
// record + durable access if the source's onboarding opt-in is later turned off).
// Returns the granted flow set. Idempotent to re-run: the allowlist is re-affirmed
// every run (Allow no-ops when already listed) — so re-running approve repairs a
// blipped Allow write — while the flow write is skipped once the account has any
// entitlement (a manual richer grant is never clobbered). Serialized under bindMu
// (the single flow-write lock, shared with cmdFlow).
func (m *Manager) Approve(ctx context.Context, accountID string) ([]string, error) {
	m.bindMu.Lock()
	defer m.bindMu.Unlock()
	a, err := m.store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	// Allowlist every handle on its REAL source (best-effort — flow already grants access
	// on an onboarding-open source; this makes it survive the opt-in being turned off, and
	// is the "who's been approved" roster the operator asked for). Runs BEFORE the
	// already-entitled early return: an Allow failure only WARNs while the reply says
	// approved, so re-running approve must reach this block to repair it.
	if g := m.accessGranter(); g != nil {
		handles, herr := m.store.HandlesForAccount(ctx, accountID)
		if herr != nil {
			slog.Warn("accounts approve: could not list handles to allowlist", "account", accountID, "err", herr)
		}
		for _, h := range handles {
			if h.BoundSource == "" || h.AccessUID == "" {
				continue
			}
			if _, aerr := g.Allow(ctx, h.BoundSource, h.AccessUID); aerr != nil {
				slog.Warn("accounts approve: allowlist failed for handle", "account", accountID, "source", h.BoundSource, "err", aerr)
			}
		}
	}
	// Already approved (has any flow): do NOT clobber. Approve sets the INITIAL grant for a
	// pending (flow-less) account; re-running it — or running it after a manual richer grant
	// like "normal,agora,billing" — must not reset to the bare "normal[,agora]" set. Return
	// the existing entitlement unchanged (the allowlist above was still re-affirmed).
	if existing := contract.EntitledFlows(a.Flow); len(existing) > 0 {
		return existing, nil
	}
	flows := []string{"normal"}
	if a.OnboardScope != "" {
		if ag := m.agoraAuthority(); ag != nil && ag.ScopeIsAgora(ctx, a.OnboardScope) {
			flows = append(flows, "agora")
		}
	}
	if err := m.store.SetAccountFlow(ctx, accountID, strings.Join(flows, ","), nowUnix()); err != nil {
		return nil, err
	}
	return flows, nil
}

// cleanupOrphan best-effort deletes an account row left behind when a bind/blank step
// failed right after CreateAccount — on a DETACHED ctx, since the failure may BE ctx
// cancellation (shutdown racing the onboarding goroutine) and reusing it would leak the
// row. Mirrors CreateAndBindSelf's orphan cleanup.
func (m *Manager) cleanupOrphan(ctx context.Context, id string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := m.store.DeleteAccount(cctx, id); err != nil {
		slog.Warn("accounts: could not clean up orphan bare account after bind failure", "account", id, "err", err)
	}
}

var _ contract.AccountProvisioner = (*Manager)(nil)
