package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
)

// TestChatStream_ToolCallDeltas — openai-compat backend streams a tool_call
// in pieces: first chunk has Index + ID + function.Name (empty Arguments),
// subsequent chunks have Arguments deltas. ChatStream must emit one
// StreamEvent{ToolDelta: …} per delta with the correct fields.
func TestChatStream_ToolCallDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request body carries tools through (commit 2/5
		// requirement: tools must reach the wire when streaming).
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("server: decode body: %v", err)
		}
		if _, ok := body["tools"]; !ok {
			t.Errorf("server: tools field missing from streaming request body")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// First chunk: tool_call slot 0 announced with name + empty args
		chunk1 := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"patch","arguments":""}}]}}]}`
		// Second chunk: first arguments delta
		chunk2 := `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\""}}]}}]}`
		// Third chunk: more args
		chunk3 := `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"orc.md\",\"append\":\"hi\"}"}}]}}]}`
		// Done
		done := `[DONE]`
		for _, c := range []string{chunk1, chunk2, chunk3, done} {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", 0, nil)
	tools := []contract.ToolSpec{{Name: "patch", Description: "write file"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := client.ChatStream(ctx, "test-model", []contract.Message{{Role: "user", Content: "hi"}}, tools)
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
		t.Fatalf("expected 3 tool_call deltas, got %d: %#v", len(deltas), deltas)
	}
	// First delta: function name set, args empty.
	if deltas[0].Name != "patch" || deltas[0].ID != "call_abc" || deltas[0].Index != 0 {
		t.Errorf("delta[0] wrong: %#v", deltas[0])
	}
	// Concatenated arguments from chunks 2 + 3 = the full JSON.
	full := deltas[0].ArgumentsDelta + deltas[1].ArgumentsDelta + deltas[2].ArgumentsDelta
	if !strings.Contains(full, `"path":"orc.md"`) || !strings.Contains(full, `"append":"hi"`) {
		t.Errorf("accumulated args wrong: %q", full)
	}
}

// TestChatStream_TextThenToolCall — backend emits text content first,
// then switches to a tool_call. Both kinds of events must come through.
func TestChatStream_TextThenToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"content":"thinking..."}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"x","function":{"name":"f","arguments":"{}"}}]}}]}`,
			`[DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := client.ChatStream(ctx, "m", []contract.Message{{Role: "user", Content: "x"}}, []contract.ToolSpec{{Name: "f"}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var text strings.Builder
	gotTool := false
	for ev := range ch {
		if ev.Text != "" {
			text.WriteString(ev.Text)
		}
		if ev.ToolDelta != nil {
			gotTool = true
		}
		if ev.Done {
			break
		}
	}
	if text.String() != "thinking..." {
		t.Errorf("text accum wrong: %q", text.String())
	}
	if !gotTool {
		t.Errorf("no tool_call delta event")
	}
}

// TestChatStream_NoToolsRequestBodyOmitsField — backwards compat: when
// tools is nil/empty, the request body must NOT include a `tools` field
// (some strict backends 400 on an empty tools array).
func TestChatStream_NoToolsRequestBodyOmitsField(t *testing.T) {
	var seenTools any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenTools = body["tools"]
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, _ := client.ChatStream(ctx, "m", []contract.Message{{Role: "user", Content: "x"}}, nil)
	for range ch {
	}
	if seenTools != nil {
		t.Errorf("tools field should be omitted when nil, got %v", seenTools)
	}
}

// TestErrorFrameStatus — D3: the in-stream error-frame
// normaliser must recover a status from a numeric code, a quoted-string
// code, or a documented type name, and yield "" otherwise.
func TestErrorFrameStatus(t *testing.T) {
	cases := []struct {
		code string // raw JSON for the `code` field ("" → absent)
		typ  string
		want string
	}{
		{code: `400`, typ: "", want: "400"},
		{code: `"413"`, typ: "", want: "413"},
		{code: ``, typ: "invalid_request_error", want: "400"},
		{code: ``, typ: "request_too_large", want: "413"},
		{code: ``, typ: "rate_limit_error", want: "429"},
		{code: `"model_not_found"`, typ: "", want: ""},
		{code: ``, typ: "", want: ""},
		{code: `42`, typ: "", want: ""},
	}
	for _, c := range cases {
		var raw json.RawMessage
		if c.code != "" {
			raw = json.RawMessage(c.code)
		}
		if got := errorFrameStatus(raw, c.typ); got != c.want {
			t.Errorf("errorFrameStatus(%q, %q) = %q, want %q", c.code, c.typ, got, c.want)
		}
	}
}

// TestChatStream_ErrorFrameCarriesStatusArrow — an in-stream error frame
// with a code must surface as `provider error → <code>: <msg>` so the
// pool's status-keyed classifiers read it like a status-line error (D3).
func TestChatStream_ErrorFrameCarriesStatusArrow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"error":{"message":"maximum context length exceeded","code":400}}`)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := client.ChatStream(ctx, "m", []contract.Message{{Role: "user", Content: "x"}}, nil)
	if err != nil {
		t.Fatalf("ChatStream pre-stream err: %v", err)
	}
	var streamErr error
	for ev := range ch {
		if ev.Err != nil {
			streamErr = ev.Err
		}
	}
	if streamErr == nil {
		t.Fatal("error frame must surface as a stream error")
	}
	if !strings.Contains(streamErr.Error(), " → 400: maximum context length exceeded") {
		t.Errorf("stream error missing the status arrow shape: %q", streamErr.Error())
	}
}

