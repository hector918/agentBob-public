// Package providers talks to model backends.
//
// For now: one streaming OpenAI-compatible chat-completions client — which covers
// self-hosted llama.cpp / Ollama / vLLM / SGLang as well as OpenAI / OpenRouter /
// any custom endpoint — plus a small registry of provider presets (base URL +
// which env var holds the key). Native Anthropic, prompt caching, fallback chains,
// the /v1/models probe, etc. are later slices.
package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agentbob/contract"
)

// streamHardTimeout bounds a single streaming call in case the backend stalls
// forever (cancellation via the caller's ctx still works on top of this).
const streamHardTimeout = 10 * time.Minute

// defaultMaxTokens caps one response's output when a models.yaml entry
// leaves max_output_tokens unset (and no defaults.max_output_tokens).
// Without a cap an openai-compat backend — a
// local llama.cpp model with weak stop behaviour especially — can keep
// generating until streamHardTimeout fires; a sane ceiling bounds it.
const defaultMaxTokens = 8192

// Client is a streaming OpenAI-compatible chat-completions client.
//
// maxTokens is one per-entry output cap (: folded the short-lived
// stream/block split back into a single per-model knob). It's sent as
// max_tokens on both the streaming tool-rounds path and the block path —
// the per-entry watcher guards streaming against runaway content, and a
// per-model cap lets a small-context sidecar (translate / asr) bound its
// own output instead of inheriting a global default.
//
// maxTokens fallback chain: per-entry max_output_tokens → defaults.
// max_output_tokens → defaultMaxTokens (8192). Resolved at construction
// time so per-call paths only read the pre-resolved value.
type Client struct {
	baseURL   string // ".../v1" root, no trailing slash
	apiKey    string
	maxTokens int            // sent as max_tokens on both ChatStream and Chat
	extraBody map[string]any // per-model raw fields merged into the request body
	http      *http.Client
}

// NewClient builds a client for the given OpenAI-compatible base URL (the part
// ending in /v1) and optional API key (sent as a Bearer token if non-empty).
// maxTokens <= 0 falls back to defaultMaxTokens. extraBody (may be nil) is
// merged verbatim into every request body, overriding the client's defaults.
func NewClient(baseURL, apiKey string, maxTokens int, extraBody map[string]any) *Client {
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	return &Client{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:    strings.TrimSpace(apiKey),
		maxTokens: maxTokens,
		extraBody: extraBody,
		http:      &http.Client{}, // no client timeout — the stream is bounded via context
	}
}

// applyExtraBody merges the per-model extra_body passthrough into the request
// body. extra_body wins on key collisions — it is an explicit override hatch,
// so a models.yaml entry can change max_tokens / cache_prompt / etc. The one
// exception is "stream": the client's response handling is hard-wired to the
// path it was called on (SSE parsing vs single-JSON parse), so letting a
// config flip it would break the reader — the key is dropped from the merge
// (full-audit MD-1). Callers still shouldn't put model / messages
// there.
func applyExtraBody(body, extra map[string]any) {
	for k, v := range extra {
		if k == "stream" {
			continue
		}
		body[k] = v
	}
}

// applyExtraHeaders sets provider-specific headers that the generic
// OpenAI-compatible path doesn't otherwise need. Currently only
// OpenRouter's two recommended attribution headers (HTTP-Referer +
// X-Title): optional per their docs, but they improve free-model queue
// priority and surface the app in the OpenRouter dashboard. Gated on
// the base URL so no other backend ever receives them.
func (c *Client) applyExtraHeaders(h http.Header) {
	if strings.Contains(c.baseURL, "openrouter.ai") {
		h.Set("HTTP-Referer", "https://github.com/hector918/agentbob")
		h.Set("X-Title", "agentbob")
	}
}

type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Reasoning is the THINKING channel. Two spellings in the wild for the
			// same thing — vllm emits `reasoning`, DeepSeek `reasoning_content` —
			// so both are decoded and reasoningDelta takes whichever is present.
			Reasoning        string         `json:"reasoning"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []sseToolCallD `json:"tool_calls,omitempty"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details,omitempty"`
	} `json:"usage"`
	Error *struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"` // number or quoted string — gateways differ
	} `json:"error"`
}

