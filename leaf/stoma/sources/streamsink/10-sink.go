package streamsink

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"agentbob/contract"
)

const (
	// flushInterval is the edit cadence for stream mode — how often the loop
	// re-renders the growing buffer. Chat platforms rate-limit edits, and a sub-second
	// cadence is also too fast to READ (content jumps faster than the eye follows). 4s
	// is the deliberate cadence — aligned with typingInterval so the stream picks up
	// right where the "typing…" indicator leaves off — giving readable chunks + far
	// fewer edits (less 429 pressure). The final reply always lands whole at Finish
	// regardless. Tunable per channel later via Caps if a platform needs a different one.
	flushInterval = 4 * time.Second
	// typingInterval re-fires the "is typing…" indicator (the chat action
	// lasts ~5s on telegram).
	typingInterval = 4 * time.Second
	// finishGrace is the budget for the final flush when the turn ctx is
	// already cancelled (/close-session or shutdown mid-turn). A cancelled /
	// salvage / best-effort reply (e.g. "[cancelled by user]") must still
	// reach the user, so Finish derives a fresh background-rooted ctx with
	// this timeout. Generous enough for a split send plus a couple of short
	// retry backoffs. telegram and email both used 30s independently.
	finishGrace = 30 * time.Second
	// maxSendAttempts bounds how many times a single wire call is tried before
	// giving up. A transient send failure (network blip, brief 5xx) often clears
	// on a second try, so it retries a couple of times with a short backoff; a
	// permanent failure simply burns the attempts quickly and then fails fast.
	// Terminal outcomes never burn extra attempts: a throttle (Wire.RateLimited —
	// 429 pacing lives in the source-level send gate, not here) and a
	// deterministic edit-state error (Wire.BenignEdit / Wire.EditGone) break out
	// immediately; see call().
	maxSendAttempts = 3
	// sendRetryBackoff is the short pause between retries of a transient
	// failure. Terminal errors (throttle / edit-state, see maxSendAttempts
	// above) never wait here.
	sendRetryBackoff = 500 * time.Millisecond
	// flushDepthCap bounds split recursion so a runaway content stream can't
	// recurse without bound. 50 ≈ 50 messages per flush call; real worst-case
	// observed on telegram was 25 (a 100KB reply). 2× headroom.
	flushDepthCap = 50
)

// New returns a streamsink.Sink rendering one reply through w, shaped by
// w.Caps() and prefs. It implements contract.Sink. deliver is the source's
// target-bound file sender (a closure over Source.SendFile + the Target) used by
// SendFile for one-off attachment delivery; nil → SendFile reports the channel
// can't deliver files. logAttrs are prepended to the core's own slog calls so a
// channel can tag them (e.g. "chat_id", id); pass pairs as slog expects. The
// sink's flush loop (if any) starts here and ends at Finish or ctx cancellation.
func New(ctx context.Context, w Wire, prefs contract.SinkPrefs, deliver func(path, caption string) error, logAttrs ...any) *Sink {
	s := &Sink{
		ctx:      ctx,
		wire:     w,
		caps:     w.Caps(),
		prefs:    prefs,
		deliver:  deliver,
		logAttrs: logAttrs,
		done:     make(chan struct{}),
		// flushCtx starts as the turn ctx. Finish re-points it at a fresh ctx
		// only when ctx is already dead (see Finish).
		flushCtx: ctx,
	}
	s.trace.kind = "trace"
	s.content.kind = "content"
	// The loop drives intermediate flushes (stream mode) and/or the typing
	// indicator. It is only needed when the channel can edit-stream OR can
	// type — a pure block channel (email/wechat) has neither and stays
	// goroutine-free, exactly as those sinks were before.
	if (s.caps.CanEdit && prefs.Stream) || s.caps.CanType {
		go s.loop()
	}
	return s
}

