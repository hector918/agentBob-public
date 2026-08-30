package streamsink

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// loop drives the sink's per-turn background work. It runs only when the
// channel can edit-stream (intermediate flushes) or can type. In stream mode it
// renders on a fixed flushInterval cadence (smooth, bounded edit rate); the typing
// ticker only pings when the channel can type and no content has shown yet.
//
// NOTE: a line-boundary flush was tried and reverted — it fired an
// edit on EVERY trace annotation (far MORE edits than the coalesced interval cadence),
// and since the flush runs on THIS goroutine through the per-chat send gate, a
// gate backoff blocked the loop → starved typing + batched edits into chunks. The
// per-chat gate (telegram) + reply-folds-into-one-turn (P1) are the rate-limit fix;
// the interval cadence stays for smooth streaming.
func (s *Sink) loop() {
	// typing ticker only when the channel can show a typing indicator — mirror the
	// flushC pattern below so a CanType=false channel never wakes this goroutine every
	// interval just to no-op.
	var typingC <-chan time.Time
	if s.caps.CanType {
		typingT := time.NewTicker(typingInterval)
		defer typingT.Stop()
		typingC = typingT.C
		s.typing() // show the indicator right away, while we wait for the model
	}
	var flushC <-chan time.Time
	if s.caps.CanEdit && s.prefs.Stream {
		flushT := time.NewTicker(flushInterval)
		defer flushT.Stop()
		flushC = flushT.C
	}
	for {
		select {
		case <-flushC:
			s.intermediateFlush()
		case <-typingC: // nil unless CanType, so this case is dead when typing is unsupported
			s.typing()
		case <-s.done:
			return
		case <-s.ctx.Done():
			return
		}
	}
}

// intermediateFlush renders both channels mid-turn. Errors are logged here (the
// loop has no caller to return to); a hard content failure is surfaced again by
// the Finish-time flush, which IS returned to the caller. Trace first so it sits
// above content (platforms order by send time).
func (s *Sink) intermediateFlush() {
	if err := s.flushChan(&s.trace); err != nil {
		s.warn("streamsink: intermediate trace flush failed", "err", s.wire.RedactErr(err))
	}
	if err := s.flushChan(&s.content); err != nil {
		s.warn("streamsink: intermediate content flush failed", "err", s.wire.RedactErr(err))
	}
}

// typing pings the indicator, but only while no content has shown yet — once
// the first content message lands the user can see progress.
func (s *Sink) typing() {
	// Gate on haveSentAny, not sentID: a block channel's Send returns "" so
	// sentID never sets even after content is delivered — keying on it would
	// type forever for a (future) CanType && !CanEdit leaf. haveSentAny means
	// exactly "content has been delivered" and is set on every successful send.
	s.mu.Lock()
	shown := s.content.haveSentAny
	s.mu.Unlock()
	if shown {
		return
	}
	s.wire.Typing(s.ctx)
}

// flushChan flushes one channel's pending data. Serialised via flushMu across
// BOTH channels so the sink never fires two concurrent wire calls (per-chat
// rate limits get unhappy fast).
func (s *Sink) flushChan(c *chanState) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	return s.flushChanLockedDepth(c, 0)
}

