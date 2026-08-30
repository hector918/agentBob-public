package migrate

import (
	"context"
	"fmt"
	"sort"

	"agentbob/contract"
)

// Step is one migration: a strictly increasing Version (starting at 1) and the
// SQL applied at that version. Keep steps additive and append-only — never edit
// a shipped step's SQL, add a new higher-version step instead.
type Step struct {
	Version int
	SQL     string
}

// Run applies every step whose Version exceeds module's currently recorded
// version, in ascending order, recording each as it goes. Idempotent: a re-run
// with the same steps applies nothing. The shared schema_version(module,
// version) table is created on first use.
//
// Each step's DDL and its version bump run in ONE transaction (Postgres DDL is
// transactional), so the pair is all-or-nothing: a crash mid-step rolls the DDL
// back and leaves the recorded version untouched, and a re-run re-applies the
// step cleanly from scratch. Steps therefore do NOT need to be individually
// idempotent.
//
// CONCURRENT boots (two processes on one DB) are serialized per step by the
// version row itself: the bump is a compare-and-swap (UPDATE ... WHERE version
// = <the version this process read>), so when two processes race the same step
// the loser's CAS matches zero rows and its WHOLE transaction — the step's DML
// included — rolls back; it then re-reads the version and skips what the
// winner applied. No advisory lock: that would be backend-specific SQL, and
// the CAS gives the same no-double-apply guarantee on both backends.
func Run(ctx context.Context, db contract.DB, module string, steps []Step) error {
	// Validate steps before touching the database (pure check).
	sorted := append([]Step(nil), steps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	for i := range sorted {
		// Versions start at 1; a <=0 version would sort to the front and be
		// silently skipped by the apply loop (cur starts at 0) — reject it loudly.
		if sorted[i].Version < 1 {
			return fmt.Errorf("migrate: %s: step version must be >= 1, got %d", module, sorted[i].Version)
		}
		if i > 0 && sorted[i].Version == sorted[i-1].Version {
			return fmt.Errorf("migrate: %s: duplicate step version %d", module, sorted[i].Version)
		}
	}

	if _, err := db.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_version (module TEXT PRIMARY KEY, version INTEGER NOT NULL)`,
	); err != nil {
		return fmt.Errorf("migrate: ensure schema_version: %w", err)
	}
	// Pre-create the module's version row (version 0) so every later bump is a
	// plain conditional UPDATE — the CAS the concurrency guarantee rests on.
	if _, err := db.Exec(ctx,
		`INSERT INTO schema_version (module, version) VALUES ($1, 0) ON CONFLICT (module) DO NOTHING`, module,
	); err != nil {
		return fmt.Errorf("migrate: seed version row for %q: %w", module, err)
	}

	// COALESCE over a subquery always returns exactly one row (0 when the module
	// has no record yet), so Scan never hits a no-rows ambiguity — any error
	// here is a real one.
	readCur := func() (int, error) {
		var cur int
		err := db.QueryRow(ctx,
			`SELECT COALESCE((SELECT version FROM schema_version WHERE module = $1), 0)`, module,
		).Scan(&cur)
		if err != nil {
			return 0, fmt.Errorf("migrate: read version for %q: %w", module, err)
		}
		return cur, nil
	}
	cur, err := readCur()
	if err != nil {
		return err
	}

	for i := 0; i < len(sorted); {
		s := sorted[i]
		if s.Version <= cur {
			i++
			continue
		}
		raced, err := applyStep(ctx, db, module, s, cur)
		if err != nil {
			return err
		}
		if raced {
			// A concurrent migrator won this step (our CAS matched nothing and
			// our tx rolled back) — re-read and re-evaluate from the new floor.
			if cur, err = readCur(); err != nil {
				return err
			}
			continue
		}
		cur = s.Version
		i++
	}
	return nil
}

// applyStep runs one step's DDL and records its version atomically in a single
// transaction. The record is a CAS on the version the caller READ (prev): when
// a concurrent migrator already advanced it, the CAS matches zero rows and the
// whole tx — this step's DML included — is rolled back; raced=true tells the
// caller to re-read and continue.
func applyStep(ctx context.Context, db contract.DB, module string, s Step, prev int) (raced bool, err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("migrate: %s v%d: begin: %w", module, s.Version, err)
	}
	if _, err := tx.Exec(ctx, s.SQL); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("migrate: %s v%d: %w", module, s.Version, err)
	}
	res, err := tx.Exec(ctx,
		`UPDATE schema_version SET version = $2 WHERE module = $1 AND version = $3`,
		module, s.Version, prev,
	)
	if err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("migrate: record %s v%d: %w", module, s.Version, err)
	}
	n, aerr := res.RowsAffected()
	if aerr != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("migrate: rows-affected %s v%d: %w", module, s.Version, aerr)
	}
	if n == 0 {
		_ = tx.Rollback() // lost the race — undo this step's DML too
		return true, nil
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("migrate: %s v%d: commit: %w", module, s.Version, err)
	}
	return false, nil
}
