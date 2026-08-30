package core

import "encoding/json"

// ToolSpec advertises a tool to the model: its name, a description for the
// model to read, and a JSON Schema describing its parameters. The handler
// and dispatch logic live at the agent-loop layer (core.Tool); the model
// backend only needs the schema to encode in the chat request.
//
// NoAutoCompress opts the tool out of design §1.8 tool-result auto-compression
// (the dispatcher's "if result > N tokens, route through the small model to
// summarize"). Tools that must return verbatim content — e.g. read_file,
// patch — set this to true. Default false: compress when oversized.
type ToolSpec struct {
	Name           string
	Description    string
	Parameters     json.RawMessage // JSON Schema for the tool's arguments
	NoAutoCompress bool
	// SelectionHint, when non-nil, contributes one scenario to the
	// assembled "工具按场景选" rubric (prompt's ToolPolicySource). The hint
	// is authored ON the tool but rendered into a single shared rubric
	// built from only the tools VISIBLE this turn — so a worker without
	// this tool never sees its scenario. nil = the tool contributes no
	// selection scenario (most tools). See docs/prompt.md §4.3.
	SelectionHint *SelectionHint
}

// SelectionHint is a tool's self-declared "when to reach for me" scenario,
// collected per-turn from the visible tool set into the prompt's tool-
// selection rubric. Write it POSITIVELY — what this tool does and the
// keyword cues that should trigger it — never naming or disparaging other
// tools (arbitration emerges from keyword match + Priority ordering, not
// from one tool badmouthing another).
type SelectionHint struct {
	When     string // 触发场景 + 关键词线索（模型面向，中文）
	Then     string // 该用这个工具做什么（自指，正面）
	Priority int    // 排序键：小=在 rubric 里排前、先被检查
}

// ToolCall is one tool invocation the model emitted in an assistant turn.
// Arguments is the raw JSON arguments string as the model produced it (the
// dispatcher validates it against the tool's JSON Schema before running).
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON-encoded arguments, as emitted by the model
}

// Message is one entry in an LLM conversation. The fields cover four shapes:
//
//	role=user/system/assistant (no tools): Role + Content
//	role=assistant with tool calls:        Role + Content (may be "") + ToolCalls
//	role=tool (one tool's result):         Role + Content + ToolCallID + ToolName
//
// No json tags: this is a Go-native value. Wire-format translation to the
// OpenAI/Anthropic shape lives in model/providers (toWire).
// Marshaling Message directly would emit Go field names — almost certainly not
// what a backend expects.
type Message struct {
	Role       string     // "system" | "user" | "assistant" | "tool"
	Content    string     // text body; "" is allowed for assistant-with-ToolCalls
	ToolCallID string     // role=tool: the tool_call this message is the result for
	ToolName   string     // role=tool: the tool function's name (some backends require it)
	ToolCalls  []ToolCall // role=assistant: the tool calls the model wants to make
	// Images attaches one or more image refs to the message. When non-nil
	// the provider wire-layer switches the message body to the array
	// content-block form (OpenAI image_url block / Anthropic image block).
	// nil = plain text message, the existing simple shape. Used by
	// browser_vision and other future vision-aware tools.
	Images []ImageRef
	// Audio attaches one or more audio refs to the message — parallel to
	// Images. When non-nil the provider wire-layer switches the message
	// body to the array content-block form (OpenAI audio_url block).
	// Used by the gateway's pre-turn voice/audio transcription path: an
	// ASR-tagged model takes the audio and returns the transcript text.
	Audio []AudioRef
	// RowKind is the A5 in-place compaction marker class
	// (docs/loop-core-spec.md §6.9): "" or "msg" = ordinary row;
	// "summary" = a compaction summary (role=system, model-visible,
	// the replay start); "commit" = the structural commit mark written
	// AFTER the recent tail was re-appended (filtered from replay; its
	// presence is what makes the preceding summary the live replay
	// boundary — an uncommitted summary is ignored). Written only via
	// SessionStore.AppendCompactionBatch.
	RowKind string
}

// Message RowKind values (see the field doc above).
const (
	RowKindMsg     = "msg"
	RowKindSummary = "summary"
	RowKindCommit  = "commit"
)

// ImageRef is one image attached to a Message. Data is the raw bytes
// (PNG / JPEG); MIME is the media type ("image/png" / "image/jpeg").
// Providers base64-encode at marshal time. Keep this small — chat
// history is replayed on every turn; a giant inlined image would
// inflate every subsequent call's prompt.
type ImageRef struct {
	Data []byte
	MIME string
}

