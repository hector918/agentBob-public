package contract

import (
	"encoding/json"
	"errors"
)

// ErrToolArgsBloat is the cross-module signal that a tool round was aborted because
// the model packed too much into a single tool_call's arguments (heading for a
// max_tokens truncation cascade). The model pool's stream watcher detects it and
// wraps this sentinel; the turn core errors.Is-checks it to run the §6.8 chunk-retry
// ladder (nudge to write smaller → after repeated trips, strip tools to force a
// plain-text reply) instead of surfacing a generic model-unavailable error.
var ErrToolArgsBloat = errors.New("model output: tool_call arguments exceeded the bloat threshold")

// ToolSpec advertises a tool to the model: its name, a description for the model
// to read, and a JSON Schema describing its parameters. The handler + dispatch
// logic live at the agent-loop layer; the model backend only needs the schema.
//
// NoAutoCompress opts the tool out of tool-result auto-compression (the
// dispatcher's "if result > N tokens, route through the small model to
// summarize"). Tools that must return verbatim content (read_file, patch) set
// it true. Default false: compress when oversized.
type ToolSpec struct {
	Name           string
	Description    string
	Parameters     json.RawMessage // JSON Schema for the tool's arguments
	NoAutoCompress bool
	// Delivers marks a production/delivery tool (writes a file, sends a message,
	// changes the world) — the turn's progress predicate (spec §6.4) counts its
	// success as PRODUCTION (a trustworthy "done" signal), not just progress.
	// Default false: a read/query tool only ever marks progress (new info).
	Delivers bool
	// SideEffect marks a tool whose FAILURE the model commonly papers over —
	// it performs a verifiable external action (send a message, deliver a file)
	// the model tends to then claim as done. The turn (§6.3.4) tracks failed
	// side-effect calls and, if one was never retried successfully, appends an
	// honest footer to the final reply — structurally contradicting a
	// "claimed delivered but it failed" answer. Default false.
	SideEffect bool
	// EndsTurn marks a tool that terminates the turn with a PROCESS exit rather than
	// producing a deliverable — it returns a ToolExit/ExitRequest to ask the user a
	// question, hand off, or stay silent (clarify / escalate_to_coo / stay_silent). All
	// three assume an interactive user channel. A delegated sub-turn has NO such channel
	// (it must always hand a finished product back to its parent), so narrowToSuite drops
	// EndsTurn tools from a sub's bag — the sub can neither ask nor hand off. Default false
	// (an ordinary product/query tool never ends the turn).
	EndsTurn bool
	// SelectionHint, when non-nil, contributes one scenario to the assembled
	// "工具按场景选" rubric. Authored ON the tool, rendered into a single shared
	// rubric built only from the tools VISIBLE this turn. nil = no scenario.
	SelectionHint *SelectionHint
}

// SelectionHint is a tool's self-declared "when to reach for me" scenario,
// collected per-turn from the visible tool set into the prompt's tool-selection
// rubric. Write it POSITIVELY — what this tool does and the keyword cues that
// trigger it — never naming other tools.
type SelectionHint struct {
	When     string // 触发场景 + 关键词线索（模型面向，中文）
	Then     string // 该用这个工具做什么（自指，正面）
	Priority int    // 排序键：小=在 rubric 里排前、先被检查
}

// ToolCall is one tool invocation the model emitted in an assistant turn.
// Arguments is the raw JSON arguments string as the model produced it.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON-encoded arguments, as emitted by the model
}

// Message is one entry in an LLM conversation. Field shapes:
//
//	role=user/system/assistant (no tools): Role + Content
//	role=assistant with tool calls:        Role + Content (may be "") + ToolCalls
//	role=tool (one tool's result):         Role + Content + ToolCallID + ToolName
//
// No json tags (except Atts, see there): this is a Go-native value. Wire-format
// translation to the OpenAI/Anthropic shape lives in the provider layer.
type Message struct {
	Role       string     // "system" | "user" | "assistant" | "tool"
	Content    string     // text body; "" is allowed for assistant-with-ToolCalls
	ToolCallID string     // role=tool: the tool_call this message is the result for
	ToolName   string     // role=tool: the tool function's name
	ToolCalls  []ToolCall // role=assistant: the tool calls the model wants to make
	// Images attaches image refs; when non-nil the provider switches the message
	// body to the array content-block form. nil = plain text message.
	Images []ImageRef
	// Audio attaches audio refs — parallel to Images; the pre-turn ASR path uses
	// it so an ASR-tagged model returns the transcript text.
	Audio []AudioRef
	// RowKind is the in-place compaction marker class: "" or "msg" = ordinary row;
	// "summary" = a compaction summary (role=system, the replay start); "replay" = a
	// recent-tail row RE-APPENDED after a summary so the model's replay window (FROM
	// the marker) is whole — its original still lives before the marker, so a viewer
	// reading the full table must drop "replay" rows to avoid showing the tail twice.
	// "commit" is RESERVED — the rebuild writes a compaction as one atomic batch, so
	// there is no half-written summary to guard against and no commit mark is written;
	// it exists for a future non-atomic backend that needs the two-phase variant
	// (spec §6.9-4).
	RowKind string
	// Author is the row's structured attribution (who produced it), decoupled from
	// Content: a user row carries the human sender; an assistant row the agent
	// (bob / agora member); a tool row the tool. Zero value = unattributed (legacy
	// rows / non-chat turns) → renderers add no speaker prefix.
	Author MsgAuthor
	// TimeUnix is the row's wall-clock (unix seconds, fractional). Populated only by
	// the read paths that surface it (the webui chat-log reader); the turn-hot path
	// leaves it zero — it is NOT sent to the model.
	TimeUnix float64
	// Atts is the row's durable structured attachments bag (a user row's placed
	// files — metadata only, never bytes). It rides along so store round-trips keep
	// RecentAttachments whole: a compaction keep-tail re-append and a cold-archive
	// export/import both carry the bag instead of NULLing the column (the 2026-07
	// F92 family). The store's read paths populate it; the model NEVER sees it (the
	// wire layer ignores it — the model reads attachments from the row text). The
	// json tag is load-bearing: archive payloads serialized the bag as "atts" (via
	// the former session-local envelope), so this field must keep that key for old
	// payloads to round-trip; omitempty keeps bag-less rows (the vast majority)
	// byte-identical to the pre-Atts shape.
	Atts []Attachment `json:"atts,omitempty"`
}

