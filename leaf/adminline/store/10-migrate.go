package store

import (
	"context"

	"agentbob/contract"
	"agentbob/migrate"
)

// schemaV1 creates the append-only admin-line audit stream. Age-pruned by the
// module's housekeeping task (PruneAdminLine), served by idx_bob_admin_line_ts.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS bob_admin_line (
    id        BIGSERIAL PRIMARY KEY,
    ts        BIGINT NOT NULL,
    level     TEXT NOT NULL,
    origin    TEXT NOT NULL DEFAULT '',
    text      TEXT NOT NULL DEFAULT '',
    forwarded TEXT NOT NULL DEFAULT 'none'
);
CREATE INDEX IF NOT EXISTS idx_bob_admin_line_ts ON bob_admin_line(ts DESC);
`

// Migrate brings the admin-line table to the current version.
func Migrate(ctx context.Context, db contract.DB) error {
	return migrate.Run(ctx, db, "adminline", []migrate.Step{
		{Version: 1, SQL: schemaV1},
	})
}
