package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"agentbob/contract"
	"agentbob/i18n"
)

// maxParallelTools caps concurrent tool dispatch within one round (spec §4.4).
const maxParallelTools = 8

// contextExceededSignature is the canonical text of leaf/model's
// ErrContextExceeded sentinel (leaf/model/15-errors.go): the pool wraps it as
// "model pool: prompt exceeds model context window" when the prompt overflows
// the serving entry. The turn layer must NOT import leaf/model (the
// arch import boundary — modules couple only through contract), and contract
// carries no sentinel for this fault, so the recoverable overflow signal is
// recognized by matching the wrapped text. (Contrast the pool's args-bloat
// abort, which the turn recognizes via a contract.ErrToolArgsBloat sentinel;
// this one has none.) If model/15-errors.go rewords the sentinel, update this.
const contextExceededSignature = "prompt exceeds model context window"

// isContextExceededErr reports whether the pool surfaced a recoverable
// context-overflow fault (leaf/model.ErrContextExceeded). The Run loop
// force-compacts history + retries rather than finishing the round with the
// generic model-unavailable notice.
func isContextExceededErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), contextExceededSignature)
}

// plainToolCallMarker is the open tag of an inline "<tool_call>...</tool_call>" the
// pool extracts post-stream (providers.HasResidualPlainToolCallMarkup / ExtractInline
// ToolCalls). The turn watches for it live to stop streaming the raw markup to the user
// (F76). Kept as a local literal so leaf/turn does not depend on the providers package;
// it is the canonical Qwen/Hermes tool-call open tag and does not drift.
const plainToolCallMarker = "<tool_call>"

// toolMarkerHoldback returns the length of the longest suffix of text that is a
// PROPER, non-empty prefix of marker — the bytes a tool round must hold back (not yet
// forward to the sink) because the next stream delta could grow them into the complete
// marker (B20: the marker straddles deltas, e.g. "…<tool" then "_call>…"). 0 when no
// trailing suffix could become the marker (nothing to hold). The full-marker case is
// handled by the caller's strings.Index, so only lengths < len(marker) are considered.
func toolMarkerHoldback(text, marker string) int {
	max := len(marker) - 1
	if max > len(text) {
		max = len(text)
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(marker, text[len(text)-n:]) {
			return n
		}
	}
	return 0
}

// thinkEnders are the characters a thought is allowed to be cut after — sentence
// terminators in both scripts, plus the newline. They bound a flush to something
// readable: the trace channel puts each TraceDelta on its own line, so cutting
// anywhere else would strand half a clause on a line of its own.
const thinkEnders = ".!?\n。！？；;"

// sentenceBoundary returns the byte length of the longest prefix of s that ends at
// a sentence boundary, or 0 when s holds no complete sentence yet. Used to pace the
// thinking channel: reasoning arrives one token at a time and often carries no
// newline until it ends, so waiting for a line break would dump the whole thought
// in one burst at stream close.
func sentenceBoundary(s string) int {
	i := strings.LastIndexAny(s, thinkEnders)
	if i < 0 {
		return 0
	}
	_, size := utf8.DecodeRuneInString(s[i:])
	return i + size
}