// MsgAuthor is a message row's structured attribution — who produced it, decoupled
// from the content. Stored as the row's `meta.author` JSON; the shape varies by Kind
// (a tool row has only Name; a human has ID+Name) so a blob beats sparse columns.
type MsgAuthor struct {
	Kind string `json:"kind,omitempty"` // "human" | "agent" | "tool"
	ID   string `json:"id,omitempty"`   // human: sender uid; agent: principal (e.g. member:<name>)
	Name string `json:"name,omitempty"` // display name (human) / agent name / tool name
}

// IsZero reports whether the author carries no attribution.
func (a MsgAuthor) IsZero() bool { return a.Kind == "" && a.ID == "" && a.Name == "" }

// Message RowKind values.
const (
	RowKindMsg     = "msg"
	RowKindSummary = "summary"
	RowKindReplay  = "replay"
	RowKindCommit  = "commit"
)

// ImageRef is one image attached to a Message. Data is the raw bytes; MIME is
// the media type. Providers base64-encode at marshal time. Keep small — history
// is replayed every turn.
type ImageRef struct {
	Data []byte
	MIME string
}

// AudioRef is one audio clip attached to a Message — same shape as ImageRef.
type AudioRef struct {
	Data []byte
	MIME string
}

// Usage holds token accounting for a model call (zero if the backend doesn't
// report it). CacheCreation/CacheReadInputTokens are Anthropic prompt-cache
// counters, both already counted INSIDE InputTokens (they break it down).
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// StreamEvent is one item from a streaming model call.
//   - Text      — a content delta (may be empty on the final event)
//   - Reasoning — a THINKING delta, the separate channel a reasoning-parser
//     backend routes <think> onto (vllm's `reasoning`, DeepSeek's
//     `reasoning_content`). Never part of Text: it is process, not product, and
//     never enters history — see ChatResponse.Reasoning.
//   - ToolDelta — when non-nil, a partial tool_call update (streaming backends)
//   - Done      — true on the last event (channel closed right after)
//   - Usage     — populated on the final event if the backend reports it
//   - Model     — populated on the final event with the pool-entry name
//   - Err       — non-nil if the stream failed; Done is also true in that case
type StreamEvent struct {
	Text      string
	Reasoning string
	ToolDelta *ToolCallDelta
	Done      bool
	Usage     Usage
	Model     string
	Err       error
}

// ToolCallDelta is one partial update to a single in-progress tool_call from a
// streaming backend. Index identifies which tool_call in the parallel-calls
// array (0-based); Name/ID appear once; ArgumentsDelta chunks accumulate per
// Index into the final arguments value.
type ToolCallDelta struct {
	Index          int
	ID             string
	Name           string
	ArgumentsDelta string
}

// StreamAccumulator is the running state ChatStreamWatch hands its watcher on
// each event: incrementally-rebuilt ToolCalls + accumulated Text + accumulated
// Reasoning. The watcher can return a non-nil error to cancel the stream early.
//
// Reasoning accumulates HERE rather than in each sink because backends push it
// one token per event: a consumer that forwarded every delta straight to a
// line-structured channel would emit one line per token. Accumulating centrally
// lets the watcher flush on its own boundaries (see leaf/turn/20-round.go).
//
// INVARIANT a watcher must respect: the accumulator is per-ATTEMPT, but the
// watcher closure is not. When the pool fails over mid-request it starts a fresh
// accumulator and calls the SAME watcher against it, so any cursor the watcher
// keeps into these fields is stale from that moment. Detect the restart by
// IDENTITY (the new value is not an extension of the previous one), never by
// length — the replacement is routinely longer than the old cursor.
type StreamAccumulator struct {
	Text      string
	Reasoning string
	ToolCalls []ToolCall
}

// StreamWatcher is the per-event hook ChatStreamWatch invokes. Return a non-nil
// error to abort the stream (it propagates out wrapped; ctx is cancelled).
// Watcher should be cheap — it runs on every event in the read goroutine.
type StreamWatcher func(ev StreamEvent, accum *StreamAccumulator) error

// ChatResponse is the result of one non-streaming model call. Either Content is
// non-empty, or ToolCalls is non-empty, or both. Model is the pool-entry name
// that served the request (set by the pool, not the underlying Chatter).
//
// Reasoning carries the backend's separate THINKING channel when it emits one.
// It is diagnostic only: nothing persists it, nothing feeds it back as history
// (a model's own past thinking is not context — replaying it burns the window
// for no gain), and the non-streaming callers are all judge/advisor/tool paths
// whose sinks drop trace anyway. Its real job is making "the model only ever
// thought" distinguishable from "the model returned nothing" — see
// leaf/model/45-chat-stream-watch.go.
type ChatResponse struct {
	Content   string
	Reasoning string
	ToolCalls []ToolCall
	Usage     Usage
	Model     string
}
