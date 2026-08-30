// model/45-chat-stream-watch.go — the stream-first tool-round entry.
//
// Pool's existing Chat() does block-mode dispatch — useful for side-LLM
// calls that only want a clean ChatResponse, but blind to mid-flight
// pathology in the model's output. ChatStreamWatch rides the shared
// failover driver (40-chat.go, same candidate iteration as Chat)
// with three additions:
//
//  1. A StreamAccumulator builds the eventual ChatResponse incrementally
//     from StreamEvents (text concatenation + tool_call reconstruction
//     from per-Index ToolDelta events).
//  2. An optional StreamWatcher hook inspects each event + the running
//     accumulator state; returning a non-nil error cancels the stream
//     and surfaces the watcher's error wrapped back to the caller.
//  3. The provided default ToolArgsBloatWatcher catches the
//     "model is filling a tool_call.arguments JSON with thousands of
//     chars and never closing it" pattern — the failure mode that
//     motivated Phase 1 of the stream-first arc.
//
// Why a separate file: this is a meaningful surface (~250 LOC including
// the watcher + accumulator + builtin) that doesn't naturally belong
// in 40-chat.go's already-large body. Cross-file refs use the same
// pick/acquire/release/recordError helpers from 10-multipool.go.
package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"agentbob/contract"
	"agentbob/leaf/model/providers"
)

// ErrStreamWatcherTripped is returned (wrapped) from ChatStreamWatch
// when a watcher returns a non-nil error. Callers errors.Is-check.
var ErrStreamWatcherTripped = errors.New("model pool: stream watcher tripped")

// ErrToolArgsBloat is what ToolArgsBloatWatcher returns when an
// in-flight tool_call's arguments exceed the size threshold without
// closing. Wraps BOTH ErrStreamWatcherTripped (the pool's non-retryable
// branch returns it to the caller — every entry would bloat the same way,
// no point failing over) AND contract.ErrToolArgsBloat (so the turn core,
// which can't import this package, recognizes the cause across the module
// boundary and runs the §6.8 chunk-retry ladder rather than a generic
// model-unavailable notice).
var ErrToolArgsBloat = fmt.Errorf("model pool: tool_call arguments grew past bloat threshold: %w: %w", ErrStreamWatcherTripped, contract.ErrToolArgsBloat)

// ToolArgsBloatThreshold is the rune ceiling for any single in-flight
// tool_call's accumulated arguments JSON before ToolArgsBloatWatcher
// trips. The watcher exists to abort BEFORE the backend's max_tokens cap
// fires mid-arguments + emits an opaque parse-error cascade.
//
// Calibration (revised — the original 3500 false-tripped):
// tool calls only ever run on llm-kind chat models, which carry the
// 8192-token output cap. The truncation ceiling is therefore ~8192
// runes for CJK content (modern BPE tokenizers carry Han/kana/hangul at
// ~1 token/rune — NOT the 3-4 chars/token the original 3500 assumed).
// A legitimate Chinese `##` section serialises to ~3000-3500 args-runes,
// so 3500 was landing right on top of well-formed sections (observed:
// args_runes=3501/3502 killing real patch(append=…) chunks, derailing
// the multi-section write before deliver_file). 7000 leaves ~1200
// runes/tokens of margin under the cap while still catching the genuine
// runaway — the model packing an entire markdown doc into one
// patch(append=…) / patch(diff=…) call (8k-30k+ runes).
//
// NOTE: this assumes tool-calling entries keep a ≥~8192 output cap. If
// an admin sets a chat model's max_output_tokens well below that, the
// principled move is to derive this threshold from the entry's
// EffectiveOutputCap instead of a constant.
const ToolArgsBloatThreshold = 7000

