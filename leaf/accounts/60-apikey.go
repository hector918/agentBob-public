package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/leaf/accounts/store"
)

// This file is the accounts module's contract.APIKeys surface + the management
// actions behind /accounts apikey. A key is a bearer credential a machine-facing
// entry point (leaf/modelgate) authenticates a caller by, billing the owning
// account: minting BINDS a synthetic ("apikey", key-id) source-handle to the
// account, so the model pool's per-handle consumption ledger rolls the key's spend
// up under the account with zero new billing machinery.

// apiKeyTokenPrefix marks a bob-minted key so a leaked token is recognizable and a
// caller can sanity-check what they pasted.
const apiKeyTokenPrefix = "sk-bob-"

// apiKeyTouchIntervalSec coarsens the last_used_at write: VerifyKey runs on EVERY
// API request, so an unconditional UPDATE would hammer one hot key row under a
// batch caller. last_used_at is a coarse "recently active?" signal, so one write
// per key per this window is plenty (mirrors the usage buffer's coalescing intent).
const apiKeyTouchIntervalSec = 300

// apiKeyHandleSource is the synthetic source-family a key's billing handle binds
// under. The model pool stamps contract.WithConsumer(ctx, apiKeyHandleSource,
// keyID) so ReportConsumption books the key's tokens to this handle, and
// AccountUsageBreakdown's (source,uid) join rolls it up to the account. Pinned to
// the single contract truth so it can never drift from modelgate's stamp.
const apiKeyHandleSource = contract.APIKeyBillingSource

// hashToken is the at-rest form of a key: sha256 hex. The plaintext never persists.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newAPIKeyToken mints a fresh random plaintext token (prefix + 32 hex = 128 bits).
func newAPIKeyToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("apikey: random: %w", err)
	}
	return apiKeyTokenPrefix + hex.EncodeToString(b[:]), nil
}

// VerifyKey implements contract.APIKeys: resolve a plaintext bearer token to its
// policy. ok=false for an empty/unknown token, a revoked key, or a paused account
// (the store's JOIN gates all three). A hit best-effort touches last_used_at.
func (m *Manager) VerifyKey(ctx context.Context, token string) (contract.APIKeyInfo, bool) {
	if token == "" {
		return contract.APIKeyInfo{}, false
	}
	k, ok, err := m.store.APIKeyByHash(ctx, hashToken(token))
	if err != nil {
		slog.Warn("accounts: apikey verify failed", "err", err)
		return contract.APIKeyInfo{}, false
	}
	if !ok {
		return contract.APIKeyInfo{}, false
	}
	// Best-effort last-used bump, COALESCED: skip the write unless the recorded
	// timestamp is stale (VerifyKey runs per request — see apiKeyTouchIntervalSec).
	// A write failure must not fail an otherwise valid key; detach the ctx so a
	// request whose ctx cancels right after auth still records.
	now := clock.Now().Unix()
	if now-k.LastUsedAt >= apiKeyTouchIntervalSec {
		if terr := m.store.TouchAPIKey(context.WithoutCancel(ctx), k.ID, now); terr != nil {
			slog.Warn("accounts: apikey touch failed", "key", k.ID, "err", terr)
		}
	}
	return contract.APIKeyInfo{
		ID:        k.ID,
		AccountID: k.AccountID,
		Kinds:     k.Kinds,
		Models:    k.Models,
	}, true
}

// APIKeyPolicy is what a minted key may reach — the mint-time form of
// contract.APIKeyInfo. A struct rather than positional args because both fields are
// []string: a caller swapping them would compile silently.
type APIKeyPolicy struct {
	Kinds  []string // lanes this key may enter (lane form)
	Models []string // explicit entry-name allowlist (pin form)
}

// MintAPIKey creates a key for accountID and returns the PLAINTEXT token (shown to
// the admin exactly once; only its hash persists). It binds the synthetic billing
// handle so the account's usage rollup captures the key's spend. pol.Kinds and
// pol.Models are the two forms — pass one; both empty is allowed but the key can
// then reach nothing (fail-closed). Policy VALIDATION (one form only, known lane
// names) belongs to the caller — the slash command — so it can explain itself to a
// human; this method only persists what it is handed.
func (m *Manager) MintAPIKey(ctx context.Context, accountID, label string, pol APIKeyPolicy) (token string, key store.APIKey, err error) {
	if _, gerr := m.store.GetAccount(ctx, accountID); gerr != nil {
		return "", store.APIKey{}, gerr // ErrAccountNotFound surfaces to the caller
	}
	token, err = newAPIKeyToken()
	if err != nil {
		return "", store.APIKey{}, err
	}
	now := clock.Now().Unix()
	key, err = m.store.CreateAPIKey(ctx, store.APIKey{
		AccountID: accountID,
		Label:     label,
		Kinds:     pol.Kinds,
		Models:    pol.Models,
	}, hashToken(token), now)
	if err != nil {
		return "", store.APIKey{}, err
	}
	// Bind the synthetic billing handle (source "apikey", uid = key-id) → account, so
	// the pool's per-handle ledger rolls the key's spend up under the account. FAIL
	// CLOSED on a bind failure: a billing credential must never exist working-but-
	// unbilled (its spend would strand under an unjoined handle forever). The token
	// hasn't been returned yet, so roll the key back and surface the error — mint is
	// all-or-nothing. The rollback revoke is best-effort: even if it also fails the
	// key is harmless (its plaintext was never handed out, so it can never be used).
	if _, berr := m.store.BindHandle(ctx, store.SourceHandle{
		AccountID:   accountID,
		Source:      apiKeyHandleSource,
		PlatformUID: key.ID,
		BoundSource: apiKeyHandleSource,
		DisplayName: label,
		BoundVia:    "apikey",
	}, now); berr != nil {
		slog.Warn("accounts: apikey billing-handle bind failed — rolling back the key (mint is atomic)", "key", key.ID, "account", accountID, "err", berr)
		if rerr := m.store.RevokeAPIKey(ctx, key.ID); rerr != nil {
			slog.Warn("accounts: apikey rollback revoke also failed (key is unusable — plaintext never returned)", "key", key.ID, "err", rerr)
		}
		return "", store.APIKey{}, fmt.Errorf("apikey: billing handle bind failed: %w", berr)
	}
	return token, key, nil
}

// ListAPIKeys returns an account's keys (or ALL keys when accountID is empty).
func (m *Manager) ListAPIKeys(ctx context.Context, accountID string) ([]store.APIKey, error) {
	return m.store.ListAPIKeys(ctx, accountID)
}

// RevokeAPIKey flips a key to revoked. ErrAPIKeyNotFound for an unknown id.
func (m *Manager) RevokeAPIKey(ctx context.Context, keyID string) error {
	return m.store.RevokeAPIKey(ctx, keyID)
}

var _ contract.APIKeys = (*Manager)(nil)
