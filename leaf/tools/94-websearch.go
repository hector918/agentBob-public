// web_search_scrapling — the merged web tool's `query` (search) side. (The `url` fetch
// side is the scrapling machinery in 90-scrapling.go; the tool surface is webTool below.)
//
// A `query` runs as two concurrent WAVES under ONE shared gate set (the harvest primitive,
// docs/web-fleet.md §12): the ENGINE wave fans out the search engines (each opened by its
// fetch_mode — plain via the in-process serpGet, render/stealth via the scrapling
// subprocess) and serpCleans each SERP into [title](url) candidate links; then the PAGE
// wave auto-reads the top candidates and query-focused-compresses them in content-cap
// rounds. Both waves share ONE time window; the model decides sufficiency afterward (turn
// loop) — this tool never judges "enough".
//
// URL LIBRARY (leaf/urllib, OPTIONAL): the engine list comes from it (lib.SearchEngines,
// operator-extensible; absent → the inlined defaultSearchEngines), prior useful URLs are
// recalled into the candidate blob ("Recently Visited") and seeded as page candidates, and
// the page wave Records successful fetches + learns the per-host fetch rung (FetchHint).
package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"agentbob/contract"
)

const webParams = `{
  "type": "object",
  "properties": {
    "query": { "type": "string", "description": "搜索词（走搜索）。并发多引擎拿候选 URL 列表。query 与 url 二选一。" },
    "max_engines": { "type": "integer", "description": "[query] 并发引擎数上限，默认 4。" },
    "url": { "type": "string", "description": "要抓取的 http(s) URL（走抓取）。取这个页面正文。query 与 url 二选一。" },
    "mode": { "type": "string", "enum": ["get","fetch","stealthy-fetch","post"], "description": "[url] 抓取模式（默认 get）：get=HTTP+TLS指纹(~1-3s)；fetch=playwright JS渲染；stealthy-fetch=反爬绕过；post=写请求。失败按 hint 升级。" },
    "format": { "type": "string", "enum": ["markdown","text","html"], "description": "[url] 输出格式，默认 markdown。" },
    "css_selector": { "type": "string", "description": "[url] 可选 CSS selector 缩小提取范围。" },
    "impersonate": { "type": "string", "description": "[url+get] TLS 指纹：chrome(默认)/firefox/safari。" },
    "timeout_ms": { "type": "integer", "description": "[url+fetch/stealthy-fetch] 毫秒超时，默认 30000。" },
    "solve_cloudflare": { "type": "boolean", "description": "[url+stealthy-fetch] 自动解 Cloudflare Turnstile，默认 true。" },
    "body_json": { "type": "object", "description": "[url+post] JSON 请求体。" },
    "method": { "type": "string", "enum": ["POST","PUT","DELETE"], "description": "[url+post] HTTP 方法，默认 POST。" }
  }
}`

// usableSERPLinkThreshold: a search whose default batch (plain+render) yielded fewer
// external result links than this is "thin" → escalate to the stealth engines
// (docs/web-fleet.md §3.3 if-thin). 0 = every batch engine empty/blocked.
const usableSERPLinkThreshold = 3

// defaultPerEngineTimeout caps each individual SERP fetch. Total wall time is
// bounded by this since all engines fire concurrently. Skeleton drew this from
// WebConfig.FetchTimeoutSecondsEff(); trunk has no config.WebConfig (the
// scrapling port inlines its own default for the same reason), so this constant
// is the single bound.
const defaultPerEngineTimeout = 5 * time.Second

// perEngineMaxChars caps how much cleaned text each engine contributes to the
// final blob, to keep the model context bounded.
const perEngineMaxChars = 16 * 1024

// serpHTMLMaxChars caps a render/stealth engine's RAW SERP HTML pulled through
// scrapeFetch — large (matches serpGet's 1MB body cap) so serpClean sees the whole
// page; the cleaned link list is then capped to perEngineMaxChars in harvestEngines.
const serpHTMLMaxChars = 1 << 20

// searchEngine is one SERP template: URL is a query-prefix the query string is
// QueryEscape'd onto. FetchMode is the connection rung that opens this engine
// correctly ("plain"/"render"/"stealth"; "" = plain) — carried for the shared
// fetcher phase (docs/web-fleet.md §3); the current plain serpGet path ignores it.
type searchEngine struct {
	URL       string
	Title     string
	FetchMode string
}

// defaultSearchEngines is the INLINED fallback used only when the URL library is
// absent (urllib disabled). The live engine list comes from urllib's bob_urls
// (SearchEngines), seeded from urllib.DefaultSeeds — keep this fallback mirrored to
// that recommended set (docs/web-fleet.md §4.1). FetchMode is advisory until the
// shared fetcher lands; plain serpGet is used regardless for now.
var defaultSearchEngines = []searchEngine{
	{URL: "https://search.brave.com/search?q=", Title: "Brave Search", FetchMode: "plain"},
	{URL: "https://search.yahoo.com/search?p=", Title: "Yahoo", FetchMode: "plain"},
	{URL: "https://www.bing.com/search?q=", Title: "Bing", FetchMode: "plain"},
	{URL: "https://www.baidu.com/s?wd=", Title: "Baidu 搜索", FetchMode: "render"},
	{URL: "https://www.google.com/search?q=", Title: "Google", FetchMode: "stealth"},
}

// webDescription — the merged tool's model-facing doc: search + fetch in one tool.
const webDescription = `网页工具：搜索 + 抓取，二合一。
- query=<词> → 搜索：并发多引擎（Brave/Yahoo/Bing/Baidu…operator 可加，各引擎按其正确方式打开）+ 你之前用过的匹配 URL，返回候选 [标题](url) 列表。再用 url= 打开其中 2-3 条核实（光看候选不算读过）。
- url=<http(s)> → 抓取该页正文。mode 默认 get（HTTP+TLS 指纹，~1-3s，普通站 90% 一步到位）；失败按结果 hint 升级 fetch（JS 渲染）/ stealthy-fetch（反爬）；post 给 API。
query 与 url 二选一。`

