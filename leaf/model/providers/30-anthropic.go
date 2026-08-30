package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agentbob/contract"
)

// AnthropicClient talks to Anthropic's native Messages API (POST /messages).
// Distinct from openaichat.Client because Anthropic's wire shape is materially
// different: system goes to a top-level field, every assistant/user turn is a
// list of typed content blocks, tool_use carries parsed JSON (not a string),
// and the SSE stream is a multi-event protocol (message_start /
// content_block_delta / message_delta / message_stop).
//
// max_tokens and extra_body are taken from the models.yaml entry (defaulting to
// anthropicMaxTokens / none) — same per-entry knobs the OpenAI-compat client honors.
type AnthropicClient struct {
	baseURL   string // ".../v1" root, no trailing slash
	apiKey    string
	maxTokens int            // per-response cap (max_tokens); from the entry, default anthropicMaxTokens
	extraBody map[string]any // per-entry raw fields merged into the request body
	http      *http.Client
}

// NewAnthropicClient builds a client for Anthropic's Messages API. baseURL is
// the v1 root (default https://api.anthropic.com/v1). apiKey is sent as the
// x-api-key header. maxTokens <= 0 falls back to anthropicMaxTokens; extraBody
// (may be nil) is the models.yaml extra_body passthrough.
func NewAnthropicClient(baseURL, apiKey string, maxTokens int, extraBody map[string]any) *AnthropicClient {
	if maxTokens <= 0 {
		maxTokens = anthropicMaxTokens
	}
	return &AnthropicClient{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:    strings.TrimSpace(apiKey),
		maxTokens: maxTokens,
		extraBody: extraBody,
		http:      &http.Client{},
	}
}

// anthropicMaxTokens caps a single response. Anthropic *requires* this field
// on every request — it isn't a target, just an upper bound. 32000 fits Opus
// 4.x (32K cap) and Sonnet 4.x (64K cap, we don't need the full ceiling).
// Haiku 3.5 caps at 8192 server-side and silently clamps oversize requests —
// no error. C23 (was 8192).
//
// It is the DEFAULT when a models.yaml entry sets no max_output_tokens; an entry
// may override it (NewAnthropicClient.maxTokens). Note Anthropic's max_tokens is
// a generation ceiling, not a token budget — tightening it truncates rather than
// budgets — so it doubles as the cost cap an operator sets per entry.
const anthropicMaxTokens = 32000

// anthropicVersion is the API version header. Pinning it makes the wire shape
// reproducible across server-side rollouts.
const anthropicVersion = "2023-06-01"

// anthropicBlock is one element of an assistant/user content array on the wire.
// Only the fields relevant to a given Type are emitted (omitempty everywhere).
type anthropicBlock struct {
	Type         string                `json:"type"`
	Text         string                `json:"text,omitempty"`          // type=text
	ID           string                `json:"id,omitempty"`            // type=tool_use
	Name         string                `json:"name,omitempty"`          // type=tool_use
	Input        json.RawMessage       `json:"input,omitempty"`         // type=tool_use — parsed JSON object, NOT a string
	ToolUseID    string                `json:"tool_use_id,omitempty"`   // type=tool_result
	Content      string                `json:"content,omitempty"`       // type=tool_result (string form)
	IsError      bool                  `json:"is_error,omitempty"`      // type=tool_result — true when content is an error envelope (C20 fix)
	Source       *anthropicImageSource `json:"source,omitempty"`        // type=image
	CacheControl *anthropicCacheCtrl   `json:"cache_control,omitempty"` // C22 — prompt cache marker
}

// anthropicCacheCtrl is the prompt-cache breakpoint marker. Putting it on a
// content block tells the server "cache everything up to and including this
// block". TTL is 5 minutes (ephemeral). At most 4 markers per request.
// C22.
type anthropicCacheCtrl struct {
	Type string `json:"type"` // always "ephemeral"
}

// anthropicEphemeral is the singleton marker — no per-call state, reusable.
var anthropicEphemeral = &anthropicCacheCtrl{Type: "ephemeral"}

// anthropicImageSource is the source block of an image content block.
// Anthropic's API accepts type="base64" (inline data) or type="url" (remote);
// we use base64 since images here originate from local browser screenshots.
type anthropicImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/png" / "image/jpeg" / ...
	Data      string `json:"data"`       // base64-encoded payload
}

