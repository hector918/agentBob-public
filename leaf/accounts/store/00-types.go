// Package store is the accounts module's persistence: the identity model
// (accounts + their bound source-handles) and per-handle usage accounting. It
// owns its tables on the shared contract.DB pool. Account / SourceHandle /
// AccountUsage and the Store interface stay HERE (not in contract) — the Manager
// and this impl are the only users, so the seam is internal to the module; the
// flow reaches accounts only through contract.Accounts.
//
// MINIMAL SLICE (trunk-rebuild): this carries identity RESOLUTION (handle →
// account → flow) + per-handle USAGE recording, the two things the flow needs.
// Claimcode binding, the slash/webui management surface, account merge, language
// and status all sit ATOP this and land with their own modules — they are not
// required for routing or billing to work.
package store

import (
	"context"
	"errors"
)

// ErrAccountNotFound is returned by GetAccount when no account has the id.
var ErrAccountNotFound = errors.New("accounts: account not found")

// ErrAPIKeyNotFound is returned by RevokeAPIKey when no key has the id.
var ErrAPIKeyNotFound = errors.New("accounts: api key not found")

// Account is one logical person. Many SourceHandles bind to one Account. Flow is
// the per-account routing assignment (the router derives entitlement from it via
// contract.EntitledFlows).
type Account struct {
	ID          string // "ac_..." assigned by the store on create
	DisplayName string
	Note        string // free-form admin note
	Flow        string // per-account flow entitlement (comma-list); "" = NO entitlement (pending approval)
	Status      string // "active" (normal) | "paused" (access falls back to intro); "" = active
	// OnboardScope is the chat scope a BARE (flow-less) account first appeared in, so
	// admin approval can grant agora-vs-normal by it. "" once approved / for pre-existing
	// accounts.
	OnboardScope string
	CreatedAt    int64 // unix seconds
	UpdatedAt    int64 // unix seconds
}

// SourceHandle is one (platform-family, stable-uid) identity bound to an account.
// The accounts ledger keys on the PLATFORM FAMILY (not the per-bot/per-account
// source name) + a cross-app-stable uid, so the same person is ONE handle across
// all your telegram bots / email mailboxes / feishu apps.
type SourceHandle struct {
	ID          string // "sh_..." assigned by the store on bind
	AccountID   string // FK → Account.ID
	Source      string // PLATFORM-KIND ("telegram" / "feishu" / "email" / ...)
	PlatformUID string // cross-app-stable uid (telegram user_id / feishu union_id / email addr)
	BoundSource string // REAL source-name at bind (telegram2 / feishu / email-support) — display/audit only
	AccessUID   string // gate-uid at bind — display/audit only (no code re-grants from these columns)
	DisplayName string // last-seen display name (optional)
	BoundAt     int64  // unix seconds
	BoundVia    string // audit: the claimcode that created this binding (may be "")
}

// APIKey is one bearer credential minted for an account: a machine-facing entry
// point (leaf/modelgate) authenticates a caller by it, bills the owning account,
// and enforces its policy. The plaintext token is NEVER stored — only SecretHash
// (sha256 hex). Kinds and Models are the two mutually-exclusive forms (see
// contract.APIKeyInfo): lane allowlist vs. exact entry-name allowlist.
//
// The pre-v8 columns (kind / tags / prefer_tags / max_inflight) are DEAD: v8 moved
// lane + tag preference into the request and deleted the per-key inflight gate. The
// columns stay (DROP COLUMN is irreversible and keeping them costs nothing) but
// nothing reads or writes them, so they have no field here.
type APIKey struct {
	ID         string   // "ak_..." assigned on create; the billing handle uid
	AccountID  string   // FK → Account.ID
	Label      string   // free-form admin label
	Kinds      []string // lanes this key may enter (parsed from the CSV column)
	Models     []string // explicit entry-name allowlist (parsed from the CSV column)
	Status     string   // "active" | "revoked"
	CreatedAt  int64    // unix seconds
	LastUsedAt int64    // unix seconds; 0 = never used
}

// AccountUsage is the rolled-up lifetime usage across an account's bound handles.
type AccountUsage struct {
	Turns, Success      int64
	InTokens, OutTokens int64                 // grand total tokens (incl legacy aggregate)
	TokensByKind        map[string]KindTokens // per-kind (llm/ocr/translate); excludes legacy aggregate
	Native              map[string]int64      // per "kind:unit" (e.g. "asr:s") → amount
}