// autoFetchTopN is how many top candidate URLs a `query` auto-reads in the same call
// (collapses the search→read round-trips, docs/web-fleet.md §11). autoFetchFallbackChars
// bounds the mechanical head-cap used when the query-focused extractor is unavailable.
// The query collection pipeline's knobs (docs/web-fleet.md §12). ONE gate set (harvest):
//   - harvestWindow: a PER-WAVE fetch window — the engine wave and the page wave EACH get
//     a fresh one from when that wave starts (a slow engine can't starve the page wave's
//     budget; a wave still running at its window is abandoned, per-fetch deadlines bound
//     each access inside). For our bounded two-wave fan-out this is the simple form of the
//     "every fired request extends the deadline" idea — each burst gets its own clock.
//   - autoFetchInputCap: a page-content round flush — accumulated page bytes ≥ this →
//     compress a round (non-terminal; the cap chunks huge harvests so one extract call
//     stays bounded; normal top-N total < cap → one final round = one extract call).
//   - autoFetchExtractTimeout: per compress-round call.
//   - autoFetchDigestCap: per-round digest output cap (what enters the model context).
const (
	autoFetchTopN          = 3
	autoFetchFallbackChars = 1500
	harvestWindow          = 12 * time.Second
	// autoFetchInputCap is the FALLBACK per-round input cap when the extract
	// model's window is unknown (no sizing seam / no small entry). The live cap
	// is sized from the model's declared window + a prefill-time budget
	// (extractInputCap) — a blind 64KB fed CJK pages ~32K tokens → ~55s prefill
	// on the small local model → every extract blew the timeout.
	autoFetchInputCap = 32 * 1024
	// extractPrefillTokBudget bounds a round's input so it prefills well inside
	// autoFetchExtractTimeout on a slow small model (~500 tok/s measured floor →
	// ~20s for 10K tokens, leaving margin for output + contention).
	extractPrefillTokBudget = 10000
	autoFetchExtractTimeout = 45 * time.Second
	autoFetchDigestCap      = 8 * 1024
)

// pageExtractFunc condenses a fetched page down to the parts relevant to the query —
// a small-model call (KindLLM, Prefer small), the query-aware sibling of the turn's
// summarize. Injected from the module; nil → the tool head-caps instead. This is the
// branch that runs in place of the generic summarize for auto-read pages (D9).
type pageExtractFunc func(ctx context.Context, query, content string) (string, error)

// webTool is the merged web_search_scrapling tool: one tool, two modes. `query` runs
// the multi-engine search (each engine opened per its fetch_mode — plain via the
// in-process serpGet, render/stealth via the scrapling subprocess fetching raw HTML),
// then auto-reads the top candidates and query-focused-extracts them; `url` fetches one
// page through the scrapling ladder. It holds the scrapling sandbox root (for
// scrapeFetch), the OPTIONAL URL library, and the OPTIONAL query-focused extractor.
type webTool struct {
	sandboxRoot string
	lib         func() contract.URLLibrary
	extract     pageExtractFunc
	extractCap  func() int     // per-round input cap in bytes, sized to the extract model; nil → autoFetchInputCap
	breaker     *engineBreaker // P4.3 per-process dead-engine circuit breaker
}

// newWebTool builds the merged tool. ALWAYS registered (unlike the old CLI-gated
// scrapling): plain-engine search works without the scrapling CLI; render/stealth
// engines and the url-fetch modes return a clear "CLI not found" error (from
// scrapeFetch) when the CLI is absent, rather than hiding the capability.
func newWebTool(sandboxRoot string, lib func() contract.URLLibrary, extract pageExtractFunc, extractCap func() int) webTool {
	return webTool{sandboxRoot: sandboxRoot, lib: lib, extract: extract, extractCap: extractCap, breaker: newEngineBreaker()}
}

// pageExtractFn builds the query-focused page extractor from the model pool: a
// small-model call (KindLLM, Prefer small — same tier as the turn's summarize). Absent
// pool → returns an error so the tool falls back to a mechanical head-cap.
func pageExtractFn(pool func() contract.ModelPool) pageExtractFunc {
	return func(ctx context.Context, query, content string) (string, error) {
		p := pool()
		if p == nil {
			return "", fmt.Errorf("model pool not available")
		}
		sys := "你是网页正文提炼器。只输出与下面【问题】相关的关键段落、事实和数据，保留原文关键句与数字，去掉导航/广告/页脚/无关内容；不要编造、不要压成一句话。" +
			"若整页没有与问题相关的内容（抓错页 / 反爬壳 / 主题跑偏），只输出 NONE 这四个大写字母、不要其它字。问题：" + query
		resp, err := p.Chat(ctx, extractReq(), []contract.Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: content},
		})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}

// extractReq is the request the query-focused extractor picks with — its own
// `extract` tag, routed through the SAME picker + declared-window sizing as the
// turn's compaction (hector, 统一). A dedicated tag (not reusing
// `compress`) keeps MODEL SELECTION unified without welding "extract-capable ==
// compress-capable": the two stay distinct OPERATIONS (extract = keep relevant
// verbatim slices; compress = paraphrase) via their different prompts, and an
// operator can dedicate a model to either by retagging — no code change. The
// models.yaml `extract → compress` fallback rule means an untagged deployment
// degrades to the compress entries (chaining compress → smart), so this works
// before any entry carries the tag.
func extractReq() contract.ModelRequest {
	return contract.ModelRequest{Kind: contract.KindLLM, Requires: []string{"extract"}}
}

// extractCapBytes converts the extract model's declared window (tokens; 0 =
// unknown) into a per-round input byte cap. Bounded by BOTH the window (leave
// the top quarter for system + output) and a conservative prefill-time budget
// so a round prefills inside autoFetchExtractTimeout. Bytes assume dense CJK
// (~3 bytes/token, the fewest of any script) so the token count — hence the
// prefill time — never exceeds the budget regardless of the page's script.
func extractCapBytes(winTok int) int {
	capTok := extractPrefillTokBudget
	if winTok > 0 && winTok*3/4 < capTok {
		capTok = winTok * 3 / 4
	}
	if b := capTok * 3; b >= 4096 {
		return b
	}
	return 4096
}

// pageExtractCapFn sizes the extractor's per-round input to the extract model's
// DECLARED window (windowSizer seam, asserted — same 声明即真相 discipline as the
// turn's compaction). No seam / nil pool → the autoFetchInputCap fallback.
func pageExtractCapFn(pool func() contract.ModelPool) func() int {
	return func() int {
		ws, ok := pool().(interface {
			WindowFor(contract.ModelRequest) int
		})
		if !ok {
			return autoFetchInputCap
		}
		return extractCapBytes(ws.WindowFor(extractReq()))
	}
}

func (webTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name:        "web_search_scrapling",
		Description: webDescription,
		Parameters:  json.RawMessage(webParams),
		// query mode returns [标题](url) candidate links — the URLs are the whole
		// value; a generic summarize would erase them. url mode returns page content
		// already capped at defaultMaxChars (16KB). So opt out of the pipeline's
		// summarize entirely; the tool bounds its own output (docs/web-fleet.md §12).
		NoAutoCompress: true,
		SelectionHint: &contract.SelectionHint{
			When:     `要网上的信息：不知道 URL 就 query=<词> 搜；已有 URL 就 url=<地址> 取正文`,
			Then:     `先 query 拿候选，再 url 打开 2-3 条核实；url 抓取失败按结果 hint 升级 mode`,
			Priority: 10,
		},
	}
}

func (webTool) Serialize() bool { return false }

