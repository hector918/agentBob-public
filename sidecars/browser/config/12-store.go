package config

import (
	"os"
)

// StoreConfig selects + parametrises the persistence backend. See
// docs/store-dual-backend.md for the full design. Two sub-blocks
// (SQLite, Postgres) describe per-backend connection params; only
// the one matching Backend is consulted at startup. Backend=""
// defaults to "sqlite" for backward compat.
type StoreConfig struct {
	// Backend = "sqlite" (default) | "postgres" | "fallback".
	// - "sqlite": single-process sqlite; SQLite block applies.
	// - "postgres": pg only; Postgres block applies. No fallback.
	// - "fallback": pg primary + sqlite secondary, runtime failover.
	//   Both blocks apply.
	// Anything else → startup fatal.
	Backend  string         `yaml:"backend"`
	SQLite   SQLiteConfig   `yaml:"sqlite"`
	Postgres PostgresConfig `yaml:"postgres"`
}

// SQLiteConfig is per-backend params for the sqlite impl.
type SQLiteConfig struct {
	// Path to the .db file. "" → $BOB_HOME/sqlite-store/state.db.
	Path string `yaml:"path"`
}

// PostgresConfig is per-backend params for the postgres impl. The
// DSN itself is read from env (never persisted in YAML) so secrets
// never end up in config.yaml.
type PostgresConfig struct {
	// DSNEnv names the env var holding the libpq DSN, e.g.
	// "BOB_POSTGRES_DSN". Default "BOB_POSTGRES_DSN".
	DSNEnv string `yaml:"dsn_env"`
	// HealthIntervalSec — how often FallbackStore's health checker
	// pings primary. 0 → default 30s.
	HealthIntervalSec int `yaml:"health_interval_sec"`
	// FailbackSuccessCount — consecutive successful pings before
	// failback to primary. 0 → default 3. Hysteresis prevents
	// flapping when primary is intermittent.
	FailbackSuccessCount int `yaml:"failback_success_count"`
}

// BackendEff resolves the active backend.
//
// Selection (in priority order):
//  1. Explicit `store.backend` in config.yaml — honoured as-is.
//  2. Auto-detect: if the configured pg DSN env var (default
//     BOB_POSTGRES_DSN) is non-empty in the process env, run in
//     "fallback" mode (pg primary + sqlite secondary).
//  3. Default: "sqlite" only.
//
// Rationale: sqlite is the always-on baseline; a deployer who drops a
// DSN into .env shouldn't also need to remember to flip a yaml flag.
// Operators who explicitly want pg-only (no sqlite secondary) still
// set backend: postgres in config.yaml.
func (s StoreConfig) BackendEff() string {
	if s.Backend != "" {
		return s.Backend
	}
	if os.Getenv(s.Postgres.DSNEnvEff()) != "" {
		return "fallback"
	}
	return "sqlite"
}

func (p PostgresConfig) DSNEnvEff() string {
	if p.DSNEnv == "" {
		return "BOB_POSTGRES_DSN"
	}
	return p.DSNEnv
}

func (p PostgresConfig) HealthIntervalSecEff() int {
	if p.HealthIntervalSec <= 0 {
		return 30
	}
	return p.HealthIntervalSec
}

func (p PostgresConfig) FailbackSuccessCountEff() int {
	if p.FailbackSuccessCount <= 0 {
		return 3
	}
	return p.FailbackSuccessCount
}
