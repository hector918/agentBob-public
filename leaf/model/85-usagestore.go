package model

import (
	"context"
	"fmt"

	"agentbob/contract"
	"agentbob/migrate"
)

// usageSchemaV1 is the model module's own table: hourly per-entry usage.
const usageSchemaV1 = `
CREATE TABLE IF NOT EXISTS bob_model_usage (
    entry_name    TEXT   NOT NULL,
    hour_start    BIGINT NOT NULL,
    calls         BIGINT NOT NULL DEFAULT 0,
    errors        BIGINT NOT NULL DEFAULT 0,
    input_tokens  BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (entry_name, hour_start)
);
CREATE INDEX IF NOT EXISTS idx_bob_model_usage_hour ON bob_model_usage(hour_start);
`

// MigrateUsage brings the model module's tables to the current version.
func MigrateUsage(ctx context.Context, db contract.DB) error {
	return migrate.Run(ctx, db, "model", []migrate.Step{{Version: 1, SQL: usageSchemaV1}})
}

// modelUsageStorePG persists hourly per-entry usage to bob_model_usage. Satisfies the
// pool's ModelUsageStore (the recorder's flush target).
type modelUsageStorePG struct{ db contract.DB }

// NewModelUsageStorePG returns a ModelUsageStore over db.
func NewModelUsageStorePG(db contract.DB) ModelUsageStore { return modelUsageStorePG{db: db} }

func (u modelUsageStorePG) AddModelUsage(ctx context.Context, entryName string, hourStart, calls, errs, inputTokens, outputTokens int64) error {
	if entryName == "" {
		return fmt.Errorf("AddModelUsage: empty entryName")
	}
	_, err := u.db.Exec(ctx,
		`INSERT INTO bob_model_usage (entry_name, hour_start, calls, errors, input_tokens, output_tokens)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (entry_name, hour_start) DO UPDATE SET
		     calls         = bob_model_usage.calls         + excluded.calls,
		     errors        = bob_model_usage.errors        + excluded.errors,
		     input_tokens  = bob_model_usage.input_tokens  + excluded.input_tokens,
		     output_tokens = bob_model_usage.output_tokens + excluded.output_tokens`,
		entryName, hourStart, calls, errs, inputTokens, outputTokens,
	)
	if err != nil {
		return fmt.Errorf("AddModelUsage: %w", err)
	}
	return nil
}

// PruneModelUsage deletes usage rows whose hour bucket starts before cutoffUnix
// (epoch seconds). bob_model_usage is one row per (entry, wall-clock hour) and only
// ever grows via the additive UPSERT, so it needs a retention sweep. Idempotent: a
// re-run at the same cutoff deletes nothing new. Returns rows removed.
func (u modelUsageStorePG) PruneModelUsage(ctx context.Context, cutoffUnix int64) (int64, error) {
	res, err := u.db.Exec(ctx, `DELETE FROM bob_model_usage WHERE hour_start < $1`, cutoffUnix)
	if err != nil {
		return 0, fmt.Errorf("PruneModelUsage: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