// errorFrameStatus recovers a 3-digit HTTP status from an in-stream error
// frame so the wrapped error carries the same " → <code>" shape as the
// block path's non-2xx errors — the pool's status-keyed error classifiers
// (prompt-level 4xx) read the code from that arrow. A gateway that opens
// the stream with 200 and then relays the upstream's 400 (e.g. context
// overflow) as an error frame would otherwise cool every candidate it
// fails over to (D3). code is the frame's raw `code` field;
// typ is the OpenAI/Anthropic-style `type` name, consulted when no
// numeric code is present. Returns "" when neither yields a status.
func errorFrameStatus(code json.RawMessage, typ string) string {
	s := strings.Trim(strings.TrimSpace(string(code)), `"`)
	if len(s) == 3 && s[0] >= '1' && s[0] <= '5' &&
		s[1] >= '0' && s[1] <= '9' && s[2] >= '0' && s[2] <= '9' {
		return s
	}
	return errorFrameTypeStatus[typ]
}

// errorFrameTypeStatus maps the error-frame `type` names both APIs
// document to the HTTP status the same failure carries on the block path.
var errorFrameTypeStatus = map[string]string{
	"invalid_request_error": "400",
	"authentication_error":  "401",
	"permission_error":      "403",
	"not_found_error":       "404",
	"request_too_large":     "413",
	"rate_limit_error":      "429",
	"api_error":             "500",
	"overloaded_error":      "529",
}

// sseToolCallD — one partial tool_call delta in an OpenAI / openai-compat
// SSE stream. The backend emits multiple of these for a single in-flight
// tool_call: typically the FIRST carries Index + ID + Function.Name (and
// often an empty Arguments delta), subsequent ones carry Arguments
// chunks that the client concatenates per Index to reconstruct the
// final arguments JSON.
//
// Some backends omit Index when there's only one tool_call (we default
// to 0 in that case). Some omit ID for early deltas (we forward the
// empty string; downstream accumulator keeps the first non-empty).
//
// `type` is omitted from the struct on purpose: OpenAI always sends
// "function" today and the consumer doesn't branch on it; adding the
// field back is one line if a non-function tool kind ever ships.
type sseToolCallD struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