// ToolArgsBloatWatcher is the default StreamWatcher for tool rounds.
// Trips when any tool_call's accumulated arguments string exceeds
// ToolArgsBloatThreshold rune count. The model's "correct" behaviour
// is to call patch(append=...) in multiple turns with one section per
// call (per the write-markdown skill); a single tool_call past the
// threshold means the model dumped far too much in one go + is on track
// for the max_tokens truncation.
func ToolArgsBloatWatcher(_ contract.StreamEvent, accum *contract.StreamAccumulator) error {
	if accum == nil {
		return nil
	}
	for _, tc := range accum.ToolCalls {
		if utf8.RuneCountInString(tc.Arguments) > ToolArgsBloatThreshold {
			return fmt.Errorf("%w (tool=%s, args_runes=%d, threshold=%d)",
				ErrToolArgsBloat, tc.Name, utf8.RuneCountInString(tc.Arguments), ToolArgsBloatThreshold)
		}
	}
	return nil
}

// ToolCallMarkupWatcher trips when accumulated CONTENT carries the
// sidecar's unrecoverable tool-call markup (`<|tool_call>` form). Unlike
// the conversation-level bloat watcher, this is ENTRY-LEVEL: it returns
// providers.ErrMalformedToolCallMarkup directly (NOT wrapped in
// ErrStreamWatcherTripped), so streamOneWatch records it against the entry
// and the pool fails over to another candidate (a different backend won't
// emit it) — UNLESS visible content already streamed to the user (committed),
// in which case streamOneWatch surfaces it as ErrStreamCommitted instead of
// re-streaming a second response into the same sink (F71). (A plain-text
// round DOES feed the user sink live, so "nothing was shown" is not a safe
// assumption — the committed gate is what makes the abort safe.)
func ToolCallMarkupWatcher(_ contract.StreamEvent, accum *contract.StreamAccumulator) error {
	if accum == nil {
		return nil
	}
	if providers.HasResidualToolCallMarkup(accum.Text) {
		return providers.ErrMalformedToolCallMarkup
	}
	return nil
}

// ToolRoundWatcher is the composite StreamWatcher for tool rounds: the
// entry-level markup check first (fail over to a clean backend), then the
// conversation-level bloat check (return to the caller for chunk-retry
// salvage). The two trips are dispatched by sentinel in streamOneWatch.
func ToolRoundWatcher(ev contract.StreamEvent, accum *contract.StreamAccumulator) error {
	if err := ToolCallMarkupWatcher(ev, accum); err != nil {
		return err
	}
	return ToolArgsBloatWatcher(ev, accum)
}

// ChatStreamWatch implements contract.ModelPool. Same candidate iteration +
// failover as Chat (the shared failover driver in
// 40-chat.go) but routes each attempt through streamOneWatch. Each
// per-candidate stream feeds events to the watcher.
//
// Conversation-level watcher trips (bloat) wrap ErrStreamWatcherTripped
// — no point trying another candidate (they'd all bloat the same way),
// so they are non-retryable: the driver returns streamOneWatch's
// response (which carries the serving entry's Name for forensics — B30)
// to the caller. Entry-level trips (malformed tool-call markup) do NOT
// wrap it: streamOneWatch already recordError'd the entry, and the
// error falls through to the driver's exclude+failover path so the pool
// retries a clean backend. Same posture as the
// IsToolArgsTruncatedErr filter in RecordError.
func (p *MultiPool) ChatStreamWatch(ctx context.Context, req contract.ModelRequest, msgs []contract.Message, watcher contract.StreamWatcher) (contract.ChatResponse, error) {
	// Tool rounds get the pool's builtin structural protection (ToolRoundWatcher =
	// entry-level markup fail-over + conversation-level args-bloat abort) composed
	// AHEAD of the caller's watcher — restoring the "provided default
	// ToolArgsBloatWatcher" this package was designed around. The turn core (the only
	// caller) can't import this package, so it passes only its own content/degenerate
	// watcher; without composing here, a model packing a whole doc into one
	// tool_call's arguments would truncate at max_tokens instead of being aborted
	// mid-stream. Non-tool-round calls (no Tools advertised) are unaffected.
	effective := watcher
	if len(req.Tools) > 0 {
		effective = composeWatchers(ToolRoundWatcher, watcher)
	}
	return failover(ctx, p, req, failoverOpts{
		logPrefix:       "model pool (stream-watch)",
		armFirstContent: true,
		nonRetryable: func(err error) bool {
			return errors.Is(err, ErrStreamWatcherTripped) || errors.Is(err, ErrStreamCommitted)
		},
	}, func(row *entryRow, fcDeadline time.Duration, lastResort bool) (contract.ChatResponse, error) {
		return p.streamOneWatch(ctx, req, msgs, row, effective, fcDeadline, lastResort)
	})
}

