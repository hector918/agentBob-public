package store

import (
	"context"

	"agentbob/contract"
	"agentbob/migrate"
)

// schemaV1 creates the accounts module's tables: logical accounts, their bound
// source-handles (identity key = (source, platform_uid)), and per-handle usage
// (one downsampled JSON blob per handle). The bob_accounts.flow column carries
// the per-account routing assignment; it defaults to 'normal' so a bound account
// routes like everyone else until an override is set.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS bob_accounts (
    id           TEXT PRIMARY KEY,
    display_name TEXT NOT NULL DEFAULT '',
    note         TEXT NOT NULL DEFAULT '',
    flow         TEXT NOT NULL DEFAULT 'normal',
    created_at   BIGINT NOT NULL,
    updated_at   BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS bob_account_handles (
    id           TEXT PRIMARY KEY,
    account_id   TEXT NOT NULL REFERENCES bob_accounts(id) ON DELETE CASCADE,
    source       TEXT NOT NULL,
    platform_uid TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    bound_at     BIGINT NOT NULL,
    bound_via    TEXT NOT NULL DEFAULT '',
    bound_source TEXT NOT NULL DEFAULT '',
    access_uid   TEXT NOT NULL DEFAULT '',
    UNIQUE(source, platform_uid)
);
CREATE INDEX IF NOT EXISTS idx_bob_account_handles_account ON bob_account_handles(account_id);

CREATE TABLE IF NOT EXISTS bob_account_handle_usage (
    source       TEXT NOT NULL,
    platform_uid TEXT NOT NULL,
    usage_json   TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY (source, platform_uid)
);
`

// Migrate brings the accounts module's tables to the current version.
func Migrate(ctx context.Context, db contract.DB) error {
	return migrate.Run(ctx, db, "accounts", []migrate.Step{
		{Version: 1, SQL: schemaV1},
		// v2: per-sender reply-language seed on the passive handle-usage row (keyed
		// (source,platform_uid), no binding needed). Data-preserving ALTER (accounts
		// holds real user data — NOT wipe-on-bump); IF NOT EXISTS makes it idempotent
		// on a fresh DB that built v1 with the column already present in a later squash.
		{Version: 2, SQL: `ALTER TABLE bob_account_handle_usage ADD COLUMN IF NOT EXISTS lang TEXT NOT NULL DEFAULT '';`},
		// v3: account status gate. 'active' (normal) | 'paused' (access falls back to the
		// intro flow with a "suspended" notice). Orthogonal to flow entitlement. Data-
		// preserving ALTER; IF NOT EXISTS keeps it idempotent.
		{Version: 3, SQL: `ALTER TABLE bob_accounts ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active';`},
		// v4: EntitledFlows dropped the implicit "normal" floor (access is now a granted
		// entitlement). Stamp "normal" onto every EXISTING account that lacks it, so no
		// current user loses basic access in the flip. Token-precise match (',flow,' vs
		// ',normal,') so it never fires on an account that already has normal and never
		// matches a substring like "abnormal". Version-gated → runs EXACTLY ONCE: future
		// bare onboarding accounts (flow='') are created AFTER v4 and are NOT re-flowed.
		{Version: 4, SQL: `UPDATE bob_accounts
			SET flow = CASE WHEN flow = '' THEN 'normal' ELSE 'normal,' || flow END
			WHERE ',' || flow || ',' NOT LIKE '%,normal,%';`},
		// v5: onboarding intake. onboard_scope records WHERE a bare account first appeared
		// (the chat scope it was minted in), so admin approval can grant agora-vs-normal by
		// ScopeIsAgora(onboard_scope) without a separate per-approval input. Empty for
		// already-approved / pre-existing accounts. Data-preserving ALTER; IF NOT EXISTS
		// keeps it idempotent.
		{Version: 5, SQL: `ALTER TABLE bob_accounts ADD COLUMN IF NOT EXISTS onboard_scope TEXT NOT NULL DEFAULT '';`},
		// v6: API keys. A bearer credential per account for machine-facing entry points
		// (leaf/modelgate). Only the sha256 hash is stored (UNIQUE — the verify lookup
		// key). models / tags are CSV allowlist columns (one form per key). ON DELETE
		// CASCADE so deleting an account reaps its keys. Data-preserving CREATE.
		{Version: 6, SQL: `
CREATE TABLE IF NOT EXISTS bob_account_apikeys (
    id            TEXT PRIMARY KEY,
    account_id    TEXT NOT NULL REFERENCES bob_accounts(id) ON DELETE CASCADE,
    secret_hash   TEXT NOT NULL UNIQUE,
    label         TEXT NOT NULL DEFAULT '',
    models        TEXT NOT NULL DEFAULT '',
    tags          TEXT NOT NULL DEFAULT '',
    max_inflight  INT NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'active',
    created_at    BIGINT NOT NULL,
    last_used_at  BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_bob_account_apikeys_account ON bob_account_apikeys(account_id);`},
		// v7: per-key routing policy beyond the allowlist — kind is the pool-entry
		// "lane" the key routes to (empty → llm) and prefer_tags is a CSV SOFT tag
		// preference the consumer passes to the pool as a tie-break (empty → the
		// consumer's ambient default). Both default to empty, so a pre-existing key
		// keeps its exact behaviour. Data-preserving ALTERs.
		{Version: 7, SQL: `
ALTER TABLE bob_account_apikeys ADD COLUMN IF NOT EXISTS kind        TEXT NOT NULL DEFAULT '';
ALTER TABLE bob_account_apikeys ADD COLUMN IF NOT EXISTS prefer_tags TEXT NOT NULL DEFAULT '';`},
		// v8: the lane moved from the KEY (a scalar fence) to the REQUEST (a routing
		// parameter the model picker consumes), so a key now carries a lane ALLOWLIST.
		// kinds is its CSV column. The v6/v7 columns kind / tags / prefer_tags /
		// max_inflight become dead — nothing reads or writes them — but are NOT
		// dropped: DROP COLUMN is irreversible and keeping them costs nothing.
		//
		// A pre-v8 TAG-form key therefore has kinds='' with nothing else v8 reads, so
		// it reaches nothing (fail-closed) — the intended breaking behaviour: re-mint
		// or UPDATE it. A pre-v8 PIN-form key (models='…') keeps working, because
		// keyIsLaneForm keys off Models being empty; it does lose v7's extra kind
		// fence on the pinned path (docs/modelgate-apikey.md §8 says to audit those
		// rows before rolling out). Data-preserving ALTER.
		{Version: 8, SQL: `
ALTER TABLE bob_account_apikeys ADD COLUMN IF NOT EXISTS kinds TEXT NOT NULL DEFAULT '';`},
	})
}
