package core

import (
	"context"
	"errors"
	"strings"
)

// AccountHandle returns the cross-entry handle key for this event's sender:
// (platform-kind, stable-uid). Per docs/accounts.md §12 L3 the accounts ledger
// keys on the PLATFORM FAMILY (not the per-bot/per-account source name) + a
// cross-app-stable uid, so the same person is ONE handle across all your
// telegram bots / email mailboxes / feishu apps. Source-name and the gate-uid
// (UserID) are unchanged for allowlist/admin matching — only the accounts key
// normalizes here. Use this everywhere accounts builds a handle key (resolve /
// bind / usage); never hand-build (Source, UserID) for accounts.
func (ev MessageEvent) AccountHandle() (source, uid string) {
	uid = ev.UserID
	if ev.StableUID != "" {
		uid = ev.StableUID
	}
	return platformKind(ev.Source), uid
}

// platformKind folds a per-bot / per-account source name to its platform
// family: telegram* → "telegram", email / email-* → "email", feishu* →
// "feishu", discord* → "discord". Anything else (wechat, local, internal,
// future singletons) is its own kind. The mapping that makes cross-bot /
// cross-mailbox dedup work.
func platformKind(source string) string {
	switch {
	case strings.HasPrefix(source, "telegram"):
		return "telegram"
	case source == "email" || strings.HasPrefix(source, "email-"):
		return "email"
	case strings.HasPrefix(source, "feishu"):
		return "feishu"
	case strings.HasPrefix(source, "discord"):
		return "discord"
	default:
		return source
	}
}

// Account / SourceHandle — the cross-entry user-identity ledger. See
// docs/accounts.md. An Account is one logical person; a SourceHandle is one
// per-platform identity (the natural key (source, platform_uid)). Many handles
// bind to one account, so bob recognises the same person across telegram /
// feishu / email.
//
// This is NOT an authorization surface: it never gates tools/skills (agora →
// inbox permissions; non-agora → the plain "user" role in permissions.yaml).
// Binding records identity; access is still the source allowlist's job. See
// docs/accounts.md §1.1 + §9.
//
// Lives in the main store (NOT the agora substore) because the ledger is
// accumulated user knowledge that must survive schema bumps — agora is
// wipe-on-bump, which would be wrong here.

// ErrAccountNotFound is returned by GetAccount when no account has the id.
var ErrAccountNotFound = errors.New("account not found")

// Account response status — the central per-person switch (docs/accounts.md).
// This is NOT a permission gate (it never touches tools/skills/grants); it only
// decides whether bob ENGAGES with the person at all, and follows them across
// every bound handle. Differs from a denylist (per-platform, per-raw-handle,
// source-config) by being one toggle on the logical person.
const (
	AccountStatusActive   = "active"   // normal — bob responds
	AccountStatusPaused   = "paused"   // bob ignores this person within agora only
	AccountStatusDisabled = "disabled" // bob ignores this person everywhere (agora + ordinary chat)
)

// ListKind selects which per-source list a writer edits (allowlist vs admins).
// Used by the per-family yaml writers (docs/accounts.md §12 L1) so one
// dispatcher signature covers both axes.
type ListKind int

const (
	ListAllow ListKind = iota // sources/*.yaml allowlist (who may talk)
	ListAdmin                 // sources/*.yaml admins (who is admin)
)

// Account is one logical person — the binding layer over per-platform handles.
type Account struct {
	ID          string // "ac_..." assigned by the store on create
	DisplayName string
	Note        string // free-form admin note
	CreatedAt   int64  // unix seconds
	UpdatedAt   int64  // unix seconds
}

