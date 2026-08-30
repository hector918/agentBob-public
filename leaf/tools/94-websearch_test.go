package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestSerpCleanUnwrapsEntityEncodedBingRedirect guards the href entity-decode in
// serpClean. Real Bing SERP hrefs encode query separators as &amp; (e.g.
// /ck/a?...&amp;u=a1<base64>). Without decoding the href before unwrapRedirector,
// url.Query().Get("u") keys on "amp;u" instead of "u", so the wrapped target
// can't be found and the link leaks to the model as an opaque bing.com/ck/a
// tracker URL. After the fix the real destination must surface.
func TestSerpCleanUnwrapsEntityEncodedBingRedirect(t *testing.T) {
	target := "https://www.gov.cn/zhengce/content_7037902.htm"
	enc := "a1" + base64.RawURLEncoding.EncodeToString([]byte(target))
	// &amp;-encoded exactly as it appears in real Bing SERP HTML.
	html := `<a href="https://www.bing.com/ck/a?!&amp;&amp;p=deadbeef&amp;u=` + enc + `&amp;ntb=1">政策解读</a>`

	out := serpClean(html)
	if !strings.Contains(out, target) {
		t.Fatalf("expected unwrapped target %q in output, got:\n%s", target, out)
	}
	if strings.Contains(out, "bing.com/ck/a") {
		t.Fatalf("bing redirect should be unwrapped, but the wrapper leaked:\n%s", out)
	}
}

// TestSerpCleanDecodesPlainHrefEntities checks the non-redirector path: a plain
// result href with &amp; separators is emitted decoded (clean &), not left as
// &amp;, so the model gets a directly usable URL.
func TestSerpCleanDecodesPlainHrefEntities(t *testing.T) {
	html := `<a href="https://example.com/s?a=1&amp;b=2">标题</a>`
	out := serpClean(html)
	if !strings.Contains(out, "https://example.com/s?a=1&b=2") {
		t.Fatalf("expected decoded href in output, got:\n%s", out)
	}
	if strings.Contains(out, "&amp;") {
		t.Fatalf("href entities should be decoded, but &amp; leaked:\n%s", out)
	}
}

// TestTopCandidates: auto-read candidate selection puts known-good library hits first,
// then fresh SERP links in deterministic engine order, excludes the engines' own hosts,
// dedupes, and respects the cap.
func TestTopCandidates(t *testing.T) {
	engines := []searchEngine{
		{URL: "https://duckduckgo.com/html/?q="},
		{URL: "https://www.bing.com/search?q="},
	}
	cleaned := map[string]string{
		// label "bing.com" sorts before "duckduckgo.com" → parsed first.
		"bing.com":       "[Login](https://www.bing.com/account)\n[EV range guide](https://greencars.com/ev)\n[dup](https://example.com/a)",
		"duckduckgo.com": "[r1](https://example.com/a)\n[r2](https://recurrentauto.com/ev)",
	}
	libHits := []recallHit{{URL: "https://carfax.com/ev-rankings", Title: "Carfax EV"}}

	got := topCandidates(cleaned, libHits, engines, 3)
	if len(got) != 3 {
		t.Fatalf("want 3 candidates, got %d: %+v", len(got), got)
	}
	if got[0].url != "https://carfax.com/ev-rankings" {
		t.Fatalf("library hit should rank first, got %q", got[0].url)
	}
	seen := map[string]int{}
	for _, c := range got {
		h := hostOf(c.url)
		if h == "bing.com" || h == "duckduckgo.com" {
			t.Fatalf("engine-host URL must be excluded, leaked %q", c.url)
		}
		seen[c.url]++
	}
	if seen["https://example.com/a"] > 1 {
		t.Fatalf("duplicate URL not deduped: %+v", got)
	}
	// Expected set: carfax (lib) + greencars + example.com/a (bing blob), recurrentauto
	// dropped by the cap of 3.
	if seen["https://greencars.com/ev"] != 1 || seen["https://example.com/a"] != 1 {
		t.Fatalf("unexpected candidate set: %+v", got)
	}
}