// anthropicMessage is one entry in the request's messages[] array.
type anthropicMessage struct {
	Role    string           `json:"role"` // "user" | "assistant"
	Content []anthropicBlock `json:"content"`
}

// anthropicTool is one entry in the request's tools[] array. Anthropic uses a
// flat shape (name / description / input_schema) — no OpenAI-style function
// wrapper.
type anthropicTool struct {
	Name         string              `json:"name"`
	Description  string              `json:"description,omitempty"`
	InputSchema  json.RawMessage     `json:"input_schema,omitempty"`
	CacheControl *anthropicCacheCtrl `json:"cache_control,omitempty"` // C22 — set on last tool to cache the whole tools array
}

// anthropicSystemBlock is the array-form system block (Anthropic accepts
// `system: "string"` OR `system: [{type:"text", text:"..."}]`). Array form
// is required to attach cache_control. C22.
type anthropicSystemBlock struct {
	Type         string              `json:"type"` // "text"
	Text         string              `json:"text"`
	CacheControl *anthropicCacheCtrl `json:"cache_control,omitempty"`
}

// toAnthropicWire translates a conversation into (system, messages) for the
// Anthropic API. Rules:
//
//   - All role=system messages are concatenated (newline-separated) into one
//     top-level system string. Anthropic does not accept system as a message
//     role.
//   - role=tool becomes a role=user message carrying a tool_result block (the
//     tool_use_id pairs it with the assistant's tool_use call).
//   - role=assistant with ToolCalls becomes one assistant message holding a
//     text block (if Content != "") followed by one tool_use block per call.
//   - Plain user/assistant turns become a single text block.
//
// Adjacent same-role turns are NOT merged here — Anthropic accepts them.
func toAnthropicWire(msgs []contract.Message) (string, []anthropicMessage) {
	var systemParts []string
	var out []anthropicMessage

	for _, m := range msgs {
		switch m.Role {
		case "system":
			if s := strings.TrimSpace(m.Content); s != "" {
				systemParts = append(systemParts, s)
			}
		case "tool":
			// C20 fix: detect D1 error envelopes ({"ok":false,...})
			// and flag the tool_result block with is_error=true. Without this
			// Anthropic treats every tool result as success, which makes the
			// model less likely to recover from failed tool calls.
			out = append(out, anthropicMessage{
				Role: "user",
				Content: []anthropicBlock{{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
					Content:   m.Content,
					IsError:   isErrorEnvelope(m.Content),
				}},
			})
		case "assistant":
			var blocks []anthropicBlock
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				// Arguments is a JSON-encoded *string* in contract.ToolCall (OpenAI's
				// quirk); Anthropic expects the parsed object. Parse it; if it's
				// invalid, fall back to an empty object so the request still
				// validates (the tool will surface the parse error at dispatch).
				input := json.RawMessage(tc.Arguments)
				if !json.Valid(input) {
					input = json.RawMessage(`{}`)
				}
				blocks = append(blocks, anthropicBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: input,
				})
			}
			// C2 fix: when an assistant message has no text and
			// no tool_use blocks, skip the message entirely rather than
			// injecting an empty text block — Anthropic's API rejects an
			// empty text block with HTTP 400 "text content blocks must
			// contain non-whitespace text". Tool_use-only assistant turns
			// are valid; a fully empty assistant turn is dropped here (the
			// loop's next iteration will repair if needed).
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
		default: // user, anything unknown
			blocks := []anthropicBlock{}
			if m.Content != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, img := range m.Images {
				mime := img.MIME
				if mime == "" {
					mime = "image/png"
				}
				blocks = append(blocks, anthropicBlock{
					Type: "image",
					Source: &anthropicImageSource{
						Type:      "base64",
						MediaType: mime,
						Data:      base64.StdEncoding.EncodeToString(img.Data),
					},
				})
			}
			// R4-3 fix: symmetric to the C2 assistant fix —
			// when a user message has no content and no images, skip it
			// entirely rather than injecting an empty text block.
			// Anthropic rejects empty user text blocks (HTTP 400), and
			// worse: C22's cache_control marker would attach to this
			// empty block, poisoning the cache.
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropicMessage{Role: "user", Content: blocks})
		}
	}
	// C22: mark the LAST block of the LAST message with a
	// cache_control breakpoint. The server caches everything up to and
	// including this block; the next iteration's request — which appends
	// new content after this prefix — will read it back instead of paying
	// full input price. The marker on THIS request "creates" the entry
	// (1.25x input price for the new portion). Net win after 1 reuse.
	// One of at most 4 breakpoints; combined with system + tools = 3.
	if len(out) > 0 {
		last := &out[len(out)-1]
		if n := len(last.Content); n > 0 {
			last.Content[n-1].CacheControl = anthropicEphemeral
		}
	}
	return strings.Join(systemParts, "\n\n"), out
}

