package urllib

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/migrate"
)

// hostOfURL returns the lowercased host of a URL with "www." stripped ("" on parse
// failure / no host). Normalizes the per-host fetch-mode key (P4.4).
func hostOfURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	// Lowercase BEFORE stripping so a mixed/upper-case "WWW."/"Www." prefix is
	// also stripped — else the per-host key splits across www./bare buckets and
	// the FetchHint lookup (exact host match) misses. Keep in sync with
	// leaf/tools.hostOf (read side).
	return strings.TrimPrefix(strings.ToLower(u.Host), "www.")
}

// queriesCap bounds the per-url query list (newline-joined in the `queries`
// column). Keeps the LIKE-matchable text small; the most recent queries win.
const queriesCap = 20

// excerptCap / queryItemCap bound a single stored excerpt / query item so one
// pathological input can't bloat the row.
const (
	titleCap     = 240
	excerptCap   = 240
	queryItemCap = 400
)

// schemaV1 is the single urllib table. DOUBLE PRECISION epoch-seconds timestamps
// (matches the session/turn stores); `queries` is a newline-joined, deduped,
// capped list of the search queries that led to this url.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS bob_urls (
  url              TEXT PRIMARY KEY,
  title            TEXT NOT NULL DEFAULT '',
  excerpt          TEXT NOT NULL DEFAULT '',
  queries          TEXT NOT NULL DEFAULT '',
  satisfied_count  INTEGER NOT NULL DEFAULT 0,
  first_seen       DOUBLE PRECISION NOT NULL,
  last_seen        DOUBLE PRECISION NOT NULL,
  is_search_engine BOOLEAN NOT NULL DEFAULT FALSE
);
`

// schemaV2 adds the per-engine connection knobs (docs/web-fleet.md §3.1/§5):
//
//	fetch_mode — the rung that opens this engine correctly ('plain'/'render'/
//	             'stealth'; '' = unknown → treat as plain). Meaningful for engine
//	             rows; '' for ordinary recorded URLs.
//	lang_scope — the query-language this engine serves ('*' = global/any, or a tag
//	             like 'zh'). Defaults '*' so an un-scoped engine matches every lang.
//
// Existing engine rows get their modes/scopes filled by seed()'s fill-if-default
// UPSERT on the next Start (operator-set values survive). Additive, idempotent.
const schemaV2 = `
ALTER TABLE bob_urls ADD COLUMN IF NOT EXISTS fetch_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE bob_urls ADD COLUMN IF NOT EXISTS lang_scope TEXT NOT NULL DEFAULT '*';
`

// schemaV3 adds a normalized host column to bob_urls (P4.4, docs/web-fleet.md §13.4) so
// the per-HOST learned fetch mode lives IN bob_urls (reusing its fetch_mode column) rather
// than a separate table: Record stamps the host, LearnFetchMode writes the worked mode on
// the URL's row, and FetchHint reads any content row for that host. One table → it rides
// bob_urls's existing value-aware Prune (no separate sweep), and the normalized host (www
// stripped at write) avoids host-prefix/www matching warts.
const schemaV3 = `
ALTER TABLE bob_urls ADD COLUMN IF NOT EXISTS host TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_bob_urls_host ON bob_urls (host);
`

// schemaV4 drops visit_count. Recall used it as a ranking tiebreak, but it is a
// rich-get-richer popularity trap: a URL rises just by being fetched, regardless of whether
// it was USEFUL for the query — a frequently-fetched portal dominated recall for any query
// it loosely matched, forever. Recall now ranks by satisfied_count (signal ② — the page
// that actually ended a satisfied turn) then last_seen, so only proven-useful pages rise and
// fresh results outrank stale ones.
const schemaV4 = `ALTER TABLE bob_urls DROP COLUMN IF EXISTS visit_count;`

// schemaV5 retunes the seeded engine rows to the measurement (see
// DefaultSeeds). seed()'s UPSERT deliberately only FILLS fetch_mode when it is still
// '' — that keeps operator tuning sticky, but it also means a row seeded with a rung
// that has since become wrong can never be corrected from code. This one-time
// correction does what seed() must not:
//
//   - drops the DuckDuckGo engine row — its no-JS endpoint is gone (302 → a 202 JS
//     shell with zero results), so it can only ever burn a slot and feed the breaker;
//     Brave takes its place and arrives through the normal seed insert;
//   - moves Bing off the render rung — its plain SERP carries the whole result set.
//
// Scoped to the exact seeded URLs and to the value we ourselves seeded, so an
// operator who has already retuned Bing by hand keeps their setting.
const schemaV5 = `
DELETE FROM bob_urls WHERE url = 'https://duckduckgo.com/html/?q=';
UPDATE bob_urls SET fetch_mode = 'plain'
 WHERE url = 'https://www.bing.com/search?q=' AND fetch_mode = 'render';