// chanState is one renderable channel — trace or content. Each sink has two,
// fully independent: its own buffer, its own message-id chain across the split
// boundary, its own last-sent string for edit dedup. kind is set at
// construction and never written again; everything else is guarded by Sink.mu.
type chanState struct {
	kind string // "trace" | "content" — for logs only

	buf            strings.Builder
	msgStartOffset int // byte offset in buf where the CURRENT message starts
	// epoch is the offset's coordinate frame: bumped whenever buf is rewritten
	// in a way that invalidates byte positions taken from the OLD buf (Finish's
	// divergence branch). An in-flight overflow flush captures it with its
	// offset advance and skips the failure rollback on mismatch — subtracting
	// an old-frame length from a new-frame offset would corrupt (or negate) it.
	epoch       int
	sentID      string // current platform message id ("" = next flush sends a new one)
	lastSent    string // last text pushed to the current message (skip no-op edits)
	haveSentAny bool   // any send on this channel succeeded (gates anchorReply to the first)
}

// Sink is the shared contract.Sink implementation. See package doc for the
// rendering model. All exported methods are safe for concurrent use — parallel
// tool dispatchers emit deltas without locking on the caller side.
type Sink struct {
	ctx     context.Context
	wire    Wire
	caps    WireCaps
	prefs   contract.SinkPrefs
	deliver func(path, caption string) error // target-bound file sender (Source.SendFile); nil → SendFile unsupported
	// deliverPhoto is the channel's PICTURE primitive (Source.SendPhoto bound to
	// the target), when it has one distinct from the attachment send. nil → a
	// picture is delivered as an attachment, which is what those channels show
	// anyway (contract.PhotoSender).
	deliverPhoto func(path, caption string) error
	logAttrs     []any
	done         chan struct{}

	// onAnchor, if set, is called for EVERY new-send that returns a non-empty
	// platform message id — on BOTH channels, and once per split chunk (a long
	// reply fans out into a chain of messages). It exists so a flow can index each
	// delivered message for reply-routing (MessageIndexer) the INSTANT it's sent —
	// not just the final content id after the turn — so a mid-turn reply, incl. one
	// to the trace line or to a MIDDLE stream chunk, routes back to the busy session
	// and folds in as a nudge instead of forking a context-less new one. The write
	// must be idempotent (repeated ids upsert). Set via OnAnchor before the first
	// delta; guarded by mu; nil → no-op.
	onAnchor func(msgID string)

	// flushCtx is the context every Wire.Send / Wire.Edit actually uses. It
	// equals ctx for the entire normal life of the sink; Finish swaps it for a
	// fresh background-rooted bounded ctx ONLY when ctx is already cancelled,
	// so the final delivery survives a dead turn ctx. Both the write (Finish)
	// and the reads (sendOrEdit, via flushChan) happen under flushMu, so the
	// swap is race-free against an in-flight loop flush.
	flushCtx context.Context

	flushMu sync.Mutex // serialises the wire call across both channels

	mu      sync.Mutex
	trace   chanState // receives TraceDelta
	content chanState // receives ContentDelta + Finish's final `full`

	finished        bool
	degradedToBlock bool // edit-stream only: sustained 429 → stay block for the rest of life
	degradedLogged  bool // log the degrade WARN once
}

var _ contract.Sink = (*Sink)(nil)

// ContentDelta appends user-facing content. Always buffered; whether it is
// flushed mid-stream depends on Caps.CanEdit + prefs.Stream (the loop flushes
// only then). For a block channel the buffer is sent once at Finish.
func (s *Sink) ContentDelta(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	s.content.buf.WriteString(text)
	s.mu.Unlock()
}

// LineBreak reports how this channel writes a line boundary in bob's own
// line-structured text — the wire's declaration (WireCaps.LineBreak), or "\n"
// when it declares nothing. It satisfies contract.LineBreakSink, so a producer
// that knows its text is line-structured (the slash dispatcher and its command
// replies) can ask the channel how to end a line instead of guessing.
//
// Trace, the core's own line-structured channel, uses it directly below.
func (s *Sink) LineBreak() string {
	if s.caps.LineBreak == "" {
		return "\n"
	}
	return s.caps.LineBreak
}

