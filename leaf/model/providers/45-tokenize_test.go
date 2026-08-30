package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCountTokens_LlamaCpp: /tokenize lives at the server root (base_url minus
// /v1), takes {"content": ...} and the count is len(tokens).
func TestCountTokens_LlamaCpp(t *testing.T) {
	var gotPath, gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotContent = body.Content
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": []int{1, 2, 3, 4, 5}})
	}))
	defer srv.Close()

	n, err := CountTokens(context.Background(), "llama-cpp", srv.URL+"/v1", "", "m", "你好世界")
	if err != nil || n != 5 {
		t.Fatalf("CountTokens = %d/%v, want 5/nil", n, err)
	}
	if gotPath != "/tokenize" || gotContent != "你好世界" {
		t.Fatalf("request path=%q content=%q, want /tokenize with the text", gotPath, gotContent)
	}
}

// TestCountTokens_VLLM: vllm's /tokenize returns a count field.
func TestCountTokens_VLLM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 7, "tokens": []int{1, 2, 3, 4, 5, 6, 7}})
	}))
	defer srv.Close()

	n, err := CountTokens(context.Background(), "vllm", srv.URL+"/v1", "", "m", "text")
	if err != nil || n != 7 {
		t.Fatalf("CountTokens = %d/%v, want 7/nil", n, err)
	}
}

// TestCountTokens_Fallbacks: no tokenize path / a dead server / empty text.
func TestCountTokens_Fallbacks(t *testing.T) {
	if _, err := CountTokens(context.Background(), "openrouter", "http://x/v1", "", "m", "text"); err == nil {
		t.Fatal("a provider without a tokenize path must error")
	}
	if _, err := CountTokens(context.Background(), "llama-cpp", "http://127.0.0.1:1/v1", "", "m", "text"); err == nil {
		t.Fatal("a dead server must error")
	}
	if n, err := CountTokens(context.Background(), "llama-cpp", "http://127.0.0.1:1/v1", "", "m", ""); err != nil || n != 0 {
		t.Fatalf("empty text = 0/nil without a round trip, got %d/%v", n, err)
	}
}
