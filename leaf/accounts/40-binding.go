package accounts

import (
	"context"
	"errors"
	"log/slog"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/accounts/store"
)

// codeKind is the claim-token kind label accounts mints (account binding). The facility
// treats kind as opaque (the gate's redeem step type-asserts contract.BatchPayload), so
// this is only a human label on the minted token.
const codeKind = "account-link"

// nowUnix is the timestamp unit accounts rows use (unix seconds), on the
// DB-calibrated clock.
func nowUnix() int64 { return clock.UnixEpoch() }

// ErrHandleBoundElsewhere is returned when a sender's handle is already bound to
// a DIFFERENT account and the bind was not authorized to repoint (only admins may).
var ErrHandleBoundElsewhere = errors.New("accounts: handle already bound to another account")

// BindFromCode binds ev's sender to accountID + auto-allowlists. Serialized so two binds
// never race on one handle. A handle already on a DIFFERENT account is refused unless
// repoint is set (an admin-minted /accounts mint token, or a real admin, carries it).
// Returns whether the sender's access was actually auto-allowlisted (false → bound but
// access pending — D8). This is the post-flow the /accounts bindto command (and the
// token batch that invokes it) runs.
func (m *Manager) BindFromCode(ctx context.Context, ev contract.MessageEvent, accountID string, repoint bool) (allowlisted bool, err error) {
	m.bindMu.Lock()
	defer m.bindMu.Unlock()
	if _, gerr := m.store.GetAccount(ctx, accountID); gerr != nil {
		return false, gerr // ErrAccountNotFound or transient
	}
	src, uid := ev.AccountHandle()
	if h, ok, herr := m.store.HandleBySourceUID(ctx, src, uid); herr != nil {
		return false, herr
	} else if ok && h.AccountID != accountID && !repoint {
		return false, ErrHandleBoundElsewhere
	}
	// On an admin repoint (A→B) the prior bind's gate allowlist entry is intentionally
	// NOT revoked (D13 accept): the entry is keyed on (ev.Source, ev.UserID) —
	// the SAME physical channel/person, who is now bound to B and SHOULD still reach bob.
	// Access is additive across repoints (no cross-account leak); revoking here would
	// mis-lock a still-bound user. Real access removal belongs to an UNBIND path (would
	// need an AccessGranter.Remove), not a repoint.
	boundVia := "self-code"
	if repoint {
		boundVia = "admin-code"
	}
	_, allowlisted, err = m.bindAndAllowlist(ctx, ev, accountID, boundVia)
	return allowlisted, err
}

// CreateAndBindSelf creates a BARE (flow-less) account and binds the caller's current
// handle to it (the open /accounts new self path). Refuses if the caller is already
// bound. Serialized with the other binds.
//
// Flow-less + NOT auto-allowlisted: self-creation is identity only, access is granted by
// an admin afterward — otherwise, once a source is onboarding-open, an un-allowlisted
// stranger could /accounts new themselves into full access with no approval. (The
// admin-CODE bind path — BindFromCode → bindAndAllowlist — DOES grant, since a code IS
// the admin's approval.)
func (m *Manager) CreateAndBindSelf(ctx context.Context, ev contract.MessageEvent, name string) (store.Account, error) {
	m.bindMu.Lock()
	defer m.bindMu.Unlock()
	src, uid := ev.AccountHandle()
	if _, ok, err := m.store.HandleBySourceUID(ctx, src, uid); err != nil {
		return store.Account{}, err
	} else if ok {
		return store.Account{}, ErrHandleBoundElsewhere
	}
	a, err := m.store.CreateBareAccount(ctx, name, ev.RoutingScope(), nowUnix())
	if err != nil {
		return store.Account{}, err
	}
	// Bind WITHOUT allowlisting (access pending an admin grant). On failure delete the
	// orphan account row (no handle bound yet, so the delete is clean).
	if _, err := m.store.BindHandle(ctx, store.SourceHandle{
		AccountID:   a.ID,
		Source:      src,       // identity key = platform-family
		PlatformUID: uid,       // identity key = stable uid
		BoundSource: ev.Source, // REAL source-name
		AccessUID:   ev.UserID, // REAL gate-uid
		DisplayName: ev.UserName,
		BoundVia:    "self-new",
	}, nowUnix()); err != nil {
		m.cleanupOrphan(ctx, a.ID)
		return store.Account{}, err
	}
	return a, nil
}

// bindAndAllowlist persists the handle binding then auto-allowlists the sender so
// their next message passes the gate. Grant at the REDEEM event's scope: a group →
// that chat's allowlist (so a per-group allowlist doesn't still block them — the
// bare-code-in-a-group case), a DM → source-wide. Returns allowlisted=false when no
// grant landed (no granter, or a write/reload blip) so the caller can be honest that
// access is pending (D8). The caller holds bindMu.
func (m *Manager) bindAndAllowlist(ctx context.Context, ev contract.MessageEvent, accountID, boundVia string) (handle store.SourceHandle, allowlisted bool, err error) {
	src, uid := ev.AccountHandle()
	h, err := m.store.BindHandle(ctx, store.SourceHandle{
		AccountID:   accountID,
		Source:      src,       // identity key = platform-family
		PlatformUID: uid,       // identity key = stable uid
		BoundSource: ev.Source, // REAL source-name (what the gate yaml is named)
		AccessUID:   ev.UserID, // REAL gate-uid (what the gate matches)
		DisplayName: ev.UserName,
		BoundVia:    boundVia,
	}, nowUnix())
	if err != nil {
		return store.SourceHandle{}, false, err
	}
	if g := m.accessGranter(); g != nil {
		var aerr error
		if ev.ChatType.IsGroupChat() {
			_, aerr = g.AllowInChat(ctx, ev.Source, ev.ChatID, ev.UserID)
		} else {
			_, aerr = g.Allow(ctx, ev.Source, ev.UserID)
		}
		if aerr != nil {
			slog.Warn("accounts: bound handle but could not auto-allowlist — add it by hand",
				"source", ev.Source, "chat", ev.ChatID, "uid", ev.UserID, "err", aerr)
		} else {
			allowlisted = true
		}
	}
	return h, allowlisted, nil
}

