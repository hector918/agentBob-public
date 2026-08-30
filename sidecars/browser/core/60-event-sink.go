package core

// Sink is where a reply is rendered: the local terminal prints deltas live; an
// IM source buffers them and edits a message at a rate-limited cadence. The hub
// creates one per inbound message via Source.NewSink.
//
// Two delta channels — both call sites classify what they emit:
//
//   - ContentDelta(text): user-facing reply content (LLM output, approval
//     prompts the user must see). Always reaches the user; the only knob is
//     stream vs block (sink-internal).
//
//   - TraceDelta(text): "what bob is doing" annotations — tool-progress lines,
//     fetch URLs, status updates, intermediate iteration markers. Hidden
//     entirely when the scope's trace pref is off; otherwise routed the same
//     way as ContentDelta.
//
// Finish(full) is called exactly once. `full` is the complete CONTENT reply
// (concatenation of ContentDeltas, not including trace). Sinks render based
// on per-scope SinkPrefs supplied to NewSink:
//
//   - prefs.Stream=true  → live edit-as-you-go (content + trace if enabled)
//   - prefs.Stream=false → silent until Finish, then one message with
//     trace (if Trace=true) then content
//   - prefs.Trace=false  → TraceDelta calls dropped at sink level
//
// All Sink methods MUST be safe for concurrent use — parallel tool dispatchers
// can emit progress without locking. Built-in sinks (local terminal, telegram)
// buffer under a mutex.
type Sink interface {
	// ContentDelta appends user-facing content (LLM output, approval prompts).
	// Always shown — stream/block only affects WHEN, not WHETHER.
	// Best-effort: intermediate-render failures aren't reported.
	ContentDelta(text string)

	// TraceDelta appends a "what bob is doing" annotation (tool-progress,
	// fetch URLs, iteration markers). Dropped entirely when the scope's
	// trace pref is off. Best-effort, like ContentDelta.
	TraceDelta(text string)

	// Finish renders the final reply text and releases any resources.
	// `full` is the complete CONTENT (== concatenation of ContentDeltas;
	// trace lines are NOT included here — sinks that buffered them in
	// block mode interleave at render time).
	//
	// Returns non-nil iff delivery failed (e.g. the IM API rejected it
	// after retries). The caller logs it; the reply may already be
	// persisted, so the caller must NOT re-send on error.
	Finish(full string) error
}

// SinkPrefs are the per-scope rendering preferences resolved at NewSink time.
// The zero value is {Trace:false, Stream:false} (Go bool zero); nothing maps
// zero to true. Callers that want today's default behaviour (both flags ON)
// must call DefaultSinkPrefs() explicitly — that is the only place the
// trace+stream defaults are produced.
//
// Set via the /trace and /stream slash commands (scope-admin only). Persisted
// per session_scope in the session store.
type SinkPrefs struct {
	// Trace=true → tool-progress lines are forwarded; false → dropped.
	Trace bool
	// Stream=true → live edit-as-you-go; false → buffer until Finish, then
	// one final message.
	Stream bool
}

// DefaultSinkPrefs returns the "preserve today's behaviour" defaults — both
// flags ON. Used when a scope has no row in the prefs table.
func DefaultSinkPrefs() SinkPrefs { return SinkPrefs{Trace: true, Stream: true} }
