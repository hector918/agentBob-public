package store

import (
	"context"

	"agentbob/contract"
	"agentbob/migrate"
)

// schemaV1 creates the agora module's entity tables. This is the CONSOLIDATED
// base shape: the skeleton reached this layout through incremental wipe-on-bump
// schema versions (v1..v10), but agora is wipe-on-bump and NOT yet deployed in
// trunk, so the skeleton's incremental ALTERs are folded into one base CREATE
// here. A future destructive schema change adds a higher migrate.Step.
//
// DROPPED relative to skeleton (dead / deferred per docs/agora-port.md):
//   - reports_to_inbox_id (retired — directory-based auth + auto-return)
//   - bob_agora_dispatch_queue (去程, a later stage)
//   - bob_agora_company_role_skill_overrides (deferred, no consumer)
//   - bob_agora_session_meta / suspended_sessions (dead since)
//   - bob_agora_role_assignments (removed)
//
// KEPT and folded into the base CREATE even though some consumers land in a
// later stage (wipe-on-bump = no migration cost to include now): permissions_yaml,
// playbook, status, inbox visibility_yaml, bridge_default_target_inbox_id,
// validation_default, idle/finalize columns.
//
// The bob_agora_companies.report_inbox_id FK is added in a post-CREATE DO block
// because companies↔inboxes form a reference cycle (a forward declaration would
// fail at CREATE-TABLE time in pg). The DO block makes the ADD CONSTRAINT
// idempotent.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS bob_agora_members (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    model_pref_tag      TEXT NOT NULL DEFAULT '',
    prompt_style        TEXT NOT NULL DEFAULT '',
    created_at          BIGINT NOT NULL,
    updated_at          BIGINT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_bob_agora_members_name ON bob_agora_members(name);

