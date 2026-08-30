package providers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"agentbob/contract"
)

// ErrMalformedToolCallMarkup signals that the model emitted tool-call
// markup as TEXT that ExtractInlineToolCalls could NOT recover into
// structured tool_calls — specifically the sidecar broken-template form
// `<|tool_call>call:NAME{…}<tool_call|>` with `<|"|>` pseudo-quotes (a
// different token family than the recoverable `<tool_call>` JSON / XML
// variants).
//
// It is ENTRY-LEVEL, not conversation-level: the markup is an artifact of
// one backend's chat template, so a different pool entry won't produce it.
// The pool treats it like a backend failure — record it against the entry
// (consecutiveFails → marked dead at the threshold) and fail over to the
// next candidate. Detection lives here in the provider layer so the
// orchestrator never sees provider wire tokens.
var ErrMalformedToolCallMarkup = errors.New("model output: unrecoverable tool-call markup leaked as text")

// ErrThinkingOnly signals that the model spent its whole output budget in the
// THINKING channel and never produced a visible answer — billed output tokens,
// non-empty reasoning, no content that survives a trim, no tool call.
//
// It lives here (rather than in the pool) because BOTH provider paths raise it:
// the streaming one via leaf/model/45-chat-stream-watch.go, the block one in
// chatOnce. Returning it rather than an empty success is what stops the two
// silent degradations it used to cause — the round kernel looping forever on an
// unchanged history, and a side-LLM caller (compress especially) taking the empty
// string for an answer and writing it over real history.
//
// Classification, for the pool's benefit: this is a per-ENTRY BEHAVIOUR fault, not
// a prompt-level one (a peer with thinking off answers the same prompt fine, so
// failing over is productive) and not an entry-HEALTH one (the backend streamed
// tokens; it is plainly alive). leaf/model deliberately keeps it off both the
// cooling breaker and the kind-exhausted admin page.
var ErrThinkingOnly = errors.New("model produced thinking but no visible content")

// HasResidualToolCallMarkup reports whether s still carries raw tool-call
// markup tokens AFTER ExtractInlineToolCalls has had its chance to recover
// the well-formed variants — i.e. the sidecar `<|tool_call>` / `<tool_call|>`
// form the extractor can't structure. Single source of truth for "this
// output is malformed tool-call text", shared by the block client (post-
// parse) and the streaming watcher (mid-stream).
func HasResidualToolCallMarkup(s string) bool {
	return strings.Contains(s, "<|tool_call") || strings.Contains(s, "<tool_call|>")
}

// HasResidualPlainToolCallMarkup reports whether s still carries an ordinary
// `<tool_call>` open token AFTER ExtractInlineToolCalls failed to recover a
// structured call from it. This happens when the block was truncated mid-body
// (`<tool_call>{"name":"f","argu` — no `</tool_call>` close so the block regex
// never matched) or when the body was neither JSON nor the XML function form
// (parseToolCallInner returned false). In both cases the visible markup would
// otherwise be answered back to the user verbatim. Callers invoke this only
// when no usable tool_call was produced; a successful extraction strips the
// `<tool_call>` markup, so a recovered response won't trip it. Distinct from
// HasResidualToolCallMarkup, which targets the sidecar `<|tool_call>` family.
func HasResidualPlainToolCallMarkup(s string) bool {
	return strings.Contains(s, "<tool_call>")
}

// sidecarBlockRe matches a complete sidecar tool-call block
// `<|tool_call>…<tool_call|>` (the close token is distinctive, so non-greedy
// `.*?` to it is safe even with nested braces in the body).
var sidecarBlockRe = regexp.MustCompile(`(?s)<\|tool_call>.*?<tool_call\|>`)

