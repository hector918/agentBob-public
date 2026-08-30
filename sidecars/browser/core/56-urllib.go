// File 56-urllib.go holds the "host" / storage interface for the URL
// library — the full contract that backend impls (sqlite, postgres,
// fallback) implement, AND the small types those impls share (Seed,
// Health, Stats).
//
// Why here, not in urllib: subpackages
// urllib/sqlite, urllib/postgres, urllib/fallback all need these
// types, and the factory in urllib root needs to import the
// subpackages. If the types lived in urllib root, subpackages would
// import urllib → cycle. Mirrors the SessionStore pattern: the
// interface in core/, the impls in store/{sqlite,postgres,fallback}.
//
// core.URLLibrary (in 57-tool.go) stays as the narrow tool-facing
// subset. URLStore embeds it + adds the operational methods (Seed,
// Stats, Ping, HealthReport, Close).
package core

import (
	"context"
	"time"
)

// URLLibrarySeed is one bootstrap row. DefaultSeeds (the 5 search
// engines) plus operator-loaded urllib.yaml rows are pushed into a
// fresh DB on factory open via URLStore.Seed.
type URLLibrarySeed struct {
	URL            string `yaml:"url"`
	Title          string `yaml:"title"`
	Excerpt        string `yaml:"excerpt"`
	IsSearchEngine bool   `yaml:"is_search_engine"`
}

// URLLibraryStats — for doctor. Cheap; do not include expensive
// aggregates.
type URLLibraryStats struct {
	TotalEntries  int
	SearchEngines int
}

// URLLibraryHealth — doctor-side health report. Mirrors StoreHealth
// (same fields, separate type) — urllib is independent of the
// session store and may be in a different healthy/failover state.
type URLLibraryHealth struct {
	Backend      string // "sqlite" / "postgres" / "fallback" / "null"
	BackendInUse string // for fallback: "primary"/"secondary"; "" otherwise
	Healthy      bool
	LastFailover time.Time
	LastFailback time.Time
	Notes        []string
}

// URLStore is the full URL-library contract — what backend impls
// satisfy and what the factory + doctor + fallback wrapper see. Tools
// only see the narrow URLLibrary subset (defined in 57-tool.go) and
// must not gain access to Seed/Close/Ping/HealthReport.
type URLStore interface {
	URLLibrary

	// Seed inserts one bootstrap row with INSERT-OR-IGNORE semantics
	// (preserves any operator edits to an existing row). Called by
	// the factory after Open, once per DefaultSeeds + yaml-loaded
	// seed.
	Seed(ctx context.Context, s URLLibrarySeed)

	// Stats — for doctor. Cheap; do not include expensive aggregates.
	Stats() URLLibraryStats

	// Ping — low-cost connectivity check. Used by the fallback
	// health checker.
	Ping(ctx context.Context) error

	// HealthReport returns backend-specific diagnostics for doctor.
	HealthReport(ctx context.Context) URLLibraryHealth

	// Prune bounds the urls / url_queries tables on a long-running
	// gateway. Every visited URL is recorded forever otherwise, so the
	// tables (and the query-match index) grow without limit. Prune keeps
	// at most maxRows non-search-engine urls, evicting the
	// lowest-value rows first (fewest visits, then oldest last_seen);
	// the FK cascade on url_queries drops their query rows too.
	// Search-engine seed rows are always preserved. Best-effort: returns
	// rows-deleted and never fails the caller (mirrors the
	// no-error-return contract of Record/Recall). maxRows <= 0 is a
	// no-op.
	Prune(ctx context.Context, maxRows int) (int64, error)

	// Close releases the underlying connection.
	Close() error
}