// SourceHandle is one per-platform identity bound to an account. A row exists
// ONLY for a real binding — the ledger does not census every passing sender.
//
// IDENTITY key = (Source, PlatformUID) where Source is the PLATFORM-KIND and
// PlatformUID the cross-app-stable uid (docs/accounts.md §12 L3) — so the same
// person is one row across all your telegram bots / feishu apps / email
// mailboxes. ACCESS coordinates (BoundSource, AccessUID) are the REAL
// source-name + gate-uid used at bind time — what SetAdmin / auto-allowlist
// write into the per-source yaml (the gate matches those, not the normalized
// identity key). Recorded at bind, not updated per message.
type SourceHandle struct {
	ID          string // "sh_..." assigned by the store on bind
	AccountID   string // FK → Account.ID
	Source      string // PLATFORM-KIND ("telegram" / "feishu" / "email" / ...)
	PlatformUID string // cross-app-stable uid (telegram user_id / feishu union_id / email addr)
	BoundSource string // REAL source-name at bind (telegram2 / feishu / email-support) — for yaml writes
	AccessUID   string // gate-uid at bind (telegram user_id / feishu open_id / email addr) — what the yaml gate matches
	DisplayName string // last-seen display name (optional)
	BoundAt     int64  // unix seconds
	BoundVia    string // audit: the claimcode that created this binding (may be "")
}

// CodeRedeemer is the bare-code 门前 hook injected into gated sources. On each
// inbound — AFTER the denylist drop but BEFORE the allowlist gate — the source
// offers the message text; if it is a live claim code, TryRedeem performs the
// bound action (account binding / agora wire) and returns (reply, true): the
// source sends reply to that chat and stops. A non-code returns ("", false) so
// normal gating continues. The code itself is the credential, so this lets an
// as-yet-unallowlisted (but not denylisted) sender bind + be let in. A nil
// CodeRedeemer is a no-op — sources nil-check. See docs/accounts.md §6.
type CodeRedeemer interface {
	TryRedeem(ctx context.Context, ev MessageEvent) (reply string, consumed bool)
}

// AccountUsage is one account's usage rolled up across its bound handles:
// grand totals plus the per-KIND token breakdown and per-service NATIVE-unit
// counts. Built from the three read seams over each handle's usage_json blob.
type AccountUsage struct {
	Turns, Success      int64
	InTokens, OutTokens int64                 // grand total tokens (incl legacy aggregate)
	TokensByKind        map[string]KindTokens // per-kind (llm/ocr/translate); excludes legacy aggregate
	Native              map[string]int64      // per "kind:unit" (e.g. "asr:s") → amount
}

// AccountRosterRow is one row of the paginated webui roster: identity, the
// distinct platform kinds of the account's bound handles, and the rolled-up
// usage totals across those handles. Built in a SINGLE pass over the page's
// handles+usage blobs (a fixed number of queries regardless of page size), so
// paging the roster does not degrade as accounts accumulate. Lighter than
// AccountUsage (no per-kind / native breakdown) — the roster shows only the
// turn count + total tokens.
type AccountRosterRow struct {
	ID          string
	DisplayName string
	Kinds       []string // distinct platform kinds (handle source), sorted
	Turns       int64
	Success     int64
	InTokens    int64
	OutTokens   int64
}

