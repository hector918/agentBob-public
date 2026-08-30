// Package pgpool is the shared database foundation: it owns the single
// PostgreSQL connection pool and provides the SQL-execution surface
// (contract.DB) that every module's persistence layer runs its own SQL against.
//
// It is the ONLY place that knows the database is Postgres — it wraps a
// database/sql pool driven by the pgx stdlib driver and hides that behind
// contract.DB. Domain modules own their own tables, migrations, and queries;
// they Require contract.DB and never import this package's internals.
//
// Resilience: pgpool deliberately has NO second backend and NO fallback swap. A
// sustained outage surfaces as plain errors; durability and graceful
// degradation are the trunk's job (entry WAL + admission control —
// docs/architecture-vision.md §8), not a backend switch. This is the
// simplification that retired the old store/{postgres,sqlite,fallback} trio.
package pgpool
