package contract

import "time"

// ClaimTokens is the project-wide token AUTHENTICATION facility (leaf/claimtoken).
// It owns ONLY token lifecycle — mint a random secret, freeze (kind, payload), store
// with a TTL, hand it back exactly once. It is channel-agnostic and post-flow-
// agnostic: WHO redeems, FROM WHICH channel (a chat event, an HTTP request), and WHAT
// a valid token does (allowlist / account-bind / inbox-wire / open a takeover session)
// all live in the owning module, not here. kind is opaque to the facility (the
// redeeming module knows its own payload type). browserd's API key is NOT a claim
// token — browserd is a service authed by a static key, with no user business here.
type ClaimTokens interface {
	// Mint freezes (kind, payload) behind a fresh random token that expires after ttl.
	// The returned string is the bearer the user/operator later presents back.
	Mint(kind string, payload any, ttl time.Duration) (token string)
	// Verify authenticates a token WITHOUT consuming it: a live token returns its
	// frozen (kind, payload); a missing/expired token → ok=false. The caller runs the
	// post-flow, then Consumes on success (or a terminal failure). On a transient
	// failure — or an ineligible redeemer — it leaves the token untouched, keeping its
	// ORIGINAL expiry (no truncation), so the user can retry the same code. The rare
	// concurrent double-Verify is benign: every post-flow is idempotent / serialized in
	// its own module (allowlist-add, bindMu, wire) and Consume is idempotent.
	Verify(token string) (kind string, payload any, ok bool)
	// Consume burns a token (idempotent). Called after the post-flow commits, or on a
	// terminal (non-retryable) failure.
	Consume(token string)
}

// BatchPayload is the ONLY thing a chat-redeemed claim token freezes (the per-kind
// typed payloads + handlers were retired, docs/token-batch-command.md): a
// batch of ready-made slash commands to run when the token is redeemed, a human
// description of what the token does, and the authority to run them under. The token
// facility stays domain-blind — it neither parses nor understands these commands; the
// gate's redeem step runs each through SlashRegistry.Dispatch. Whoever presents a live
// token runs its whole batch.
type BatchPayload struct {
	// Commands are "/name args" strings, run in order through SlashRegistry.Dispatch.
	Commands []string
	// Desc is shown to the redeemer (and to an admin inspecting the token): what this
	// token does. May be "".
	Desc string
	// AsAdmin freezes the authority the batch runs under. true → each command runs with
	// IsAdmin=true regardless of who redeems (a bearer pre-authorization the minter — an
	// admin — deliberately handed out). false → the batch runs under the REDEEMER's real
	// admin status, so an AdminOnly command in it only fires for a real admin. The
	// auto-minted bounce code uses false: it is shown to the applicant, who must NOT be
	// able to self-admit — only a real admin redeeming it can run its /gate allow.
	AsAdmin bool
}
