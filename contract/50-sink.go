package contract

// Button is one inline-button: a label and the callback id minted for it. The
// id is resolved back to its handler when the user clicks.
type Button struct {
	Text     string
	Callback string
}

// Sink is where a reply is rendered: the local terminal prints deltas live; an
// IM source buffers them and edits a message at a rate-limited cadence. The bus
// creates one per inbound message via Source.NewSink.
//
// Two delta channels — the call site classifies what it emits:
//
//   - ContentDelta(text): user-facing reply content (LLM output, approval
//     prompts). Always reaches the user; the only knob is stream vs block.
//     Deliberate exception: a BareProductSink renders nothing live — content
//     reaches the user (or consumer) only as Finish's full. Anything that MUST
//     surface mid-turn on such a sink needs its own channel, not ContentDelta.
//   - TraceDelta(text): "what bob is doing" annotations (tool-progress, fetch
//     URLs, iteration markers). Hidden entirely when the scope's trace pref is
//     off; otherwise routed like ContentDelta.
//
// Finish(full) is called exactly once with the complete CONTENT reply
// (concatenation of ContentDeltas, not trace). All methods MUST be safe for
// concurrent use — parallel tool dispatchers emit progress without locking.
type Sink interface {
	// ContentDelta appends user-facing content. Always shown — stream/block only
	// affects WHEN, not WHETHER. Best-effort: intermediate-render failures
	// aren't reported.
	ContentDelta(text string)
	// TraceDelta appends a "what bob is doing" annotation. Dropped entirely when
	// the scope's trace pref is off. Best-effort, like ContentDelta.
	TraceDelta(text string)
	// Finish renders the final reply text and releases resources. full is the
	// complete CONTENT (== concatenation of ContentDeltas; trace lines are NOT
	// included — sinks that buffered them in block mode interleave at render
	// time). Returns non-nil iff delivery failed; the caller logs it and MUST
	// NOT re-send (the reply may already be persisted).
	Finish(full string) error
	// SendFile delivers a local file as a platform attachment to THIS turn's
	// chat (the sink is already bound to the target). path is a real local
	// filesystem path; caption is an optional one-line description. It is an
	// independent one-off send — NOT routed through the streaming text machinery.
	// A returned error is a REJECTION (mirrors Source.SendFile), not a delivery
	// confirmation; the caller surfaces it to the model.
	SendFile(path, caption string) error
	// LastSent returns the platform message id of the final CONTENT message this
	// sink delivered, or "" if nothing was sent / the sink has no platform id
	// (the local terminal, the agora internal-return sink, a capture sink). The
	// flow reads it after the turn to index the reply for routing (a later reply
	// to that message reconnects to this session — MessageIndexer). Read after
	// Finish.
	LastSent() string
}

// BareProductSink is an OPTIONAL marker interface a Sink implements to declare
// "my Finish full IS the product, consumed verbatim": its ContentDelta is a
// no-op (nothing is ever rendered live) and whatever Finish receives is used
// as-is downstream — the agora bridge return sink emits it as the caller's
// inbound MessageEvent.Text, the email coalescer takes it as a merged-mail
// segment, the sub-turn capture sink hands it to the parent as the delegation
// product. For such a sink the turn core MUST pass the bare trimmed reply to
// Finish, never the byte-exact ContentDelta accumulation (tool-round preambles
// + reply) that RENDERING sinks require — a rendering sink's Finish must extend
// the already-streamed bytes (see Sink.Finish), but a product sink streamed
// nothing, so handing it the accumulation glues preamble onto the product.
//
// Implement this ONLY on sinks whose ContentDelta genuinely renders nothing;
// a sink that shows deltas to a user must NOT implement it, or a mid-stream
// edit-split re-sends the whole reply from offset 0.
type BareProductSink interface {
	// BareProductFinish is a pure marker — never called; implementing it is the
	// declaration.
	BareProductFinish()
}