// runRound runs one single-path round (spec §4.2, A1 — tool rounds and the final
// reply share one streaming call). It rebuilds the prompt, advertises the turn's
// tools, makes one streaming model call rendering content to the sink, then
// resolves: no tool calls → a final reply (or Continue on empty); tool calls →
// the tool branch (persist → dispatch → persist → continue/exit).
//
// replay is the loop-top compaction pass's GetReplay result, reused here so one
// round issues ONE history read; nil → buildMessages reads fresh.
func (c *core) runRound(ctx context.Context, spec contract.TurnSpec, st *turnState, iter int, replay []contract.Message) roundResult {
	// Snapshot the replay for the rubric gate BEFORE buildMessages consumes the
	// slice in place (cleanse/speaker/orphan passes) — the gate's evidence must be
	// the raw history, not the prompt-rendered view. Shallow copy: elements are
	// value types, and the copy's own backing array is untouched by the in-place
	// passes. Only taken while a rubric is armed (nil otherwise — the fallback
	// read inside rubricEvidence covers the rare replay==nil round).
	var evReplay []contract.Message
	if len(st.rubricArmed) > 0 && replay != nil {
		evReplay = append([]contract.Message(nil), replay...)
	}
	msgs, err := c.buildMessages(ctx, spec, replay)
	if err != nil {
		// I8/I14: a cancelled ctx surfaces as the store error here — that is a stop, not a
		// storage fault, so exit silently (no notice), like the persist-fail door. Still
		// release the sink (Finish("")) so the coalescer's per-turn counter is balanced.
		if ctx.Err() != nil {
			c.releaseSink(spec)
			return roundResult{kind: roundErr, err: ctx.Err()}
		}
		// Fail-closed: don't answer against empty history (silent context loss). Surface
		// the same honest "internal storage error, please retry" notice as a persist fail.
		slog.Warn("turn: history read failed — aborting round", "sid", spec.Sid, "err", err)
		if ferr := spec.Sink.Finish(i18n.T(persistFailKey, turnLang(spec))); ferr != nil {
			slog.Warn("turn: sink finish (history-read fail) failed", "sid", spec.Sid, "err", ferr)
		}
		return roundResult{kind: roundErr, err: fmt.Errorf("turn: read history: %w", err)}
	}

	// Advertise the turn's authorized tool specs to the model. The flow already
	// projected + authorized them into spec.Tools; the core just surfaces them.
	req := spec.ModelReq
	// §6.10: soft cache affinity — key the request by session so the pool keeps
	// this conversation on the backend that last served it (round-to-round AND
	// turn-to-turn: the pool remembers per-key, TTL'd). A local-copy scalar (no
	// aliasing of spec.ModelReq); the pool honors it only as a load-balancing
	// tie-break, so it never overrides failover or spreading across equal peers.
	req.AffinityKey = spec.Sid
	if spec.Tools != nil && !st.stripTools {
		// §6.8: stripTools (set after repeated args-bloat aborts) drops tools so the
		// model can only answer in plain text — breaking a model stuck packing huge
		// arguments into one call round after round.
		req.Tools = spec.Tools.Specs()
		req = routeToolRound(req, st) // §6.10: prefer toolcall / smart-on-failures
	}

	// Single-path streaming call. The watcher forwards content to the sink live,
	// but only while no tool call has emerged — once the model starts a tool call
	// the text is preamble (not the user-visible reply). It also re-tests for a
	// degenerate repetition wall every degenCheckEvery bytes and aborts the runaway
	// (§6.6①).
	lastDegenCheck := 0
	lastThinkDegenCheck := 0 // the same counter for the thinking channel, scanned independently
	var shownPartial string  // user-visible content streamed so far (§4.3 cancel persist)
	// A rubric-armed turn BUFFERS its product: the gate (finalReply) must judge it
	// before the user sees it, so live streaming is suppressed and the (possibly
	// redone) reply is delivered in one Finish once it passes. Nothing is shown, so
	// shownPartial stays empty — no cancel/break partial to persist either.
	streamLive := len(st.rubricArmed) == 0
	// F76: a plain <tool_call> (Qwen/Hermes local models) arrives as TEXT — accum.ToolCalls
	// stays empty — so without this the whole markup streams to the user as raw JSON; the
	// pool only strips it AFTER the stream closes. Once the marker appears we stop showing
	// further content and truncate "shown" to the text before it (the post-stream
	// ExtractInlineToolCalls still does the real extraction). Only on a TOOL round
	// (req.Tools offered) so a plain-chat message literally discussing "<tool_call>" isn't
	// frozen.
	toolRound := len(req.Tools) > 0
	markupSeen := false
	// sent counts how many bytes of THIS round's accum.Text have already reached the
	// sink — the forward horizon. On a tool round the tail is held back byte-exactly
	// (see below) so sent may trail accum.Text; st.sunk always equals exactly these
	// forwarded bytes so the finalizePartial/finalReply HasSuffix ties stay byte-exact.
	sent := 0
	// st.sunk is only ever READ for rendering sinks (finalReply / finalizePartial
	// compose Finish from it); a bare-product sink's Finish is the bare product,
	// so skip the write — a deep looping turn would otherwise hold every round's
	// preamble in a buffer nothing consumes. ContentDelta is still forwarded
	// (a bare sink no-ops it by contract), so delta wiring stays observable.
	bareProduct := isBareProduct(spec.Sink)
	forward := func(s string) {
		if s == "" {
			return
		}
		spec.Sink.ContentDelta(s)
		if !bareProduct {
			st.sunk.WriteString(s) // the exact bytes the sink holds — Finish's full must extend them
		}
	}
	// Thinking rides the TRACE channel, never the content one: it is process, not
	// product. That is also its whole gating story — trace is dropped at the sink
	// contract when the scope's trace pref is off, and it is excluded from
	// Finish(full), so a thought never lands in the reply, in history, or in the
	// next round's context.
	//
	// reasonSent is this round's flush horizon into accum.Reasoning. Backends push
	// reasoning ONE TOKEN per event while TraceDelta terminates every call with a
	// line break, so forwarding raw deltas would print one word per line; flushing
	// on sentence boundaries keeps the trace message growing every tick without
	// waiting for the newline that reasoning prose often never emits until the end.
	reasonSent := 0
	thinkPrefix := "💭 "
	if !streamLive || bareProduct {
		// The product is BUFFERED — nothing this round writes to the content channel
		// will reach the user unless it survives what comes next. So this thinking is
		// a DRAFT that may yet be thrown away: by the rubric gate judging it FAIL and
		// demanding a redo (!streamLive), or by the looping driver converging on a
		// later round (bareProduct — quietSink drops ContentDelta but passes TraceDelta
		// straight through, so the user sees the thinking of every round and the
		// content of none). Mark it. A rubric-bearing skill is typically an extraction
		// one, and a number read out of the trace must not be mistaken for the answer
		// that finally ships — which is just as true of a superseded looping round.
		thinkPrefix = "💭⚠️ "
	}
	var thinkAll string // last reasoning seen, for the continuation test and the post-stream tail
	flushThinking := func(all string, tail bool) {
		if !strings.HasPrefix(all, thinkAll) {
			// The pool failed over mid-request and restarted the stream on another
			// entry, so this is a DIFFERENT thought — not a continuation of the one
			// reasonSent points into. The test has to be IDENTITY, not length: the
			// replacement thought is routinely already longer than the old horizon
			// (any backend that batches reasoning deltas), and a length test would
			// then slice a fresh string at a stale offset — eating its head, or
			// cutting mid-rune on CJK. Cheap: an ordinary delta always extends.
			reasonSent = 0
		}
		thinkAll = all
		end := len(all)
		if !tail {
			b := sentenceBoundary(all[reasonSent:])
			if b == 0 {
				return
			}
			end = reasonSent + b
		}
		chunk := strings.TrimSpace(all[reasonSent:end])
		reasonSent = end
		if chunk != "" {
			spec.Sink.TraceDelta(thinkPrefix + chunk)
		}
	}
	watch := func(ev contract.StreamEvent, accum *contract.StreamAccumulator) error {
		if ev.Reasoning != "" {
			flushThinking(accum.Reasoning, false)
		}
		if streamLive && len(accum.ToolCalls) == 0 && !markupSeen {
			text := accum.Text
			switch {
			case !toolRound:
				// Plain-chat round: no tool-call marker guard (a message literally discussing
				// "<tool_call>" must stream verbatim). Forward the delta as-is.
				forward(ev.Text)
				sent = len(text)
				shownPartial = text
			default:
				// F76: a plain <tool_call> arrives as TEXT (accum.ToolCalls stays empty); the
				// pool only strips it AFTER the stream closes, so the turn must stop showing
				// content once the marker appears. F76-follow-up (B20): the marker can also
				// arrive SPLIT across deltas ("…<tool" then "_call>…") — so hold back the
				// longest trailing suffix that is a proper prefix of the marker until the next
				// delta disambiguates it (flush once it proves non-marker; discard once the
				// marker completes), instead of streaming the partial markup bytes raw.
				if i := strings.Index(text, plainToolCallMarker); i >= 0 {
					// Complete marker: flush real content up to it (including bytes previously
					// held that turned out to precede the real marker), then freeze — nothing
					// from the marker onward is shown (post-stream ExtractInlineToolCalls does
					// the real extraction).
					if i > sent {
						forward(text[sent:i])
						sent = i
					}
					markupSeen = true
					shownPartial = text[:i] // freeze "shown" at the text before the markup
				} else {
					// No complete marker yet: forward everything except a trailing suffix that
					// is still a viable prefix of the marker (it may grow into the marker next).
					safe := text[:len(text)-toolMarkerHoldback(text, plainToolCallMarker)]
					if len(safe) > sent {
						forward(safe[sent:])
						sent = len(safe)
					}
					shownPartial = safe // true SHOWN content only (held tail excluded)
				}
			}
		}
		if len(accum.ToolCalls) == 0 && len(accum.Text)-lastDegenCheck >= degenCheckEvery {
			lastDegenCheck = len(accum.Text)
			if isDegenerate(accum.Text) {
				return errDegenerate
			}
		}
		// The same repetition-wall test on the THINKING channel. Without it the
		// detector is blind to a model that falls into a loop before ever emitting
		// content: nothing lands in accum.Text, so nothing is ever checked, and the
		// stall guard cannot help either — a reasoning delta counts as liveness
		// (leaf/model/45-chat-stream-watch.go), which is correct but means the runaway
		// now runs to the provider's 10-minute hard cap instead of failing fast. A
		// think that walls is exactly as dead as a reply that walls; abort it the same
		// way, on its own byte counter so a long healthy think isn't re-scanned.
		if len(accum.Reasoning)-lastThinkDegenCheck >= degenCheckEvery {
			lastThinkDegenCheck = len(accum.Reasoning)
			if isDegenerate(accum.Reasoning) {
				return errDegenerate
			}
		}
		return nil
	}
	resp, err := c.pool.ChatStreamWatch(ctx, req, msgs, watch)
	// Flush the trailing thought — the part after the last sentence boundary, which
	// the in-stream flush deliberately held back.
	//
	// On SUCCESS only resp.Reasoning is flushed: it belongs to the attempt that
	// actually answered. If the pool failed over, a discarded attempt's leftover
	// tail must NOT print here — it would land BELOW the winning answer, reading as
	// the reasoning behind a reply it had nothing to do with. Empty resp.Reasoning
	// (the winner had thinking off) therefore flushes nothing.
	//
	// On FAILURE the last attempt's thinking is all there is, and nothing else is
	// coming, so cutting it mid-sentence would be the only dishonest option.
	if err != nil {
		if thinkAll != "" {
			flushThinking(thinkAll, true)
		}
	} else if resp.Reasoning != "" {
		flushThinking(resp.Reasoning, true)
	}
	if err != nil {
		// A silent-partial sink rendered nothing live, so a cancel/break partial is
		// content NOBODY saw — it must not ship as product. Zeroing shownPartial
		// routes the §4.3 cancel door to the silent release and the §4.2 break door
		// to the plain notice. The machine-facing bare sinks (capture / agora
		// return / email coalescer) deliberately keep their partial+marker: their
		// consumers can use a partial product.
		if _, silent := spec.Sink.(silentPartialSink); silent {
			shownPartial = ""
		}
		if errors.Is(err, errDegenerate) {
			return c.handleDegenerate(spec, st) // §6.6: discard → degen-salvage exit
		}
		if ctx.Err() != nil {
			// §4.3 cancel cleanup: if visible content was already streamed, persist it
			// + a cut-here marker and finalize (history must match what the user saw;
			// the marker is honest, not an apology — I8). Nothing shown → stay silent.
			c.persistCancelledPartial(spec, st, shownPartial)
			return roundResult{kind: roundErr, err: ctx.Err()}
		}
		if errors.Is(err, contract.ErrToolArgsBloat) {
			return c.handleBloat(spec, st) // §6.8: chunk-retry ladder (no model error surfaced)
		}
		if isContextExceededErr(err) {
			// The prompt overflowed the serving entry's window; the pool surfaces
			// this straight (window-blind picker — churning peers would re-collect
			// the same 400) as a recoverable prompt-level fault. Don't finish with the model-unavailable notice — signal the Run
			// loop to force-compact history and retry the round on the shrunk window.
			// Nothing streamed (rejected pre-generation), so there is no partial to
			// preserve.
			return roundResult{kind: roundOversized}
		}
		slog.Warn("turn: model call failed", "sid", spec.Sid, "iter", iter, "err", err)
		if strings.TrimSpace(shownPartial) != "" {
			// §4.2: the stream broke AFTER visible content was streamed. Preserve the
			// partial + an interrupt marker instead of clobbering it with the generic
			// notice (Finish replaces the window). A partial answer is usually valid;
			// the user keeps what they saw and can re-ask.
			c.finalizePartial(ctx, spec, st, shownPartial, i18n.T(streamInterruptedMarkerKey, turnLang(spec)))
			return roundResult{kind: roundErr, err: err}
		}
		// Persist the notice as an assistant row (via finalizeTurn), not just Finish it:
		// the model failed but the STORE is healthy, so history must record what the user
		// actually saw — otherwise the transcript ends at the last tool result, diverging
		// from the wire and leaving a dangling tool-call→result sequence the next turn's
		// model reads as unfinished.
		notice := i18n.T(modelUnavailableKey, turnLang(spec))
		_ = c.finalizeTurn(ctx, spec, notice, notice)
		return roundResult{kind: roundErr, err: err}
	}
	// §6.8: the model responded without an args-bloat abort — the chunk-retry nudge has
	// done its job. Clear it (one-shot, like the other transient layers) so it doesn't
	// go stale across the rest of the turn — its "上一次…被中断了" text stops being true
	// once the model complies. (stripTools, if set, independently keeps tools off.)
	if spec.Prompt != nil {
		spec.Prompt.SetLayer("bloat_nudge", "")
	}
	st.usage.InputTokens += resp.Usage.InputTokens
	st.usage.OutputTokens += resp.Usage.OutputTokens
	// Record the real prompt size the model just reported — the trigger's truth
	// floor (10-core.go promptFloor): it includes tool specs + template overhead
	// the loop-top weigh can't see, and on pools without a tokenize path it is
	// the only real reading.
	c.recordPromptFloor(spec.Sid, resp.Usage.InputTokens)

	if len(resp.ToolCalls) == 0 {
		// B20 follow-up: on a tool round the watcher holds back a trailing suffix that
		// is a viable <tool_call> prefix. If the stream ended without the marker ever
		// completing, those held bytes are genuine content — flush them now so st.sunk
		// (and shownPartial) match resp.Content and the byte-exact st.sunk finish path
		// stays armed. Gate on sent>0 so a non-streaming/failover edge that skipped the
		// watcher (sent==0, shownPartial behind resp.Content) still finishes via `full`.
		if streamLive && !isBareProduct(spec.Sink) && !markupSeen && sent > 0 && sent < len(resp.Content) {
			forward(resp.Content[sent:])
			sent = len(resp.Content)
			shownPartial = resp.Content
		}
		// The reply already reached the sink iff the watcher streamed it in full
		// (shownPartial tracks exactly what went out; a non-streaming/failover edge
		// that skipped the watcher leaves it behind resp.Content). A bare-product
		// sink (contract.BareProductSink: agora bridge return / email coalescer /
		// sub-turn capture) is excluded even when the watcher "streamed": its
		// ContentDelta is a no-op and Finish's full is consumed AS the product,
		// which must stay the bare reply, not preamble + reply.
		streamedReply := streamLive && !isBareProduct(spec.Sink) && shownPartial == resp.Content
		return c.finalReply(ctx, spec, st, resp, evReplay, streamedReply)
	}
	return c.toolRound(ctx, spec, st, iter, resp)
}

