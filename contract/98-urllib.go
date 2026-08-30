package contract

import "context"

// URLCandidate is one recalled or seeded URL — the model-facing shape web_search
// renders (a [title](url) line plus an optional excerpt). The library stores and
// returns these for Recall and SearchEngines.
//
// FetchMode / LangScope are meaningful only for search-engine rows: FetchMode is
// the connection rung that opens this engine correctly ("plain"/"render"/"stealth";
// "" = unknown, treat as plain), LangScope is the query-language this engine serves
// ("*" = global/any, or a language tag like "zh"). They are empty/"*" for recalled
// non-engine URLs.
type URLCandidate struct {
	URL       string
	Title     string
	Excerpt   string
	FetchMode string
	LangScope string
}

// URLLibrary is the URL-memory seam: a small, pg-backed record of URLs the agent
// has SUCCESSFULLY fetched, plus the configured search engines. It exists to feed
// "this was the URL that ended a satisfied turn" (satisfied count) back into web_search
// so future searches can surface proven-useful URLs above raw SERP noise. (There is no
// popularity/visit count — that was a rich-get-richer trap; only proven usefulness ranks.)
//
// Provided by leaf/urllib; consumed OPTIONALLY (TryRequire) by the web tools (they
// Record successful fetches and Recall on search) and by the normal flow (it
// MarkSatisfied-s when a turn ends with a final answer). Every method is a no-op /
// empty when the library is absent, so all callers stay nil-safe.
type URLLibrary interface {
	// Record upserts a successfully-fetched URL (signal ① — callers ONLY call this on a
	// real successful fetch): appends query to the url's query list (deduped, capped),
	// updates last_seen. sid lets the lib track this turn's last-recorded URL for
	// MarkSatisfied. query may be "" (then only title/excerpt recall applies).
	Record(ctx context.Context, sid, url, title, excerpt, query string)
	// MarkSatisfied bumps satisfied_count for the LAST url this sid recorded this
	// turn (signal ② — the flow calls it when a turn ends with a final answer: the
	// last URL fetched satisfied the need). No-op if the sid recorded nothing.
	MarkSatisfied(ctx context.Context, sid string)
	// Forget drops the sid's tracked last-url WITHOUT bumping satisfied_count — the
	// flow calls it when a turn ends without a clean final answer (error / partial),
	// so the per-sid tracking map can't leak entries for turns MarkSatisfied skips.
	Forget(sid string)
	// Recall returns prior useful URLs whose query-list / title / excerpt LIKE-match
	// query (is_search_engine=0), ranked satisfied_count DESC, last_seen DESC. max caps
	// the result.
	Recall(ctx context.Context, query string, max int) []URLCandidate
	// FetchHint returns the connection rung that last worked for this HOST (a scrapling
	// mode like "get"/"fetch"; "" = nothing learned), so the fetch ladder can start at
	// the right rung instead of re-climbing from the bottom (P4.4, docs/web-fleet.md
	// §13.4). Per-host (caller passes a normalized host); content-only.
	FetchHint(ctx context.Context, host string) string
	// LearnFetchMode records that `mode` successfully fetched `url` (a content row Record
	// has already created), so the next visit to ANY url on that host starts at that rung
	// (FetchHint reads it by host). Best-effort.
	LearnFetchMode(ctx context.Context, url, mode string)
	// SearchEngines returns the configured engines (is_search_engine=1) for web_search,
	// each carrying its FetchMode + LangScope. lang filters by language scope: a
	// non-empty lang returns only engines whose LangScope is "*" (global) or equals
	// lang; lang "" returns all engines (no language filter). Ordered cheapest-rung
	// first (plain < render < stealth) then title, so the max_engines cap keeps the
	// cheap/global engines and drops the expensive ones first.
	SearchEngines(ctx context.Context, lang string) []URLCandidate
}
