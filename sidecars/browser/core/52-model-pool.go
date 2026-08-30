package core

import (
	"context"
	"errors"
	"time"
)

// ErrRequiredModelMissing is returned by a turn when the event's
// MessageEvent.RequiredModelTags cannot be satisfied by any live
// pool entry. Distinct from model.ErrNoModelAvailable because the
// turn has explicitly opted out of fallback — sources use this to
// signal "no reply at all is better than a wrong-model reply".
var ErrRequiredModelMissing = errors.New("required model tags not satisfied")

// Model-entry kinds. Kind is a pool entry's routing class: a ModelRequest is
// only ever matched against entries of the same Kind (an empty
// ModelRequest.Kind defaults to KindLLM). It keeps a non-chat model — a
// translator, a speech recogniser — structurally out of the general chat
// candidate pool.
const (
	KindLLM       = "llm"       // chat-capable language model (the default)
	KindASR       = "asr"       // speech-to-text / audio transcription
	KindTranslate = "translate" // machine-translation model
	KindOCR       = "ocr"       // image-to-text / optical character recognition
)

// ModelRequest is what the agent asks the pool for. The pool picks an entry
// of the requested Kind whose Tags satisfy Requires, ranked by how many of
// Prefer it has, then dispatches the call to that entry's Chatter.
//
// Tag semantics differ by pool impl:
//   - NullPool  — every call fails (no entries).
//   - MultiPool — strict: an entry must have every Requires tag to qualify.
//
// PinnedEntry forces a specific entry by Name; if it isn't a live entry the
// pool errors out rather than silently picking another one (the pinned path
// does not check Kind — the caller named an exact entry).
type ModelRequest struct {
	Kind        string     // entry kind to route to; empty → KindLLM
	Requires    []string   // tags the chosen entry MUST have (empty → any)
	Prefer      []string   // tags to break ties among qualifying entries
	Tools       []ToolSpec // advertised to the model; forwarded by Chat, ChatStream, and ChatStreamWatch alike (ChatStream callers should leave it empty for the final user-visible reply)
	PinnedEntry string     // force this exact entry by Name; empty → tag-match
}

// ModelInfo describes one pool entry for snapshot / listing.
// State and InFlight are dynamic (updated by the pool); everything else is
// configured at pool-build time. ContextSource / ConcurrencySource label where
// the effective values came from ("yaml" / "probe:<provider> ..." / "default")
// so doctor can show provenance.
type ModelInfo struct {
	Name              string   // user-given name in models.yaml (e.g. "ollama-7b")
	Provider          string   // "ollama" | "openai" | "openrouter" | …
	Model             string   // the backend's model id
	Kind              string   // "llm" (chat-capable); default for absent config
	Priority          int      // static tie-breaker; enabled range -10000..10000 (0 = neutral), < -10000 = disabled
	Tags              []string // user-given tags
	ContextWindow     int      // effective max input tokens
	ContextSource     string   // "yaml" | "<probe-source>" | "default"
	Concurrency       int      // effective per-entry concurrency cap (0 → unlimited)
	ConcurrencySource string   // "yaml" | "<probe-source>" | "default"
	State             string   // "live" | "tentative" | "paused" | "cooling" | "disabled"
	InFlight          int      // current in-flight requests against this entry

	// LastError is the text of the most recent counted backend error — the
	// "why" behind a cooling / tentative State. Empty when the entry has never
	// errored this process. Admin-only diagnostic (surfaced read-only in webui).
	LastError string
	// DeadUntilUnix is the unix-second timestamp the current cooldown ends, or
	// 0 when not cooling. Lets a viewer show "cooling — Nm left".
	DeadUntilUnix int64

	// Process-lifetime in-memory usage counters (reset on restart).
	TotalCalls        int64
	TotalErrors       int64
	TotalInputTokens  int64
	TotalOutputTokens int64
}