// ChatStream performs one streaming chat completion. The returned error is for
// pre-stream failures (bad request, non-2xx, …). Once streaming starts, content
// deltas arrive on the channel; the final event has Done=true (and Usage if the
// backend reports it, or Err if the stream broke or was cancelled), and the
// channel is then closed.
func (c *Client) ChatStream(ctx context.Context, model string, messages []contract.Message, tools []contract.ToolSpec) (<-chan contract.StreamEvent, error) {
	if model == "" {
		return nil, fmt.Errorf("no model specified")
	}
	reqMap := map[string]any{
		"model":          model,
		"messages":       toWireSlice(messages),
		"max_tokens":     c.maxTokens, // per-entry output cap — without it a runaway model generates until the deadline
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true}, // ignored by backends that don't support it
		// cache_prompt: llama.cpp extension. Tells the server to keep
		// this request's KV cache around past the request so future
		// requests with a matching prefix can reuse it (slot prefill
		// skipped). Required for agentic loops where iters 2..N share
		// the iter-1 prefix (system prompt + early history).
		// Other OpenAI-compatible backends (OpenAI proper, vLLM,
		// sglang, ollama) ignore unknown fields. No-op on Anthropic
		// (different client). Safe to send unconditionally.
		"cache_prompt": true,
	}
	//: tool rounds now go through the streaming path via
	// ChatStreamWatch, so a non-empty tools slice must reach the wire.
	// Mirrors the Chat() body shape — toolsToWire constructs the same
	// `[{type:"function", function:{name,description,parameters}}]`
	// envelope OpenAI / vLLM / llama.cpp expect.
	if w := toolsToWire(tools); len(w) > 0 {
		reqMap["tools"] = w
	}
	applyExtraBody(reqMap, c.extraBody)
	reqBody, err := json.Marshal(reqMap)
	if err != nil {
		return nil, err
	}

	streamCtx, streamCancel := context.WithTimeout(ctx, streamHardTimeout)

	url := c.baseURL + "/chat/completions"
	start := time.Now()
	pair := LogRequest(ctx, "openai-compat", url, model, reqBody)
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		LogResponse(pair, "openai-compat", url, model, 0, time.Since(start), nil, err)
		streamCancel()
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	c.applyExtraHeaders(httpReq.Header)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		LogResponse(pair, "openai-compat", url, model, 0, time.Since(start), nil, err)
		streamCancel()
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		// C14 fix: drain any remaining body bytes past the
		// 1MB cap so the underlying connection can be returned to the
		// idle pool for reuse. Without this an oversized error body
		// would leak the conn.
		_, _ = io.Copy(io.Discard, resp.Body)
		LogResponse(pair, "openai-compat", url, model, resp.StatusCode, time.Since(start), raw,
			fmt.Errorf("http %s", resp.Status))
		resp.Body.Close()
		streamCancel()
		return nil, fmt.Errorf("POST %s → %s: %s", url, resp.Status, snippet(raw))
	}

	out := make(chan contract.StreamEvent, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		defer streamCancel()

		var usage contract.Usage
		var accum strings.Builder
		var streamErr error
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(line[len("data:"):])
			if data == "[DONE]" {
				break
			}
			var ch sseChunk
			if json.Unmarshal([]byte(data), &ch) != nil {
				continue // skip a malformed line rather than aborting the whole stream
			}
			if ch.Error != nil {
				// An explicit error frame means the provider declared the
				// stream failed. Always surface a non-empty error so the pool
				// fails over and records the entry as errored — never fall
				// through to the success envelope below, which would bank a
				// fake success (resetting the entry's consecutive-fail count).
				// A proxy / shim can emit `{"error":{}}` or an empty message;
				// fall back to a generic message in that case. When the frame
				// carries a code/type, normalise it into the block path's
				// " → <status>" shape so the pool classifies in-stream 4xx
				// the same way (D3). Mirrors the Anthropic "error" event
				// fallback in 30-anthropic.go.
				code := errorFrameStatus(ch.Error.Code, ch.Error.Type)
				msg := ch.Error.Message
				if msg == "" {
					msg = "(empty message)"
				}
				if code != "" {
					streamErr = fmt.Errorf("provider error → %s: %s", code, msg)
				} else if ch.Error.Message != "" {
					streamErr = fmt.Errorf("provider error: %s", msg)
				} else {
					streamErr = fmt.Errorf("provider error (empty message)")
				}
				// If the error frame ALSO carried completion content (some shims pack
				// the final tail + a soft error into one frame), flush that text before
				// the error so already-produced output isn't silently dropped.
				for _, cc := range ch.Choices {
					if cc.Delta.Content != "" {
						accum.WriteString(cc.Delta.Content)
						out <- contract.StreamEvent{Text: cc.Delta.Content}
					}
				}
				out <- contract.StreamEvent{Done: true, Err: streamErr}
				logStreamResponse(pair, "openai-compat", url, model, resp.StatusCode, start, accum.String(), usage, streamErr)
				return
			}
			if ch.Usage != nil {
				usage = contract.Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
				if ch.Usage.PromptTokensDetails != nil {
					usage.CacheReadInputTokens = ch.Usage.PromptTokensDetails.CachedTokens
				}
			}
			for _, cc := range ch.Choices {
				// Thinking first: it precedes the answer on every backend that
				// emits it. Deliberately NOT written to accum — accum is the
				// CONTENT the request log records as the reply, and folding
				// process into product there would corrupt every downstream
				// reader of that log.
				if r := cc.Delta.Reasoning; r != "" {
					out <- contract.StreamEvent{Reasoning: r}
				} else if r := cc.Delta.ReasoningContent; r != "" {
					out <- contract.StreamEvent{Reasoning: r}
				}
				if cc.Delta.Content != "" {
					accum.WriteString(cc.Delta.Content)
					out <- contract.StreamEvent{Text: cc.Delta.Content}
				}
				// Emit a ToolCallDelta event per partial tool_call slot
				// the backend reported. Watchers can inspect the running
				// arguments string and ctx.Cancel early if the model's
				// going off the rails (e.g. blowing past a sane arg-size
				// threshold mid-stream — see model.ChatStreamWatch's
				// toolArgsBloatWatcher). Index defaults to 0 when the
				// backend omits it (single tool_call case).
				for _, td := range cc.Delta.ToolCalls {
					idx := 0
					if td.Index != nil {
						idx = *td.Index
					}
					out <- contract.StreamEvent{ToolDelta: &contract.ToolCallDelta{
						Index:          idx,
						ID:             td.ID,
						Name:           td.Function.Name,
						ArgumentsDelta: td.Function.Arguments,
					}}
				}
			}
		}
		if err := sc.Err(); err != nil {
			streamErr = fmt.Errorf("reading stream from %s: %w", url, err)
			out <- contract.StreamEvent{Done: true, Err: streamErr}
			logStreamResponse(pair, "openai-compat", url, model, resp.StatusCode, start, accum.String(), usage, streamErr)
			return
		}
		out <- contract.StreamEvent{Done: true, Usage: usage}
		logStreamResponse(pair, "openai-compat", url, model, resp.StatusCode, start, accum.String(), usage, nil)
	}()
	return out, nil
}