// toolBlockMeta stashes a content_block_start's tool_use id+name so
// the subsequent input_json_delta events (which only carry the block
// index) can be tagged with both fields for the watcher accumulator.
type toolBlockMeta struct {
	ID   string
	Name string
}

// toAnthropicTools translates contract.ToolSpec to Anthropic's flat tools[] shape.
// C22: the last tool gets a cache_control marker so the entire
// tools array is cached together as one block.
func toAnthropicTools(tools []contract.ToolSpec) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, len(tools))
	for i, t := range tools {
		out[i] = anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		}
	}
	out[len(out)-1].CacheControl = anthropicEphemeral
	return out
}

// toAnthropicSystem builds the array-form system field with a cache_control
// marker on the (single) text block. Returns nil when system is empty so the
// caller can omit the field entirely. C22.
func toAnthropicSystem(system string) []anthropicSystemBlock {
	if system == "" {
		return nil
	}
	return []anthropicSystemBlock{{
		Type:         "text",
		Text:         system,
		CacheControl: anthropicEphemeral,
	}}
}

// anthropicMessagesResp is the non-streaming /messages response shape (only
// the fields we read).
type anthropicMessagesResp struct {
	Content []anthropicBlock `json:"content"`
	Usage   struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"` // C22 — fresh cache write (1.25x input)
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`     // C22 — cache hit (0.1x input)
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Chat performs one non-streaming Messages call. Used by the agent's tool
// rounds — tool_use blocks come out cleanly.
func (c *AnthropicClient) Chat(ctx context.Context, model string, messages []contract.Message, tools []contract.ToolSpec) (contract.ChatResponse, error) {
	if model == "" {
		return contract.ChatResponse{}, fmt.Errorf("no model specified")
	}
	system, wireMsgs := toAnthropicWire(messages)
	body := map[string]any{
		"model":      model,
		"messages":   wireMsgs,
		"max_tokens": c.maxTokens,
	}
	if sb := toAnthropicSystem(system); len(sb) > 0 {
		body["system"] = sb
	}
	if w := toAnthropicTools(tools); len(w) > 0 {
		body["tools"] = w
	}
	applyExtraBody(body, c.extraBody)
	reqBody, err := json.Marshal(body)
	if err != nil {
		return contract.ChatResponse{}, err
	}

	// Bound a single non-streaming call so a wedged backend can't pin the
	// caller indefinitely. Mirrors ChatStream's streamCtx.
	callCtx, callCancel := context.WithTimeout(ctx, streamHardTimeout)
	defer callCancel()

	url := c.baseURL + "/messages"
	start := time.Now()
	pair := LogRequest(ctx, "anthropic", url, model, reqBody)
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		LogResponse(pair, "anthropic", url, model, 0, time.Since(start), nil, err)
		return contract.ChatResponse{}, err
	}
	c.setHeaders(httpReq, false)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		LogResponse(pair, "anthropic", url, model, 0, time.Since(start), nil, err)
		return contract.ChatResponse{}, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBytes, rdErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if rdErr != nil {
		LogResponse(pair, "anthropic", url, model, resp.StatusCode, time.Since(start), respBytes, rdErr)
		return contract.ChatResponse{}, fmt.Errorf("read response body: %w", rdErr)
	}
	// Drain any bytes past the 4MB cap so the underlying connection can
	// be returned to the idle pool for reuse (mirrors the openai-compat
	// sibling in 00-client.go). Without this an oversized error body
	// (verbose 5xx HTML, huge tool_use echo) would leak the conn.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		LogResponse(pair, "anthropic", url, model, resp.StatusCode, time.Since(start), respBytes,
			fmt.Errorf("http %s", resp.Status))
		return contract.ChatResponse{}, fmt.Errorf("POST %s → %s: %s", url, resp.Status, snippet(respBytes))
	}
	LogResponse(pair, "anthropic", url, model, resp.StatusCode, time.Since(start), respBytes, nil)

	var r anthropicMessagesResp
	if err := json.Unmarshal(respBytes, &r); err != nil {
		return contract.ChatResponse{}, fmt.Errorf("decode messages response: %w", err)
	}
	if r.Error != nil {
		// A 2xx body carrying an error object — normalise the type into the
		// " → <status>" shape the non-2xx path and the in-stream error event
		// use, so the pool's prompt-level 4xx classifier reads the code from
		// the arrow rather than cooling a healthy entry (D3). A
		// known type with an empty message must still be surfaced (never fall
		// through to the success path and bank an empty response as a hit).
		msg := r.Error.Message
		if code := errorFrameStatus(nil, r.Error.Type); code != "" {
			if msg == "" {
				msg = "(empty message)"
			}
			return contract.ChatResponse{}, fmt.Errorf("anthropic error → %s: %s", code, msg)
		}
		if msg != "" {
			return contract.ChatResponse{}, fmt.Errorf("anthropic error: %s", msg)
		}
		return contract.ChatResponse{}, fmt.Errorf("anthropic error (empty error object, type %q)", r.Error.Type)
	}

	out := contract.ChatResponse{
		Usage: contract.Usage{
			// Anthropic's input_tokens EXCLUDES the cache counters, but
			// contract.Usage documents CacheCreation/CacheRead as counted INSIDE
			// InputTokens (the OpenAI-compat provider's prompt_tokens already does).
			// Fold them in here so billing/telemetry don't undercount and hit_pct
			// can't exceed 100% (B30). Keep the counters as the breakdown fields.
			InputTokens:              r.Usage.InputTokens + r.Usage.CacheCreationInputTokens + r.Usage.CacheReadInputTokens,
			OutputTokens:             r.Usage.OutputTokens,
			CacheCreationInputTokens: r.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     r.Usage.CacheReadInputTokens,
		},
	}
	var textParts []string
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			// Anthropic sends input as a parsed JSON object; contract.ToolCall holds
			// the JSON-encoded string (matching OpenAI's quirk). Marshal back.
			argsStr := string(b.Input)
			if argsStr == "" || !json.Valid(b.Input) {
				argsStr = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, contract.ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: argsStr,
			})
		}
	}
	out.Content = strings.Join(textParts, "")
	return out, nil
}

// anthropicSSE is one parsed SSE event. event is "message_start" /
// "content_block_start" / "content_block_delta" / "content_block_stop" /
// "message_delta" / "message_stop" / "ping" / "error". data is the JSON payload.
type anthropicSSE struct {
	event string
	data  []byte
}

// ChatStream performs one streaming Messages call. Used for the final user-
// visible reply AND, since, for tool-call rounds via
// ChatStreamWatch. Anthropic's wire format streams tool_use as a
// content_block_start (carrying id + name) followed by zero or more
// content_block_delta events with type=input_json_delta carrying
// arguments JSON chunks; this loop emits a StreamEvent{ToolDelta:…}
// per chunk so the pool's watcher can inspect mid-stream.
func (c *AnthropicClient) ChatStream(ctx context.Context, model string, messages []contract.Message, tools []contract.ToolSpec) (<-chan contract.StreamEvent, error) {
	if model == "" {
		return nil, fmt.Errorf("no model specified")
	}
	system, wireMsgs := toAnthropicWire(messages)
	body := map[string]any{
		"model":      model,
		"messages":   wireMsgs,
		"max_tokens": c.maxTokens,
		"stream":     true,
	}
	if sb := toAnthropicSystem(system); len(sb) > 0 {
		body["system"] = sb
	}
	//: tool rounds now flow through ChatStream via the
	// pool's ChatStreamWatch wrapper; advertise tools when the caller
	// supplies them. Same toAnthropicTools helper Chat() uses.
	if w := toAnthropicTools(tools); len(w) > 0 {
		body["tools"] = w
	}
	applyExtraBody(body, c.extraBody)
	reqBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	streamCtx, streamCancel := context.WithTimeout(ctx, streamHardTimeout)

	url := c.baseURL + "/messages"
	start := time.Now()
	pair := LogRequest(ctx, "anthropic", url, model, reqBody)
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		LogResponse(pair, "anthropic", url, model, 0, time.Since(start), nil, err)
		streamCancel()
		return nil, err
	}
	c.setHeaders(httpReq, true)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		LogResponse(pair, "anthropic", url, model, 0, time.Since(start), nil, err)
		streamCancel()
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		// C14 fix: drain any remaining body bytes past the
		// 1MB cap so the underlying connection can be returned to the
		// idle pool for reuse.
		_, _ = io.Copy(io.Discard, resp.Body)
		LogResponse(pair, "anthropic", url, model, resp.StatusCode, time.Since(start), raw,
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

		// Track per-block tool_use metadata so input_json_delta events
		// can be tagged with the matching Index / ID / Name. Map key is
		// the content_block index Anthropic assigns.
		toolBlocks := make(map[int]toolBlockMeta)

		var curEvent string
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event:") {
				curEvent = strings.TrimSpace(line[len("event:"):])
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(line[len("data:"):])
			if data == "" {
				continue
			}
			switch curEvent {
			case "content_block_start":
				var d struct {
					Index        int `json:"index"`
					ContentBlock struct {
						Type string `json:"type"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content_block"`
				}
				if json.Unmarshal([]byte(data), &d) != nil {
					continue
				}
				if d.ContentBlock.Type == "tool_use" {
					toolBlocks[d.Index] = toolBlockMeta{ID: d.ContentBlock.ID, Name: d.ContentBlock.Name}
					// Emit a "tool_call announced" delta — Name + ID set,
					// args empty. Subsequent input_json_delta events fill in.
					out <- contract.StreamEvent{ToolDelta: &contract.ToolCallDelta{
						Index: d.Index,
						ID:    d.ContentBlock.ID,
						Name:  d.ContentBlock.Name,
					}}
				}
			case "content_block_delta":
				var d struct {
					Index int `json:"index"`
					Delta struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(data), &d) != nil {
					continue
				}
				if d.Delta.Type == "text_delta" && d.Delta.Text != "" {
					accum.WriteString(d.Delta.Text)
					out <- contract.StreamEvent{Text: d.Delta.Text}
				}
				if d.Delta.Type == "input_json_delta" && d.Delta.PartialJSON != "" {
					meta := toolBlocks[d.Index]
					out <- contract.StreamEvent{ToolDelta: &contract.ToolCallDelta{
						Index:          d.Index,
						ID:             meta.ID,
						Name:           meta.Name,
						ArgumentsDelta: d.Delta.PartialJSON,
					}}
				}
			case "message_start":
				var d struct {
					Message struct {
						Usage struct {
							InputTokens              int `json:"input_tokens"`
							OutputTokens             int `json:"output_tokens"`
							CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
							CacheReadInputTokens     int `json:"cache_read_input_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				// C15 fix: only set InputTokens at message_start.
				// Anthropic frequently reports OutputTokens=1 here as a
				// placeholder; the real count arrives in message_delta. If
				// the stream is cut off before message_delta lands, leaving
				// OutputTokens=0 is more honest than reporting "1" as final.
				//
				// C22: cache_* counters arrive only at message_start
				// (message_delta does not refresh them).
				if json.Unmarshal([]byte(data), &d) == nil {
					// Fold the cache counters into InputTokens to match the
					// contract.Usage doc (cache_* counted INSIDE InputTokens) —
					// Anthropic's input_tokens excludes them (B30).
					usage.InputTokens = d.Message.Usage.InputTokens + d.Message.Usage.CacheCreationInputTokens + d.Message.Usage.CacheReadInputTokens
					usage.CacheCreationInputTokens = d.Message.Usage.CacheCreationInputTokens
					usage.CacheReadInputTokens = d.Message.Usage.CacheReadInputTokens
				}
			case "message_delta":
				var d struct {
					Usage struct {
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				}
				if json.Unmarshal([]byte(data), &d) == nil && d.Usage.OutputTokens > 0 {
					usage.OutputTokens = d.Usage.OutputTokens
				}
			case "error":
				// Reaching this event means the provider explicitly declared
				// the stream failed. Always surface a non-empty error so the
				// pool fails over and records the entry as errored — never
				// fall through, which would end the loop on the success
				// envelope below and bank a fake success (resetting the
				// entry's consecutive-fail count). A proxy / OpenAI-compat
				// shim fronting Anthropic can emit an error frame with a
				// missing message or malformed body; fall back to a generic
				// message in that case.
				// When the event names a documented error type, normalise it
				// into the block path's " → <status>" shape so the pool
				// classifies in-stream 4xx the same way it classifies the
				// status line (D3 — e.g. invalid_request_error /
				// request_too_large are prompt-level, not entry health).
				var d struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal([]byte(data), &d) == nil && (d.Error.Message != "" || d.Error.Type != "") {
					msg := d.Error.Message
					if code := errorFrameStatus(nil, d.Error.Type); code != "" {
						if msg == "" {
							msg = "(empty message)"
						}
						streamErr = fmt.Errorf("anthropic error → %s: %s", code, msg)
					} else if msg != "" {
						streamErr = fmt.Errorf("anthropic error: %s", msg)
					} else {
						streamErr = fmt.Errorf("anthropic error (empty error object, type %q)", d.Error.Type)
					}
				} else {
					streamErr = fmt.Errorf("anthropic stream error (unparseable error event: %q)", data)
				}
				out <- contract.StreamEvent{Done: true, Err: streamErr}
				logStreamResponse(pair, "anthropic", url, model, resp.StatusCode, start, accum.String(), usage, streamErr)
				return
			case "message_stop":
				// handled by loop exit
			}
		}
		if err := sc.Err(); err != nil {
			streamErr = fmt.Errorf("reading stream from %s: %w", url, err)
			out <- contract.StreamEvent{Done: true, Err: streamErr}
			logStreamResponse(pair, "anthropic", url, model, resp.StatusCode, start, accum.String(), usage, streamErr)
			return
		}
		out <- contract.StreamEvent{Done: true, Usage: usage}
		logStreamResponse(pair, "anthropic", url, model, resp.StatusCode, start, accum.String(), usage, nil)
	}()
	return out, nil
}

// setHeaders writes the auth + content + accept headers. anthropic uses
// x-api-key (not Bearer) and a pinned anthropic-version header.
func (c *AnthropicClient) setHeaders(req *http.Request, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
	req.Header.Set("anthropic-version", anthropicVersion)
}

// ListModels hits GET /v1/models and returns the available model ids. Used by
// probeAnthropic (40-probe.go) to validate the model id the user configured
// against what the account can actually call. Best-effort: a short timeout, a
// silent failure (return nil, err — caller logs and continues).
func (c *AnthropicClient) ListModels(ctx context.Context) ([]string, error) {
	url := c.baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, false)

	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req = req.WithContext(cctx)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		// Drain past the cap so the conn returns to the idle pool
		// instead of being discarded on Close (mirrors 00-client.go).
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("GET %s → %s: %s", url, resp.Status, snippet(raw))
	}
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(r.Data))
	for _, m := range r.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// isErrorEnvelope reports whether s is a D1 tool-result envelope with
// ok=false. Mirrors prompt.IsErrEnvelope but kept local here so the
// providers package doesn't gain a cross-package import for one
// 4-line helper. C20 fix.
func isErrorEnvelope(s string) bool {
	if s == "" {
		return false
	}
	var env struct {
		OK *bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return false
	}
	return env.OK != nil && !*env.OK
}

// Ping is a cheap REACHABILITY check: a GET on {baseURL}/models — the
// Anthropic model-list endpoint, no tokens consumed. Used by MultiPool's
// heartbeat to notice a dead entry recovering.
//
// Like the openai-compat Ping (B2): a non-2xx reply still
// proves the host answered, so we treat ANY HTTP response as reachable and
// only count a transport-level failure (the http.Client.Do error) as
// still-unreachable. Ping success re-admits the entry TENTATIVELY, so an
// over-permissive probe cannot keep a broken backend live — one real
// completions failure re-deads it. Auth rides the same setHeaders path as
// a chat call — the x-api-key header, if an API key is configured.
func (c *AnthropicClient) Ping(ctx context.Context) error {
	url := c.baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req, false)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	// Any HTTP status = the host answered = reachable. Do not gate on 2xx.
	return nil
}

var _ contract.Chatter = (*AnthropicClient)(nil)
