package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agentbob/contract"
)

// TestApplyExtraBody_StreamNotOverridable — regression for full-audit MD-1
//: a models.yaml extra_body entry must not be able to flip the
// "stream" field the client hard-wired for its response-parsing path, while
// every other key still passes through (and still wins collisions).
func TestApplyExtraBody_StreamNotOverridable(t *testing.T) {
	body := map[string]any{"stream": true, "cache_prompt": true}
	applyExtraBody(body, map[string]any{"stream": false, "cache_prompt": false, "top_k": 40})
	if body["stream"] != true {
		t.Errorf("stream overridden by extra_body: got %v, want true", body["stream"])
	}
	if body["cache_prompt"] != false {
		t.Errorf("cache_prompt not overridden: got %v, want false", body["cache_prompt"])
	}
	if body["top_k"] != 40 {
		t.Errorf("top_k not merged: got %v, want 40", body["top_k"])
	}
}

// TestChatStream_ExtraBodyCannotDisableStream — end-to-end shape check: with
// extra_body {"stream": false} configured, the streaming request that reaches
// the wire still carries stream:true (and the SSE reply parses normally).
func TestChatStream_ExtraBodyCannotDisableStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("server: decode body: %v", err)
		}
		if body["stream"] != true {
			t.Errorf("server: stream = %v, want true", body["stream"])
		}
		if body["top_k"] != float64(40) {
			t.Errorf("server: top_k = %v, want 40 (extra_body passthrough)", body["top_k"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", 0, map[string]any{"stream": false, "top_k": 40})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := client.ChatStream(ctx, "test-model", []contract.Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		got += ev.Text
	}
	if got != "ok" {
		t.Errorf("streamed content = %q, want %q", got, "ok")
	}
}

// TestChat_ExtraBodyCannotEnableStream — block-path counterpart: extra_body
// {"stream": true} must not inject a stream field into the non-streaming
// request (the block path parses a single JSON response, not SSE).
func TestChat_ExtraBodyCannotEnableStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("server: decode body: %v", err)
		}
		if v, ok := body["stream"]; ok {
			t.Errorf("server: stream field present on block path: %v", v)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", 0, map[string]any{"stream": true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Chat(ctx, "test-model", []contract.Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Chat content = %q, want %q", resp.Content, "ok")
	}
}