// logStreamResponse emits the res record for a streaming call: a tiny
// summary JSON (stream:true, accumulated output_text, usage, error)
// rather than the raw SSE bytes which would balloon the log without
// helping cache analysis.
func logStreamResponse(pair int64, provider, url, model string, status int, start time.Time, outputText string, usage contract.Usage, err error) {
	summary := map[string]any{
		"stream":      true,
		"output_text": outputText,
		"usage": map[string]int{
			"input_tokens":            usage.InputTokens,
			"output_tokens":           usage.OutputTokens,
			"cache_read_input_tokens": usage.CacheReadInputTokens,
		},
	}
	body, _ := json.Marshal(summary)
	LogResponse(pair, provider, url, model, status, time.Since(start), body, err)
}

// isTransientConnErr reports whether err looks like a connection-level
// failure (EOF / connection reset / broken pipe / "connection closed").
//
// LABEL-ONLY: as of C13 retries are routed through
// isPreSendErr — once the server has seen our bytes we no longer retry
// (would risk double-billing + duplicate side-effects). This predicate
// is now used solely to wrap the surfaced error with a clearer message
// ("server connection broke; likely OOM / KV-cache exhausted") so the
// admin grepping logs has a hint about where to look (model server-side
// vs network), not to gate retry behaviour. Context cancel / deadline
// are still excluded so those propagate as-is.
func isTransientConnErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "eof") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection closed")
}

// isPreSendErr reports whether err happened BEFORE any request bytes
// hit the server (DNS resolve, dial refused, dial timeout, no route).
// C13 fix: only this class is safe to retry. Once the
// server saw our bytes the model may have started generating; a
// retry would risk double-billing AND duplicate side-effects (e.g.
// tools the model fired in attempt 1 vs attempt 2). EOF / connection
// reset mid-stream are NOT pre-send — they happened after the server
// accepted the request.
func isPreSendErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "no route to host") ||
		strings.Contains(s, "network is unreachable") ||
		strings.Contains(s, "tls: handshake") ||
		(strings.Contains(s, "dial tcp") && strings.Contains(s, "timeout"))
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "(empty body)"
	}
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

// chatCompletion is the non-streaming OpenAI /chat/completions response shape.
type chatCompletion struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			// Reasoning is the THINKING channel — same two spellings as the
			// streaming delta (vllm `reasoning`, DeepSeek `reasoning_content`).
			Reasoning        string         `json:"reasoning"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []wireToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details,omitempty"`
	} `json:"usage"`
	Error *struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"` // number or quoted string — gateways differ
	} `json:"error"`
}