// KindTokens is per-model-kind token accounting (input/output). It mirrors
// contract.KindTokens; the store keeps its own copy so the persistence seam does
// not depend on the contract shape, and the buffer translates at the boundary.
type KindTokens struct{ In, Out int64 }

// Store is the accounts persistence surface for the minimal slice: account +
// handle CRUD, the (source,uid) resolution the flow routes on, and per-handle
// usage recording + rollup. Management-only methods (roster, language, status,
// skill forks, merge) are deferred with their owning surfaces.
type Store interface {
	// Account + handle CRUD.
	CreateAccount(ctx context.Context, displayName, note string, nowUnix int64) (Account, error)
	// CreateBareAccount creates an account with NO flow entitlement (identity only) and
	// records onboardScope (where it first appeared). The onboarding intake path — kept
	// distinct from CreateAccount (which writes flow="normal" explicitly) so a bare account
	// can't accidentally start with access.
	CreateBareAccount(ctx context.Context, displayName, onboardScope string, nowUnix int64) (Account, error)
	// DeleteAccount removes an account row by id — used to clean up an orphan when a
	// bind fails right after the account was created. A no-op for an absent id.
	DeleteAccount(ctx context.Context, id string) error
	GetAccount(ctx context.Context, id string) (Account, error)
	ListAccounts(ctx context.Context) ([]Account, error)
	// AccountRosterPage returns one page of the roster (newest first) plus the
	// grand total — true server-side paging for the webui roster (COUNT +
	// LIMIT/OFFSET, not fetch-all-then-slice).
	AccountRosterPage(ctx context.Context, limit, offset int) ([]Account, int64, error)
	BindHandle(ctx context.Context, h SourceHandle, nowUnix int64) (SourceHandle, error)
	HandlesForAccount(ctx context.Context, accountID string) ([]SourceHandle, error)
	// HandleCountsByAccount returns the bound-handle count per account in ONE
	// aggregate query — the roster listing's per-row channel count without an
	// N+1 fan-out. Accounts with no handles are simply absent (count 0).
	HandleCountsByAccount(ctx context.Context) (map[string]int64, error)
	// SetAccountFlow sets the per-account flow assignment (admin-only upstream).
	SetAccountFlow(ctx context.Context, accountID, flow string, nowUnix int64) error

	// SetAccountStatus sets the account status ("active"|"paused") (admin-only upstream).
	SetAccountStatus(ctx context.Context, accountID, status string, nowUnix int64) error

	// Resolution: the flow's hot path.
	HandleBySourceUID(ctx context.Context, source, platformUID string) (SourceHandle, bool, error)

	// Usage: handle-keyed recording (merge-safe) + per-account rollup.
	AddTurnUsage(ctx context.Context, source, platformUID string, turns, success int64, tokens map[string]KindTokens, native map[string]int64) error
	AccountUsageBreakdown(ctx context.Context, accountID string) (AccountUsage, error)

	// API keys: mint / verify / list / revoke. CreateAPIKey stores only the hash.
	// APIKeyByHash resolves an ACTIVE key whose owning account is ACTIVE (paused
	// account or revoked key → ok=false); it also returns the account status so a
	// caller need not re-read. TouchAPIKey bumps last_used_at (best-effort).
	CreateAPIKey(ctx context.Context, k APIKey, secretHash string, nowUnix int64) (APIKey, error)
	APIKeyByHash(ctx context.Context, secretHash string) (key APIKey, ok bool, err error)
	TouchAPIKey(ctx context.Context, id string, nowUnix int64) error
	ListAPIKeys(ctx context.Context, accountID string) ([]APIKey, error)
	RevokeAPIKey(ctx context.Context, id string) error

	// Per-sender reply-language seed (on the passive handle-usage row, no binding
	// needed). GetHandleLang returns "" when none. SetHandleLangIfEmpty is seed-once
	// — it sets the language only when none is recorded yet, so the seed stays stable
	// (mid-conversation switches live in the session runtime, not here).
	GetHandleLang(ctx context.Context, source, platformUID string) (string, error)
	SetHandleLangIfEmpty(ctx context.Context, source, platformUID, lang string) error

	Ping(ctx context.Context) error
}