// hardWrapTrace terminates each line of one annotation with the channel's line
// break. Trace is LINE-structured content — one 🔧/✓/✗/flow annotation per line
// — and on a markdown channel a bare "\n" is a soft break that collapses the
// whole run into one paragraph (see WireCaps.LineBreak).
//
// Annotations are usually a single line, but some carry an embedded body whose
// internal newlines are line boundaries too (rubric verdicts, advisor reviews,
// diagnoses append "\n" + reason).
func hardWrapTrace(text, sep string) string {
	return strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", sep) + sep
}

// TraceDelta appends a "what bob is doing" annotation. Dropped entirely when
// the channel can't render trace (Caps.TraceRender=false) or the scope's trace
// pref is off — no trace message is ever created in that case.
func (s *Sink) TraceDelta(text string) {
	if !s.caps.TraceRender || !s.prefs.Trace || text == "" {
		return
	}
	s.mu.Lock()
	// Each TraceDelta is one complete annotation, so it gets its own line —
	// otherwise consecutive annotations concatenate into one unreadable run.
	s.trace.buf.WriteString(hardWrapTrace(text, s.LineBreak()))
	s.mu.Unlock()
}

// Finish performs the final render. Trace gets a final flush of whatever
// TraceDelta produced (persisted, not collapsed); content gets `full` — the
// canonical content reply per the contract.Sink contract (== concatenation of
// prior ContentDeltas, or, for direct-Finish callers, the whole answer).
// Idempotent: a second call returns nil without re-sending.
func (s *Sink) Finish(full string) error {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return nil
	}
	s.finished = true
	// Content channel: drop everything in the CURRENT message window and
	// replace with `full`. strings.Builder has no Truncate, so reset + rewrite
	// the already-rendered prefix (earlier-message bytes from split overflow)
	// then append full. Trace is left untouched — whatever TraceDelta produced
	// stays in its buf for one final flush below.
	if s.content.msgStartOffset > s.content.buf.Len() {
		s.content.msgStartOffset = s.content.buf.Len()
	}
	prefix := s.content.buf.String()[:s.content.msgStartOffset]
	// `full` is the WHOLE canonical reply; on an edit-stream channel that
	// already split mid-stream, `prefix` holds the already-sent earlier-
	// message bytes, which are also the leading bytes of `full`. Writing
	// prefix + full would double-count them and re-emit the entire reply
	// into the last message window (duplicate text + clobbered message).
	// So append only the part of `full` the prefix hasn't covered. If
	// `full` diverges from what was streamed (defensive), drop the prefix
	// and render `full` alone from offset 0.
	rest := full
	if strings.HasPrefix(full, prefix) {
		rest = full[len(prefix):]
	} else {
		s.content.msgStartOffset = 0
		s.content.epoch++ // old-buf byte positions are now meaningless
		prefix = ""
	}
	s.content.buf.Reset()
	s.content.buf.WriteString(prefix)
	s.content.buf.WriteString(rest)
	s.mu.Unlock()

	// Survive a cancelled turn ctx. On /close-session mid-stream s.ctx is
	// already Done, so the final flush's Send / Edit would fail instantly and
	// the cancelled reply would never reach the user. If s.ctx is dead, derive
	// a fresh background-rooted ctx with a grace budget and flush on THAT. When
	// s.ctx is live this is a no-op — flushCtx stays == s.ctx and the path is
	// byte-identical.
	//
	// Order: this swap MUST happen BEFORE close(s.done). loop()'s select races
	// the flush tick against s.done — if a flush tick is already ready when we
	// close s.done, Go picks pseudo-randomly; a flush win would otherwise run
	// against the still-cancelled flushCtx. Swap first → any concurrent loop
	// flush already sees the grace ctx before we tell loop to exit.
	if s.ctx.Err() != nil {
		fctx, cancel := context.WithTimeout(context.Background(), finishGrace)
		defer cancel()
		s.flushMu.Lock()
		s.flushCtx = fctx
		s.flushMu.Unlock()
	}
	close(s.done) // stop loop() if it's running
	// Trace first so it sits above content (platforms order by send time).
	// Trace errors are logged only; content's error is the canonical result.
	if tErr := s.flushChan(&s.trace); tErr != nil {
		s.warn("streamsink: final trace flush failed", "err", s.wire.RedactErr(tErr))
	}
	// Redact before handing the error OUT of the core: Finish's caller (the
	// orchestrator / pipeline) logs it directly and is cross-source, so it can't
	// scrub a platform-specific error itself. Without this, a wire error that
	// embeds a secret (e.g. Telegram puts the bot token in the request URL, so a
	// transport *url.Error echoes it) would reach slog raw. RedactErr is a no-op
	// pass-through when the error carries no secret, so type identity is kept in
	// the common case. Mirrors the Wire contract: the core redacts before logging.
	return s.wire.RedactErr(s.flushChan(&s.content))
}

