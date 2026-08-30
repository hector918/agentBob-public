package modelgate

import (
	"encoding/json"

	"agentbob/contract"
)

// This file is the OpenAI ⇄ contract wire translation — the only place the
// OpenAI chat-completions JSON shape is known. The handlers speak contract
// (ModelRequest / Message / ToolSpec / ChatResponse); everything OpenAI-shaped
// lives here.

// chatRequest is the subset of the OpenAI /v1/chat/completions body modelgate
// honors. Unlisted fields (temperature, top_p, n, tool_choice, response_format,
// logprobs, …) are accepted-and-ignored — modelgate forwards only
// kind/requires/prefer/model/messages/stream/tools to the pool (the same list
// GET /v1/key advertises as honoredParams).
//
// Kind / Requires / Prefer are bob EXTENSIONS (not OpenAI fields) carrying the
// routing address — and they are the picker's OWN vocabulary, spelled the same way
// an internal caller spells it (contract.ModelRequest{Kind, Requires, Prefer}).
// That is deliberate: an external request routes through the exact same mechanism
// as bob's own turns — hard Requires, the admin-declared tag-fallback chain, and a
// loud failure when the chain runs dry — rather than a softened variant invented at
// this edge. An OpenAI SDK sends them via extra_body. The rest of the wire
// (endpoints, message shape, response shape, SSE framing) stays byte-for-byte
// OpenAI-compatible.
type chatRequest struct {
	Model    string        `json:"model"`
	Kind     string        `json:"kind"`
	Requires []string      `json:"requires"`
	Prefer   []string      `json:"prefer"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Tools    []oaiTool     `json:"tools"`
}

// chatMessage is one inbound OpenAI message. Content is RawMessage because OpenAI
// allows a string OR an array of content parts; contentString normalizes both.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaiToolCall   `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

type oaiTool struct {
	Type     string      `json:"type"`
	Function oaiToolFunc `json:"function"`
}

type oaiToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiToolCallFunc `json:"function"`
}

type oaiToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// contentString normalizes an OpenAI message content (string, or array of
// {type,text} parts) into plain text. Non-text parts (image_url etc.) are dropped
// — v1 is text-only (multimodal input is a backlog item). "" on null/parse-fail.
func contentString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	out := ""
	for _, p := range parts {
		if p.Type == "text" || p.Text != "" {
			out += p.Text
		}
	}
	return out
}

// toPoolMessages maps inbound OpenAI messages to contract.Messages. An assistant
// message with tool_calls carries them into Message.ToolCalls; a tool-role message
// carries its tool_call_id + result content.
func toPoolMessages(in []chatMessage) []contract.Message {
	out := make([]contract.Message, 0, len(in))
	for _, m := range in {
		msg := contract.Message{
			Role:       m.Role,
			Content:    contentString(m.Content),
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, contract.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		out = append(out, msg)
	}
	return out
}

// toPoolTools maps OpenAI tool definitions to contract.ToolSpecs (function tools
// only; a non-function tool type is skipped).
func toPoolTools(in []oaiTool) []contract.ToolSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]contract.ToolSpec, 0, len(in))
	for _, t := range in {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		out = append(out, contract.ToolSpec{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return out
}

// toOpenAIToolCalls maps contract tool calls to the OpenAI response shape.
func toOpenAIToolCalls(in []contract.ToolCall) []oaiToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]oaiToolCall, 0, len(in))
	for _, tc := range in {
		out = append(out, oaiToolCall{
			ID:       tc.ID,
			Type:     "function",
			Function: oaiToolCallFunc{Name: tc.Name, Arguments: tc.Arguments},
		})
	}
	return out
}

// --- response shapes ------------------------------------------------------

type chatCompletion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   completionUsage    `json:"usage"`
}

type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type completionMessage struct {
	Role      string        `json:"role"`
	Content   string        `json:"content"`
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
}

type completionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// --- streaming chunk shapes ----------------------------------------------

type chatChunk struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []chunkChoice    `json:"choices"`
	Usage   *completionUsage `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role      string             `json:"role,omitempty"`
	Content   string             `json:"content,omitempty"`
	ToolCalls []oaiToolCallDelta `json:"tool_calls,omitempty"`
}

// oaiToolCallDelta is a streaming tool-call fragment (carries Index so a client
// can reassemble parallel calls).
type oaiToolCallDelta struct {
	Index    int             `json:"index"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Function oaiToolCallFunc `json:"function"`
}

// usageOf maps a pool Usage to the OpenAI usage block.
func usageOf(u contract.Usage) completionUsage {
	return completionUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
}

// finishReasonFor is "tool_calls" when the model wants tools, else "stop".
func finishReasonFor(resp contract.ChatResponse) string {
	if len(resp.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}
