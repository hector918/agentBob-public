package store

import (
	"context"

	"agentbob/contract"
	"agentbob/migrate"
)

// schemaV1: two tables.
//   - defs: one row per arrangement (the orchestration). spec is the validated JSON,
//     frozen once status='started'. status is a free string (started|done|cancelled).
//   - items: the live queue. role = the current bucket; engine acts only on
//     status='queued' (dispatch/pull) and 'in_flight' (await submit) — every other
//     status string is a PARKED item (shown, not acted on).
//
// NOT wipe-on-bump: in-flight work is real — prefer additive ALTERs.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS bob_arrangement_defs (
	id         TEXT PRIMARY KEY,
	company    TEXT   NOT NULL,
	author     TEXT   NOT NULL DEFAULT '',
	status     TEXT   NOT NULL DEFAULT 'started',
	spec       TEXT   NOT NULL DEFAULT '',
	created_at BIGINT NOT NULL,
	updated_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bob_arrangement_defs_status ON bob_arrangement_defs (status);

CREATE TABLE IF NOT EXISTS bob_arrangement_items (
	id              TEXT PRIMARY KEY,
	arrangement_id  TEXT    NOT NULL,
	role            TEXT    NOT NULL,
	priority        INTEGER NOT NULL DEFAULT 0,
	payload         TEXT    NOT NULL DEFAULT '',
	status          TEXT    NOT NULL DEFAULT 'queued',
	claimed_by      TEXT    NOT NULL DEFAULT '',
	claimed_at      BIGINT  NOT NULL DEFAULT 0,
	created_at      BIGINT  NOT NULL,
	updated_at      BIGINT  NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bob_arrangement_items_bucket
	ON bob_arrangement_items (arrangement_id, role, status, priority DESC, created_at);
CREATE INDEX IF NOT EXISTS idx_bob_arrangement_items_status
	ON bob_arrangement_items (status);
`

// Migrate applies the arrangement schema (module key "arrangement").
func Migrate(ctx context.Context, db contract.DB) error {
	return migrate.Run(ctx, db, "arrangement", []migrate.Step{
		{Version: 1, SQL: schemaV1},
	})
}