func (s webTool) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var a struct {
		Query      string `json:"query"`
		MaxEngines int    `json:"max_engines"`
		scrapeArgs
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return contract.ErrResult("web_search_scrapling: invalid arguments: " + err.Error())
	}
	hasURL := strings.TrimSpace(a.URL) != ""
	hasQuery := strings.TrimSpace(a.Query) != ""
	switch {
	case hasURL && hasQuery:
		return contract.ErrResult("web_search_scrapling: give either query (search) or url (fetch), not both")
	case hasURL:
		return s.runURL(ctx, tc, &a.scrapeArgs)
	case hasQuery:
		return s.runSearch(ctx, tc, a.Query, a.MaxEngines)
	default:
		return contract.ErrResult("web_search_scrapling: need query (to search) or url (to fetch)")
	}
}

// runURL fetches one page through the scrapling ladder, Records the success, and
// builds the page envelope. (The former scrapling.Run.)
func (s webTool) runURL(ctx context.Context, tc contract.ToolContext, p *scrapeArgs) contract.ToolResult {
	out, errMsg, hint := s.scrapeFetch(ctx, tc.Sid, p, 0) // 0 → default 16KB page cap
	if errMsg != "" {
		r := contract.ErrResult(errMsg)
		if hint != "" {
			r = r.WithHint(hint)
		}
		return r
	}
	target := strings.TrimSpace(p.URL)
	mode := scrapeModeOrDefault(p.Mode)
	// Signal ① — record the deliberate visit so future searches can recall it, and learn
	// the rung that worked for this host (P4.4 §13.4) so the next fetch starts there. This
	// is the library's WRITE path; auto-read is read-only (it must not feed the recall loop).
	if s.lib != nil {
		if lib := s.lib(); lib != nil {
			lib.Record(ctx, tc.Sid, target, "", scrapeExcerpt(out.Content), tc.UserText)
			lib.LearnFetchMode(ctx, target, mode)
		}
	}
	format := scrapeFormatOrDefault(p.Format)
	resp := map[string]any{
		"url":        target,
		"mode":       mode,
		"format":     format,
		"content":    out.Content,
		"byte_count": out.ByteCount,
		"truncated":  out.Truncated,
		"fetched_at": time.Now().UTC().Format(time.RFC3339),
	}
	if mode == "get" {
		impersonate := p.Impersonate
		if impersonate == "" {
			impersonate = "chrome"
		}
		resp["impersonate"] = impersonate
	}
	if p.CSSSelector != "" {
		resp["css_selector"] = p.CSSSelector
	}
	b, mErr := json.Marshal(resp)
	if mErr != nil {
		return contract.ErrResult("web_search_scrapling: marshal: " + mErr.Error())
	}
	return contract.OKResult(string(b))
}

// runSearch fans out the engines (each opened per its fetch_mode), folds in URL-library
// recall, and returns the candidate-URL blob. If the default batch (plain+render) comes
// back thin (< usableSERPLinkThreshold external links) it escalates to the stealth
// engines (docs/web-fleet.md §3.3). web_search no longer Records — SERP links are
// unvisited candidates; a real visit is recorded by the url mode / browser.
func (s webTool) runSearch(ctx context.Context, tc contract.ToolContext, query string, maxEng int) contract.ToolResult {
	q := strings.TrimSpace(query)
	if q == "" {
		return contract.ErrResult("web_search_scrapling: query is required")
	}
	if maxEng <= 0 {
		maxEng = 4
	}
	var lib contract.URLLibrary
	if s.lib != nil {
		lib = s.lib()
	}
	engines := searchEngineList(ctx, lib)
	if len(engines) == 0 {
		return contract.ErrResult("web_search_scrapling: no search engines configured — the operator needs to set one up")
	}

	// Split FIRST, then cap the non-stealth batch. Capping the cheapest-first engine
	// list before the split would drop the last-ordered stealth engines and make the
	// if-thin escalation below dead code (docs/web-fleet.md §3.3). The default batch
	// (plain + render) fires concurrently; the expensive stealth engines are held back
	// as an if-thin fallback (cost + Google anti-bot isolation), max_engines-exempt.
	batch, stealth := splitStealthEngines(engines)
	if len(batch) > maxEng {
		batch = batch[:maxEng]
	}
	// Engine wave (+ if-thin stealth wave), then the page wave — each goes through the
	// shared harvest gate set with its OWN fresh window (a slow engine can't starve the
	// page wave's budget — that was the one-shared-window bug).
	cleaned, status := s.harvestEngines(ctx, tc.Sid, q, batch, true)
	if externalLinkCount(cleaned, engines) < usableSERPLinkThreshold && len(stealth) > 0 {
		sc, ss := s.harvestEngines(ctx, tc.Sid, q, stealth, false)
		for k, v := range sc {
			cleaned[k] = v
		}
		for k, v := range ss {
			status[k] = v
		}
	}

	libHits := libraryRecall(ctx, lib, q, 5)
	content := assembleSerp(cleaned, libHits)
	if content == "" {
		return contract.ErrResult(allEnginesFailedMsg(len(engines), status))
	}
	out := map[string]any{
		"query":         q,
		"content":       content,
		"engine_status": status,
		"library_count": len(libHits),
	}
	// Page wave: auto-read the top candidates in the SAME window, compressed in
	// content-cap rounds (collapses search→read into one tool call). The model decides
	// sufficiency afterward (answer, or call again). Best-effort: a page that won't fetch
	// stays a candidate in `content` for a manual url= read (reversible).
	if read, digest := s.harvestPages(ctx, tc.Sid, q, cleaned, libHits, engines); len(read) > 0 {
		out["read"] = read
		if digest != "" {
			out["digest"] = digest
		}
	}
	b, mErr := json.Marshal(out)
	if mErr != nil {
		return contract.ErrResult("web_search_scrapling: marshal: " + mErr.Error())
	}
	return contract.OKResult(string(b))
}

// allEnginesFailedMsg renders the "nothing came back" error. When every engine reported
// the SAME error text, that is not a statement about the engines: independent hosts do
// not fail identically, so the fetch machinery in front of them is what is broken (a bad
// scrapling/playwright install, no egress). Say that instead of listing five hosts and
// letting the model conclude the web is down and retry the same query all turn.
func allEnginesFailedMsg(engineCount int, status map[string]string) string {
	var first string
	have := false
	same := len(status) > 1
	for _, v := range status {
		if !have { // sentinel on `have`, not on first=="" — a status value could be empty
			first, have = v, true
			continue
		}
		if v != first {
			same = false
			break
		}
	}
	if same && strings.HasPrefix(first, "error: ") {
		// len(status), not engineCount: only the engines that were actually FIRED can
		// corroborate each other, and the batch is capped by max_engines.
		return fmt.Sprintf(
			"web_search_scrapling: all %d engines failed with the SAME error, so the fetch machinery is broken, not the engines — retrying this search will not help. Tell the operator: %s",
			len(status), strings.TrimPrefix(first, "error: "))
	}
	return fmt.Sprintf(
		"web_search_scrapling: all %d engines returned empty/error and the library had no matches. per-engine: %v",
		engineCount, status)
}