`

// migrateURLs brings the urllib table to the current version (own "urllib"
// namespace in the shared schema_version table).
func migrateURLs(ctx context.Context, db contract.DB) error {
	return migrate.Run(ctx, db, "urllib", []migrate.Step{
		{Version: 1, SQL: schemaV1},
		{Version: 2, SQL: schemaV2},
		{Version: 3, SQL: schemaV3},
		{Version: 4, SQL: schemaV4},
		{Version: 5, SQL: schemaV5},
	})
}

// library is the pg-backed contract.URLLibrary. It also keeps an in-memory
// map[sid]url of the LAST url each sid recorded this process, so MarkSatisfied can
// bump the right row when the flow signals a satisfied turn.
type library struct {
	db contract.DB

	mu        sync.Mutex
	lastBySid map[string]string
}

func now() float64 { return clock.UnixSeconds() }

// seed inserts the bootstrap rows if absent and FILLS fetch_mode/lang_scope on
// rows that still hold their unset default. Idempotent and value-preserving:
//   - counts / title / is_search_engine of an existing row are never touched;
//   - fetch_mode is filled only when currently ” (so an operator-set rung survives);
//   - lang_scope is filled only when currently '*' (the default; operator-set survives);
//   - last_seen IS refreshed on every re-seed — non-engine seed rows (Wikipedia)
//     otherwise keep their first-boot last_seen forever and Prune drops them at
//     retention age (engine rows are prune-immune anyway). Re-seeding at Start keeps
//     seed rows alive as long as the process restarts within the retention window.
//
// This lets a pre-existing engine row (added before schemaV2) pick up its designed
// rung/scope on the next Start without a hardcoded migration backfill, while keeping
// operator tuning sticky.
func (l *library) seed(ctx context.Context, seeds []Seed) {
	t := now()
	for _, s := range seeds {
		u := strings.TrimSpace(s.URL)
		if u == "" {
			continue
		}
		ls := strings.TrimSpace(s.LangScope)
		if ls == "" {
			ls = "*" // never store '' — it would match no lang filter (engine never fired)
		}
		_, err := l.db.Exec(ctx,
			`INSERT INTO bob_urls (url, title, excerpt, queries, satisfied_count, first_seen, last_seen, is_search_engine, fetch_mode, lang_scope)
			 VALUES ($1, $2, '', '', 0, $3, $4, $5, $6, $7)
			 ON CONFLICT (url) DO UPDATE SET
			   last_seen  = EXCLUDED.last_seen,
			   fetch_mode = CASE WHEN bob_urls.fetch_mode = ''  THEN EXCLUDED.fetch_mode ELSE bob_urls.fetch_mode END,
			   lang_scope = CASE WHEN bob_urls.lang_scope = '*' THEN EXCLUDED.lang_scope ELSE bob_urls.lang_scope END`,
			u, s.Title, t, t, s.IsSearchEngine, s.FetchMode, ls,
		)
		if err != nil {
			slog.Warn("urllib: seed insert failed", "url", u, "err", err)
		}
	}
}

// Record — signal ①. Upserts a successfully-fetched url: last_seen now, title/excerpt
// refreshed to the latest non-empty, and query merged into the row's deduped/capped query
// list. Best-effort: errors log and return (a URL-memory miss must never fail the turn).
// Tracks this sid's last url for MarkSatisfied.
func (l *library) Record(ctx context.Context, sid, url, title, excerpt, query string) {
	u := strings.TrimSpace(url)
	if u == "" {
		return
	}
	title = truncRunes(title, titleCap)
	excerpt = truncRunes(excerpt, excerptCap)
	q := truncRunes(strings.TrimSpace(query), queryItemCap)

	// Read-merge-write the query list ATOMICALLY: a per-URL advisory lock (held for
	// the tx) serializes concurrent Record calls for the same URL — including the
	// first insert — so a concurrent writer can't clobber the merged query list (the
	// merge is app-side dedup+cap, not expressible in the UPSERT). Best-effort: a tx
	// hiccup logs and drops this Record.
	tx, err := l.db.Begin(ctx)
	if err != nil {
		slog.Warn("urllib: Record begin failed", "url", u, "err", err)
		return
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, u); err != nil {
		slog.Warn("urllib: Record lock failed", "url", u, "err", err)
		return
	}
	var existing string
	_ = tx.QueryRow(ctx, `SELECT queries FROM bob_urls WHERE url = $1`, u).Scan(&existing) // miss → empty
	merged := mergeQueries(existing, q)

	t := now()
	if _, err := tx.Exec(ctx,
		`INSERT INTO bob_urls (url, title, excerpt, queries, satisfied_count, first_seen, last_seen, is_search_engine, host)
		 VALUES ($1, $2, $3, $4, 0, $5, $6, FALSE, $7)
		 ON CONFLICT (url) DO UPDATE SET
		   title     = CASE WHEN EXCLUDED.title   != '' THEN EXCLUDED.title   ELSE bob_urls.title   END,
		   excerpt   = CASE WHEN EXCLUDED.excerpt != '' THEN EXCLUDED.excerpt ELSE bob_urls.excerpt END,
		   queries   = EXCLUDED.queries,
		   last_seen = EXCLUDED.last_seen,
		   host      = EXCLUDED.host`,
		u, title, excerpt, merged, t, t, hostOfURL(u),
	); err != nil {
		slog.Warn("urllib: Record failed", "url", u, "err", err)
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Warn("urllib: Record commit failed", "url", u, "err", err)
		return
	}

	if strings.TrimSpace(sid) != "" {
		l.mu.Lock()
		l.lastBySid[sid] = u
		l.mu.Unlock()
	}
}

// Forget drops the sid's tracked last-url without bumping satisfied_count — the
// flow's consume hook for turns that end without a clean final (error / partial),
// so lastBySid stays bounded to in-flight sids instead of leaking those entries.
func (l *library) Forget(sid string) {
	l.mu.Lock()
	delete(l.lastBySid, sid)
	l.mu.Unlock()
}

// MarkSatisfied — signal ②. Bumps satisfied_count for the LAST url this sid
// recorded this process. No-op if the sid recorded nothing this run.
func (l *library) MarkSatisfied(ctx context.Context, sid string) {
	l.mu.Lock()
	u := l.lastBySid[sid]
	delete(l.lastBySid, sid) // consume — bounds the map to in-flight sids, not all-time
	l.mu.Unlock()
	if u == "" {
		return
	}
	if _, err := l.db.Exec(ctx,
		`UPDATE bob_urls SET satisfied_count = satisfied_count + 1 WHERE url = $1`, u,
	); err != nil {
		slog.Warn("urllib: MarkSatisfied failed", "url", u, "err", err)
	}
}

// Recall returns prior useful (non-engine) urls whose query-list / title / excerpt
// LIKE-match the query, ranked by satisfied_count then last_seen — usefulness first
// (a page that actually ended a satisfied turn), recency as the tiebreak (fresh results
// beat stale ones). NO popularity term: a URL must prove useful to rise, not just be
// fetched. LIKE metachars in the query are escaped so they match literally.
func (l *library) Recall(ctx context.Context, query string, max int) []contract.URLCandidate {
	q := strings.TrimSpace(query)
	if q == "" || max <= 0 {
		return nil
	}
	if max > 50 {
		max = 50
	}
	pattern := "%" + escapeLike(strings.ToLower(q)) + "%"
	rows, err := l.db.Query(ctx, `
		SELECT url, title, excerpt
		FROM bob_urls
		WHERE is_search_engine = FALSE
		  AND (LOWER(queries) LIKE $1 ESCAPE '\'
		       OR LOWER(title) LIKE $1 ESCAPE '\'
		       OR LOWER(excerpt) LIKE $1 ESCAPE '\')
		ORDER BY satisfied_count DESC, last_seen DESC
		LIMIT $2
	`, pattern, max)
	if err != nil {
		slog.Warn("urllib: Recall failed", "err", err)
		return nil
	}
	defer rows.Close()
	out := make([]contract.URLCandidate, 0, max)
	for rows.Next() {
		var c contract.URLCandidate
		if err := rows.Scan(&c.URL, &c.Title, &c.Excerpt); err != nil {
			continue
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("urllib: Recall row iteration failed", "err", err)
		return nil
	}
	return out
}

// SearchEngines returns the configured engines (is_search_engine=1) with their
// fetch_mode + lang_scope. A non-empty lang keeps only engines whose lang_scope is
// '*' (global) or equals lang; lang "" applies no language filter. Ordered
// cheapest-rung-first (plain<render<stealth, unknown ” treated as plain) then
// title — so the caller's max_engines cap keeps the cheap/global engines and drops
// the expensive ones (e.g. stealth google) first.
func (l *library) SearchEngines(ctx context.Context, lang string) []contract.URLCandidate {
	lang = strings.TrimSpace(lang)
	rows, err := l.db.Query(ctx, `
		SELECT url, title, excerpt, fetch_mode, lang_scope FROM bob_urls
		WHERE is_search_engine = TRUE AND ($1 = '' OR lang_scope IN ('*', $1))
		ORDER BY CASE fetch_mode WHEN 'render' THEN 1 WHEN 'stealth' THEN 2 ELSE 0 END, title`, lang)
	if err != nil {
		slog.Warn("urllib: SearchEngines failed", "err", err)
		return nil
	}
	defer rows.Close()
	out := make([]contract.URLCandidate, 0)
	for rows.Next() {
		var c contract.URLCandidate
		if err := rows.Scan(&c.URL, &c.Title, &c.Excerpt, &c.FetchMode, &c.LangScope); err == nil {
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil {
		slog.Warn("urllib: SearchEngines row iteration failed", "err", err)
		return nil
	}
	return out
}

// Prune drops never-useful stale rows: not a search engine, never satisfied
// (satisfied_count = 0), and not seen since the cutoff (last_seen < cutoffUnix).
// This is VALUE-AWARE on purpose — a URL that ever proved useful (satisfied_count > 0)
// or was seen recently is kept regardless of age, so recall keeps its long tail and
// only one-off noise ages out. bob_urls only grows via Record's UPSERT, so it needs
// this retention sweep. Non-engine SEED rows (Wikipedia) match this predicate once
// stale — they stay ahead of it because seed() refreshes last_seen on every Start.
// Idempotent; returns rows removed.
func (l *library) Prune(ctx context.Context, cutoffUnix float64) (int64, error) {
	res, err := l.db.Exec(ctx,
		`DELETE FROM bob_urls
		 WHERE is_search_engine = FALSE AND satisfied_count = 0 AND last_seen < $1`,
		cutoffUnix,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// mergeQueries splits the existing newline-joined list, appends q (skipping blanks
// and exact dups), and caps to the last `queriesCap` items: newest distinct queries
// win; a re-issued query keeps its existing position. q "" → the existing list is
// returned unchanged.
func mergeQueries(existing, q string) string {
	var list []string
	seen := map[string]bool{}
	for _, e := range strings.Split(existing, "\n") {
		e = strings.TrimSpace(e)
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		list = append(list, e)
	}
	if q != "" && !seen[q] {
		list = append(list, q)
	}
	if len(list) > queriesCap {
		list = list[len(list)-queriesCap:]
	}
	return strings.Join(list, "\n")
}

// escapeLike escapes LIKE metacharacters (\ % _) so a query containing them
// matches literally. Pairs with `ESCAPE '\'` on the LIKE clause. Ported from
// skeleton's escapeLike.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// truncRunes truncates s to at most n bytes without splitting a rune.
func truncRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (not a
// continuation byte 0b10xxxxxx).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// FetchHint returns the last-worked fetch mode for host ("" if none / on error): the
// most-recent CONTENT row of that host carrying a learned fetch_mode. Best-effort — a
// miss just means the ladder starts from the bottom. Rides bob_urls's Prune (the host
// hint ages out with its rows; useful/satisfied hosts are kept).
func (l *library) FetchHint(ctx context.Context, host string) string {
	h := strings.TrimSpace(host)
	if h == "" {
		return ""
	}
	var mode string
	if err := l.db.QueryRow(ctx,
		`SELECT fetch_mode FROM bob_urls
		 WHERE host = $1 AND is_search_engine = FALSE AND fetch_mode <> ''
		 ORDER BY last_seen DESC LIMIT 1`, h).Scan(&mode); err != nil {
		return "" // miss or error → no hint
	}
	return mode
}

// LearnFetchMode records the mode that just succeeded for url (a content row Record has
// already created), so a future fetch of ANY url on the same host can start at that rung
// (FetchHint reads it by host). Best-effort; engine rows are never touched.
func (l *library) LearnFetchMode(ctx context.Context, url, mode string) {
	u := strings.TrimSpace(url)
	m := strings.TrimSpace(mode)
	if u == "" || m == "" {
		return
	}
	if _, err := l.db.Exec(ctx,
		`UPDATE bob_urls SET fetch_mode = $2 WHERE url = $1 AND is_search_engine = FALSE`,
		u, m); err != nil {
		slog.Warn("urllib: LearnFetchMode failed", "url", u, "mode", m, "err", err)
	}
}

var _ contract.URLLibrary = (*library)(nil)