// finalizeTurn is the single terminal tail every reply-producing exit door uses:
// persist the assistant row (persist), then Finish the sink (finish). The two texts
// diverge only when part of the finish text was already STREAMED: the Sink contract
// (contract/50-sink.go) requires Finish's full to extend the ContentDeltas
// byte-for-byte — else an edit-stream sink that split mid-stream re-sends the whole
// reply from offset 0 — while the persisted row must not duplicate what other rows
// already carry (a tool-round preamble is persisted with its own assistant-calls
// row). Doors with nothing streamed pass the same text twice. It persists on a
// DURABLE ctx — an independent short-timeout token when the turn ctx is already dead
// (e.g. a /close-session that landed during the multi-second rubric judge call) — so
// what gets Finished and what gets persisted always agree (no history hole). Both
// texts already include any sideEffectFooter.
// Returns the sink Finish error (nil on success) so a reply-producing door can tell
// whether the user actually RECEIVED the reply — a total delivery failure must not be
// booked as a satisfied turn (F107). The persist error is logged (history hole) but not
// returned: persistence and delivery are independent, and the delivery signal is what
// the satisfied/recall accounting keys on.
func (c *core) finalizeTurn(ctx context.Context, spec contract.TurnSpec, persist, finish string) error {
	pctx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		pctx, cancel = context.WithTimeout(context.Background(), salvageTimeout)
		defer cancel()
	}
	if err := c.store.AppendMessage(pctx, spec.Sid, "assistant", persist, spec.Agent); err != nil {
		slog.Warn("turn: persist assistant message failed", "sid", spec.Sid, "err", err)
	}
	if err := spec.Sink.Finish(finish); err != nil {
		slog.Warn("turn: sink finish failed", "sid", spec.Sid, "err", err)
		return err
	}
	return nil
}