// TestTopCandidates_ExcludesEngineSubdomains: an engine that returns its own nav/login
// chrome (baidu → passport.baidu.com, news.baidu.com) must not have that chrome picked
// for auto-read — exclusion is by registered domain + subdomains, not just exact host.
func TestTopCandidates_ExcludesEngineSubdomains(t *testing.T) {
	engines := []searchEngine{{URL: "https://www.baidu.com/s?wd="}}
	cleaned := map[string]string{
		"baidu.com": "[登录](https://passport.baidu.com/v2/?login)\n[新闻](https://news.baidu.com)\n[real result](https://gov.cn/policy)",
	}
	got := topCandidates(cleaned, nil, engines, 3)
	for _, c := range got {
		h := hostOf(c.url)
		if h == "baidu.com" || strings.HasSuffix(h, ".baidu.com") {
			t.Fatalf("baidu chrome subdomain leaked into auto-read candidates: %q", c.url)
		}
	}
	if len(got) != 1 || got[0].url != "https://gov.cn/policy" {
		t.Fatalf("expected only the real external result, got %+v", got)
	}
}

// TestExternalLinkCount: the if-thin signal counts DISTINCT external result links,
// excluding engine host/subdomain chrome and dupes — a chrome-only engine reads as 0.
func TestExternalLinkCount(t *testing.T) {
	engines := []searchEngine{
		{URL: "https://www.baidu.com/s?wd="},
		{URL: "https://duckduckgo.com/html/?q="},
	}
	cleaned := map[string]string{
		// pure baidu nav chrome → 0 external
		"baidu.com": "[登录](https://passport.baidu.com/v2/?login)\n[新闻](https://news.baidu.com)",
		// real results + one dup of a baidu-chrome host (must not count)
		"duckduckgo.com": "[r1](https://gov.cn/a)\n[r2](https://zaobao.com.sg/b)\n[dup](https://gov.cn/a)\n[nav](https://news.baidu.com)",
	}
	got := externalLinkCount(cleaned, engines)
	if got != 2 {
		t.Fatalf("externalLinkCount = %d, want 2 (gov.cn/a + zaobao/b; baidu chrome + dup excluded)", got)
	}
	// chrome-only batch → 0 (would trigger if-thin escalation)
	if n := externalLinkCount(map[string]string{"baidu.com": cleaned["baidu.com"]}, engines); n != 0 {
		t.Fatalf("chrome-only engine should yield 0 external links, got %d", n)
	}
}

// TestCondenseRelevanceSignal: condense's second return is the §13.5 relevance signal —
// false only when the extractor RAN and returned the NONE sentinel; missing/failed
// extractor falls back to a head-cap and stays relevant.
func TestCondenseRelevanceSignal(t *testing.T) {
	ctx := context.Background()
	off := webTool{extract: func(context.Context, string, string) (string, error) { return "NONE", nil }}
	if _, ok := off.condense(ctx, "q", "a page"); ok {
		t.Fatal("NONE sentinel must mark the page off-topic (relevant=false)")
	}
	good := webTool{extract: func(context.Context, string, string) (string, error) { return "relevant passage", nil }}
	if ex, ok := good.condense(ctx, "q", "page"); !ok || ex != "relevant passage" {
		t.Fatalf("expected relevant extract, got %q ok=%v", ex, ok)
	}
	none := webTool{} // no extractor → head-cap, relevant
	if ex, ok := none.condense(ctx, "q", "raw content"); !ok || ex == "" {
		t.Fatalf("no extractor should head-cap and stay relevant, got %q ok=%v", ex, ok)
	}
	errx := webTool{extract: func(context.Context, string, string) (string, error) { return "", errors.New("boom") }}
	if _, ok := errx.condense(ctx, "q", "raw"); !ok {
		t.Fatal("extractor error should fall back and stay relevant")
	}
}

// TestExtractCapBytes: the extractor's per-round input is sized to the extract
// model's window AND a prefill-time budget, in CJK-safe bytes (≤ budget tokens
// on any script), with a floor.
func TestExtractCapBytes(t *testing.T) {
	// Unknown window → the prefill-budget cap (10000 tok × 3 CJK bytes).
	if got := extractCapBytes(0); got != extractPrefillTokBudget*3 {
		t.Fatalf("unknown window cap=%d, want %d", got, extractPrefillTokBudget*3)
	}
	// Large window → still bounded by the prefill budget, not the window.
	if got := extractCapBytes(131072); got != extractPrefillTokBudget*3 {
		t.Fatalf("large-window cap=%d, want prefill-bounded %d", got, extractPrefillTokBudget*3)
	}
	// Small window binds below the prefill budget (window×3/4 tokens × 3 bytes).
	if got := extractCapBytes(8192); got != 8192*3/4*3 {
		t.Fatalf("small-window cap=%d, want window-bounded %d", got, 8192*3/4*3)
	}
	// Tiny window → the 4096-byte floor (absurdly small segments are pointless).
	if got := extractCapBytes(1000); got != 4096 {
		t.Fatalf("tiny-window cap=%d, want floor 4096", got)
	}
}