// Chat performs one non-streaming chat completion. Used by the agent's tool
// rounds — the full JSON response is parsed once, so tool_calls come out
// cleanly without partial-SSE assembly. The final user-visible turn uses
// ChatStream instead.
func (c *Client) Chat(ctx context.Context, model string, messages []contract.Message, tools []contract.ToolSpec) (contract.ChatResponse, error) {
	if model == "" {
		return contract.ChatResponse{}, fmt.Errorf("no model specified")
	}
	body := map[string]any{
		"model":      model,
		"messages":   toWireSlice(messages),
		"max_tokens": c.maxTokens, // per-entry output cap (block path) — same per-model knob as streaming
		// cache_prompt: llama.cpp KV-cache retention hint. See
		// ChatStream above for the full rationale. Safe for all
		// OpenAI-compatible backends (ignored when unknown).
		"cache_prompt": true,
	}
	if w := toolsToWire(tools); len(w) > 0 {
		body["tools"] = w
	}
	applyExtraBody(body, c.extraBody)
	reqBody, err := json.Marshal(body)
	if err != nil {
		return contract.ChatResponse{}, err
	}

	// Bound a single non-streaming call so a wedged backend can't pin the
	// caller indefinitely (the caller's ctx still cancels on top of this).
	// Mirrors ChatStream's streamCtx — this path previously had no cap.
	callCtx, callCancel := context.WithTimeout(ctx, streamHardTimeout)
	defer callCancel()

	url := c.baseURL + "/chat/completions"
	start := time.Now()
	pair := LogRequest(ctx, "openai-compat", url, model, reqBody)
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		LogResponse(pair, "openai-compat", url, model, 0, time.Since(start), nil, err)
		return contract.ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	c.applyExtraHeaders(httpReq.Header)

	resp, err := c.http.Do(httpReq)
	// C13 fix: retry ONLY on pre-send errors (DNS,
	// dial refused, dial timeout, no route). Once the server has seen
	// our bytes, a transient mid-stream EOF could mean the model
	// already started generating — retrying would risk double-billing
	// AND duplicate model side-effects. Surface mid-stream errors
	// unmodified; the caller decides whether to escalate.
	//
	// Also: log the first attempt's failure under its own pair_id
	// BEFORE the retry, then mint a fresh pair_id for the retry so
	// the log file holds one clean (req, res) pair per attempt instead
	// of one req paired with two res records.
	if err != nil && isPreSendErr(err) {
		LogResponse(pair, "openai-compat", url, model, 0, time.Since(start), nil, err)
		retryStart := time.Now()
		retryPair := LogRequest(ctx, "openai-compat", url, model, reqBody)
		retryReq, rerr := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(reqBody))
		if rerr == nil {
			retryReq.Header = httpReq.Header.Clone()
			resp, err = c.http.Do(retryReq)
		} else {
			err = rerr
		}
		// Reassign pair + start so subsequent LogResponse calls land
		// under the retry attempt's accounting.
		pair = retryPair
		start = retryStart
	}
	if err != nil {
		LogResponse(pair, "openai-compat", url, model, 0, time.Since(start), nil, err)
		if isTransientConnErr(err) {
			return contract.ChatResponse{}, fmt.Errorf("POST %s: server connection broke (likely OOM / KV-cache exhausted on this prompt; check model server logs): %w", url, err)
		}
		return contract.ChatResponse{}, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	// Read the response body in full so we can log it AND still
	// json.Unmarshal below. 4 MB cap matches what we'd accept anyway.
	respBytes, rdErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if rdErr != nil {
		LogResponse(pair, "openai-compat", url, model, resp.StatusCode, time.Since(start), respBytes, rdErr)
		return contract.ChatResponse{}, fmt.Errorf("read response body: %w", rdErr)
	}
	// R4-11: drain any bytes past the 4MB cap so the
	// underlying connection can be returned to the idle pool for reuse.
	// C14 fixed the streaming variants but missed this non-streaming
	// path — without this an oversized error body would leak the conn.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		LogResponse(pair, "openai-compat", url, model, resp.StatusCode, time.Since(start), respBytes,
			fmt.Errorf("http %s", resp.Status))
		return contract.ChatResponse{}, fmt.Errorf("POST %s → %s: %s", url, resp.Status, snippet(respBytes))
	}
	LogResponse(pair, "openai-compat", url, model, resp.StatusCode, time.Since(start), respBytes, nil)

	var cc chatCompletion
	if err := json.Unmarshal(respBytes, &cc); err != nil {
		return contract.ChatResponse{}, fmt.Errorf("decode chat response: %w", err)
	}
	if cc.Error != nil {
		// A 2xx body carrying an error object (aggregators relay the
		// upstream's 4xx this way instead of an HTTP status). Normalise it
		// into the same " → <status>" shape the non-2xx path and the
		// in-stream error frame use, so the pool's prompt-level 4xx
		// classifier reads the code from the arrow rather than cooling a
		// healthy entry over a prompt-level rejection (D3).
		// Guard on the error OBJECT, not a non-empty message: a body with a
		// code/type but empty message (`{"error":{"type":"invalid_request_error"}}`)
		// must still take this branch — mirror the Anthropic non-streaming path
		// and surface "(empty message)" so it never falls through to the
		// arrow-less "no choices" string (which the 4xx classifier can't read).
		msg := cc.Error.Message
		if msg == "" {
			msg = "(empty message)"
		}
		if code := errorFrameStatus(cc.Error.Code, cc.Error.Type); code != "" {
			return contract.ChatResponse{}, fmt.Errorf("provider error → %s: %s", code, msg)
		}
		return contract.ChatResponse{}, fmt.Errorf("provider error: %s", msg)
	}
	if len(cc.Choices) == 0 {
		return contract.ChatResponse{}, fmt.Errorf("no choices in chat response")
	}
	choice := cc.Choices[0].Message
	reasoning := choice.Reasoning
	if reasoning == "" {
		reasoning = choice.ReasoningContent
	}
	out := contract.ChatResponse{Content: choice.Content, Reasoning: reasoning}
	for _, tc := range choice.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, contract.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	// Some models / server templates don't promote inline-text
	// <tool_call>…</tool_call> blocks (Hermes / Qwen XML and JSON
	// forms) to the structured tool_calls field. Without this
	// salvage step the dispatcher would treat the visible markup as
	// the model's final reply text and answer with the tags shown
	// to the user. Only runs when the structured field is empty AND
	// the content actually contains the marker — never overrides
	// real tool_calls.
	if len(out.ToolCalls) == 0 {
		if cleaned, inline := ExtractInlineToolCalls(out.Content); len(inline) > 0 {
			out.Content = cleaned
			out.ToolCalls = inline
		}
	}
	// After the recovery attempt, if unrecoverable tool-call markup still
	// sits in the content (the sidecar `<|tool_call>` form), this entry
	// produced unstructurable output — the orchestrator must never see wire
	// tokens. No usable tool_call → fail the call so the pool fails over.
	// A real tool_call WAS produced alongside the junk → keep it, strip the
	// residual. (Block path; the streaming path catches the same via
	// ToolCallMarkupWatcher.)
	if HasResidualToolCallMarkup(out.Content) {
		if len(out.ToolCalls) == 0 {
			return contract.ChatResponse{}, ErrMalformedToolCallMarkup
		}
		out.Content = StripResidualToolCallMarkup(out.Content)
	}
	// Ordinary `<tool_call>` markup the recovery couldn't structure (truncated
	// mid-body, or a body that's neither JSON nor the XML function form). With
	// no usable tool_call, answering this back would leak the wire token to the
	// user — fail the call so the pool fails over, same posture as the sidecar
	// case above. When a real tool_call WAS produced alongside it, ExtractInline-
	// ToolCalls was skipped (structured field already non-empty), so a leftover
	// block / dangling open survives — strip it rather than leak it (B8),
	// mirroring the sidecar StripResidualToolCallMarkup branch.
	if HasResidualPlainToolCallMarkup(out.Content) {
		if len(out.ToolCalls) == 0 {
			return contract.ChatResponse{}, ErrMalformedToolCallMarkup
		}
		out.Content = StripResidualPlainToolCallMarkup(out.Content)
	}
	if cc.Usage != nil {
		out.Usage = contract.Usage{InputTokens: cc.Usage.PromptTokens, OutputTokens: cc.Usage.CompletionTokens}
		if cc.Usage.PromptTokensDetails != nil {
			out.Usage.CacheReadInputTokens = cc.Usage.PromptTokensDetails.CachedTokens
		}
	}
	// Block-path twin of the stream path's guard (leaf/model/45-chat-stream-watch.go):
	// tokens billed, visible channel empty. Now that the thinking channel is
	// decoded the two causes are DISTINGUISHABLE — say which one it was, because
	// they have different fixes: thinking-only wants enable_thinking:false (or a
	// bigger output cap) on THIS entry, an empty-everything reply wants a look at
	// the backend. Side-LLM callers (salvage, compress, judge) ride this path and
	// degrade just as quietly. See there for the full rationale.
	if strings.TrimSpace(out.Content) == "" && len(out.ToolCalls) == 0 && (out.Usage.OutputTokens > 0 || out.Reasoning != "") {
		if out.Reasoning != "" {
			slog.Warn("model: model only ever thought — visible content empty, whole reply went to the thinking channel; failing over (disable thinking on this entry or raise its output cap)",
				"entry", entryNameFromCtx(ctx), "model", model,
				"output_tokens", out.Usage.OutputTokens, "reasoning_bytes", len(out.Reasoning), "path", "block")
			// FAIL, don't hand back an empty success. Every caller on this path is a
			// side-LLM that treats the answer as data and has no user watching: compress
			// (leaf/turn/90-compact.go) would write the empty string over a real slice of
			// history, the rubric judge would fail its gate open, salvage would give up.
			// All of them degrade in silence. Returning the sentinel routes the request
			// through the pool's ordinary failover to a candidate that answers instead.
			// Usage is carried on the error path by the pool, not dropped.
			return out, ErrThinkingOnly
		}
		slog.Warn("model: tokens generated but no visible content and no thinking either — truncated or empty reply",
			"entry", entryNameFromCtx(ctx), "model", model,
			"output_tokens", out.Usage.OutputTokens, "path", "block")
	}
	return out, nil
}

