package urllib

// Seed is one bootstrap URL row inserted on Start if absent. For engine rows it
// also carries FetchMode ("plain"/"render"/"stealth" — how to open this engine
// correctly) and LangScope ("*" global, or a language tag like "zh"). Re-seeding
// never clobbers accumulated counts; for an already-present row it refreshes
// last_seen (keeps non-engine seed rows ahead of the retention prune) and fills
// FetchMode/LangScope only when they are still at their unset default (so operator
// tuning survives, see store.seed).
type Seed struct {
	URL            string
	Title          string
	IsSearchEngine bool
	FetchMode      string // "" → stored as-is ('' = unknown/plain); engines set it explicitly
	LangScope      string // "" → normalized to "*" at seed time
}

// DefaultSeeds — the bootstrap set. Recommended engine集 (实测, see
// docs/web-fleet.md §4.1): Brave/Yahoo/Bing open with plain GET; Baidu needs a JS
// render; Google needs stealth. Baidu is zh-scoped; the rest are global. Wikipedia
// is a reference site (IsSearchEngine=false → recallable but not fired as engine).
// Perplexity (stealth-only) is deferred to the shared-fetcher phase (P2/P3) — it is
// useless until render/stealth fetching is wired, so seeding it now would只 add a
// dead engine. Yandex/Excite are intentionally excluded (CAPTCHA-walled / 404).
//
// re-measurement from the deploy host (schemaV5 retunes existing rows):
//   - DuckDuckGo is OUT. `duckduckgo.com/html/?q=` now 302s to html.duckduckgo.com,
//     which answers 202 with a ~14KB JS shell and zero result links — the no-JS
//     endpoint we relied on is gone, at every rung. lite.duckduckgo.com is the same
//     shell. Brave replaces it: plain GET, ~50 external result links, and its hrefs
//     are the real targets (no redirector to unwrap).
//   - Bing moves render → plain. Its plain SERP carries the full result set as
//     /ck/a?…&u=a1<base64> redirect links, which unwrapRedirector already decodes;
//     spending a browser launch on it was pure self-harm.
//     Baidu genuinely needs the render rung (plain returns nav chrome only).
//
// Brave is the strongest of these when it answers and the least reliable overall: from
// a datacenter IP it 429s about half the time even at one search per two minutes
// (measured on the deploy host). Treat it as a bonus engine — Yahoo and Bing are what
// the wave actually rests on. Its 429 is handled in two places: the rung ladder stops
// there instead of re-asking through a browser (fetchEngineSERP), and the breaker still
// counts the search as dead, so three in a row cool it down normally.
var DefaultSeeds = []Seed{
	{URL: "https://search.brave.com/search?q=", Title: "Brave Search", IsSearchEngine: true, FetchMode: "plain", LangScope: "*"},
	{URL: "https://search.yahoo.com/search?p=", Title: "Yahoo", IsSearchEngine: true, FetchMode: "plain", LangScope: "*"},
	{URL: "https://www.bing.com/search?q=", Title: "Bing", IsSearchEngine: true, FetchMode: "plain", LangScope: "*"},
	{URL: "https://www.baidu.com/s?wd=", Title: "Baidu 搜索", IsSearchEngine: true, FetchMode: "render", LangScope: "zh"},
	{URL: "https://www.google.com/search?q=", Title: "Google", IsSearchEngine: true, FetchMode: "stealth", LangScope: "*"},
	{URL: "https://en.wikipedia.org/wiki/", Title: "Wikipedia (EN)", IsSearchEngine: false},
}