// StripResidualToolCallMarkup removes the sidecar `<|tool_call>…<tool_call|>`
// markup from s — complete blocks first, then a dangling unterminated
// `<|tool_call` open (truncated output) to the end. Used ONLY when a real
// tool_call was ALSO produced (a different sidecar quirk): keep the usable
// call, just clean the junk out of the surrounding text so it isn't streamed
// or persisted. When NO usable call exists the caller fails over instead
// (HasResidualToolCallMarkup + ErrMalformedToolCallMarkup).
func StripResidualToolCallMarkup(s string) string {
	s = sidecarBlockRe.ReplaceAllString(s, "")
	if i := strings.Index(s, "<|tool_call"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// StripResidualPlainToolCallMarkup is the ordinary-`<tool_call>` twin of
// StripResidualToolCallMarkup: it removes complete `<tool_call>…</tool_call>`
// blocks, then truncates at a dangling unterminated `<tool_call>` open
// (truncated output) to the end. Used ONLY when a real tool_call was ALSO
// produced — ExtractInlineToolCalls is skipped in that case (structured field
// already non-empty), so a leftover complete block or a truncated trailing
// `<tool_call>{…` open would otherwise leak the wire markup into the visible
// content (B8). When NO usable call exists the caller fails over instead
// (HasResidualPlainToolCallMarkup + ErrMalformedToolCallMarkup).
func StripResidualPlainToolCallMarkup(s string) string {
	s = toolCallBlockRe.ReplaceAllString(s, "")
	if i := strings.Index(s, "<tool_call>"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// ExtractInlineToolCalls finds <tool_call>…</tool_call> blocks the
// model emitted as plain text content (Qwen / Hermes / some Mistral
// fine-tunes do this when the OpenAI-server-side template doesn't
// promote them to the structured `tool_calls` field).
//
// Two formats are handled:
//
//  1. JSON inside:
//     <tool_call>{"name": "X", "arguments": {"k": "v"}}</tool_call>
//     (Hermes 2 / Qwen 2.5 native chatml-tools.)
//
//  2. XML function/parameter:
//     <tool_call><function=X><parameter=k>v</parameter></function></tool_call>
//     (Some Anthropic-style fine-tunes; the bug-report sample.)
//
// Returns the cleaned content (markup stripped) and the synthesised
// tool calls. The caller is responsible for invoking this only when the
// structured tool_calls field is empty, so there's no double-firing.
//
// Generated call IDs are 8-hex; the dispatcher's uniqueness check
// (dedupeToolCalls) is unaffected.
func ExtractInlineToolCalls(content string) (cleaned string, calls []contract.ToolCall) {
	if !strings.Contains(content, "<tool_call>") {
		return content, nil
	}
	loc := toolCallBlockRe.FindAllStringSubmatchIndex(content, -1)
	if len(loc) == 0 {
		return content, nil
	}

	var b strings.Builder
	cursor := 0
	for _, m := range loc {
		// m[0]/m[1] = whole match; m[2]/m[3] = inside-block group
		b.WriteString(content[cursor:m[0]])
		inner := strings.TrimSpace(content[m[2]:m[3]])
		if tc, ok := parseToolCallInner(inner); ok {
			calls = append(calls, tc)
		}
		cursor = m[1]
	}
	b.WriteString(content[cursor:])
	cleaned = strings.TrimSpace(b.String())
	return cleaned, calls
}

// toolCallBlockRe matches a single <tool_call>…</tool_call> block,
// non-greedy so multiple back-to-back blocks each get matched
// individually. (?s) makes . span newlines (the inner body usually has them).
var toolCallBlockRe = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>`)

// fnXMLRe + paramXMLRe parse format 2 (XML function/parameter form).
var (
	fnXMLRe    = regexp.MustCompile(`<function=([A-Za-z0-9_.-]+)>`)
	paramXMLRe = regexp.MustCompile(`(?s)<parameter=([A-Za-z0-9_.-]+)>(.*?)</parameter>`)
)

// parseToolCallInner tries format-1 (JSON) first, then format-2 (XML).
// Returns ok=false on unrecognised body so the caller can drop it.
func parseToolCallInner(inner string) (contract.ToolCall, bool) {
	// Try JSON: { "name": "X", "arguments": ... }
	if strings.HasPrefix(inner, "{") {
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(inner), &p); err == nil && p.Name != "" {
			args := string(p.Arguments)
			if args == "" {
				args = "{}"
			}
			return contract.ToolCall{
				ID:        newCallID(),
				Name:      p.Name,
				Arguments: args,
			}, true
		}
	}
	// Try XML: <function=X><parameter=k>v</parameter></function>
	if m := fnXMLRe.FindStringSubmatch(inner); m != nil {
		name := m[1]
		argMap := map[string]string{}
		for _, pm := range paramXMLRe.FindAllStringSubmatch(inner, -1) {
			argMap[pm[1]] = strings.TrimSpace(pm[2])
		}
		args, _ := json.Marshal(argMap)
		return contract.ToolCall{
			ID:        newCallID(),
			Name:      name,
			Arguments: string(args),
		}, true
	}
	return contract.ToolCall{}, false
}

// newCallID generates a synthetic tool_call id (8 hex chars) for
// inline-extracted calls. The model didn't supply one, but the
// dispatcher needs ids to pair calls with results.
func newCallID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "inline_" + hex.EncodeToString(b[:])
}