// flushChanLockedDepth renders the channel's CURRENT message window, splitting
// over the cap into a chain of messages. flushMu is held by the caller for the
// whole recursion. depth caps the split so a runaway stream can't recurse
// without bound.
func (s *Sink) flushChanLockedDepth(c *chanState, depth int) error {
	if depth >= flushDepthCap {
		s.mu.Lock()
		finished := s.finished
		s.mu.Unlock()
		if !finished {
			// Intermediate (streaming) flush: bounded-accept — the unsent tail sits in c.buf
			// and a LATER flush (incl. the final Finish flush) re-sends it. The cap exists to
			// stop a still-growing runaway stream from recursing without bound.
			s.warn("streamsink: flushChan recursion cap hit; deferring split tail to a later flush",
				"channel", c.kind, "depth", depth, "buf_bytes", c.buf.Len())
			return nil
		}
		// Terminal Finish flush: there is NO later flush to carry the tail, so capping here
		// would PERMANENTLY truncate the reply while reporting success. The buffer no longer
		// grows (Finish took the final content), so the split is bounded by buf size /
		// MaxChars — keep splitting uncapped to deliver the whole reply.
	}
	s.mu.Lock()
	full := c.buf.String()
	if c.msgStartOffset > len(full) {
		c.msgStartOffset = len(full)
	}
	text := strings.TrimSpace(full[c.msgStartOffset:])
	degraded := s.degradedToBlock
	finished := s.finished
	max := s.wire.MaxChars()

	// In degraded (block) mode, suppress ALL mid-flight sends/edits — only
	// render at Finish (finished=true). Hoisted above the fits/overflow
	// split so BOTH paths are gated: an intermediate flush whose buffer
	// has grown past MaxChars must NOT fan out split Send calls either, or
	// the sink keeps hammering the very platform whose 429 triggered the
	// degrade. Finish still delivers via the unchanged overflow path below.
	if degraded && !finished {
		s.mu.Unlock()
		return nil
	}

	if s.wire.WireLen(text) <= max {
		if text == "" || text == c.lastSent {
			s.mu.Unlock()
			return nil
		}
		prevLastSent := c.lastSent
		c.lastSent = text
		sentID := c.sentID
		epoch := c.epoch
		s.mu.Unlock()
		if err := s.sendOrEdit(c, sentID, text); err != nil {
			// Hard failure (sendOrEdit already exhausted its retries): roll
			// back lastSent so the dedup guard above doesn't skip re-delivering
			// this same text on a later flush — including the final Finish
			// flush, which would otherwise silently drop the whole reply on an
			// edit-stream channel. Mirrors the overflow path's rollback, incl.
			// the epoch guard: skip if a concurrent Finish rebuilt the buffer.
			s.mu.Lock()
			if c.epoch == epoch {
				c.lastSent = prevLastSent
			}
			s.mu.Unlock()
			return err
		}
		return nil
	}

	// Overflow: split. Find the best cutoff in the CURRENT message slice.
	cur := full[c.msgStartOffset:]
	fitBytes := s.wireCutoff(cur, max)
	// Prefer a newline within the last ~500 bytes before the hard cap so
	// messages don't end mid-line. If none, accept the cap.
	splitAt := fitBytes
	lookback := fitBytes - 500
	if lookback < 1 {
		lookback = 1
	}
	for i := fitBytes; i >= lookback; i-- {
		if cur[i-1] == '\n' {
			splitAt = i
			break
		}
	}
	if splitAt == 0 {
		// A single rune wider than MaxChars: wireCutoff returned 0. Emit that
		// rune alone rather than sending "" and never advancing msgStartOffset
		// (which would burn the whole depth cap on empty sends, then drop the
		// tail). Pathological cap config only, but a shared core must not loop.
		_, sz := utf8.DecodeRuneInString(cur)
		splitAt = sz
	}
	firstPart := strings.TrimRight(cur[:splitAt], " \t\r\n")
	if firstPart == "" {
		// All whitespace before the cut. Sending it verbatim is a wire error on
		// some platforms (telegram 400 "message text is empty") that no retry can
		// clear, wedging the window. Drop the run instead: advance past it and
		// keep flushing the remainder. sentID/lastSent stay untouched — nothing
		// was ever sent in this window (any earlier fits-flush of it would have
		// trimmed the same whitespace to "" and skipped).
		c.msgStartOffset += splitAt
		s.mu.Unlock()
		return s.flushChanLockedDepth(c, depth+1)
	}

	sentID := c.sentID
	prevLastSent := c.lastSent
	// Advance past what we just rendered; reset sentID + lastSent so the next
	// flush sends a brand-new message for whatever follows. epoch is captured
	// with the advance: it ties the rollback below to THIS buf's coordinates.
	epoch := c.epoch
	c.msgStartOffset += splitAt
	c.sentID = ""
	c.lastSent = ""
	if sentID != "" && firstPart == prevLastSent {
		// The split point landed exactly at the end of the already-streamed
		// text: the current message ALREADY shows firstPart (the last fits-flush
		// put it there), so re-editing it would be a wire no-op. Treat the chunk
		// as rendered — the clears above froze the window — and go straight to
		// the tail. Without this, a deterministic split point re-edits the same
		// text every tick ("message is not modified" on telegram).
		s.mu.Unlock()
		return s.flushChanLockedDepth(c, depth+1)
	}
	s.mu.Unlock()

	if err := s.sendOrEdit(c, sentID, firstPart); err != nil {
		// Roll back the offset advance so this chunk is re-rendered on the
		// next flush instead of being silently skipped (data loss). sendOrEdit
		// already exhausted its own retries, so this only matters on a hard
		// failure where the caller may flush again later. Skipped when a
		// concurrent Finish rebuilt the buffer (epoch bumped) while the send
		// was in flight: the offset is in a new coordinate frame, subtracting
		// an old-frame length would corrupt it, and the canonical `full` Finish
		// installed already covers this chunk's content from offset 0.
		s.mu.Lock()
		if c.epoch == epoch {
			c.msgStartOffset -= splitAt
			c.sentID = sentID
			c.lastSent = prevLastSent
		}
		s.mu.Unlock()
		return err
	}
	// firstPart fills the current message completely — freeze it so the
	// remaining text starts a FRESH message. sendOrEdit's new-send path stored
	// the just-sent id in c.sentID; clear it (the edit path already left it "").
	// Without this, a single flush over 2× the cap would edit the first chunk's
	// message with the second chunk, losing the first chunk.
	s.mu.Lock()
	c.sentID = ""
	s.mu.Unlock()
	return s.flushChanLockedDepth(c, depth+1)
}

