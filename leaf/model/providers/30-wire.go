package providers

import (
	"encoding/base64"
	"encoding/json"

	"agentbob/contract"
)

// wireMessage is the OpenAI-compatible chat-completions wire shape for one
// message. It is the wire form of contract.Message — produced by toWire just
// before request marshaling, never escapes this package. Keeping the wire
// shape here (instead of on contract.Message) decouples the contract envelope
// from provider quirks: the day we add a native Anthropic backend, its wire
// types live alongside this one without touching contract.
//
// Content is `any` so the same field can serialize either as a plain
// string (the common case) or as an array of content blocks (multimodal:
// text + image_url, etc.).
type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`      // string OR []wireContentBlock for multimodal
	Name       string         `json:"name,omitempty"`         // role=tool: function name (some backends require it)
	ToolCallID string         `json:"tool_call_id,omitempty"` // role=tool: the assistant tool_call this answers
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`   // role=assistant: calls the model wants to make
}

// wireContentBlock is one element of the array-form content: type="text",
// type="image_url", or type="audio_url". NOTE (S-15): audio_url (a data-URI) is
// what whisper-style ASR shims accept — OpenAI PROPER expects type="input_audio"
// with {data,format}, so whether audio is understood depends on the configured
// KindASR backend, not a universal OpenAI-compat guarantee.
type wireContentBlock struct {
	Type     string             `json:"type"`
	Text     string             `json:"text,omitempty"`
	ImageURL *wireImageURLBlock `json:"image_url,omitempty"`
	AudioURL *wireAudioURLBlock `json:"audio_url,omitempty"`
}

type wireImageURLBlock struct {
	URL string `json:"url"`
}

type wireAudioURLBlock struct {
	URL string `json:"url"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string (not a parsed object) — OpenAI's quirk
}

// toWire translates a contract.Message to its OpenAI-compatible wire form.
//
//   - user/system → role + content
//   - assistant (no tool calls)            → role + content
//   - assistant with ToolCalls             → role + content (kept, even "") + tool_calls
//   - tool                                  → role + content + tool_call_id + name
func toWire(m contract.Message) wireMessage {
	wm := wireMessage{Role: m.Role}
	switch m.Role {
	case "tool":
		wm.Content = m.Content
		wm.ToolCallID = m.ToolCallID
		wm.Name = m.ToolName
	case "assistant":
		if len(m.ToolCalls) > 0 {
			// content must be PRESENT even when tool_calls is set: some
			// strict providers reject an omitted/null content with
			// "content must be a string or an array of content". m.Content
			// is a string, so assigning it (even "") makes wm.Content a
			// non-nil interface — omitempty keeps it and it serializes as
			// "content":"" rather than dropping the field.
			wm.Content = m.Content
			wm.ToolCalls = make([]wireToolCall, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				wm.ToolCalls[i] = wireToolCall{
					ID:       tc.ID,
					Type:     "function",
					Function: wireToolFunction{Name: tc.Name, Arguments: tc.Arguments},
				}
			}
		} else {
			wm.Content = m.Content
		}
	default: // user, system, anything unknown — pass through as plain content
		// Multimodal: when the message carries image or audio refs,
		// switch to the content-array form (OpenAI's text + image_url /
		// audio_url block layout).
		if len(m.Images) > 0 || len(m.Audio) > 0 {
			blocks := []wireContentBlock{}
			if m.Content != "" {
				blocks = append(blocks, wireContentBlock{Type: "text", Text: m.Content})
			}
			for _, img := range m.Images {
				mime := img.MIME
				if mime == "" {
					mime = "image/png"
				}
				url := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
				blocks = append(blocks, wireContentBlock{
					Type:     "image_url",
					ImageURL: &wireImageURLBlock{URL: url},
				})
			}
			for _, a := range m.Audio {
				mime := a.MIME
				if mime == "" {
					mime = "audio/ogg"
				}
				url := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(a.Data)
				blocks = append(blocks, wireContentBlock{
					Type:     "audio_url",
					AudioURL: &wireAudioURLBlock{URL: url},
				})
			}
			wm.Content = blocks
		} else {
			wm.Content = m.Content
		}
	}
	return wm
}

// toWireSlice translates a whole conversation slice.
func toWireSlice(msgs []contract.Message) []wireMessage {
	out := make([]wireMessage, len(msgs))
	for i, m := range msgs {
		out[i] = toWire(m)
	}
	return out
}

// wireTool is the OpenAI tools[] entry: type=function + a function spec.
type wireTool struct {
	Type     string       `json:"type"` // always "function"
	Function wireToolFunc `json:"function"`
}

type wireToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// toolsToWire translates the agent's ToolSpec list into the OpenAI wire shape.
func toolsToWire(tools []contract.ToolSpec) []wireTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]wireTool, len(tools))
	for i, t := range tools {
		out[i] = wireTool{
			Type: "function",
			Function: wireToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		}
	}
	return out
}