// composeWatchers chains StreamWatchers in order — the first non-nil error
// short-circuits (nil elements skipped). Used to run the pool's builtin
// ToolRoundWatcher ahead of a caller's watcher on tool rounds.
func composeWatchers(ws ...contract.StreamWatcher) contract.StreamWatcher {
	return func(ev contract.StreamEvent, accum *contract.StreamAccumulator) error {
		for _, w := range ws {
			if w == nil {
				continue
			}
			if err := w(ev, accum); err != nil {
				return err
			}
		}
		return nil
	}
}

// failStream is the shared failure epilogue for streamOneWatch's four abort
// sites (ev.Err, entry-level markup watcher trip, sidecar residual markup,
// plain residual markup): record err against the entry, then honor the F71
// committed invariant. Once user-VISIBLE content has streamed to the sink
// (committed), failing over would re-stream a second response into the SAME
// sink — a garbled duplicate — so surface err wrapped in ErrStreamCommitted,
// carrying the serving entry's Name for forensics, and let the driver return it
// (no failover). Before any visible content, return the plain err so the driver
// fails over to a clean candidate. NOTE: the conversation-level bloat abort
// does NOT route through here — it must not recordError (not a health signal).
func (p *MultiPool) failStream(ctx context.Context, row *entryRow, committed bool, err error) (contract.ChatResponse, error) {
	p.recordError(ctx, row, err)
	if committed {
		return contract.ChatResponse{Model: row.info.Name}, fmt.Errorf("%w: %w", ErrStreamCommitted, err)
	}
	return contract.ChatResponse{}, err
}