// Ping is a cheap REACHABILITY check: a GET on {baseURL}/models (the
// OpenAI-compatible model-list endpoint — no tokens consumed). Used by
// MultiPool's heartbeat to notice a dead entry recovering.
//
// Reachability ≠ /models being implemented. A non-2xx reply (404 from a
// shim that proxies /chat/completions but not /models, 401 from a gateway
// that gates /models behind a different scope, etc.) still PROVES the host
// answered — the backend is reachable. Only a transport-level failure
// (connection refused, DNS, dial timeout, TLS) means still-unreachable.
// We must not gate recovery on a 2xx here: a backend whose completions are
// healthy but whose /models returns 4xx would otherwise stay dead forever,
// since the cooldown-expiry path is structurally unreachable while the
// heartbeat keeps re-arming deadUntil (B2).
//
// 5xx is the one ambiguous case (the server is up but erroring); we still
// treat it as reachable because Ping success only re-admits the entry
// TENTATIVELY — one real completions failure re-deads it immediately, so an
// over-permissive Ping cannot keep a genuinely-broken backend live.
//
// Auth rides the same path as a chat call — the Authorization: Bearer
// header, if an API key is configured.
func (c *Client) Ping(ctx context.Context) error {
	url := c.baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	c.applyExtraHeaders(req.Header)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	// Any HTTP status = the host answered = reachable. Do not gate on 2xx.
	return nil
}

var _ contract.Chatter = (*Client)(nil)