// releaseSink terminates a turn that delivers NO content (a silent cancel, a
// stay_silent exit). Finish("") is the contract's "called exactly once" terminal
// signal with no visible output: streamsink skips the empty send, the agora return
// sink no-ops (so the dispatch chain stops — stay_silent's loop guard is preserved),
// and the email coalescer decrements its per-turn counter (else its buffered replies
// wedge undelivered — the active-counter leak). Every exit door MUST end the turn
// through this or finalizeTurn so the sink is always released exactly once.
func (c *core) releaseSink(spec contract.TurnSpec) {
	if err := spec.Sink.Finish(""); err != nil {
		slog.Warn("turn: sink release (silent) failed", "sid", spec.Sid, "err", err)
	}
}

// persistCancelledPartial finalizes a turn cancelled mid-stream (§4.3). If visible
// content was already streamed, it is persisted (history must match what was shown)
// and re-rendered with a cut-here marker. Empty partial → nothing was shown → still
// release the sink (Finish("")) so the coalescer's per-turn counter is balanced; the
// turn returns no content.
func (c *core) persistCancelledPartial(spec contract.TurnSpec, st *turnState, partial string) {
	if strings.TrimSpace(partial) == "" {
		c.releaseSink(spec)
		return
	}
	// The turn ctx is dead (cancel) — finalize on an independent short-timeout token.
	ctx, cancel := context.WithTimeout(context.Background(), salvageTimeout)
	defer cancel()
	c.finalizePartial(ctx, spec, st, partial, i18n.T(cancelledMarkerKey, turnLang(spec)))
}