// TestEngineBreaker: an engine trips into cooldown after engineDeadThreshold consecutive
// dead searches, and a live result resets it before the threshold.
func TestEngineBreaker(t *testing.T) {
	b := newEngineBreaker()
	const h = "baidu.com"
	if b.skip(h) {
		t.Fatal("fresh engine must not be skipped")
	}
	// dead < threshold → still fired
	for i := 0; i < engineDeadThreshold-1; i++ {
		b.record(h, false)
		if b.skip(h) {
			t.Fatalf("engine skipped too early after %d dead", i+1)
		}
	}
	// the threshold-th dead trips cooldown
	b.record(h, false)
	if !b.skip(h) {
		t.Fatal("engine should be in cooldown after threshold consecutive dead searches")
	}
	// a live result clears it
	b.record(h, true)
	if b.skip(h) {
		t.Fatal("a live result must reset the breaker")
	}

	// a live result mid-streak resets the dead count (no trip)
	b2 := newEngineBreaker()
	b2.record(h, false)
	b2.record(h, false)
	b2.record(h, true) // reset
	b2.record(h, false)
	if b2.skip(h) {
		t.Fatal("dead count should have reset after the live result")
	}
}

// TestSerpCleanUnwrapsYahoo: Yahoo SERP links bury the target in a /RU=<url-encoded>/
// path segment; serpClean must surface the real destination, not the r.search.yahoo.com
// redirect.
func TestSerpCleanUnwrapsYahoo(t *testing.T) {
	html := `<a href="https://r.search.yahoo.com/_ylt=Aw/RV=2/RE=1/RO=10/RU=https%3a%2f%2fwww.gov.cn%2fzhengce%2fpage/RK=2/RS=xyz">政策解读</a>`
	out := serpClean(html)
	if !strings.Contains(out, "https://www.gov.cn/zhengce/page") {
		t.Fatalf("expected unwrapped yahoo target, got:\n%s", out)
	}
	if strings.Contains(out, "r.search.yahoo.com") {
		t.Fatalf("yahoo redirect should be unwrapped, leaked:\n%s", out)
	}
}

// TestPlanEngineWave_AllCooledStillFires: the breaker may thin a wave but never empty
// it — an all-cooled wave fires the full set (and reports no "deprioritized" status),
// because skipping everything answers nothing and lets no engine prove it recovered.
func TestPlanEngineWave_AllCooledStillFires(t *testing.T) {
	engines := []searchEngine{
		{URL: "https://search.brave.com/search?q=", FetchMode: "plain"},
		{URL: "https://search.yahoo.com/search?p=", FetchMode: "plain"},
	}

	// One cooled of two → that one is held back, the other fires.
	jobs, status := planEngineWave(engines, func(h string) bool { return h == "search.brave.com" }, true)
	if len(jobs) != 1 || jobs[0].host != "search.yahoo.com" {
		t.Fatalf("expected only yahoo to fire, got %+v", jobs)
	}
	if got := status["search.brave.com"]; !strings.Contains(got, "cooldown") {
		t.Fatalf("cooled engine should be reported as deprioritized, got %q", got)
	}

	// Everything cooled → everything fires anyway, with no leftover status entries.
	jobs, status = planEngineWave(engines, func(string) bool { return true }, true)
	if len(jobs) != len(engines) {
		t.Fatalf("all-cooled wave must fire every engine, got %d of %d", len(jobs), len(engines))
	}
	if len(status) != 0 {
		t.Fatalf("engines that end up firing must not also be reported as deprioritized: %v", status)
	}

	// No engines configured → nothing to fire, and no phantom warning path.
	if jobs, _ := planEngineWave(nil, func(string) bool { return true }, true); len(jobs) != 0 {
		t.Fatalf("empty engine list must plan no jobs, got %+v", jobs)
	}
}