// readURL is one auto-read page recorded in the result — a verbatim handle the model can
// re-read with url= or cite.
type readURL struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

type candidate struct{ url, title string }

// harvestItem is one producer result + its content size (for the content-cap gate).
type harvestItem[T any] struct {
	v    T
	size int
}

// harvest is the SHARED gate set for a query's concurrent waves: it fires produce(i) for
// i in [0,n) concurrently and delivers results to onBatch in content-capped rounds. ctx
// carries the overall window deadline (shared across the engine + page waves). Gates:
//   - all n in              → final onBatch, done.
//   - ctx deadline (window) → onBatch what's in hand, abandon stragglers (their fetch is
//     cancelled via the same ctx; the late send lands in the buffered channel, no block).
//   - accumulated size ≥ capBytes → onBatch a round, reset, KEEP collecting (non-terminal;
//     pass a huge cap to disable this gate for batches that don't compress).
//
// Producer panics are recovered (zero value, size 0). onBatch runs in ONE goroutine, so it
// may freely mutate caller state.
func harvest[T any](ctx context.Context, n, capBytes int, produce func(i int) (T, int), onBatch func([]T)) {
	if n <= 0 {
		return
	}
	ch := make(chan harvestItem[T], n)
	for i := 0; i < n; i++ {
		go func() {
			it := harvestItem[T]{}
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("web: harvest producer panicked", "i", i, "panic", r)
						it = harvestItem[T]{}
					}
				}()
				it.v, it.size = produce(i)
			}()
			ch <- it // exactly one send on every path
		}()
	}
	batch := make([]T, 0, n)
	sum := 0
	for got := 0; got < n; got++ {
		select {
		case it := <-ch:
			batch = append(batch, it.v)
			if it.size > 0 {
				if sum += it.size; sum >= capBytes {
					onBatch(batch)
					batch = make([]T, 0, n)
					sum = 0
				}
			}
		case <-ctx.Done():
			if len(batch) > 0 {
				onBatch(batch)
			}
			return
		}
	}
	if len(batch) > 0 {
		onBatch(batch)
	}
}

// harvestPages is the page wave: auto-read the top candidates concurrently under the window
// (wctx), compressing in content-cap rounds (autoFetchInputCap → one query-focused extract
// per round, reusing condense: NONE → drop, slow/failed → head-cap). Returns the pages read
// + the joined per-round digests. The extract runs under the PARENT ctx with its own
// autoFetchExtractTimeout (not the fetch window — a slow compress isn't cut by it).
func (s webTool) harvestPages(ctx context.Context, sid, query string, cleaned map[string]string, libHits []recallHit, engines []searchEngine) ([]readURL, string) {
	cands := topCandidates(cleaned, libHits, engines, autoFetchTopN)
	if len(cands) == 0 {
		return nil, ""
	}
	wctx, cancel := context.WithTimeout(ctx, harvestWindow) // page wave's OWN fresh window
	defer cancel()
	// Per-round input cap, sized to the extract model's window when the seam is
	// present; the fallback const otherwise. Nil-safe: a webTool built without a
	// cap sizer (tests) uses the fallback.
	inputCap := autoFetchInputCap
	if s.extractCap != nil {
		if c := s.extractCap(); c > 0 {
			inputCap = c
		}
	}
	type page struct {
		idx                 int
		url, title, content string
	}
	var read []readURL
	var digests []string
	harvest(wctx, len(cands), inputCap,
		func(i int) (page, int) {
			c := cands[i]
			content, em := s.fetchPageLadder(wctx, sid, c.url)
			if em != "" {
				return page{idx: i}, 0
			}
			return page{idx: i, url: c.url, title: c.title, content: content}, len(content)
		},
		func(batch []page) {
			// Results arrive in fetch-completion order — restore candidate priority (lib
			// hits first, then SERP order) so read[]/digest are deterministic.
			sort.Slice(batch, func(a, b int) bool { return batch[a].idx < batch[b].idx })
			var blob strings.Builder
			got := false
			for _, p := range batch {
				if p.content == "" {
					continue
				}
				got = true
				read = append(read, readURL{URL: p.url, Title: p.title})
				fmt.Fprintf(&blob, "## %s（%s）\n%s\n\n", p.title, p.url, p.content)
			}
			if !got {
				return
			}
			ectx, ec := context.WithTimeout(ctx, autoFetchExtractTimeout)
			digest, relevant := s.condense(ectx, query, blob.String())
			ec()
			if relevant && digest != "" {
				digests = append(digests, truncateRunes(digest, autoFetchDigestCap))
			}
		})
	return read, strings.Join(digests, "\n\n")
}

// pageModeOrder is the auto-read content ladder: cheap HTTP+TLS first, then a JS render.
// Stealth is NOT auto-tried (expensive; the TTL would abandon it anyway — the model can
// url= a hard page explicitly).
var pageModeOrder = []string{"get", "fetch"}

// pageLadderFrom puts the learned-hint rung FIRST (skip a known-failing cheaper rung)
// but keeps the other rungs as fallback — narrowing to just the hint would drop a cheaper
// rung that might still work for a DIFFERENT url on the host. Unknown/empty hint → the
// full ladder from the cheapest rung.
func pageLadderFrom(start string) []string {
	known := false
	for _, m := range pageModeOrder {
		if m == start {
			known = true
			break
		}
	}
	if !known {
		return pageModeOrder
	}
	out := make([]string, 0, len(pageModeOrder))
	out = append(out, start)
	for _, m := range pageModeOrder {
		if m != start {
			out = append(out, m)
		}
	}
	return out
}

// fetchPageLadder fetches one content URL for auto-read. It STARTS at the rung that last
// worked for this host (urllib.FetchHint, P4.4 §13.4) — skipping a known-failing cheap
// rung — then climbs get→fetch. Auto-read is READ-ONLY on the library: it reads FetchHint
// but does NOT Record (signal ①) or LearnFetchMode — a speculative top-N pre-fetch is not a
// "useful visit" (only a deliberate manual url= is, and that path is the writer). Recording
// auto-read fetches fed a recall→auto-read→Record loop that inflated junk like a portal's
// visit_count. Returns (markdown, errMsg).
func (s webTool) fetchPageLadder(ctx context.Context, sid, target string) (content, errMsg string) {
	host := hostOf(target)
	start := ""
	if s.lib != nil {
		if lib := s.lib(); lib != nil {
			start = lib.FetchHint(ctx, host)
		}
	}
	for _, m := range pageLadderFrom(start) {
		out, em, _ := s.scrapeFetch(ctx, sid, &scrapeArgs{URL: target, Mode: m, Format: "markdown"}, 0)
		if em == "" {
			return out.Content, ""
		}
		errMsg = em
	}
	return "", errMsg
}