// AudioRef is one audio clip attached to a Message — same shape as
// ImageRef. Data is the raw bytes; MIME is the media type ("audio/ogg",
// "audio/mpeg", …). Providers base64-encode at marshal time into an
// audio_url data URI.
type AudioRef struct {
	Data []byte
	MIME string
}

// Usage holds token accounting for a model call (zero if the backend doesn't report it).
//
// CacheCreation/CacheReadInputTokens are Anthropic-specific prompt-cache
// counters (C22). CacheReadInputTokens — input tokens served
// from a previously-created cache entry (priced ~10% of fresh). CacheCreation
// — input tokens used to create a fresh cache entry on this request
// (priced ~125% of fresh). Both are counted INSIDE the InputTokens total
// already; they break it down. Always zero for providers that don't
// OpenAI-compat backends (llama.cpp / vLLM / OpenRouter / OpenAI) report
// reused cache via `prompt_tokens_details.cached_tokens` (OpenAI 2024-10
// spec); model/providers/00-client.go parses that and fills
// CacheReadInputTokens too (CacheCreationInputTokens stays 0 — those
// backends don't separate fresh-write from cache-hit billing).
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// StreamEvent is one item from a streaming model call.
//   - Text       — a content delta (may be empty on the final event)
//   - ToolDelta  — when non-nil, this event is a partial tool_call
//     update (incremental arguments from a streaming
//     backend that supports tool-call deltas). Mutually
//     exclusive in practice with non-empty Text but the
//     shape allows both for backends that interleave.
//   - Done       — true on the last event (channel is closed right after)
//   - Usage      — populated on the final event if the backend reports it
//   - Model      — populated on the final event with the pool-entry name
//     that served this call (so the agent can record "which
//     model answered" in the session row).
//   - Err        — non-nil if the stream failed; Done is also true in that case
type StreamEvent struct {
	Text      string
	ToolDelta *ToolCallDelta
	Done      bool
	Usage     Usage
	Model     string
	Err       error
}

// ToolCallDelta is one partial update to a single in-progress tool_call
// emitted by a streaming backend that supports tool-call deltas
// (OpenAI / openai-compat: tool_calls[i].function.arguments chunks;
// Anthropic native: input_json_delta events). Backends that don't
// stream tool calls (some llama.cpp configs buffer them internally and
// emit only the complete object) never produce ToolCallDelta events;
// the watcher infrastructure handles both shapes gracefully.
//
// Index identifies which tool_call this delta belongs to in the
// parallel-calls array the model is building (0-based). Name appears
// once on the first delta for a given Index (function name). ID
// appears once when the backend assigns one (some backends omit until
// the call completes). ArgumentsDelta is a partial JSON chunk that
// accumulates over events into the final arguments value — the
// accumulator (see model.ChatStreamWatch) concatenates these per Index
// to reconstruct the complete tool_call.
type ToolCallDelta struct {
	Index          int
	ID             string
	Name           string
	ArgumentsDelta string
}

// StreamAccumulator is the running state ChatStreamWatch hands to its
// watcher on each StreamEvent. The watcher can inspect ToolCalls
// (incrementally rebuilt from ToolDelta events) + accumulated Text +
// the live event itself, and return a non-nil error to cancel the
// stream early.
//
// Implementations live in the model package; this type stays in core
// only because StreamWatcher's signature mentions it.
type StreamAccumulator struct {
	Text      string     // concatenated Text deltas so far
	ToolCalls []ToolCall // tool_calls reconstructed from ToolDelta events
}

// StreamWatcher is the per-event hook ChatStreamWatch invokes. Return
// a non-nil error to abort the stream — the error propagates back out
// of ChatStreamWatch wrapped; ctx is cancelled to stop the backend.
// Return nil to let the stream continue.
//
// Watcher should be cheap — runs on every event in the read goroutine.
type StreamWatcher func(ev StreamEvent, accum *StreamAccumulator) error

// ChatResponse is the result of one non-streaming model call. Either Content
// is non-empty (the model gave a text reply), or ToolCalls is non-empty (the
// model wants to call tools), or both (text plus tool calls — rare but valid).
// Model is the pool-entry name that served this request (set by Pool, not by
// the underlying Chatter).
type ChatResponse struct {
	Content   string
	ToolCalls []ToolCall
	Usage     Usage
	Model     string
}