// PoolSnapshot is the live state of every entry. Cheap; safe to call at any
// time. Used by the future /pool dashboard and chat commands.
//
// The Heartbeat / Reload / Usage sub-structs were added when the pool's
// internals were split into independent sub-components (see
// docs/model-pool-design.md). They are additive readouts — existing
// readers that only look at Entries / InFlight are unaffected.
type PoolSnapshot struct {
	Entries   []ModelInfo
	InFlight  int // total in-flight requests across all entries
	Heartbeat HeartbeatInfo
	Reload    ReloadInfo
	Usage     UsageInfo
}

// HeartbeatInfo is the on-demand liveness probe's snapshot view.
// Running=true means a goroutine is currently probing dead entries.
// Cheap; populated by HeartbeatRunner.SnapshotInfo().
type HeartbeatInfo struct {
	Running bool
}

// ReloadInfo is the config-reloader's snapshot view. ConfigPath is the
// models.yaml the mtime watcher stats; empty → mtime watch disabled.
// LastMtime is the last mtime the watcher saw — zero before the first
// observation.
type ReloadInfo struct {
	ConfigPath string
	LastMtime  time.Time
}

// UsageInfo is the usage-recorder's snapshot view. HasStore=true means
// a store handle has been wired (FlushUsage will persist); false means
// the per-entry hourly buckets accumulate in memory and Flush no-ops.
type UsageInfo struct {
	HasStore bool
}

// ModelPool is the agent's entry point for model calls. The agent never knows
// which entry served its request — it builds a ModelRequest with the right
// tags/tools, and the pool routes. Two implementations live in
// model:
//
//   - NullPool  — no model configured; every call returns MsgNoModelConfigured
//   - MultiPool — full models.yaml routing, liveness, fallback
type ModelPool interface {
	// Ready reports whether a call can be served right now. nil → yes; otherwise
	// a descriptive error (no entries, all dead, …). Used by the agent loop to
	// short-circuit before allocating session resources.
	Ready() error
	// ChatStream picks an entry, opens a streaming call. The final user-visible
	// turn flows through here. req.Tools should be empty in this mode.
	ChatStream(ctx context.Context, req ModelRequest, messages []Message) (<-chan StreamEvent, error)
	// Chat picks an entry, does a non-streaming call. Used by side-LLM callers
	// (compression, judge, learning) that want a clean ChatResponse without
	// watcher infrastructure.
	Chat(ctx context.Context, req ModelRequest, messages []Message) (ChatResponse, error)
	// ChatStreamWatch is the streaming-with-inspection entry — picks an entry,
	// opens a streaming call (with tools if req.Tools is non-empty), drains
	// stream events through `watcher` (if non-nil), and returns the assembled
	// ChatResponse at the end. The watcher can ctx.Cancel mid-stream by
	// returning a non-nil error; ChatStreamWatch returns that error wrapped.
	//
	// Used by tool rounds (forward) so a model that generates
	// pathological tool_call arguments (e.g. blowing past a sane size threshold
	// mid-stream) can be aborted before the backend's max_tokens cap fires +
	// triggers an opaque parse-error cascade.
	ChatStreamWatch(ctx context.Context, req ModelRequest, messages []Message, watcher StreamWatcher) (ChatResponse, error)
	// Snapshot returns the live state of every entry.
	Snapshot() PoolSnapshot
	// FlushUsage persists any completed-hour usage the pool accumulated
	// in memory to the store. Called from the housekeeping tick — it
	// leaves the in-progress hour to keep accumulating. NullPool is a
	// no-op; only MultiPool keeps an hourly history.
	FlushUsage(ctx context.Context)
	// FlushUsageFinal persists ALL accumulated usage including the
	// in-progress hour. Called once on a graceful shutdown so a clean
	// exit doesn't lose the partial hour. NullPool is a no-op.
	FlushUsageFinal(ctx context.Context)
	// Close releases pool-lifetime background machinery (MultiPool's
	// on-demand liveness heartbeat). Called once on shutdown. NullPool is
	// a no-op; only MultiPool has anything to cancel.
	Close()
}