// AccountStore is the cross-entry identity ledger surface. Satisfied by the
// same concrete store impls that satisfy SessionStore — see the
// store/{sqlite,postgres,fallback} packages. Account-link codes themselves are
// NOT here: they live in the in-memory claimcode.Store (short-lived); only the
// durable bindings land in this store.
type AccountStore interface {
	// CreateAccount inserts a new account and returns it with its assigned id.
	CreateAccount(ctx context.Context, displayName, note string, nowUnix int64) (Account, error)

	// GetAccount returns the account with id, or ErrAccountNotFound.
	GetAccount(ctx context.Context, id string) (Account, error)

	// ListAccounts returns every account, newest first.
	ListAccounts(ctx context.Context) ([]Account, error)

	// BindHandle binds (Source, PlatformUID) to AccountID. UPSERT on
	// (source, platform_uid): re-binding an already-bound handle REPOINTS it to
	// the new account (admin re-wire). Returns the stored handle (id assigned).
	BindHandle(ctx context.Context, h SourceHandle, nowUnix int64) (SourceHandle, error)

	// HandleBySourceUID returns the binding for (source, platformUID).
	// ok=false when the handle is unbound (a plain sender).
	HandleBySourceUID(ctx context.Context, source, platformUID string) (SourceHandle, bool, error)

	// HandlesForAccount returns every handle bound to accountID.
	HandlesForAccount(ctx context.Context, accountID string) ([]SourceHandle, error)

	// AddTurnUsage records one turn's usage against a handle in a SINGLE
	// read-modify-write of the downsampled usage_json blob — one row per
	// (source, platformUID), date-keyed + age-downsampled so it's bounded
	// forever yet the summed total is exact. Keyed by the raw handle — NOT the
	// account — so usage is merge-safe: account totals are a JOIN, recomputed
	// correctly after a re-wire/merge. Records in one write (core.BumpTurnUsage):
	// read-modify-write of the downsampled usage_json blob (core.BumpTurnUsage):
	// turns + successes, per-KIND token counts (tokens: kind → {In,Out}), and
	// per-service NATIVE-unit counts (native: "kind:unit" → amount, e.g.
	// "asr:s"). Tokens and native units are stored disjoint so neither sum
	// conflates the other. Best-effort. See docs/accounts.md §13.
	AddTurnUsage(ctx context.Context, source, platformUID string, turns, success int64, tokens map[string]KindTokens, native map[string]int64) error

	// MergeAccounts folds fromID into toID: re-points all of fromID's handles to
	// toID, then deletes fromID. Handle uniqueness guarantees no key clash (the
	// two accounts' handles are disjoint). Usage is handle-keyed so it follows
	// automatically (the account-totals JOIN now rolls those handles up to
	// toID). MUST re-point before delete (FK CASCADE would otherwise drop the
	// handles). See docs/accounts.md (cheap-merge design).
	MergeAccounts(ctx context.Context, fromID, toID string) error

	// AccountUsageBreakdown sums an account's usage across all its bound handles'
	// blobs (the JOIN), broken down: grand totals (turns/success/in/out tokens),
	// per-KIND tokens (TokensByKind), and per-service NATIVE units (Native). Built
	// from the three pure-Go read seams (SumHandleUsage / SumTokensByKind /
	// SumServiceUsage). Zero value when no handles / no usage.
	AccountUsageBreakdown(ctx context.Context, accountID string) (AccountUsage, error)

	// AccountRosterPage returns one page of the roster (newest first) plus the
	// total account count: each row carries the account's distinct platform
	// kinds and rolled-up usage totals (turns/success/tokens), assembled in a
	// single pass over the page's handles+usage blobs. For the webui accounts
	// view.
	AccountRosterPage(ctx context.Context, limit, offset int) (rows []AccountRosterRow, total int64, err error)

	// SetAccountLanguage sets (or clears, with "") the account's reply-language
	// preference — set by /language, follows the person across bound handles.
	// docs/accounts.md §13.4.
	SetAccountLanguage(ctx context.Context, accountID, language string) error

	// AccountLanguage returns the account's reply-language preference ("" =
	// unset). Read per-turn to drive i18n + the prompt reply-language line.
	AccountLanguage(ctx context.Context, accountID string) (string, error)

	// SetAccountStatus sets the account's response status (AccountStatus* const:
	// active / paused / disabled) — the central per-person engage switch set by
	// /accounts pause|disable|resume. Follows the person across bound handles.
	SetAccountStatus(ctx context.Context, accountID, status string) error

	// AccountStatus returns the account's response status. A missing/legacy row
	// reads as AccountStatusActive (""→active is normalized by the store). Read
	// at inbound admission to decide whether to engage. docs/accounts.md.
	AccountStatus(ctx context.Context, accountID string) (string, error)

	// --- per-account skill forks (docs/skill-fork.md §6) ---

	// GetSkillOverride returns the raw JSON manifest of accountID's fork of
	// skillName, ok=false when there is no fork. The store round-trips the JSON
	// opaquely (a core.SkillManifest); accounts.Manager owns marshal/unmarshal.
	GetSkillOverride(ctx context.Context, accountID, skillName string) (manifest string, ok bool, err error)

	// SetSkillOverride UPSERTs the fork manifest JSON for (accountID, skillName).
	// Last-writer-wins — re-customising overwrites. nowUnix → updated_at.
	SetSkillOverride(ctx context.Context, accountID, skillName, manifest string, nowUnix int64) error

	// DeleteSkillOverride drops accountID's fork of skillName (the /uncustomize
	// path). existed=false when there was nothing to delete (idempotent).
	DeleteSkillOverride(ctx context.Context, accountID, skillName string) (existed bool, err error)

	// ListSkillOverrideNames returns the skill names accountID has forked
	// (sorted), for the skill_manager(list) fork marking + sandbox-sync filter.
	ListSkillOverrideNames(ctx context.Context, accountID string) ([]string, error)
}