// condense query-focused-extracts content to the parts relevant to query. The second
// return is the §13.5 relevance signal: false ONLY when the extractor RAN and judged the
// page off-topic (NONE sentinel) → the caller drops the page. A missing/failed/empty
// extractor falls back to a mechanical head-cap and is treated as relevant (we couldn't
// judge — keep it). docs/web-fleet.md §12/§13.5.
func (s webTool) condense(ctx context.Context, query, content string) (extract string, relevant bool) {
	if s.extract != nil {
		if out, err := s.extract(ctx, query, content); err == nil {
			t := strings.TrimSpace(out)
			if isNoneRelevant(t) {
				return "", false // extractor judged the page off-topic → drop
			}
			if t != "" {
				return t, true
			}
			// empty (no sentinel, no content) → fall through to head-cap
		} else {
			slog.Warn("web: query-focused extract failed — head-cap fallback", "err", err)
		}
	}
	return truncateRunes(strings.TrimSpace(content), autoFetchFallbackChars), true
}

// isNoneRelevant matches the extractor's "nothing relevant" sentinel (the §13.5 signal).
func isNoneRelevant(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	return t == "none" || t == "无相关" || t == "无相关内容"
}

// topCandidates returns up to n distinct candidate URLs to auto-read: known-good
// library hits first, then fresh SERP links in assembleSerp's deterministic engine
// order, skipping the engines' own hosts and duplicates.
func topCandidates(cleaned map[string]string, libHits []recallHit, engines []searchEngine, n int) []candidate {
	// Exclude the engine host AND any subdomain of it, so auto-read never wastes a slot
	// on the engine's own nav/login/redirect chrome (baidu → passport./news.baidu.com,
	// yahoo → r.search.yahoo.com, bing → www.bing.com/ck/a). Shared with externalLinkCount.
	isEngineHost := engineHostMatcher(engines)
	seen := map[string]bool{}
	out := make([]candidate, 0, n)
	add := func(url, title string) {
		if len(out) >= n || url == "" || seen[url] || isEngineHost(hostOf(url)) {
			return
		}
		seen[url] = true
		out = append(out, candidate{url: url, title: title})
	}
	for _, h := range libHits {
		add(h.URL, h.Title)
	}
	labels := make([]string, 0, len(cleaned))
	for k := range cleaned {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	for _, label := range labels {
		for _, c := range parseCandidateLinks(cleaned[label]) {
			add(c.url, c.title)
		}
	}
	return out
}

// mdLinkRe matches a serpClean output line: [title](http(s)://url).
var mdLinkRe = regexp.MustCompile(`\[([^\]]*)\]\((https?://[^)\s]+)\)`)

func parseCandidateLinks(blob string) []candidate {
	ms := mdLinkRe.FindAllStringSubmatch(blob, -1)
	out := make([]candidate, 0, len(ms))
	for _, m := range ms {
		out = append(out, candidate{url: m[2], title: strings.TrimSpace(m[1])})
	}
	return out
}

// splitStealthEngines partitions engines into the default batch (fetch_mode != "stealth")
// and the held-back stealth set (fetch_mode == "stealth").
func splitStealthEngines(engines []searchEngine) (batch, stealth []searchEngine) {
	for _, e := range engines {
		if e.FetchMode == "stealth" {
			stealth = append(stealth, e)
		} else {
			batch = append(batch, e)
		}
	}
	return batch, stealth
}

// engineHostMatcher returns a predicate matching a host against the engine set: the
// exact engine host OR any subdomain of it (baidu.com matches passport.baidu.com). An
// engine's SERP carries its own nav/login/redirect chrome on these hosts — they are
// NOT real results. Shared by externalLinkCount and topCandidates.
func engineHostMatcher(engines []searchEngine) func(host string) bool {
	hosts := make([]string, 0, len(engines))
	for _, e := range engines {
		hosts = append(hosts, hostOf(e.URL))
	}
	return func(h string) bool {
		for _, eh := range hosts {
			if h == eh || strings.HasSuffix(h, "."+eh) {
				return true
			}
		}
		return false
	}
}

// externalLinkCount counts DISTINCT external result links across the cleaned engine
// blobs — links to a real target site, excluding each engine's own host/subdomains
// (nav/login/unparsed-redirect chrome) and dupes. This is the "did the batch actually
// deliver results" signal (P4.1, docs/web-fleet.md §13.1): a chrome-only engine (baidu)
// or unparsed-redirect links (yahoo r.search.yahoo.com) count as 0, so the if-thin
// escalation fires correctly — unlike the old naive any-`](http` count which they fooled.
func externalLinkCount(cleaned map[string]string, engines []searchEngine) int {
	isEng := engineHostMatcher(engines)
	seen := map[string]bool{}
	for _, blob := range cleaned {
		for _, c := range parseCandidateLinks(blob) {
			if c.url == "" || seen[c.url] || isEng(hostOf(c.url)) {
				continue
			}
			seen[c.url] = true
		}
	}
	return len(seen)
}

// externalLinkCountBlob is the single-blob variant: distinct external result links in
// ONE engine's cleaned output. Drives per-engine self-upgrade (P4.2) and the
// dead-engine breaker (P4.3) — 0 means that engine returned only its own chrome.
func externalLinkCountBlob(blob string, isEng func(string) bool) int {
	seen := map[string]bool{}
	for _, c := range parseCandidateLinks(blob) {
		if c.url == "" || seen[c.url] || isEng(hostOf(c.url)) {
			continue
		}
		seen[c.url] = true
	}
	return len(seen)
}

// libraryRecall asks the URL library for prior useful URLs matching query. Absent
// library → no hits (assembleSerp omits the "Recently Visited" section).
func libraryRecall(ctx context.Context, lib contract.URLLibrary, query string, max int) []recallHit {
	if lib == nil {
		return nil
	}
	cands := lib.Recall(ctx, query, max)
	if len(cands) == 0 {
		return nil
	}
	out := make([]recallHit, 0, len(cands))
	for _, c := range cands {
		out = append(out, recallHit{URL: c.URL, Title: c.Title, Excerpt: c.Excerpt})
	}
	return out
}

// searchEngineList returns the engine templates: from the URL library when
// present (operator-extensible), else the inlined defaultSearchEngines fallback. A
// library that returns no engines also falls back, so an empty/unseeded library
// can't silently disable search.
func searchEngineList(ctx context.Context, lib contract.URLLibrary) []searchEngine {
	if lib != nil {
		// lang "" → no language filter yet: threading the inbound ev.Lang into
		// ToolContext is a separate step (docs/web-fleet.md D4). urllib already orders
		// cheapest-rung-first, so the max_engines cap keeps the cheap/global engines.
		if cands := lib.SearchEngines(ctx, ""); len(cands) > 0 {
			out := make([]searchEngine, 0, len(cands))
			for _, c := range cands {
				out = append(out, searchEngine{URL: c.URL, Title: c.Title, FetchMode: c.FetchMode})
			}
			return out
		}
	}
	return defaultSearchEngines
}

// recallHit mirrors contract.URLCandidate (the recall shape). Kept local so the
// assembly path stays decoupled from the contract type name.
type recallHit struct {
	URL     string
	Title   string
	Excerpt string
}

// engineJob is one engine to fire in a wave: the engine itself plus the display label
// (its host, deduped with an ordinal suffix) and the breaker key (its host).
type engineJob struct {
	label, host string
	eng         searchEngine
}

// planEngineWave decides which engines this wave actually fires, and pre-fills the
// status entries for the ones it holds back. `skip` is the breaker's cooldown predicate.
//
// rescue asks for the one exception to the cooldown: a cooldown may thin the DEFAULT
// wave but must never EMPTY it, because then each remaining search of the turn returns
// "all engines deprioritized" with nothing attempted — the model can only burn rounds
// re-asking, and no engine can ever prove it recovered. So an all-cooled default wave
// fires the full set instead (recording normally: a real recovery resets the counter, a
// still-dead engine re-trips). The stealth wave passes rescue=false: it is an optional
// if-thin escalation, not the search itself, so an all-cooled stealth wave that fires
// nothing costs only its own fallback — while rescuing it would make the breaker a no-op
// for a stealth engine that is alone in its wave (the seeded set has exactly one), and
// spend a stealth browser launch on a known-dead engine every single search.
func planEngineWave(engines []searchEngine, skip func(host string) bool, rescue bool) ([]engineJob, map[string]string) {
	status := map[string]string{}
	label := func(seen map[string]int, eng searchEngine) string {
		l := hostOf(eng.URL)
		seen[l]++
		if k := seen[l]; k > 1 {
			l = fmt.Sprintf("%s#%d", l, k)
		}
		return l
	}
	var jobs []engineJob
	seen := map[string]int{}
	for _, eng := range engines {
		l := label(seen, eng)
		host := hostOf(eng.URL)
		if skip(host) {
			status[l] = "deprioritized (cooldown — repeatedly returned no usable results)"
			continue
		}
		jobs = append(jobs, engineJob{l, host, eng})
	}
	if !rescue || len(jobs) > 0 || len(engines) == 0 {
		return jobs, status
	}
	slog.Warn("web: every engine is in cooldown — firing them anyway rather than answering nothing",
		"engines", len(engines))
	status = map[string]string{}
	seen = map[string]int{}
	for _, eng := range engines {
		jobs = append(jobs, engineJob{label(seen, eng), hostOf(eng.URL), eng})
	}
	return jobs, status
}

// harvestEngines is the engine wave: fetch each engine's SERP concurrently under the
// window (wctx; per-engine deadline inside fetchEngineSERP), record the breaker, build
// cleaned/status. Goes through the shared harvest gate set with the content-cap DISABLED
// (engines don't compress — only all-in / window gate them). Cooldown'd engines (P4.3) are
// skipped before firing (see planEngineWave for rescueIfAllCooled). Label = engine host
// (deduped with an ordinal suffix).
func (s webTool) harvestEngines(ctx context.Context, sid, query string, engines []searchEngine, rescueIfAllCooled bool) (map[string]string, map[string]string) {
	wctx, cancel := context.WithTimeout(ctx, harvestWindow)
	defer cancel()
	cleaned := map[string]string{}
	jobs, status := planEngineWave(engines, s.breaker.skip, rescueIfAllCooled)
	type eres struct {
		label, host, text, ferr string
		alive, toolSide         bool
	}
	harvest(wctx, len(jobs), math.MaxInt, // engines don't compress → no content-cap gate
		func(i int) (eres, int) {
			j := jobs[i]
			clean, hasExt, ferr, toolSide := s.fetchEngineSERP(wctx, sid, j.eng, query, defaultPerEngineTimeout)
			return eres{j.label, j.host, clean, ferr, ferr == "" && hasExt, toolSide}, len(clean)
		},
		func(batch []eres) {
			for _, r := range batch {
				// P4.3: alive resets; dead → cooldown. A failure our own side owns (the
				// browser never launched, the CLI is missing) says nothing about the engine,
				// so it neither trips nor resets the counter — otherwise a broken install
				// cools down every engine and outlives its own fix by a 10-minute window.
				if !r.toolSide {
					s.breaker.record(r.host, r.alive)
				}
				switch {
				case r.ferr != "":
					emsg := r.ferr
					if len(emsg) > 120 {
						emsg = truncateRunes(emsg, 120) + "…"
					}
					status[r.label] = "error: " + emsg
					slog.Debug("web search engine failed", "engine", r.label, "err", r.ferr)
				case strings.TrimSpace(r.text) == "":
					status[r.label] = "empty"
				default:
					cleaned[r.label] = r.text
					status[r.label] = fmt.Sprintf("ok (%d chars)", len(r.text))
					// P4.6 (§13.6): links but ZERO external — all self-referential (engine
					// chrome or an unparsed redirect format). Name it so a dev adds an unwrap
					// branch once, instead of failing silently.
					if !r.alive {
						if raw := len(parseCandidateLinks(r.text)); raw > 0 {
							slog.Warn("web: engine returned only self-referential links (0 external) — needs an unwrap branch or is anti-botted from this host",
								"engine", r.host, "links", raw)
						}
					}
				}
			}
		})
	return cleaned, status
}

// fetchEngineSERP fetches one engine's SERP and returns its CLEANED [title](url) blob,
// self-upgrading the connection rung (P4.2, §13.2): it starts at the engine's fetch_mode
// and, if the cleaned result has zero EXTERNAL links (pure nav/redirect chrome — e.g.
// baidu's homepage), escalates one rung (plain→render→stealth) and retries, capping at
// stealth. Returns ("", errMsg) only when every attempted rung errored — with toolSide
// set when EVERY one of those rungs failed on our own side (so the caller can keep the
// engine's breaker counter out of it).
func (s webTool) fetchEngineSERP(ctx context.Context, sid string, eng searchEngine, query string, perEngineTimeout time.Duration) (cleaned string, hasExternal bool, errMsg string, toolSide bool) {
	target := eng.URL + url.QueryEscape(query)
	isEng := engineHostMatcher([]searchEngine{eng})
	rungs := rungsFrom(eng.FetchMode)
	allToolSide := true
	for i, rung := range rungs {
		a := s.fetchSERPRaw(ctx, sid, target, rung, perEngineTimeout)
		if a.errMsg != "" {
			// Keep the FIRST rung's error, not the last. The engine's own configured
			// rung is where it says something about itself ("http 429 Too Many
			// Requests"); the rungs above it can only report how the escalation went,
			// and overwriting with those buries the one actionable line — a rate-limited
			// engine ends up reported as a stealth-fetch timeout.
			if errMsg == "" {
				errMsg = a.errMsg
			}
			allToolSide = allToolSide && a.toolSide
			// 429 ends the climb. The ladder exists for engines that are opened WRONG
			// (needs JS, needs stealth) — a rate limit says the opening was understood
			// and we are asking too fast, so the rungs above can only re-ask the same
			// question from the same IP, more expensively (a browser launch each) and
			// more conspicuously. Back off instead: the breaker still counts this as a
			// dead search, so three in a row cool the engine down.
			if a.rateLimited {
				slog.Info("web: engine rate-limited — not escalating the rung ladder",
					"engine", hostOf(eng.URL), "rung", rung, "err", a.errMsg)
				return "", false, errMsg, false
			}
			continue // this rung errored — try the next
		}
		clean := truncateRunes(serpClean(a.html), perEngineMaxChars)
		ext := externalLinkCountBlob(clean, isEng)
		// Stop at the first rung that yields a real (external) result, or at the top rung
		// regardless (no point escalating past stealth). Return the count so the caller
		// (breaker/alive) needn't re-parse the same blob.
		if ext > 0 || i == len(rungs)-1 {
			return clean, ext > 0, "", false
		}
		// 0 external = chrome only → escalate to the next rung. The engine ANSWERED, it
		// just answered with its own chrome — that is engine evidence, not a tool fault.
		allToolSide = false
	}
	return "", false, errMsg, allToolSide
}

// serpHTTPError is an engine's non-2xx answer. Typed (rather than the flat
// fmt.Errorf it used to be) so the rung ladder can read the status back: "you are
// asking too fast" and "you are opening me wrong" call for opposite reactions.
type serpHTTPError struct {
	code   int
	status string
}

func (e serpHTTPError) Error() string { return "http " + e.status }

// serpFailure classifies a failed plain-rung GET. A plain GET only ever fails against
// the engine itself (transport, HTTP status, our own window expiring) — never a local
// install problem — so toolSide stays false; the one distinction worth drawing is 429,
// which ends the rung climb (fetchEngineSERP).
func serpFailure(err error) serpAttempt {
	var he serpHTTPError
	return serpAttempt{
		errMsg:      err.Error(),
		rateLimited: errors.As(err, &he) && he.code == http.StatusTooManyRequests,
	}
}

var rungOrder = []string{"plain", "render", "stealth"}

// rungsFrom returns the connection rungs from `start` up to stealth (unknown/"" → the
// full ladder from plain).
func rungsFrom(start string) []string {
	for i, r := range rungOrder {
		if r == start {
			return rungOrder[i:]
		}
	}
	return rungOrder
}

// fetchSERPRaw fetches one SERP at a specific rung: plain → in-process serpGet
// (perEngineTimeout); render → scrapling fetch; stealth → scrapling stealthy-fetch (both
// format=html + serpHTMLMaxChars so the full page reaches serpClean).
type serpAttempt struct {
	html   string
	errMsg string // "" = this rung worked
	// toolSide: our own machinery failed (the browser never came up, the CLI is
	// missing) rather than the engine — the breaker must not charge it to the engine.
	toolSide bool
	// rateLimited: the engine answered "you are asking too fast" (HTTP 429). Ends the
	// rung climb — see fetchEngineSERP.
	rateLimited bool
}

func (s webTool) fetchSERPRaw(ctx context.Context, sid, target, rung string, perEngineTimeout time.Duration) serpAttempt {
	switch rung {
	case "render", "stealth":
		mode := "fetch"
		if rung == "stealth" {
			mode = "stealthy-fetch"
		}
		out, em, _ := s.scrapeFetch(ctx, sid, &scrapeArgs{URL: target, Mode: mode, Format: "html"}, serpHTMLMaxChars)
		if em != "" {
			// No rateLimited here: the subprocess hands back prose, not a status code.
			// It doesn't matter in practice — a 429 is seen on the way IN to the ladder
			// (the rung the engine is configured for), which ends the climb before this.
			return serpAttempt{errMsg: em, toolSide: out.ToolFailure}
		}
		return serpAttempt{html: out.Content}
	default: // "plain" or unset → cheap in-process GET
		ectx, cancel := context.WithTimeout(ctx, perEngineTimeout)
		defer cancel()
		body, err := serpGet(ectx, target)
		if err != nil {
			return serpFailure(err)
		}
		return serpAttempt{html: string(body)}
	}
}

// engineBreaker is the per-process dead-engine circuit breaker (P4.3, §13.3): an engine
// that returns no external results for engineDeadThreshold consecutive searches enters a
// cooldown during which harvestEngines skips it — so an engine anti-botted to death from
// this host auto-deprioritizes without an operator edit. Any search that yields results
// resets it. State is in-memory (resets on restart; mirrors the old dnsFailEntry breaker).
type engineBreaker struct {
	mu    sync.Mutex
	dead  map[string]int
	until map[string]time.Time
}

const (
	engineDeadThreshold = 3                // consecutive dead searches → cooldown
	engineCooldown      = 10 * time.Minute // how long a tripped engine stays skipped
)

func newEngineBreaker() *engineBreaker {
	return &engineBreaker{dead: map[string]int{}, until: map[string]time.Time{}}
}

// skip reports whether host is in cooldown (and lazily clears an elapsed cooldown,
// giving the engine a fresh chance).
func (b *engineBreaker) skip(host string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.until[host]
	if !ok {
		return false
	}
	if time.Now().After(t) {
		delete(b.until, host)
		b.dead[host] = 0
		return false
	}
	return true
}

// record updates the consecutive-dead count for host: alive resets it; otherwise it
// increments and trips a cooldown at the threshold.
func (b *engineBreaker) record(host string, alive bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if alive {
		b.dead[host] = 0
		delete(b.until, host)
		return
	}
	b.dead[host]++
	if b.dead[host] >= engineDeadThreshold {
		if _, tripped := b.until[host]; !tripped {
			b.until[host] = time.Now().Add(engineCooldown)
			slog.Warn("web: engine deprioritized — repeatedly returned no usable results (errors or only self-referential links)",
				"engine", host, "dead_searches", b.dead[host], "cooldown", engineCooldown)
		}
	}
}

// assembleSerp builds the one big blob the model reads. Each engine becomes a
// header section; library hits go under "Recently Visited". Empty/errored
// engines are omitted (their status is reported separately).
func assembleSerp(byEngine map[string]string, libHits []recallHit) string {
	if len(byEngine) == 0 && len(libHits) == 0 {
		return ""
	}
	var b strings.Builder
	// Library section first — URLs the model has successfully used before,
	// ordered by usefulness then recency (satisfied_count, last_seen).
	if len(libHits) > 0 {
		b.WriteString("### Recently Visited\n")
		for _, h := range libHits {
			title := h.Title
			if title == "" {
				title = h.URL
			}
			fmt.Fprintf(&b, "[%s](%s)\n", title, h.URL)
			if e := strings.TrimSpace(h.Excerpt); e != "" {
				if len(e) > 200 {
					e = truncateRunes(e, 200) + "…"
				}
				fmt.Fprintf(&b, "  %s\n", e)
			}
		}
		b.WriteString("\n")
	}
	// Engines, sorted by label for deterministic output.
	labels := make([]string, 0, len(byEngine))
	for k := range byEngine {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	for _, label := range labels {
		fmt.Fprintf(&b, "### %s\n%s\n\n", label, strings.TrimSpace(byEngine[label]))
	}
	return strings.TrimRight(b.String(), "\n")
}

// serpGet is the shared HTTP GET for SERP fetching: realistic UA, reasonable
// accept headers, 1 MB body cap (SERPs are tiny).
//
// Uses SafeHTTPClient which blocks SSRF targets (loopback / link-local /
// RFC1918) at dial time. Drains the response body after LimitReader so the
// connection can be reused.
//
// No client-level Timeout: every serpGet runs under the caller's per-engine
// context deadline (harvestEngines), so that one deadline is the single, honest
// bound.
var serpClient = SafeHTTPClient(0)

func serpGet(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7")
	resp, err := serpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Bounded drain: connection reuse is only worth draining a small tail.
		// Past the cap, Close drops the connection.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256*1024))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode/100 != 2 {
		return nil, serpHTTPError{code: resp.StatusCode, status: resp.Status}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
}

// --- serpClean: HTML → flat list of [title](url) lines ------------------
//
// SERP HTML is wildly inconsistent across engines. Pragmatic approach: ignore
// the rest of the page, extract ONLY <a href> tags, format each as a markdown
// link on its own line, drop non-http(s) and tracker URLs, dedupe identical
// ones, unwrap DDG/Bing tracking redirects. The model loses SERP snippets but
// gains a clean, dense list of real URLs it can pick from.

var entityReplacer = strings.NewReplacer(
	"&nbsp;", " ",
	"&amp;", "&",
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", `"`,
	"&#39;", "'",
	"&apos;", "'",
	"&ldquo;", `"`,
	"&rdquo;", `"`,
	"&lsquo;", "'",
	"&rsquo;", "'",
	"&mdash;", "—",
	"&ndash;", "–",
	"&hellip;", "…",
)

var whitespaceRe = regexp.MustCompile(`[\t \xa0]+`)

func collapseWS(s string) string {
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(s, " "))
}

// aHrefRe matches `<a ... href="..." ...>...</a>`. Greedy on the inner body —
// fine since <a> tags don't legally nest.
var aHrefRe = regexp.MustCompile(`(?is)<a\b[^>]*\bhref\s*=\s*["']([^"']+)["'][^>]*>(.*?)</a>`)

// innerTagsRe strips any HTML tag — used to clean inside aHrefRe's inner body
// (which may have <span>, <em>, etc. for highlighting).
var innerTagsRe = regexp.MustCompile(`(?s)<[^>]+>`)

func serpClean(html string) string {
	matches := aHrefRe.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		return ""
	}
	var b strings.Builder
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		// Decode HTML entities on the href BEFORE any parsing/unwrapping. SERP
		// hrefs encode query separators as &amp; (e.g. Bing's
		// /ck/a?...&amp;u=a1<base64>); left encoded, url.Query().Get("u") keys on
		// "amp;u" and unwrapRedirector can't find the wrapped target, so the link
		// leaks as an opaque tracker URL. Decoding here also yields clean output
		// URLs for the non-redirector case.
		href := entityReplacer.Replace(strings.TrimSpace(m[1]))
		hrefLower := strings.ToLower(href)
		// Skip non-navigable hrefs.
		if href == "" ||
			strings.HasPrefix(hrefLower, "javascript:") ||
			strings.HasPrefix(hrefLower, "mailto:") ||
			strings.HasPrefix(hrefLower, "#") ||
			strings.HasPrefix(hrefLower, "data:") {
			continue
		}
		// Only http(s) (also accept protocol-relative //...).
		if !(strings.HasPrefix(hrefLower, "http://") ||
			strings.HasPrefix(hrefLower, "https://") ||
			strings.HasPrefix(href, "//")) {
			continue
		}
		href = unwrapRedirector(href)
		// Inner text: strip nested tags, decode entities, collapse WS.
		inner := strings.TrimSpace(innerTagsRe.ReplaceAllString(m[2], ""))
		inner = entityReplacer.Replace(inner)
		inner = collapseWS(inner)
		if inner == "" {
			// Anchor with no visible text — usually icons. Skip.
			continue
		}
		key := inner + "|" + href
		if seen[key] {
			continue
		}
		seen[key] = true
		fmt.Fprintf(&b, "[%s](%s)\n", inner, href)
	}
	return b.String()
}