// TestPlanEngineWave_DuplicateHostsGetOrdinalLabels: two engines on one host keep
// distinct status keys (the label dedupe), and the labels are the SAME whether the wave
// was planned normally or rebuilt by the all-cooled rescue — a label that shifted between
// the two paths would make one search's status keys incomparable with the next one's.
func TestPlanEngineWave_DuplicateHostsGetOrdinalLabels(t *testing.T) {
	engines := []searchEngine{
		{URL: "https://search.yahoo.com/search?p="},
		{URL: "https://search.yahoo.com/search;_ylt=x?p="},
	}
	normal, _ := planEngineWave(engines, func(string) bool { return false }, true)
	rescued, _ := planEngineWave(engines, func(string) bool { return true }, true)
	for _, jobs := range [][]engineJob{normal, rescued} {
		if len(jobs) != 2 || jobs[0].label == jobs[1].label {
			t.Fatalf("same-host engines need distinct labels, got %+v", jobs)
		}
		if jobs[0].host != jobs[1].host {
			t.Fatalf("label dedupe must not change the breaker key: %q vs %q", jobs[0].host, jobs[1].host)
		}
	}
	if normal[0].label != rescued[0].label || normal[1].label != rescued[1].label {
		t.Fatalf("labels must not shift between the normal and rescued path: %v vs %v",
			[]string{normal[0].label, normal[1].label}, []string{rescued[0].label, rescued[1].label})
	}
}

// TestPlanEngineWave_RescueIsOptIn: the all-cooled rescue is for the DEFAULT wave only.
// The stealth wave is an if-thin fallback and is usually a single engine, so rescuing it
// would make the breaker a no-op there and spend a stealth browser launch on a
// known-dead engine every search.
func TestPlanEngineWave_RescueIsOptIn(t *testing.T) {
	stealth := []searchEngine{{URL: "https://www.google.com/search?q=", FetchMode: "stealth"}}
	jobs, status := planEngineWave(stealth, func(string) bool { return true }, false)
	if len(jobs) != 0 {
		t.Fatalf("a cooled stealth engine must stay cooled without rescue, got %+v", jobs)
	}
	if got := status["google.com"]; !strings.Contains(got, "cooldown") {
		t.Fatalf("the held-back engine should still be reported, got %q", got)
	}
}

// TestAllEnginesFailedMsg: engines failing with ONE identical error means the fetch
// machinery in front of them is broken (independent hosts don't fail identically), and
// the model must be told to stop retrying rather than handed a list of hosts.
func TestAllEnginesFailedMsg(t *testing.T) {
	same := map[string]string{
		"search.brave.com": "error: scrapling exited exit status 1",
		"search.yahoo.com": "error: scrapling exited exit status 1",
		"www.bing.com":     "error: scrapling exited exit status 1",
	}
	msg := allEnginesFailedMsg(len(same), same)
	if !strings.Contains(msg, "SAME error") || !strings.Contains(msg, "operator") {
		t.Fatalf("identical failures should blame the machinery, got: %s", msg)
	}

	mixed := map[string]string{
		"search.brave.com": "error: http 429 Too Many Requests",
		"search.yahoo.com": "empty",
	}
	if msg := allEnginesFailedMsg(len(mixed), mixed); strings.Contains(msg, "SAME error") {
		t.Fatalf("differing failures are per-engine news, got: %s", msg)
	}

	// A single engine can't corroborate anything — one error is not a pattern.
	one := map[string]string{"search.brave.com": "error: scrapling exited exit status 1"}
	if msg := allEnginesFailedMsg(1, one); strings.Contains(msg, "SAME error") {
		t.Fatalf("a single engine must not trigger the machinery verdict, got: %s", msg)
	}
}

// TestSerpFailure_RateLimitIsTyped: only a 429 ends the rung climb. The status has to
// survive as a TYPE — when serpGet returned a flat fmt.Errorf the code was unreadable,
// and a rate-limited engine got re-asked from the same IP through a browser launch and
// then a stealth launch. 403 deliberately does NOT count: anti-bot is the exact case the
// higher rungs exist for.
func TestSerpFailure_RateLimitIsTyped(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"429", serpHTTPError{code: 429, status: "429 Too Many Requests"}, true},
		{"403", serpHTTPError{code: 403, status: "403 Forbidden"}, false},
		{"503", serpHTTPError{code: 503, status: "503 Service Unavailable"}, false},
		{"wrapped 429 still counts", fmt.Errorf("engine x: %w", serpHTTPError{code: 429, status: "429 Too Many Requests"}), true},
		{"plain transport error", errors.New("dial tcp: i/o timeout"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := serpFailure(tc.err)
			if a.rateLimited != tc.want {
				t.Fatalf("rateLimited = %v, want %v (err=%q)", a.rateLimited, tc.want, a.errMsg)
			}
			if a.errMsg != tc.err.Error() {
				t.Fatalf("the engine's own words must survive verbatim: %q vs %q", a.errMsg, tc.err.Error())
			}
			if a.toolSide {
				t.Fatal("an engine's answer is never our own machinery failing")
			}
			if a.html != "" {
				t.Fatalf("a failure carries no html, got %q", a.html)
			}
		})
	}
}
