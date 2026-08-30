package core

import "context"

// Chatter is the minimal model-backend interface — one connection to one
// provider. Higher-level routing (multi-entry tag matching, liveness,
// fallback) lives in core.ModelPool; a Chatter just talks to one backend.
//
// Two call modes:
//   - ChatStream: streaming chat completion. Used for the final user-visible
//     reply (token-by-token via the Sink) AND, since, for tool-
//     call rounds via model.ChatStreamWatch — backends that stream tool_call
//     deltas (OpenAI / openai-compat / Anthropic native) emit ToolCallDelta
//     events the watcher consumes for mid-flight inspection (e.g. abort
//     a tool_call whose arguments JSON grows past a sanity threshold). When
//     tools is nil/empty the stream is text-only as before.
//   - Chat: non-streaming chat completion. Still used by side-LLM callers
//     (compression, judge calls, side runner) that want a clean ChatResponse
//     without watcher infrastructure.
//
// ChatStream's returned error is for pre-stream failures; once the channel is
// open, transport errors arrive on it as StreamEvent{Err: ..., Done: true}.
type Chatter interface {
	ChatStream(ctx context.Context, model string, messages []Message, tools []ToolSpec) (<-chan StreamEvent, error)
	Chat(ctx context.Context, model string, messages []Message, tools []ToolSpec) (ChatResponse, error)
	// Ping is a cheap reachability check — a token-free request (the
	// provider's model-list endpoint) that the pool's heartbeat uses to
	// notice a dead backend recovering. A nil error means the backend is
	// REACHABLE (the host answered at all — a non-2xx status still counts,
	// since a probe endpoint a shim doesn't implement must not pin the entry
	// dead forever); it does NOT prove the backend can serve completions, so
	// the pool treats a Ping success as tentative recovery only (one real
	// failure re-deads the entry). Only a transport-level failure (connection
	// refused, DNS, dial timeout, TLS) returns a non-nil error.
	Ping(ctx context.Context) error
}