// sendOrEdit dispatches to Wire.Send (sentID=="" — first message in the
// current split window for this channel) or Wire.Edit otherwise, recording the
// returned id and gating anchorReply to the channel's first-ever send. flushMu
// is held by the caller. On a sustained throttle the sink degrades to block.
// Edit failures with deterministic edit-state semantics are recovered here: a
// benign no-op (Wire.BenignEdit) counts as success, a gone edit target
// (Wire.EditGone) drops the dead id and re-delivers via a fresh Send.
func (s *Sink) sendOrEdit(c *chanState, sentID, text string) error {
	fctx := s.flushCtx // safe: caller holds flushMu, where Finish writes it
	if sentID == "" {
		s.mu.Lock()
		anchor := !c.haveSentAny
		s.mu.Unlock()
		var msgID string
		err := s.call(fctx, func() error {
			var e error
			msgID, e = s.wire.Send(fctx, text, anchor)
			return e
		})
		if err != nil {
			s.maybeDegrade(err)
			return err
		}
		s.mu.Lock()
		// Fire onAnchor on EVERY new-send that returns a non-empty id — NOT just the
		// channel's first. A long reply splits into a chain of messages: each split
		// window freezes the current message and does a fresh Send (see the overflow
		// path above), so a channel emits many ids over one turn. Every id must enter
		// the reply-routing index, else a reply to a MIDDLE chunk clean-misses and forks
		// a context-less session (only the first + last would otherwise be indexed).
		// onAnchor's write is an idempotent upsert, so per-chunk firing is safe. A send
		// that returns "" (b==nil / a nil-message API quirk) produced no addressable
		// message → nothing to index. Captured under mu, fired after unlock so user code
		// (the flow's RecordSent) never runs under the sink lock.
		var fireAnchor func(string)
		if msgID != "" {
			c.sentID = msgID
			fireAnchor = s.onAnchor // nil → no-op
		}
		c.haveSentAny = true
		s.mu.Unlock()
		if fireAnchor != nil {
			fireAnchor(msgID)
		}
		return nil
	}
	err := s.call(fctx, func() error { return s.wire.Edit(fctx, sentID, text) })
	if err == nil {
		return nil
	}
	if s.wire.BenignEdit(err) {
		// No-op edit ("message is not modified"): the message already shows
		// exactly this text, so the flush is complete. Swallow the error —
		// propagating it would roll back lastSent and re-fire the same no-op
		// edit on every later flush, including Finish's (a spurious failure
		// on a reply the user in fact received).
		return nil
	}
	if s.wire.EditGone(err) {
		// The edit target is gone (user deleted the streamed message) or the
		// platform locked it. The id is dead: every further Edit — including
		// Finish's — would fail the same way and the rest of the reply would
		// silently never render. Drop the id and deliver the current window as
		// a brand-new message instead.
		s.mu.Lock()
		if c.sentID == sentID {
			c.sentID = ""
		}
		s.mu.Unlock()
		return s.sendOrEdit(c, "", text)
	}
	s.maybeDegrade(err)
	return err
}