// finalizePartial persists already-shown partial content as an assistant row with a
// marker and finalizes — the shared tail of §4.3 (cancel) and §4.2 (stream break).
// The persisted row is partial+marker (this round's shown text; earlier tool-round
// preambles sit on their own assistant-calls rows). The FINISH text must extend the
// whole streamed byte stream (Sink contract): partial is only this round's tail, so
// when earlier rounds streamed a preamble, compose from st.sunk — else an edit-stream
// sink that split mid-stream re-sends the partial from offset 0 (frozen chunks
// duplicated). The suffix check is the defensive tie: a path where partial didn't
// actually stream (a fake / non-streaming edge) falls back to partial+marker.
// A bare-product sink (contract.BareProductSink) never composes from st.sunk —
// nothing was rendered, and its Finish full is consumed verbatim as the product,
// so it must stay partial+marker (no earlier-round preamble glued on).
func (c *core) finalizePartial(ctx context.Context, spec contract.TurnSpec, st *turnState, partial, marker string) {
	persist := partial + marker
	finish := persist
	if !isBareProduct(spec.Sink) {
		if sunk := st.sunk.String(); sunk != "" && strings.HasSuffix(sunk, partial) {
			finish = sunk + marker
		}
	}
	c.finalizeTurn(ctx, spec, persist, finish)
}

// isBareProduct reports whether the sink declared (via the optional
// contract.BareProductSink marker) that its Finish full is consumed verbatim as
// a product — the turn core then hands it the bare trimmed reply instead of the
// sunk ContentDelta accumulation rendering sinks require.
func isBareProduct(s contract.Sink) bool {
	_, ok := s.(contract.BareProductSink)
	return ok
}

// silentPartialSink is the package-private capability a sink implements to
// declare "never ship a partial": it is bare-product (renders nothing live) AND
// its Finish reaches a live human chat, so a cancel/stream-break partial —
// bytes nobody has seen — must be dropped (silent release / plain notice), not
// delivered as if it were the product. Only quietSink (the looping driver's
// wrapper) implements it; the machine-facing bare sinks (sub-turn capture,
// agora return, email coalescer) deliberately do NOT — a partial product is
// still useful to their consumers. Package-private on purpose: promote to
// contract only when an out-of-package sink needs the semantics.
type silentPartialSink interface{ silentPartialFinish() }

