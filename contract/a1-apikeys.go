package contract

import "context"

// APIKeyBillingSource is the synthetic source-family an API key's usage is billed
// under: the key's spend is recorded against the consumer handle
// (APIKeyBillingSource, key-id). accounts binds a matching source-handle at mint
// time and the pool stamps the same pair via WithConsumer, so the two meet in the
// per-handle usage ledger. It lives HERE (not copied into each leaf) because
// billing correctness depends on the producer (modelgate) and the ledger
// (accounts) agreeing on the exact string byte-for-byte.
const APIKeyBillingSource = "apikey"

// APIKeys is the API-key verification authority: it maps a bearer token an
// external caller presents to the account it bills and the model policy it may
// use. Provided by leaf/accounts (a key is a cross-entry identity credential, same
// family as a source-handle / claimcode — it lives with the identity model);
// consumed by leaf/modelgate (the OpenAI-compatible HTTP surface) and any future
// machine-facing entry point that authenticates callers by key.
//
// An ABSENT APIKeys (accounts down / not wired) means no caller can verify — the
// consumer fails closed (every request 401), never open.
type APIKeys interface {
	// VerifyKey resolves a plaintext bearer token to its policy. ok=false covers
	// every non-serving case indistinguishably (unknown token, revoked key, or a
	// paused owning account) so a caller cannot probe which one it is. A hit
	// best-effort touches the key's last-used timestamp. NEVER logs the token.
	VerifyKey(ctx context.Context, token string) (info APIKeyInfo, ok bool)
}

// APIKeyInfo is a verified key's runtime policy — what the bearer may reach.
// Kinds and Models are the two mutually-exclusive forms (a key carries one or the
// other):
//
//   - Kinds is a LANE allowlist: the key may enter these pool-entry kinds, and each
//     REQUEST names the one it wants. Within a lane the caller expresses only a SOFT
//     tag preference, so the pool's entry names are bob's internal inventory and are
//     never part of this form's contract.
//   - Models is an explicit pool-entry-name allowlist: the caller pins one exact
//     entry, bypassing routing. For admin-issued keys that must hit a fixed backend
//     (evaluation runs), or to fence a key away from specific backends.
//
// Both empty = the key can reach nothing (fail-closed, not "everything").
//
// A Kinds-form key deliberately carries NO tag allowlist: a request's tags are a
// soft preference (ModelRequest.Prefer), and a soft tag only reorders candidates
// within the lane — it cannot widen or narrow reachability, so fencing on it would
// be self-deception. Fence on entries (Models) when that is what is meant.
type APIKeyInfo struct {
	ID        string   // key-id — the billing handle uid (source "apikey")
	AccountID string   // owning account (for display / audit; billing keys on ID via the bound handle)
	Kinds     []string // pool-entry kinds ("lanes") this key may enter; empty when Models-form
	Models    []string // explicit pool-entry-name allowlist (exact match); empty when Kinds-form
}
