package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"agentbob/contract"
)

// TestCountTokens_CachePerModel: readings are cached by (entry, model, text) —
// a repeat weigh of the same text never re-hits the backend, while a different
// backing model (e.g. after a reload swap) gets its own cache line.
func TestCountTokens_CachePerModel(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": []int{1, 2, 3}})
	}))
	defer srv.Close()

	row := sizedEntry("tok", 5, 131072)
	row.info.Provider = "llama-cpp"
	row.baseURL = srv.URL + "/v1"
	p := newTestPool(row)
	defer p.Close()

	if n, ok := p.CountTokens(context.Background(), contract.ModelRequest{}, "你好"); !ok || n != 3 {
		t.Fatalf("CountTokens = %d/%v, want 3/true", n, ok)
	}
	if n, ok := p.CountTokens(context.Background(), contract.ModelRequest{}, "你好"); !ok || n != 3 || hits.Load() != 1 {
		t.Fatalf("repeat = %d/%v hits=%d, want cache hit (1 backend call)", n, ok, hits.Load())
	}
	// Same entry name, different backing model → its own cache line.
	row.info.Model = "swapped-model"
	if _, ok := p.CountTokens(context.Background(), contract.ModelRequest{}, "你好"); !ok || hits.Load() != 2 {
		t.Fatalf("model swap must miss the cache, hits=%d want 2", hits.Load())
	}
}

// TestCountTokens_SkipsIncapableEntry: a higher-priority entry whose provider
// has no tokenize endpoint (structural, e.g. openrouter) is skipped — the
// count comes from the next capable entry instead of failing the whole path.
func TestCountTokens_SkipsIncapableEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": []int{1, 2}})
	}))
	defer srv.Close()

	remote := sizedEntry("remote", 9, 131072) // outranks — but openrouter has no /tokenize
	remote.info.Provider = "openrouter"
	local := sizedEntry("local", 5, 131072)
	local.info.Provider = "llama-cpp"
	local.baseURL = srv.URL + "/v1"
	p := newTestPool(remote, local)
	defer p.Close()

	if n, ok := p.CountTokens(context.Background(), contract.ModelRequest{}, "hi"); !ok || n != 2 {
		t.Fatalf("CountTokens = %d/%v, want 2/true via the capable lower-priority entry", n, ok)
	}
}

// TestCountTokens_NoEntry: an unpickable request reports ok=false (caller
// falls back to its estimator) — and failures are never cached.
func TestCountTokens_NoEntry(t *testing.T) {
	p := newTestPool(sizedEntry("t", 5, 131072)) // openrouter-ish: no provider set → no tokenize path
	defer p.Close()
	if _, ok := p.CountTokens(context.Background(), contract.ModelRequest{Requires: []string{"nope"}}, "x"); ok {
		t.Fatal("no entry with the tag → ok=false")
	}
	if _, ok := p.CountTokens(context.Background(), contract.ModelRequest{}, "x"); ok {
		t.Fatal("entry without a tokenize path → ok=false")
	}
}