// §6.8 bloat ladder.
const (
	// maxBloatTrips is how many times a tool round may abort on args-bloat before the
	// core stops asking the model to chunk and just strips tools (forces plain text).
	maxBloatTrips = 3
	// bloatChunkNudge rides the dedicated "bloat_nudge" prompt layer while tools are
	// still offered — ask the model to split the write across calls.
	bloatChunkNudge = "上一次的工具调用因参数过大被中断了。请把内容分多次写：每次工具调用只放一小段，不要一次塞入大段文字。"
	// bloatStripNudge replaces it once tools have been stripped — there is no tool to
	// call now, so tell the model to answer in plain text.
	bloatStripNudge = "工具调用的参数反复过大、无法完成。请直接用纯文本把要点写出来，本轮不要再调用任何工具。"
)

// handleBloat runs the §6.8 chunk-retry ladder when a tool round aborted because the
// model packed too much into one tool_call's arguments. Up to maxBloatTrips: keep the
// tools, push a "write smaller chunks" nudge, and rebuild the same round. Past the
// cap: strip tools (stripTools) so the next round is plain-text-only. Either way it
// loops (roundContinue) — the abort is recovered, not surfaced as a model error.
func (c *core) handleBloat(spec contract.TurnSpec, st *turnState) roundResult {
	st.bloatTrips++
	nudge := bloatChunkNudge
	if st.bloatTrips > maxBloatTrips {
		st.stripTools = true
		nudge = bloatStripNudge
	}
	if spec.Prompt != nil {
		spec.Prompt.SetLayer("bloat_nudge", nudge)
	}
	slog.Warn("turn: tool-args bloat — chunk-retry", "sid", spec.Sid, "trips", st.bloatTrips, "stripped", st.stripTools)
	return roundResult{kind: roundContinue}
}

// finalReply resolves a round with no tool calls (spec §4.2 final branch).
// evReplay is the round's raw replay snapshot for the rubric gate's evidence
// (nil → the gate reads fresh). streamed reports whether this reply already
// reached the sink live via ContentDelta (computed by runRound).
func (c *core) finalReply(ctx context.Context, spec contract.TurnSpec, st *turnState, resp contract.ChatResponse, evReplay []contract.Message, streamed bool) roundResult {
	full := strings.TrimSpace(resp.Content)
	if full == "" {
		// I5: an empty reply never finalizes — loop again. Bounded by loopCap.
		return roundResult{kind: roundContinue}
	}
	if isDegenerate(full) {
		return c.handleDegenerate(spec, st) // §6.6②: non-streaming backends
	}
	// Rubric gate: a skill the model engaged this turn (skill_view) carrying a
	// RUBRIC.md judges this product before it ships — a fail re-prompts (redo) or,
	// past the retry cap, declares failure instead of delivering ungrounded output.
	if len(st.rubricArmed) > 0 {
		if rr := c.rubricGate(ctx, spec, st, full, evReplay); rr != nil {
			return *rr
		}
	} else if rr := c.advisorReviewGate(ctx, spec, st, full); rr != nil {
		// docs/advisor.md §4/§5: with no rubric armed, a LOOPING delivery that carries
		// an artifact is accepted by the advisor instead — the convergence contract's
		// acceptance half was otherwise empty. Either/or, never both: one delivery
		// judged by two standards doubles the cost and can produce two verdicts.
		return *rr
	}
	// §6.3.4: if a side-effect tool failed and was never retried successfully, append
	// an honest footer — contradicting a reply that claims the action succeeded. The
	// advisor's unresolved objection (if the review gate spent its redos) rides the
	// same shape: deliver, but say what is doubtful.
	footer := sideEffectFooter(st) + advisorConcernFooter(st)
	full += footer
	// Sink contract (contract/50-sink.go): Finish's full must extend the ContentDeltas
	// already streamed. A live-streamed reply's canonical finish text is therefore the
	// exact streamed bytes (earlier tool-round preambles + this reply, UN-trimmed — a
	// frozen split prefix must keep matching byte-for-byte) plus the un-streamed
	// footer. Passing only the trimmed reply made an edit-stream sink that split
	// mid-stream take its divergence branch and re-send the whole reply from offset 0,
	// duplicating the frozen chunks. The PERSISTED row stays the trimmed reply: the
	// preamble is already persisted with its own round's assistant-calls row, so
	// persisting it again would duplicate history.
	finish := full
	if streamed && st.sunk.Len() > 0 {
		finish = st.sunk.String() + footer
	}
	// I10: persist BEFORE finishing, on a durable ctx — the rubric judge above may have
	// just been cancelled, leaving the turn ctx dead; finalizeTurn falls back so the row
	// is never lost while the reply still ships (no history hole).
	if derr := c.finalizeTurn(ctx, spec, full, finish); derr != nil {
		// Delivery FAILED — the user received nothing. Do NOT report OutcomeFinal: the
		// downstream MarkSatisfied / recall feed / pending-WAL clear would book a phantom
		// "answered" turn and never resend it. Surface an error so it is not counted as
		// satisfied; the reply is still persisted to history, so the model's work isn't
		// lost (the user can see it once the channel recovers). (F107)
		return roundResult{kind: roundErr, err: fmt.Errorf("turn: reply computed but delivery failed: %w", derr)}
	}
	return roundResult{kind: roundFinal, text: full}
}