// streamOneWatch runs one streaming call against row, drains the
// channel into a ChatResponse via a StreamAccumulator, and invokes
// watcher (if non-nil) on every event. Releases row's concurrency
// slot on return. Cancels the stream early when the watcher returns
// a non-nil error.
//
// firstContentDeadline, when > 0, bounds the wait for the FIRST content
// event (Text or ToolDelta). If the entry produces nothing within it the
// attempt is cancelled, recorded as a liveness failure (errStreamStall),
// and an error returned so ChatStreamWatch fails over. 0 disables the
// deadline (the last candidate / a pinned / all-saturated entry — let it
// run to the provider's streamHardTimeout). Once first content arrives
// the timer is disabled; post-content stalls are streamHardTimeout's job.
//
// busyRetry (the driver's last-resort candidate) covers the stream OPEN only:
// a backend that is busy / mid-restart refuses there, before a single event
// exists, so waiting it out is free. Once the channel is open the committed-
// stream invariant owns the failure — a broken stream is never silently
// re-opened underneath a sink that may already have shown content.
func (p *MultiPool) streamOneWatch(ctx context.Context, req contract.ModelRequest, msgs []contract.Message, row *entryRow, watcher contract.StreamWatcher, firstContentDeadline time.Duration, busyRetry bool) (contract.ChatResponse, error) {
	defer p.release(row)
	slog.Debug("pool dispatched (stream-watch)", "entry", row.info.Name, "model", row.info.Model,
		"requires", req.Requires, "prefer", req.Prefer, "tools", len(req.Tools), "msgs", len(msgs))

	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	cctx := providers.WithEntryName(streamCtx, row.info.Name)
	ch, err := withBusyRetry(ctx, row, busyRetry, func() (<-chan contract.StreamEvent, error) {
		return row.chatter.ChatStream(cctx, row.info.Model, msgs, req.Tools)
	})
	if err != nil {
		p.recordError(ctx, row, err)
		return contract.ChatResponse{}, err
	}

	var (
		accum      contract.StreamAccumulator
		toolArgs   = map[int]*strings.Builder{}   // per-Index running arg buffers
		toolMeta   = map[int]*contract.ToolCall{} // per-Index metadata (Name+ID)
		toolOrder  []int                          // first-seen ordering for stable output
		usage      contract.Usage
		watcherErr error
	)

	// First-content stall guard: arm a timer that fires if the entry
	// accepts the request then produces no content (Text or ToolDelta)
	// within firstContentDeadline. Disabled (fcTimeout stays nil) when
	// the deadline is 0, and disarmed the moment the first content event
	// is seen — after that, post-content stalls are streamHardTimeout's
	// job.
	var (
		fcTimer    *time.Timer
		fcTimeout  <-chan time.Time
		gotContent bool // ANY first content (text or tool delta) — clears the stall guard
		committed  bool // user-VISIBLE content emitted — gates the no-failover decision
	)
	if firstContentDeadline > 0 {
		fcTimer = time.NewTimer(firstContentDeadline)
		defer fcTimer.Stop()
		fcTimeout = fcTimer.C
	}

readLoop:
	for {
		var (
			ev contract.StreamEvent
			ok bool
		)
		select {
		case <-ctx.Done():
			// Caller cancelled (e.g. /close-session) — not the entry's
			// fault, no recordError. Cancel + drain so the provider
			// goroutine exits even if it's parked on a full channel send.
			// This is the escape hatch for a Chatter that opens the
			// stream then stalls without honouring ctx: on the
			// fcDeadline==0 paths (pinned / last candidate / all-
			// saturated) the loop would otherwise block on <-ch forever,
			// leaking the goroutine AND the entry's concurrency slot
			// (D19).
			streamCancel()
			for range ch {
			}
			return contract.ChatResponse{}, ctx.Err()
		case <-fcTimeout:
			// Tie-break: a first-content event may have become ready on ch at the SAME
			// tick fcTimeout fired (Go select picks a ready case at random). Re-check ch
			// non-blockingly before declaring a stall, so on-time content isn't misrecorded
			// as errStreamStall + a wasted failover hop + an unfair cooling strike.
			select {
			case ev, ok = <-ch:
				if !ok {
					break readLoop // stream closed
				}
				// content arrived in time → fall through to process it below
			default:
				// Genuine stall — entry accepted the request then produced no first content
				// within firstContentDeadline. A real liveness failure (unlike a watcher
				// trip): record it, cancel + drain so the provider goroutine exits, and
				// return so ChatStreamWatch fails over. Wrap errStreamStall so the transport
				// classifier counts it toward the lifecycle breaker (D2).
				err := fmt.Errorf("entry %q: %w: %s", row.info.Name, errStreamStall, firstContentDeadline)
				p.recordError(ctx, row, err)
				streamCancel()
				for range ch {
				}
				return contract.ChatResponse{}, err
			}
		case ev, ok = <-ch:
			if !ok {
				break readLoop // stream closed
			}
		}

		if ev.Text != "" {
			accum.Text += ev.Text
		}
		if ev.Reasoning != "" {
			accum.Reasoning += ev.Reasoning
		}
		if td := ev.ToolDelta; td != nil {
			// First time we see this Index: stash metadata + order.
			if _, seen := toolMeta[td.Index]; !seen {
				toolMeta[td.Index] = &contract.ToolCall{ID: td.ID, Name: td.Name}
				toolArgs[td.Index] = &strings.Builder{}
				toolOrder = append(toolOrder, td.Index)
			} else {
				// Late-arriving ID / Name (some backends omit on early
				// deltas — patch in when finally present).
				meta := toolMeta[td.Index]
				if meta.ID == "" && td.ID != "" {
					meta.ID = td.ID
				}
				if meta.Name == "" && td.Name != "" {
					meta.Name = td.Name
				}
			}
			toolArgs[td.Index].WriteString(td.ArgumentsDelta)
			// Refresh accum.ToolCalls snapshot so watcher sees current
			// state. Could optimise to update only the changed index;
			// for now rebuild — N is typically 1-2.
			accum.ToolCalls = accum.ToolCalls[:0]
			for _, idx := range toolOrder {
				meta := toolMeta[idx]
				accum.ToolCalls = append(accum.ToolCalls, contract.ToolCall{
					ID:        meta.ID,
					Name:      meta.Name,
					Arguments: toolArgs[idx].String(),
				})
			}
		}
		// First content (text, a tool delta, or THINKING) clears the stall guard —
		// from here streamHardTimeout owns any later stall, not us. Reasoning counts:
		// the guard exists to catch a backend that never answers, and one streaming
		// thinking tokens is demonstrably alive. Leaving it out would time out a
		// healthy model whose think runs past firstContentTimeout — exactly the
		// entries /thinking routes to.
		if !gotContent && (ev.Text != "" || ev.Reasoning != "" || ev.ToolDelta != nil) {
			gotContent = true
			if fcTimer != nil {
				fcTimer.Stop()
				fcTimeout = nil
			}
		}
		// "committed" tracks user-VISIBLE content, distinct from the stall guard: only
		// plain text with NO tool call in flight can have reached the sink (a tool-call
		// round streams nothing — its text is preamble). A pure tool round must stay
		// retriable on a mid-stream error (failing over re-streams nothing), so it must
		// NOT be classified committed. (A rubric-armed turn buffers text too, so this can
		// still over-count there — the pool can't see the turn's streamLive choice; that
		// narrower case is a separate follow-up.)
		if ev.Text != "" && len(accum.ToolCalls) == 0 {
			committed = true
		}
		if ev.Usage.OutputTokens > 0 || ev.Usage.InputTokens > 0 {
			usage = ev.Usage
		}
		if watcher != nil {
			if werr := watcher(ev, &accum); werr != nil {
				watcherErr = werr
				streamCancel()
				// Drain the rest so the read goroutine can exit cleanly.
				for range ch {
				}
				break readLoop
			}
		}
		if ev.Err != nil {
			// A pure tool round (gotContent but not committed) stays retriable —
			// nothing reached the user; committed → surface (no failover).
			return p.failStream(ctx, row, committed, ev.Err)
		}
		if ev.Done {
			break readLoop
		}
	}

	if watcherErr != nil {
		// ENTRY-level trip (malformed tool-call markup): this backend
		// produced unstructurable output — a different entry won't. Record
		// it against the row (consecutiveFails → marked dead at the
		// threshold) and return a PLAIN error (not wrapped in
		// ErrStreamWatcherTripped) so ChatStreamWatch's loop fails over to
		// the next candidate instead of returning to the caller.
		if errors.Is(watcherErr, providers.ErrMalformedToolCallMarkup) {
			return p.failStream(ctx, row, committed, watcherErr)
		}
		// CONVERSATION-level abort (bloat): don't recordError — it's not a
		// backend health signal, and another entry would bloat the same way
		// (same posture as IsToolArgsTruncatedErr). Wrap with the
		// ErrStreamWatcherTripped sentinel so the loop returns to the caller
		// (no failover) regardless of what the watcher itself returned.
		// Carry the serving entry's Name so the caller's forensics
		// (degenerate.log served_model, bloat-salvage logs) can name the
		// entry that tripped (review #3 F8). Backfilled HERE — once, for
		// every path into streamOneWatch (main loop, pinned, saturated-wait,
		// last resort) — and in the same namespace (entry Name) every
		// success path uses for ChatResponse.Model (B30).
		if errors.Is(watcherErr, ErrStreamWatcherTripped) {
			return contract.ChatResponse{Model: row.info.Name}, watcherErr
		}
		return contract.ChatResponse{Model: row.info.Name}, fmt.Errorf("%w: %w", ErrStreamWatcherTripped, watcherErr)
	}

	// Normalize empty tool_call arguments to "{}" so no-param tools
	// (anthropic emits no input_json_delta; openai-compat may send only
	// the name) match the block path (providers/30-anthropic.go:381,
	// providers/35-toolcall-extract.go) instead of leaving Arguments=""
	// which is invalid JSON for the dispatch-side json.RawMessage.
	for i := range accum.ToolCalls {
		if accum.ToolCalls[i].Arguments == "" {
			accum.ToolCalls[i].Arguments = "{}"
		}
	}

	resp := contract.ChatResponse{
		Content:   accum.Text,
		Reasoning: accum.Reasoning,
		ToolCalls: accum.ToolCalls,
		Usage:     usage,
		Model:     row.info.Name,
	}

	// Inline-tool-call salvage — mirrors the block path's step in
	// providers/00-client.go (chatOnce, ~line 522) so streaming and
	// non-streaming behave identically. Qwen / Hermes-style fine-tunes
	// emit <tool_call>{"name":…,"arguments":…}</tool_call> as PLAIN TEXT
	// content when the server template doesn't promote them to the
	// structured tool_calls field; the stream then carries them as Text
	// deltas, never as ToolDelta. Without this, the visible markup lands
	// in resp.Content and gets answered back verbatim to the user.
	//
	// Same guard as the block path: only when the structured field is
	// empty (no double-firing a tool_call that already arrived as
	// ToolDelta) AND the content actually produced an inline call — never
	// override real tool_calls, and strip the markup only when it parsed.
	if len(resp.ToolCalls) == 0 {
		if cleaned, inline := providers.ExtractInlineToolCalls(resp.Content); len(inline) > 0 {
			resp.Content = cleaned
			resp.ToolCalls = inline
		}
	}
	// Residual sidecar `<|tool_call>` markup the recovery couldn't structure.
	// Checked BEFORE recordChatSuccess so a rejected response never books a
	// phantom success (which would also reset consecutiveFails and defeat the
	// die-at-threshold contract). Mirrors the block path:
	//   - no usable tool_call → fail over (record against the entry); caught
	//     here for nil-watcher callers or a mid-stream slip-past.
	//   - a real tool_call WAS produced alongside the junk → keep the call,
	//     strip the residual so it isn't streamed/persisted.
	if providers.HasResidualToolCallMarkup(resp.Content) {
		if len(resp.ToolCalls) == 0 {
			return p.failStream(ctx, row, committed, providers.ErrMalformedToolCallMarkup)
		}
		resp.Content = providers.StripResidualToolCallMarkup(resp.Content)
	}
	// Ordinary `<tool_call>` markup the recovery couldn't structure (truncated
	// mid-body, or a non-JSON/non-XML body). With no usable tool_call, this
	// would leak the wire token to the user — fail over instead. When a real
	// tool_call WAS produced alongside it, ExtractInlineToolCalls was skipped
	// (structured field non-empty) so a leftover block / dangling open survives
	// — strip it rather than leak it (B8). Mirrors the block path in
	// providers/00-client.go.
	if providers.HasResidualPlainToolCallMarkup(resp.Content) {
		if len(resp.ToolCalls) == 0 {
			return p.failStream(ctx, row, committed, providers.ErrMalformedToolCallMarkup)
		}
		resp.Content = providers.StripResidualPlainToolCallMarkup(resp.Content)
	}

	// THINKING-ONLY (resolved): the backend billed output
	// tokens but the visible channel came back empty. Before the thinking channel
	// was decoded, "the model only ever thought" and "the model truly returned
	// nothing" were indistinguishable here, and the round kernel read either as an
	// empty reply and looped again on an UNCHANGED history (leaf/turn/20-round.go,
	// I5) — spinning the turn to its fuse while every leg was a clean HTTP 200.
	//
	// resp.Reasoning now separates them. Thinking-only FAILS the attempt so the
	// driver fails over to the next candidate — unlike a context overflow, a peer
	// does NOT hit this identically: the same prompt on an entry with thinking off
	// answers fine, which is exactly what failing over reaches for. Handing the
	// kernel an empty reply instead is what produced the spin.
	//
	// Deliberately NOT recordError (so no failStream): the backend streamed tokens,
	// it is demonstrably alive. What lost the race is this entry's CONFIG — thinking
	// on, against its output cap — and cooling it would pull the one entry /thinking
	// routes to out of rotation for five minutes over a single long think. Same
	// posture the prompt-level classes take in 15-errors.go, for the same reason:
	// don't punish an entry's health for a fault that isn't its health.
	//
	// "Empty" must mean what the ROUND KERNEL means by it: leaf/turn/20-round.go's
	// finalReply trims before testing, so a reply of "\n\n" — exactly what a
	// reasoning backend leaves behind when the think ends and the output cap lands
	// right after </think> — is empty downstream. Testing == "" here would let that
	// shape sail past as a clean success and reproduce the very spin this guard
	// exists to kill, just one whitespace byte later.
	//
	// The `committed` bar is deliberately NOT applied here, and that is safe by
	// construction rather than by luck. Reaching this branch requires no tool call,
	// so neither strip path above ran (both return early unless ToolCalls is
	// non-empty) and resp.Content is still exactly accum.Text. Trimmed-empty
	// therefore means the sink only ever received WHITESPACE — `committed` can be
	// true here, but only for bytes that render as nothing. ErrStreamCommitted
	// exists to stop a second reply landing under a VISIBLE partial one; there is
	// no visible partial to garble.
	if strings.TrimSpace(resp.Content) == "" && len(resp.ToolCalls) == 0 && (usage.OutputTokens > 0 || resp.Reasoning != "") {
		if resp.Reasoning != "" {
			slog.Warn("model: model only ever thought — visible content empty, whole reply went to the thinking channel; failing over (disable thinking on this entry or raise its output cap)",
				"entry", row.info.Name, "model", row.info.Model,
				"output_tokens", usage.OutputTokens, "reasoning_bytes", len(resp.Reasoning),
				"content_bytes", len(resp.Content), "path", "stream-watch")
			// Book the tokens even though the attempt failed: they were really spent,
			// and this is the one failure class whose trigger condition guarantees a
			// bill. NOT recordChatSuccess — that would also clear the exhaustion latch
			// and reset the fail counter for a call that answered nobody.
			p.recordChatTokens(ctx, row, usage, "stream-watch", 1)
			return contract.ChatResponse{}, fmt.Errorf("entry %q: %w", row.info.Name, ErrThinkingOnly)
		}
		slog.Warn("model: tokens generated but no visible content and no thinking either — truncated or empty reply",
			"entry", row.info.Name, "model", row.info.Model,
			"output_tokens", usage.OutputTokens, "path", "stream-watch")
	}

	// Clean success — fold liveness reset + usage into the shared
	// bookkeeping. streamOneWatch silently skipped this before,
	// starving /model + bob_model_usage of the main tool-round traffic.
	p.recordChatSuccess(ctx, row, usage, "stream-watch")
	// Cache affinity rides the same per-success point (mirrors chatOne): the
	// conversation's warm KV cache is on the entry that just served it.
	p.recordAffinity(req.AffinityKey, row.info.Name)

	return resp, nil
}