// unwrapRedirector turns common SERP tracking-redirect URLs into the real
// target. Currently handles DuckDuckGo (//duckduckgo.com/l/?uddg=…), Bing
// (https://www.bing.com/ck/a?...&u=a1<base64>), and Yahoo
// (https://r.search.yahoo.com/…/RU=<url-encoded target>/RK=…). Others pass through.
func unwrapRedirector(s string) string {
	if strings.HasPrefix(s, "//") {
		s = "https:" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	host := strings.ToLower(strings.TrimPrefix(u.Host, "www."))
	switch {
	case strings.HasSuffix(host, "duckduckgo.com") && u.Path == "/l/":
		if real := u.Query().Get("uddg"); real != "" {
			if dec, err := url.QueryUnescape(real); err == nil {
				return dec
			}
		}
	case strings.HasSuffix(host, "bing.com") && u.Path == "/ck/a":
		enc := u.Query().Get("u")
		if enc != "" {
			enc = strings.TrimPrefix(enc, "a1")
			for _, codec := range bingDecoders {
				if dec, err := codec.DecodeString(enc); err == nil && len(dec) > 0 {
					s2 := string(dec)
					if strings.HasPrefix(s2, "http") {
						return s2
					}
				}
			}
		}
	case strings.HasSuffix(host, "search.yahoo.com") && strings.Contains(s, "/RU="):
		// Yahoo SERP links bury the target in a /RU=<url-encoded>/RK=… segment. Work on
		// the RAW string (u.Path would have already %-decoded the target's own slashes,
		// breaking the "next /" split); the next LITERAL '/' is the /RK= delimiter.
		if i := strings.Index(s, "/RU="); i >= 0 {
			rest := s[i+len("/RU="):]
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				rest = rest[:j]
			}
			if dec, err := url.QueryUnescape(rest); err == nil && strings.HasPrefix(dec, "http") {
				return dec
			}
		}
	}
	return s
}

// bingDecoders covers the four common base64 variants Bing has used in its
// /ck/a redirector parameter. Try each until one yields a plausible URL.
var bingDecoders = []*base64.Encoding{
	base64.RawURLEncoding,
	base64.URLEncoding,
	base64.RawStdEncoding,
	base64.StdEncoding,
}

// hostOf returns the lowercased host of a URL with "www." stripped.
// Lowercase BEFORE stripping so a mixed/upper-case "WWW."/"Www." prefix is also
// stripped (keep in sync with leaf/urllib.hostOfURL, the write side).
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return strings.TrimPrefix(strings.ToLower(u.Host), "www.")
}
