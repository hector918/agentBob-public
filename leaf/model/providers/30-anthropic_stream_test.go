package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
)

// TestAnthropicChatStream_ToolUseDeltas — Anthropic emits tool_use as
// `content_block_start` (carrying id + name) followed by
// `content_block_delta` events with type=input_json_delta + partial
// JSON. ChatStream must emit a ToolCallDelta per fragment with the
// matching index/id/name tagging.
func TestAnthropicChatStream_ToolUseDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["tools"]; !ok {
			t.Errorf("server: tools field missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		// Standard Anthropic stream sequence for one tool_use.
		writeEvent := func(name, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
			if fl != nil {
				fl.Flush()
			}
		}
		writeEvent("message_start", `{"message":{"usage":{"input_tokens":10}}}`)
		writeEvent("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_abc","name":"patch"}}`)
		writeEvent("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\""}}`)
		writeEvent("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"orc.md\",\"append\":\"hi\"}"}}`)
		writeEvent("content_block_stop", `{"index":0}`)
		writeEvent("message_delta", `{"usage":{"output_tokens":15}}`)
		writeEvent("message_stop", `{}`)
	}))
	defer srv.Close()

	client := NewAnthropicClient(srv.URL, "test-key", 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := client.ChatStream(ctx, "claude-test", []contract.Message{{Role: "user", Content: "hi"}}, []contract.ToolSpec{{Name: "patch"}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var deltas []contract.ToolCallDelta
	for ev := range ch {
		if ev.ToolDelta != nil {
			deltas = append(deltas, *ev.ToolDelta)
		}
		if ev.Done {
			break
		}
	}
	if len(deltas) != 3 {
		t.Fatalf("expected 3 deltas (announce + 2 partials), got %d: %#v", len(deltas), deltas)
	}
	// First delta = the content_block_start announce.
	if deltas[0].Name != "patch" || deltas[0].ID != "toolu_abc" || deltas[0].Index != 0 || deltas[0].ArgumentsDelta != "" {
		t.Errorf("announce delta wrong: %#v", deltas[0])
	}
	// Subsequent deltas should preserve index + carry the partial JSON.
	for i := 1; i < 3; i++ {
		if deltas[i].Index != 0 || deltas[i].ID != "toolu_abc" || deltas[i].Name != "patch" {
			t.Errorf("partial delta[%d] metadata wrong: %#v", i, deltas[i])
		}
	}
	full := deltas[1].ArgumentsDelta + deltas[2].ArgumentsDelta
	if !strings.Contains(full, `"path":"orc.md"`) || !strings.Contains(full, `"append":"hi"`) {
		t.Errorf("accumulated args wrong: %q", full)
	}
}

// TestAnthropicChatStream_TextAndToolUseInterleaved — text content
// blocks + tool_use blocks at different indices both flow through.
func TestAnthropicChatStream_TextAndToolUseInterleaved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeEvent := func(name, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
			if fl != nil {
				fl.Flush()
			}
		}
		writeEvent("message_start", `{"message":{"usage":{"input_tokens":5}}}`)
		writeEvent("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`)
		writeEvent("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"thinking"}}`)
		writeEvent("content_block_stop", `{"index":0}`)
		writeEvent("content_block_start", `{"index":1,"content_block":{"type":"tool_use","id":"t1","name":"f"}}`)
		writeEvent("content_block_delta", `{"index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)
		writeEvent("content_block_stop", `{"index":1}`)
		writeEvent("message_stop", `{}`)
	}))
	defer srv.Close()
	client := NewAnthropicClient(srv.URL, "k", 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, _ := client.ChatStream(ctx, "claude-test", []contract.Message{{Role: "user", Content: "x"}}, []contract.ToolSpec{{Name: "f"}})
	var text strings.Builder
	gotTool := false
	for ev := range ch {
		if ev.Text != "" {
			text.WriteString(ev.Text)
		}
		if ev.ToolDelta != nil && ev.ToolDelta.Index == 1 {
			gotTool = true
		}
		if ev.Done {
			break
		}
	}
	if text.String() != "thinking" {
		t.Errorf("text wrong: %q", text.String())
	}
	if !gotTool {
		t.Errorf("missed tool_use delta at index 1")
	}
}
