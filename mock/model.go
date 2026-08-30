// Package mock provides a fake OpenAI-compatible model endpoint for end-to-end
// tests: it lets the real model pool + provider client + turn pipeline run
// against a deterministic backend, with no real LLM and no network.
//
// Usage:
//
//	srv := mock.NewModelServer(func(lastUser string) string { return "echo: " + lastUser })
//	defer srv.Close()
//	// point models.yaml's base_url at srv.URL + "/v1"
package mock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
)

// ReplyFunc maps the last user message to the assistant's reply text.
type ReplyFunc func(lastUser string) string

// NewModelServer starts a mock OpenAI-compat server on a random port (tests).
func NewModelServer(reply ReplyFunc) *httptest.Server {
	return httptest.NewServer(Handler(reply))
}

// Handler returns the mock OpenAI-compat http.Handler. reply produces the
// assistant text from the last user message (nil → a fixed echo). It serves
// GET /v1/models (probe) and POST /v1/chat/completions (both streaming SSE and
// non-streaming, picked by the request's "stream" flag). Use NewModelServer for
// tests; use this directly to run a standalone server on a fixed port.
func Handler(reply ReplyFunc) http.Handler {
	if reply == nil {
		reply = func(u string) string { return "mock reply to: " + u }
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "mock", "max_model_len": 8192}},
		})
	})

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		lastUser := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				lastUser = m.Content
			}
		}
		text := reply(lastUser)
		usage := map[string]any{"prompt_tokens": 10, "completion_tokens": 20}

		if !req.Stream {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
				"usage":   usage,
			})
			return
		}

		// Streaming SSE: a few content deltas, then a final usage frame, then [DONE].
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		for _, chunk := range splitRunes(text, 12) {
			payload, _ := json.Marshal(map[string]any{
				"choices": []map[string]any{{"delta": map[string]any{"content": chunk}}},
			})
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
		final, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   usage,
		})
		fmt.Fprintf(w, "data: %s\n\n", final)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})

	return mux
}

// splitRunes chunks s into rune-groups of at most n (so the reply streams as
// several deltas, exercising the client's SSE accumulation).
func splitRunes(s string, n int) []string {
	r := []rune(s)
	if len(r) == 0 {
		return []string{""}
	}
	var out []string
	for i := 0; i < len(r); i += n {
		end := i + n
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}
