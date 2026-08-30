package urllib

import (
	"context"
	"testing"

	"agentbob/contract"
)

// TestSearchEngines_SeedModesAndLangFilter exercises P1 (docs/web-fleet.md §4.1/§5):
// seed populates fetch_mode/lang_scope, SearchEngines returns them, orders
// cheapest-rung-first, and filters by language scope. liveLib skips without
// PGPOOL_TEST_DSN.
func TestSearchEngines_SeedModesAndLangFilter(t *testing.T) {
	lib, done := liveLib(t)
	defer done()
	ctx := context.Background()

	lib.seed(ctx, DefaultSeeds)

	all := lib.SearchEngines(ctx, "")
	byTitle := map[string]contract.URLCandidate{}
	for _, e := range all {
		byTitle[e.Title] = e
	}

	// fetch_mode / lang_scope populated from the recommended seed set.
	for title, wantMode := range map[string]string{
		"Brave Search": "plain",
		"Yahoo":        "plain",
		"Bing":         "plain",
		"Baidu 搜索":     "render",
		"Google":       "stealth",
	} {
		if got := byTitle[title].FetchMode; got != wantMode {
			t.Fatalf("%s fetch_mode = %q, want %q", title, got, wantMode)
		}
	}
	if got := byTitle["Baidu 搜索"].LangScope; got != "zh" {
		t.Fatalf("Baidu lang_scope = %q, want zh", got)
	}
	if _, ok := byTitle["Wikipedia (EN)"]; ok {
		t.Fatal("Wikipedia is not a search engine, must not appear in SearchEngines")
	}

	// Ordering: plain rung first, stealth (Google) last.
	if all[0].FetchMode != "plain" {
		t.Fatalf("first engine should be a plain rung, got %q (%s)", all[0].FetchMode, all[0].Title)
	}
	if all[len(all)-1].Title != "Google" {
		t.Fatalf("stealth Google should sort last, got %q", all[len(all)-1].Title)
	}

	// Language filter: lang=es drops the zh-scoped Baidu, keeps globals.
	for _, e := range lib.SearchEngines(ctx, "es") {
		if e.LangScope != "*" && e.LangScope != "es" {
			t.Fatalf("lang=es returned engine scope %q (%s)", e.LangScope, e.Title)
		}
		if e.Title == "Baidu 搜索" {
			t.Fatal("zh-scoped Baidu must not appear for lang=es")
		}
	}
	// lang=zh keeps Baidu.
	var baiduForZh bool
	for _, e := range lib.SearchEngines(ctx, "zh") {
		if e.Title == "Baidu 搜索" {
			baiduForZh = true
		}
	}
	if !baiduForZh {
		t.Fatal("Baidu (zh) must appear for lang=zh")
	}
}

// TestSeed_FillIfDefault: a pre-existing engine row with empty fetch_mode gets
// filled on re-seed (the schemaV2 backfill path), while an operator-set fetch_mode
// survives — and counts are never touched.
func TestSeed_FillIfDefault(t *testing.T) {
	lib, done := liveLib(t)
	defer done()
	ctx := context.Background()

	// Rows as they'd look right after the schemaV2 ALTER (fetch_mode '' default),
	// plus an operator-customized one and a satisfied count to prove it's preserved.
	mustExec(t, lib, ctx,
		`INSERT INTO bob_urls (url, first_seen, last_seen, is_search_engine, satisfied_count)
		 VALUES ('https://www.bing.com/search?q=', 1, 1, TRUE, 7)`)
	mustExec(t, lib, ctx,
		`INSERT INTO bob_urls (url, first_seen, last_seen, is_search_engine, fetch_mode)
		 VALUES ('https://www.baidu.com/s?wd=', 1, 1, TRUE, 'stealth')`)

	lib.seed(ctx, DefaultSeeds)

	var bingMode string
	var bingSatisfied int
	if err := lib.db.QueryRow(ctx,
		`SELECT fetch_mode, satisfied_count FROM bob_urls WHERE url='https://www.bing.com/search?q='`).
		Scan(&bingMode, &bingSatisfied); err != nil {
		t.Fatal(err)
	}
	if bingMode != "plain" {
		t.Fatalf("empty Bing fetch_mode should be filled to plain, got %q", bingMode)
	}
	if bingSatisfied != 7 {
		t.Fatalf("seed must not touch satisfied_count, got %d want 7", bingSatisfied)
	}

	var baiduMode string
	if err := lib.db.QueryRow(ctx,
		`SELECT fetch_mode FROM bob_urls WHERE url='https://www.baidu.com/s?wd='`).Scan(&baiduMode); err != nil {
		t.Fatal(err)
	}
	if baiduMode != "stealth" {
		t.Fatalf("operator-set Baidu fetch_mode should survive re-seed, got %q", baiduMode)
	}
}

// TestFetchHintRoundtrip: P4.4 per-host learned fetch mode stored on bob_urls — Record
// stamps the host, LearnFetchMode writes the mode on the url's row, FetchHint reads it by
// host (any url of the host serves), latest-wins, engine rows untouched.
func TestFetchHintRoundtrip(t *testing.T) {
	lib, done := liveLib(t)
	defer done()
	ctx := context.Background()

	if got := lib.FetchHint(ctx, "gov.cn"); got != "" {
		t.Fatalf("unlearned host should hint \"\", got %q", got)
	}

	// Record a content URL (creates the row + stamps host=gov.cn), then learn its mode.
	lib.Record(ctx, "sid1", "https://www.gov.cn/page1", "", "", "q")
	lib.LearnFetchMode(ctx, "https://www.gov.cn/page1", "fetch")
	if got := lib.FetchHint(ctx, "gov.cn"); got != "fetch" {
		t.Fatalf("FetchHint = %q, want fetch", got)
	}

	// A DIFFERENT url on the same host inherits the host hint (per-host lookup), and a
	// newer learned mode wins.
	lib.Record(ctx, "sid1", "https://gov.cn/page2", "", "", "q")
	lib.LearnFetchMode(ctx, "https://gov.cn/page2", "get")
	if got := lib.FetchHint(ctx, "gov.cn"); got != "get" {
		t.Fatalf("FetchHint after newer learn = %q, want get", got)
	}

	// Empty url / mode are no-ops; learning a url with no row is a no-op.
	lib.LearnFetchMode(ctx, "", "get")
	lib.LearnFetchMode(ctx, "https://gov.cn/page2", "")
	lib.LearnFetchMode(ctx, "https://norow.example/x", "fetch")
	if got := lib.FetchHint(ctx, "norow.example"); got != "" {
		t.Fatalf("learning a url with no row must be a no-op, got %q", got)
	}
}

func mustExec(t *testing.T, lib *library, ctx context.Context, sql string) {
	t.Helper()
	if _, err := lib.db.Exec(ctx, sql); err != nil {
		t.Fatalf("exec: %v", err)
	}
}