// toolRound is the tool branch (spec §4.2 step 15-24): persist the assistant row
// with its calls (I2), dispatch them, persist each result (I3), then Continue (so
// the next round shows the model the results) or Exit (a tool requested the end).
func (c *core) toolRound(ctx context.Context, spec contract.TurnSpec, st *turnState, iter int, resp contract.ChatResponse) roundResult {
	// §6.1: drop same-id duplicates (keep first) + backfill empty ids with unique
	// synth ids BEFORE persist/dispatch, so the calls and their result rows pair
	// cleanly on replay.
	calls := dedupeCalls(resp.ToolCalls)
	for _, call := range calls {
		st.toolsRun = append(st.toolsRun, call.Name) // trail for the no-LLM salvage brief
	}

	// I2: assistant-with-calls persisted BEFORE dispatch. If THIS write fails we must
	// NOT dispatch — persisting result rows with no parent assistant row would leave
	// reverse orphans that orphan-repair (forward-only) can't heal and a strict
	// template rejects on replay. End the turn with a readable notice, surfaced as a
	// STORAGE error (roundErr → OutcomeError, like the user-row and history-read
	// doors) — not as a deliberate tool exit, which would read as a clean reply
	// downstream (recall feeds, failure learning).
	if err := c.store.AppendAssistantCalls(ctx, spec.Sid, resp.Content, calls, spec.Agent); err != nil {
		slog.Error("turn: persist assistant tool-calls failed — aborting dispatch", "sid", spec.Sid, "err", err)
		notice := i18n.T(persistFailKey, turnLang(spec))
		if aerr := c.store.AppendMessage(ctx, spec.Sid, "assistant", notice, spec.Agent); aerr != nil {
			slog.Warn("turn: persist abort notice failed", "sid", spec.Sid, "err", aerr)
		}
		if ferr := spec.Sink.Finish(notice); ferr != nil {
			slog.Warn("turn: sink finish (persist abort) failed", "sid", spec.Sid, "err", ferr)
		}
		return roundResult{kind: roundErr, err: fmt.Errorf("turn: persist assistant calls: %w", err)}
	}

	results := c.dispatchAll(ctx, spec, calls)

	// I3: every call gets exactly one result row (matched by call id), each through
	// the result pipeline (belt → scrub → summarize → empty-guard → lock-retry).
	var exit *contract.ToolExit
	for i, call := range calls {
		res := results[i]
		if res.ExitRequest != nil && exit == nil {
			exit = res.ExitRequest // first tool that asks to end wins
		}
		c.pipelineAndPersist(ctx, spec, call, res)
		c.recordToolOutcome(st, iter, spec, call, res) // §6.4 progress + §6.5 guards
		c.armRubricOnSkillView(st, spec, call, res)    // arm the rubric gate when a rubric-bearing skill is engaged (skill_view)
	}

	if exit != nil {
		// NB: a tool-authored exit reply is delivered WITHOUT the rubric gate, even when
		// a rubric-bearing skill armed it this round. Intentional: the only exit producers
		// (clarify / escalate_to_coo / stay_silent) are PROCESS replies — ask the user a
		// question, hand off a browser, end silently — not skill PRODUCTS, so judging them
		// against a skill's RUBRIC.md is a category error. If a future exit-producing tool
		// ever emits a skill product, route this path through c.rubricGate like the
		// final-reply door (the rubric gate above near len(st.rubricArmed) > 0).
		full := strings.TrimSpace(exit.Reply)
		if full == "" {
			// A tool exit with NO reply = stay silent (the stay_silent tool): deliver
			// nothing. releaseSink issues the contract's one terminal Finish("") with no
			// visible output — for an agent worker the return sink no-ops on empty, so the
			// dispatch chain still ends here (loop guard intact); for an email turn it
			// balances the coalescer counter. (The only empty-reply exit producer is
			// stay_silent; clarify/escalate_to_coo always carry a reply, path below.)
			slog.Debug("turn: silent tool exit — nothing delivered", "sid", spec.Sid)
			c.releaseSink(spec)
			return roundResult{kind: roundExit, exit: &turnExit{state: exitToolRequested}}
		}
		// §6.3.4: a tool-authored exit reply must still carry the honest footer if a
		// side-effect call failed un-recovered — same anti "claimed delivered" guarantee
		// as the final/salvage doors. The exit reply was never streamed, so persist and
		// finish are the same text (Finish REPLACES the window; any streamed preamble is
		// deliberately collapsed, like the salvage/notice doors).
		full += sideEffectFooter(st)
		if ferr := c.finalizeTurn(ctx, spec, full, full); ferr != nil {
			// Delivery FAILED — the user received nothing. Do NOT report a clean exit: the
			// exit would book a satisfied/delivered turn downstream. Surface an error so it
			// isn't counted as delivered (the reply is still persisted to history). (F107)
			return roundResult{kind: roundErr, err: fmt.Errorf("turn: exit reply computed but delivery failed: %w", ferr)}
		}
		return roundResult{kind: roundExit, exit: &turnExit{state: exitToolRequested, detail: full}}
	}
	return roundResult{kind: roundContinue}
}