// LineBreakSink is an OPTIONAL interface a Sink implements to declare how its
// channel writes a line boundary inside bob's OWN line-structured text — a
// command list, a session dump, a receipt. A sink that doesn't implement it is
// a plain-text channel by default: "\n".
//
// It splits a decision that has two owners. WHICH text is line-structured is
// the producer's: only the slash dispatcher knows a command reply is bob's UI
// and not a model-written table, whose newlines must never be rewritten. HOW a
// line boundary is written is the channel's: a markdown-rendering channel reads
// a single newline as a soft break and collapses the whole reply into one
// paragraph, so there the boundary is a GFM hard break instead.
//
// The producer asks, then joins its own lines with the answer; nothing
// downstream rewrites text it can't classify.
type LineBreakSink interface {
	// LineBreak returns the separator that ends one line on this channel.
	LineBreak() string
}

// SinkPhotoSender is the turn-scoped half of PhotoSender: the same "show it as a
// picture, not as a file" choice, made from inside a turn where the sink is
// already bound to the chat. See PhotoSender for why the two are not the same
// send, and for which callers should want which.
//
// Optional, like every other sink extra: a sink that does not implement it is
// handed the file through SendFile by DeliverPictureTo.
type SinkPhotoSender interface {
	SendPhoto(path, caption string) error
}

// DeliverPictureTo sends path through the turn's sink as a picture when that sink
// can, and as an ordinary attachment otherwise. The Source-side twin is
// DeliverPicture; both exist so the fallback rule is written once.
func DeliverPictureTo(s Sink, path, caption string) error {
	if ps, ok := s.(SinkPhotoSender); ok {
		return ps.SendPhoto(path, caption)
	}
	return s.SendFile(path, caption)
}

// SinkPictureHolder is an OPTIONAL Sink capability: a sink that decides WHEN a
// picture goes out, rather than sending it the moment a tool produces one. A looping
// turn holds them until its outcome is settled, so no picture reaches the chat while
// the turn is still deciding whether the work is done — and the ones it produced
// arrive together rather than one per round.
//
// Optional like every other sink extra: DeliverPictureWhenReady sends immediately
// through a sink that does not implement it.
type SinkPictureHolder interface {
	// HoldPicture keeps path until the turn ends, then runs onSent exactly once
	// with the send's error or nil. A holder that never sends at all (the process
	// died before the turn was released) must never run it — the producer reads
	// that silence as "the outcome is unknown", which is what its own recovery
	// path is for. The file is the producer's throughout: a holder never moves or
	// deletes it.
	HoldPicture(path, caption string, onSent func(error))
}

// DeliverPictureWhenReady is DeliverPictureTo for a producer that can wait: a sink
// that holds pictures takes it for the end of the turn, and any other sends it
// straight away. held says which happened, so a producer can still report a failure
// it already knows about — when held is true the send has NOT been attempted yet and
// the outcome arrives only through onSent, which may run on another goroutine long
// after this call returned.
func DeliverPictureWhenReady(s Sink, path, caption string, onSent func(error)) (held bool, err error) {
	if h, ok := s.(SinkPictureHolder); ok {
		h.HoldPicture(path, caption, onSent)
		return true, nil
	}
	err = DeliverPictureTo(s, path, caption)
	onSent(err)
	return false, err
}

// SinkPrefs are the per-scope rendering preferences resolved at NewSink time.
// The zero value is {Trace:false, Stream:false}; nothing maps zero to true.
// Callers wanting today's default behaviour (both ON) call DefaultSinkPrefs().
// Set via the /trace and /stream slash commands; persisted per session scope.
type SinkPrefs struct {
	// Trace=true → tool-progress lines are forwarded; false → dropped.
	Trace bool
	// Stream=true → live edit-as-you-go; false → buffer until Finish, then one
	// final message.
	Stream bool
}

// DefaultSinkPrefs returns the "preserve today's behaviour" defaults — both
// flags ON. Used when a scope has no row in the prefs table.
func DefaultSinkPrefs() SinkPrefs { return SinkPrefs{Trace: true, Stream: true} }