CREATE TABLE IF NOT EXISTS bob_agora_companies (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    config              JSONB NOT NULL DEFAULT '{}'::jsonb,
    permissions_yaml    TEXT NOT NULL DEFAULT '',
    playbook            TEXT NOT NULL DEFAULT '',
    -- report_inbox_id FK is added via the DO block below, after
    -- bob_agora_inboxes exists (companies↔inboxes form a cycle).
    report_inbox_id     TEXT,
    status              TEXT NOT NULL DEFAULT 'active',
    created_at          BIGINT NOT NULL,
    updated_at          BIGINT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_bob_agora_companies_name ON bob_agora_companies(name);

CREATE TABLE IF NOT EXISTS bob_agora_employments (
    id                  TEXT PRIMARY KEY,
    member_id           TEXT NOT NULL REFERENCES bob_agora_members(id)   ON DELETE CASCADE,
    company_id          TEXT NOT NULL REFERENCES bob_agora_companies(id) ON DELETE CASCADE,
    role_name           TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'active',
    started_at          BIGINT NOT NULL,
    ended_at            BIGINT
);
-- At most one ACTIVE employment per (member,company,role). Terminated rows (ended_at
-- set) are excluded, so terminate→rehire into the same role works even within the
-- same wall-clock second — the old UNIQUE(member,company,role,started_at) collided on
-- a same-second rehire because started_at is seconds-granular (#313).
CREATE UNIQUE INDEX IF NOT EXISTS idx_bob_agora_employments_active ON bob_agora_employments(member_id, company_id, role_name) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_bob_agora_employments_member  ON bob_agora_employments(member_id);
CREATE INDEX IF NOT EXISTS idx_bob_agora_employments_company ON bob_agora_employments(company_id);

CREATE TABLE IF NOT EXISTS bob_agora_inboxes (
    id                              TEXT PRIMARY KEY,
    kind                            TEXT NOT NULL DEFAULT 'member',
    member_id                       TEXT REFERENCES bob_agora_members(id)     ON DELETE CASCADE,
    owner_company_id                TEXT REFERENCES bob_agora_companies(id)   ON DELETE CASCADE,
    scope                           TEXT NOT NULL,
    name                            TEXT NOT NULL,
    status                          TEXT NOT NULL DEFAULT 'active',
    created_at                      BIGINT NOT NULL,
    archived_at                     BIGINT,
    bridge_default_target_inbox_id  TEXT REFERENCES bob_agora_inboxes(id) ON DELETE SET NULL,
    validation_default              TEXT NOT NULL DEFAULT 'auto',
    idle_timeout_seconds            INTEGER NOT NULL DEFAULT 86400,
    finalize_grace_seconds          INTEGER NOT NULL DEFAULT 300,
    context                         TEXT NOT NULL DEFAULT '',
    visibility_yaml                 TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_bob_agora_inboxes_scope         ON bob_agora_inboxes(scope);
CREATE INDEX        IF NOT EXISTS idx_bob_agora_inboxes_member        ON bob_agora_inboxes(member_id);
CREATE INDEX        IF NOT EXISTS idx_bob_agora_inboxes_owner_company ON bob_agora_inboxes(owner_company_id);

CREATE TABLE IF NOT EXISTS bob_agora_inbox_sources (
    id                          TEXT PRIMARY KEY,
    inbox_id                    TEXT NOT NULL REFERENCES bob_agora_inboxes(id) ON DELETE CASCADE,
    source_id                   TEXT NOT NULL,
    chat_type                   TEXT NOT NULL DEFAULT '',
    chat_id                     TEXT NOT NULL DEFAULT '',
    thread_id                   TEXT NOT NULL DEFAULT '',
    outgoing_credential_ref     TEXT,
    is_default                  BOOLEAN NOT NULL DEFAULT false,
    created_at                  BIGINT NOT NULL,
    -- per-binding unique: one inbox can't double-bind the same chat, but DIFFERENT
    -- inboxes MAY bind the same GROUP (virtual-group fan; the member is picked at
    -- routing time — see RouteGroupScope / docs).
    UNIQUE (inbox_id, source_id, chat_type, chat_id, thread_id)
);
CREATE INDEX IF NOT EXISTS idx_bob_agora_inbox_sources_inbox ON bob_agora_inbox_sources(inbox_id);
-- a DM chat routes to AT MOST ONE inbox (DM is 1:1); groups are exempt (fan to many).
CREATE UNIQUE INDEX IF NOT EXISTS idx_bob_agora_inbox_sources_dm
    ON bob_agora_inbox_sources(source_id, chat_id, thread_id) WHERE chat_type = 'dm';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
         WHERE table_schema = 'public'
           AND table_name = 'bob_agora_companies'
           AND constraint_name = 'bob_agora_companies_report_inbox_id_fkey'
    ) THEN
        ALTER TABLE bob_agora_companies
            ADD CONSTRAINT bob_agora_companies_report_inbox_id_fkey
            FOREIGN KEY (report_inbox_id)
            REFERENCES bob_agora_inboxes(id) ON DELETE SET NULL;
    END IF;
END$$;
`

// wipeAllAgora drops every agora table. agora is wipe-on-bump: a destructive schema
// change discards agora data (companies/members/employments/inboxes are cheap to
// re-bootstrap) rather than carry incremental ALTERs. CASCADE so FK order is moot.
const wipeAllAgora = `
DROP TABLE IF EXISTS
  bob_agora_inbox_sources,
  bob_agora_inboxes,
  bob_agora_employments,
  bob_agora_members,
  bob_agora_companies
CASCADE;
`

// Migrate brings the agora module's tables to the current version.
//
// v2 (#313, partial-unique active employment) is a DESTRUCTIVE schema change, so it
// runs the wipe-on-bump path: drop all agora tables, then recreate from the current
// base (schemaV1). This applies AUTOMATICALLY on next boot (migrate runs v2) — no
// manual wipe — and discards agora data, which is re-bootstrapped after deploy. A
// fresh DB harmlessly creates (v1) then drop+recreates (v2) the empty tables.
func Migrate(ctx context.Context, db contract.DB) error {
	return migrate.Run(ctx, db, "agora", []migrate.Step{
		{Version: 1, SQL: schemaV1},
		{Version: 2, SQL: wipeAllAgora + schemaV1},
		// v3: drop the never-produced/never-consumed member columns (tools_known,
		// memory_prompt — agora authorizes via company permissions_yaml, not per-member).
		// Data-preserving DROP (the columns only ever held defaults), so no wipe needed;
		// IF EXISTS makes it a no-op on a fresh DB that built the v3 base directly.
		{Version: 3, SQL: `ALTER TABLE bob_agora_members DROP COLUMN IF EXISTS tools_known;
ALTER TABLE bob_agora_members DROP COLUMN IF EXISTS memory_prompt;`},
		// v4: relax inbox-source binding for virtual-group routing — DM stays one-chat-one-
		// inbox; a GROUP may bind multiple member inboxes. Data-preserving (no wipe): swap the
		// global unique for a per-binding unique + a DM-only partial unique, and add is_default.
		{Version: 4, SQL: `
ALTER TABLE bob_agora_inbox_sources ADD COLUMN IF NOT EXISTS is_default BOOLEAN NOT NULL DEFAULT false;
DO $$
DECLARE c text;
BEGIN
  FOR c IN SELECT conname FROM pg_constraint
           WHERE conrelid = 'bob_agora_inbox_sources'::regclass AND contype = 'u'
  LOOP EXECUTE 'ALTER TABLE bob_agora_inbox_sources DROP CONSTRAINT ' || quote_ident(c); END LOOP;
END $$;
ALTER TABLE bob_agora_inbox_sources
  ADD CONSTRAINT bob_agora_inbox_sources_binding_key UNIQUE (inbox_id, source_id, chat_type, chat_id, thread_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_bob_agora_inbox_sources_dm
  ON bob_agora_inbox_sources(source_id, chat_id, thread_id) WHERE chat_type = 'dm';`},
	})
}