// dispatchAll runs every tool call, parallel by default (capped) and serial for
// tools that opt out of concurrency, returning one result per call IN CALL ORDER.
// An unknown tool / a panic resolves to an error result — never a loop error (I4).
func (c *core) dispatchAll(ctx context.Context, spec contract.TurnSpec, calls []contract.ToolCall) []contract.ToolResult {
	results := make([]contract.ToolResult, len(calls))

	// §4.4 step 1: a progress line per call, IN ORDER, before any dispatch. The
	// sink drops these when the scope's trace pref is off (/trace), so they cost
	// nothing then; with trace on they are the "what bob is doing" stream.
	for _, call := range calls {
		spec.Sink.TraceDelta(traceCallLine(call))
	}

	if spec.Tools == nil {
		for i, call := range calls {
			results[i] = contract.ErrResult("no tools available: " + call.Name)
		}
	} else {
		var serial []int
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxParallelTools)
		for i, call := range calls {
			tool, ok := spec.Tools.Lookup(call.Name)
			if !ok {
				results[i] = contract.ErrResult("unknown tool: " + call.Name).WithHint("call only tools advertised this turn")
				continue
			}
			if tool.Serialize() {
				serial = append(serial, i)
				continue
			}
			wg.Add(1)
			go func(i int, tool contract.Tool, call contract.ToolCall) {
				defer wg.Done()
				// §4.4 cancel semantics: don't dispatch a tool the caller already
				// cancelled. Some side-effect tools (send_message.Run) never inspect
				// ctx, so without this guard a cancelled turn would still fire them
				// (message sent AFTER the user cancelled) (F60). Race the semaphore
				// against ctx.Done, and re-check after acquiring in case cancel landed
				// while queued.
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					results[i] = contract.ErrResult("skipped: turn cancelled")
					return
				}
				defer func() { <-sem }()
				if ctx.Err() != nil {
					results[i] = contract.ErrResult("skipped: turn cancelled")
					return
				}
				results[i] = c.dispatchOne(ctx, spec, tool, call)
			}(i, tool, call)
		}
		wg.Wait()
		// Serial tools after the parallel batch — never concurrent with anything.
		for _, i := range serial {
			if ctx.Err() != nil { // same cancel guard as the parallel path (F60)
				results[i] = contract.ErrResult("skipped: turn cancelled")
				continue
			}
			tool, _ := spec.Tools.Lookup(calls[i].Name)
			results[i] = c.dispatchOne(ctx, spec, tool, calls[i])
		}
	}

	// §4.4 step 5: a status line per result, IN ORDER, emitted by THIS (main)
	// goroutine — so trace output never interleaves across the parallel tools.
	for i, call := range calls {
		spec.Sink.TraceDelta(traceResultLine(call.Name, results[i]))
	}
	return results
}

// traceCallLine renders the §4.4 step-1 progress annotation: the symbol + the tool
// being called + its arguments (collapsed to one line, capped at 120 bytes). The
// args are kept so a trace reader can judge whether the call is the RIGHT one;
// language-neutral (tool name + raw args, no prose).
func traceCallLine(call contract.ToolCall) string {
	return "🔧 " + call.Name + "(" + truncateBytes(collapseLine(call.Arguments), 120) + ")"
}

// traceResultLine renders the §4.4 step-5 status annotation: ✓ + the tool on
// success (the symbol IS the outcome — no prose), ✗ + the tool + the error (first
// 80 bytes) on failure, since a failure's reason is the one thing worth surfacing.
func traceResultLine(name string, res contract.ToolResult) string {
	if res.OK {
		return "✓ " + name
	}
	return "✗ " + name + " — " + truncateBytes(collapseLine(res.Error), 80)
}

// collapseLine squashes a multi-line value into one line so a trace annotation
// stays a single line.
func collapseLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "\t", " ")
}

// dispatchOne runs one tool, isolating a panic into an error result so one bad
// tool can't take down the round or its siblings.
func (c *core) dispatchOne(ctx context.Context, spec contract.TurnSpec, tool contract.Tool, call contract.ToolCall) (res contract.ToolResult) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("turn: tool panicked", "sid", spec.Sid, "tool", call.Name, "panic", r)
			res = contract.ErrResult("tool panicked: " + call.Name)
		}
	}()
	tc := contract.ToolContext{
		Sid: spec.Sid, Scope: spec.Scope, Sink: spec.Sink, UserText: spec.UserText, Channels: spec.Channels, Credentials: spec.Credentials, Skills: spec.Skills,
		Attachments: spec.Attachments,
		Caller:      spec.Caller,                                          // §4: relayed verbatim for identity-aware tools (recall_memory)
		Sub:         subRunner{core: c, parent: spec, depth1: spec.IsSub}, // §7: refusing inside a sub-turn
		History:     spec.History,                                         // read-only log view: delegator's (ShareHistory sub) or own (compacted main turn)
	}
	return tool.Run(ctx, tc, json.RawMessage(call.Arguments))
}