// TestChat_BlockErrorObjectCarriesStatusArrow — a non-streaming 2xx response
// whose body carries an error object (aggregators relay the upstream's 4xx
// this way) must surface as `… → <code>: <msg>` so the pool's prompt-level
// 4xx classifier reads the code from the arrow instead of cooling a healthy
// entry (D3). Covers both provider clients.
func TestChat_BlockErrorObjectCarriesStatusArrow(t *testing.T) {
	t.Run("openai_compat_numeric_code", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"error":{"message":"maximum context length exceeded","code":400}}`)
		}))
		defer srv.Close()

		client := NewClient(srv.URL, "", 0, nil)
		_, err := client.Chat(context.Background(), "m", []contract.Message{{Role: "user", Content: "x"}}, nil)
		if err == nil {
			t.Fatal("a 2xx error-object body must surface as an error")
		}
		if !strings.Contains(err.Error(), " → 400: maximum context length exceeded") {
			t.Errorf("block error missing the status arrow shape: %q", err.Error())
		}
	})

	t.Run("openai_compat_type_only", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"error":{"message":"too long","type":"request_too_large"}}`)
		}))
		defer srv.Close()

		client := NewClient(srv.URL, "", 0, nil)
		_, err := client.Chat(context.Background(), "m", []contract.Message{{Role: "user", Content: "x"}}, nil)
		if err == nil || !strings.Contains(err.Error(), " → 413: too long") {
			t.Errorf("type-only error must map to 413 arrow, got: %v", err)
		}
	})

	t.Run("anthropic_type", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`)
		}))
		defer srv.Close()

		client := NewAnthropicClient(srv.URL, "k", 0, nil)
		_, err := client.Chat(context.Background(), "m", []contract.Message{{Role: "user", Content: "x"}}, nil)
		if err == nil || !strings.Contains(err.Error(), " → 400: prompt is too long") {
			t.Errorf("anthropic block error must carry the 400 arrow, got: %v", err)
		}
	})

	t.Run("unmapped_type_keeps_arrowless_fallback", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"error":{"message":"weird gateway thing","type":"mystery_error"}}`)
		}))
		defer srv.Close()

		client := NewClient(srv.URL, "", 0, nil)
		_, err := client.Chat(context.Background(), "m", []contract.Message{{Role: "user", Content: "x"}}, nil)
		if err == nil || strings.Contains(err.Error(), " → ") {
			t.Errorf("an unmapped type must stay arrowless (entry-health fault), got: %v", err)
		}
		if !strings.Contains(err.Error(), "weird gateway thing") {
			t.Errorf("fallback must still carry the message, got: %v", err)
		}
	})
}

// TestChatStream_ReasoningDeltas — a reasoning-parser backend (vllm) puts the
// <think> block on delta.reasoning, one token per chunk, and only then streams
// delta.content. Both must surface as SEPARATE StreamEvent channels: thinking is
// process, product is content, and folding either into the other corrupts one of
// the two consumers (the trace channel / the reply).
func TestChatStream_ReasoningDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"reasoning":"We"}}]}`,
			`{"choices":[{"delta":{"reasoning":" need"}}]}`,
			// DeepSeek spells the same channel reasoning_content — both decode.
			`{"choices":[{"delta":{"reasoning_content":" Paris."}}]}`,
			`{"choices":[{"delta":{"content":"Paris"}}]}`,
			`[DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := NewClient(srv.URL, "", 0, nil).ChatStream(ctx, "test-model", []contract.Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var think, text string
	for ev := range ch {
		think += ev.Reasoning
		text += ev.Text
		if ev.Done {
			break
		}
	}
	if think != "We need Paris." {
		t.Errorf("reasoning = %q, want %q", think, "We need Paris.")
	}
	if text != "Paris" {
		t.Errorf("content = %q, want %q — thinking must never leak into the content channel", text, "Paris")
	}
}

// TestChat_BlockReasoning — same split on the non-streaming path, where the whole
// think arrives in message.reasoning alongside a normal content field.
func TestChat_BlockReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"\n\nParis","reasoning":"User asks the capital. Paris."}}],"usage":{"prompt_tokens":8,"completion_tokens":9}}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := NewClient(srv.URL, "", 0, nil).Chat(ctx, "test-model", []contract.Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Reasoning != "User asks the capital. Paris." {
		t.Errorf("Reasoning = %q", resp.Reasoning)
	}
	if strings.TrimSpace(resp.Content) != "Paris" {
		t.Errorf("Content = %q, want Paris", resp.Content)
	}
}

// TestChat_BlockThinkingOnlyErrors — on the block path a thinking-only reply must
// FAIL, not return an empty success. Every caller here is a side-LLM that takes the
// answer as data with nobody watching: compress would write the empty string over a
// real slice of history, the rubric judge would fail its gate open. The whitespace
// variant is the one that actually happens — the think ends, "\n\n" is emitted, the
// output cap lands — so it must classify identically.
func TestChat_BlockThinkingOnlyErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"empty content", ""},
		{"whitespace-only content", "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q,"reasoning_content":"thinking, and thinking, and out of budget."}}],"usage":{"prompt_tokens":8,"completion_tokens":900}}`, tc.content)
			}))
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resp, err := NewClient(srv.URL, "", 0, nil).Chat(ctx, "test-model", []contract.Message{{Role: "user", Content: "hi"}}, nil)
			if !errors.Is(err, ErrThinkingOnly) {
				t.Fatalf("err = %v, want ErrThinkingOnly", err)
			}
			// Usage rides out on the error so the pool can still bill the tokens that
			// were really spent — this is the one failure class guaranteed to have a bill.
			if resp.Usage.OutputTokens != 900 {
				t.Errorf("Usage.OutputTokens = %d, want 900 carried on the error path", resp.Usage.OutputTokens)
			}
		})
	}
}