// SendFile delivers a local file as a platform attachment to this sink's chat.
// It is an INDEPENDENT one-off send — it does not touch the streaming text
// machinery (no buffer, no flush loop, no Finish interaction), so it may be
// called before, during, or after the text reply without disturbing it. The
// deliver closure (Source.SendFile bound to the target) does the platform work;
// a nil deliver means this channel has no file-attachment primitive.
func (s *Sink) SendFile(path, caption string) error {
	if s.deliver == nil {
		return fmt.Errorf("streamsink: this channel cannot deliver files")
	}
	return s.deliver(path, caption)
}

// WithPhotoDeliverer teaches this sink the channel's picture primitive — the
// inline "here is an image" send, as opposed to the attachment send (see
// contract.PhotoSender). Returns the sink so it can be chained onto New.
//
// A separate setter rather than another New parameter: every source builds its
// sink through New, and most have no picture primitive at all, so a new argument
// would be a nil passed in five places to serve one.
func (s *Sink) WithPhotoDeliverer(f func(path, caption string) error) *Sink {
	s.mu.Lock()
	s.deliverPhoto = f
	s.mu.Unlock()
	return s
}

// SendPhoto delivers a picture the way this channel shows pictures, degrading to
// the ordinary attachment send when it has no separate way to do that.
//
// The degradation lives HERE, not in the caller: every streamsink sink satisfies
// contract.SinkPhotoSender, so a caller's type assertion can't tell whether the
// channel really has a picture primitive — and must not have to.
func (s *Sink) SendPhoto(path, caption string) error {
	// Copy under the lock, call outside it: the deliverer talks to the platform,
	// and holding the sink's mutex across a network send would stall every delta a
	// parallel tool dispatcher is emitting meanwhile.
	s.mu.Lock()
	send := s.deliverPhoto
	s.mu.Unlock()
	if send == nil {
		return s.SendFile(path, caption)
	}
	return send(path, caption)
}

// LastSent returns the platform message id of the final CONTENT message (the one
// a reply would target), or "" if nothing was sent. Read after Finish for reply
// routing (contract.MessageIndexer).
func (s *Sink) LastSent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.content.sentID
}

// OnAnchor registers a callback fired for EVERY new-send that returns a non-empty
// platform message id — on both the trace and content channels, and once per split
// chunk of a multi-message reply. fn must therefore be IDEMPOTENT (a repeated id
// upserts). Set it before any delta; a later set still takes effect for sends that
// have not yet happened. fn must be cheap (it runs on the flush goroutine while
// flushMu is held); a flow uses it to index the reply-routing anchor at send-time.
// fn == nil clears it.
func (s *Sink) OnAnchor(fn func(msgID string)) {
	s.mu.Lock()
	s.onAnchor = fn
	s.mu.Unlock()
}

// warn logs at WARN with the channel's logAttrs prepended.
func (s *Sink) warn(msg string, args ...any) {
	slog.Warn(msg, append(append([]any{}, s.logAttrs...), args...)...)
}