// mint mints a binding token after verifying the account exists. The token is a batch
// carrying one command — "/accounts bindto <id>" (+ " repoint" for an admin code) — with
// AsAdmin=true: /accounts bindto is admin-gated (a raw non-admin can't self-bind), so the
// token's frozen authority is what lets the recipient run it. The redeemer's own reply
// (localized) comes from cmdBindTo; no per-token Desc.
func (m *Manager) mint(ctx context.Context, accountID string, repoint bool) (string, error) {
	if _, err := m.store.GetAccount(ctx, accountID); err != nil {
		return "", err
	}
	cmd := "/accounts bindto " + accountID
	if repoint {
		cmd += " repoint"
	}
	return m.tokens.Mint(codeKind, contract.BatchPayload{Commands: []string{cmd}, AsAdmin: true}, m.ttl), nil
}

// MintForSelf mints a code a user redeems to bind ANOTHER of their channels to
// their own account. Self-codes can only bind unbound senders (no repoint).
func (m *Manager) MintForSelf(ctx context.Context, accountID string) (string, error) {
	return m.mint(ctx, accountID, false)
}

// MintForAdmin mints a code an admin hands to a user to bind them to accountID. The token
// freezes AsAdmin=true, so redeeming it runs /accounts bindto with repoint authority (an
// admin-code MAY rebind a handle already on another account).
func (m *Manager) MintForAdmin(ctx context.Context, accountID string) (string, error) {
	return m.mint(ctx, accountID, true)
}

// AccountForHandle resolves a sender's handle to its account id (ok=false when
// unbound).
func (m *Manager) AccountForHandle(ctx context.Context, source, uid string) (string, bool, error) {
	h, ok, err := m.store.HandleBySourceUID(ctx, source, uid)
	if err != nil || !ok {
		return "", false, err
	}
	return h.AccountID, true, nil
}

// AccountFor resolves ev's handle to a display-ready account (contract.Accounts).
// ok=false means the handle is genuinely UNBOUND. This read is load-bearing for
// ROUTING (the router's front gate sends unbound non-admins to intro), not just
// display, so a store error on the handle read is NOT folded into "unbound":
// intro'ing an entitled member on a hiccup would swallow their turn and send a
// false "registered, pending admin approval" notice. A handle-read blip instead
// mirrors the account-row blip below — bound with Flow UNKNOWN (FlowKnown=false)
// — so FlowKnown=false makes the router fail CLOSED for non-admins this turn
// (F43, reversing D27), keeping only the basic floor for admins; it self-heals
// next turn once the store read succeeds.
func (m *Manager) AccountFor(ctx context.Context, ev contract.MessageEvent) (contract.AccountInfo, bool) {
	src, uid := ev.AccountHandle()
	id, ok, err := m.AccountForHandle(ctx, src, uid)
	if err != nil {
		// Transient store blip: binding UNKNOWN, not unbound. Never route to intro on a
		// hiccup — report bound with flow unread (see the doc comment above).
		slog.Warn("accounts: handle read failed — binding unknown, reporting bound with flow unread", "source", src, "uid", uid, "err", err)
		return contract.AccountInfo{}, true // FlowKnown=false → router fails CLOSED for non-admins this turn (F43, reversing D27), self-heals next turn; admins keep the basic floor
	}
	if !ok {
		return contract.AccountInfo{}, false
	}
	a, err := m.store.GetAccount(ctx, id)
	if err != nil {
		// Bound handle but the account row didn't read (transient store blip). We name the
		// binding by id but Status/Flow are unknown (FlowKnown=false). The router fails
		// CLOSED for non-admins this turn (F43, reversing D27), keeping only the basic floor
		// for admins, and self-heals next turn once the account row reads. Log it since the
		// row is load-bearing for routing (paused gate + entitlement).
		slog.Warn("accounts: GetAccount failed for bound handle — status/flow unknown", "account", id, "err", err)
		return contract.AccountInfo{ID: id}, true // FlowKnown=false → router fails CLOSED for non-admins this turn (F43, reversing D27), self-heals next turn; admins keep the basic floor
	}
	return contract.AccountInfo{ID: a.ID, DisplayName: a.DisplayName, Flow: a.Flow, Status: a.Status, FlowKnown: true}, true
}

// accessGranter returns the lazily-resolved access writer, or nil if unwired.
func (m *Manager) accessGranter() contract.AccessGranter {
	if m.granter == nil {
		return nil
	}
	return m.granter()
}