// call runs one wire call, retrying up to maxSendAttempts times on transient
// failures (short fixed sendRetryBackoff, ctx-aware). A throttle
// (Wire.RateLimited) is TERMINAL here: 429 handling belongs to the source-level
// send gate/relay (sources/sendgate), which has already paced and redialed the
// call before it surfaced — a 429 still standing is a sustained throttle, so
// burning more attempts re-entering the gate only holds the turn longer; the
// caller's maybeDegrade flips the edit-stream to block mode instead. A benign
// no-op edit / gone edit target is deterministic edit state, equally terminal:
// sendOrEdit recovers (swallow / re-send fresh), so no retry and no WARN — just
// a debug line. After the final attempt it logs (redacted) and returns the error.
func (s *Sink) call(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; attempt <= maxSendAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt == maxSendAttempts {
			break // out of attempts — fall through to logging
		}
		if _, ok := s.wire.RateLimited(err); ok {
			s.warn("streamsink: sustained rate-limit from the send gate — not retrying", "attempt", attempt)
			break // sustained throttle — degrade, don't re-queue (see doc above)
		}
		if s.wire.BenignEdit(err) || s.wire.EditGone(err) {
			break // deterministic edit-state outcome — retrying the same call can't change it
		}
		s.warn("streamsink: wire call failed — retrying", "wait", sendRetryBackoff, "attempt", attempt,
			"err", s.wire.RedactErr(err))
		select {
		case <-time.After(sendRetryBackoff):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	switch {
	case s.wire.BenignEdit(err):
		slog.Debug("streamsink: edit was a no-op (content unchanged)",
			append(append([]any{}, s.logAttrs...), "err", s.wire.RedactErr(err))...)
	case s.wire.EditGone(err):
		slog.Debug("streamsink: edit target gone — re-sending as a new message",
			append(append([]any{}, s.logAttrs...), "err", s.wire.RedactErr(err))...)
	default:
		s.warn("streamsink: wire call failed — reply may not have been delivered", "err", s.wire.RedactErr(err))
	}
	return err
}

// maybeDegrade flips an edit-stream sink to block mode when err is a sustained
// throttle (the source-level send gate already relayed the 429 in-source, so a
// still-throttled err is sustained). Logged once. Safe from any goroutine.
func (s *Sink) maybeDegrade(err error) {
	if err == nil {
		return
	}
	if _, ok := s.wire.RateLimited(err); !ok {
		return
	}
	s.mu.Lock()
	s.degradedToBlock = true
	logIt := !s.degradedLogged
	s.degradedLogged = true
	s.mu.Unlock()
	if logIt {
		s.warn("streamsink: rate-limit detected, degrading to block mode")
	}
}

// wireCutoff returns the byte offset of the longest prefix of str whose
// WireLen fits in max. Binary-searches over rune boundaries (so the cut is
// always between codepoints) using the channel's WireLen — no per-rune cost
// model is assumed, keeping the core wire-unit-agnostic.
func (s *Sink) wireCutoff(str string, max int) int {
	if s.wire.WireLen(str) <= max {
		return len(str)
	}
	// Rune-boundary byte offsets, plus the end.
	offs := make([]int, 0, len(str)+1)
	for i := range str {
		offs = append(offs, i)
	}
	offs = append(offs, len(str))
	// Largest offs[k] with WireLen(str[:offs[k]]) <= max.
	lo, hi := 0, len(offs)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if s.wire.WireLen(str[:offs[mid]]) <= max {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return offs[lo]
}
