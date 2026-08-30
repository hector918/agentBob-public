package turn

import (
	"context"
	"log/slog"
	"sync"

	"agentbob/contract"
	"agentbob/i18n"
)

// This file is the LOOPING loop driver (docs/turn-driver-split.md §4): the
// convergence-driven work policy over the shared round kernel (20-round.go) and
// guard family. The designed exit is a CONVERGED product — the model claims done
// (roundFinal) with any armed acceptance (the round's rubric gate) passed. The
// round budget degrades to a fuse: hitting it is pathological, and the shared
// teardown in Run salvages an honest brief rather than pretending completion.
// The guard family applies unchanged — it is pace-invariant (80-nudge.go), so a
// stalled looping turn still dies at stale 14 however large the fuse; only a
// turn that keeps genuinely progressing may run deep.
//
// Sink policy: QUIET — a work turn runs tool rounds for minutes, and streaming
// every preamble floods the chat. Per-channel policy lives on quietSink (below).

// loopingFuseCap is the looping driver's default round fuse; spec.IterCap > 0
// overrides (the flow may bound a turn tighter). A var only so tests can lower
// it; production never reassigns.
var loopingFuseCap = 500

// driveLooping runs the convergence-driven round loop. Same return contract as
// driveRegular: (result, true) when the turn ended through an in-loop door;
// (zero, false) when the fuse blew or a guard set st.exit — the caller's shared
// teardown (iter-cap default → cancel door → salvage) then finishes the turn.
//
// The loop skeleton mirrors driveRegular deliberately (the split's isolation:
// exit-semantics evolve here without regular ever seeing the diff); the round
// kernel and guards stay single-copy in their own files.
func (c *core) driveLooping(ctx context.Context, spec contract.TurnSpec, st *turnState) (contract.TurnResult, bool) {
	// Quiet wrap on the driver's own spec copy: every round (streaming, tool
	// preambles, in-round finalize doors) renders through quietSink. Run's
	// teardown keeps the RAW sink — fine either way: nothing was streamed, so a
	// salvage Finish renders whole from offset 0 (Sink contract holds trivially).
	qs := &quietSink{inner: spec.Sink}
	spec.Sink = qs
	// Held pictures are released on the way out, whatever the way out is. The wrap
	// lives on this spec COPY, so every path that leaves through Run's shared
	// teardown — fuse, guard, loop-top cancel, salvage — finishes on the RAW sink and
	// would never run quietSink.Finish. Unwinding covers the one that no explicit
	// hook can: a panic, which would otherwise strand the picture AND leave its WAL
	// record claimed, blinding the recovery sweep to the very record it exists for.
	// A no-op on the paths where Finish already drained.
	defer qs.flushPictures()
	// 内置 (docs/advisor.md): the advisor is PART OF the looping turn — standard
	// equipment the driver (the POLICY layer) arms, not a warrant-granted user
	// capability, and no longer a tool the model can call. Arming is a state stamp,
	// NOT a spec.Mode read: Mode is consumed exactly once at the core's entry
	// dispatch, and the round kernel / guard family stay pace-invariant. It gates
	// both harness triggers — the stall diagnosis below and the delivery review at
	// the final-reply door (85-rubric.go). Regular turns never get either.
	st.advisorArmed = true

	limit := loopingFuseCap
	if spec.IterCap > 0 {
		limit = spec.IterCap
	}
	base := limit // rubric redo grants another `base` — same mechanics as regular
	for iter := 0; iter < limit; iter++ {
		if ctx.Err() != nil {
			st.exit = &turnExit{state: exitCancelled}
			break
		}
		if st.exit != nil { // a guard/tool set the exit last round (tools: step 3)
			break
		}
		// §6.9 in-place compaction — a deep work turn leans on this every round;
		// the returned replay is reused by the round (one history read per round).
		replay := c.maybeCompressInPlace(ctx, spec)
		// Compacted session → arm the own-log read-back (pull 化 P1): the summary
		// is a table of contents; read_conversation_log pulls originals back.
		c.maybeArmHistoryTool(&spec, replay)
		// §6.3 loop-top nudge push + progress-budget exit (pace-invariant ladder).
		if ex := c.loopTopNudge(spec, st, iter); ex != nil {
			st.exit = ex
			break
		}
		// User-nudge fast-lane (see driveRegular): at most ONE busy-arrived message
		// folded into the turn as a real user row. It matters MORE here than on a chat
		// reply — a long work turn is exactly when the user interjects, and this
		// driver's sink is quiet, so an unanswered nudge leaves no visible trace at all.
		replay = c.foldUserNudge(ctx, &spec, st, replay)
		// docs/advisor.md §2: a stall of exactly advisorStale rounds buys ONE advisor
		// consult, whose diagnosis rides the advisor_note layer into THIS round — the
		// harness owns the trigger because staleness is a harness fact the model
		// cannot see. Placed after the nudge ladder so a turn already exiting on the
		// budget guard never pays for a consult it can't use.
		c.maybeAdvisorDiagnose(ctx, spec, st, iter)
		r := c.runRound(ctx, spec, st, iter, replay)
		switch r.kind {
		case roundFinal:
			// Convergence: the model declared done and the round's acceptance gate
			// (rubric, when armed) passed. The product just shipped via Finish.
			return contract.TurnResult{Reply: r.text, Usage: st.usage, Outcome: contract.OutcomeFinal}, true
		case roundExit:
			return contract.TurnResult{Reply: r.exit.detail, Usage: st.usage, Outcome: r.exit.state.outcome(), RejectedProduct: r.exit.rejected}, true
		case roundErr:
			// ctx-gated cancel/error disambiguation — same rationale as
			// driveRegular (F106: a provider's inner hardTimeout is a backend
			// hang, not a user stop).
			if ctx.Err() != nil {
				return contract.TurnResult{Err: r.err, Usage: st.usage, Outcome: contract.OutcomeCancelled}, true
			}
			return contract.TurnResult{Err: r.err, Usage: st.usage, Outcome: contract.OutcomeError}, true
		case roundOversized:
			// Prompt exceeds the model window even before proactive compaction
			// caught it — force-compact and retry the round on the shrunk window.
			if !c.forceCompact(ctx, spec) {
				notice := i18n.T(contextTooLongKey, turnLang(spec))
				_ = c.finalizeTurn(ctx, spec, notice, notice)
				return contract.TurnResult{Usage: st.usage, Outcome: contract.OutcomeDegraded}, true
			}
			continue
		case roundContinue:
			if r.resetBudget {
				// Acceptance-gate redo → fresh work budget. Bounded by whichever gate
				// asked: maxRubricRetries (rubric) or maxReviewRetries (advisor review,
				// docs/advisor.md §4) — never both on one delivery, they are either/or.
				// The §6.10 struggle counter follows the watermarks (see driveRegular).
				st.lastProgressIter, st.lastProductiveIter = iter, iter
				st.failedSinceProgress = 0
				limit = iter + 1 + base
			}
			continue
		}
	}
	return contract.TurnResult{}, false
}

// quietSink is the looping driver's sink policy: rounds render nothing live.
// ContentDelta is dropped (tool-round preambles never hit the wire); TraceDelta,
// SendFile and LastSent pass through to the wrapped rendering sink; Finish
// delivers the converged product in one piece. Implementing
// contract.BareProductSink makes the round kernel hand Finish the bare trimmed
// reply — never the sunk preamble accumulation — riding the same hardened path
// as the sub-turn captureSink and the agora return sink (finalReply /
// finalizePartial / streamedReply all already gate on isBareProduct).
//
// It also HOLDS pictures (contract.SinkPictureHolder — the policy lives there). No
// picture reaches the chat until the turn's outcome is settled; then all of them go,
// in one burst. Attempts a gate rejected go too: nobody can tell the model's third
// try from its first without eyes, so the choice is the user's to make.
type quietSink struct {
	inner contract.Sink

	mu   sync.Mutex
	held []heldPicture
}

// heldPicture is one picture waiting for the turn to end. onSent belongs to the
// producer (the WAL record's release, for image_create) and runs exactly once, when
// the send has been attempted — never if the picture is never sent at all.
type heldPicture struct {
	path    string
	caption string
	onSent  func(error)
}

func (q *quietSink) ContentDelta(string)    {}
func (q *quietSink) TraceDelta(text string) { q.inner.TraceDelta(text) }

// Finish releases the held pictures first, then ends the turn as usual — so they
// arrive BEFORE the closing text that talks about them.
//
// Finish is the flush point for the doors the driver closes itself (a converged
// product, a delivery past the retry cap). It is NOT the whole story: salvage and
// the loop-top cancel finish on Run's raw sink and never reach this method, which is
// why driveLooping also defers the drain. Both are idempotent, and between them no
// exit can swallow a picture.
//
// What matters here is the negative: an acceptance-gate redo is not an exit and
// never calls Finish, so held pictures survive a redo without that being written
// down as a separate rule.
func (q *quietSink) Finish(full string) error {
	q.flushPictures()
	return q.inner.Finish(full)
}

func (q *quietSink) SendFile(path, caption string) error {
	return q.inner.SendFile(path, caption)
}

// HoldPicture (contract.SinkPictureHolder): keep it until the turn ends.
func (q *quietSink) HoldPicture(path, caption string, onSent func(error)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.held = append(q.held, heldPicture{path: path, caption: caption, onSent: onSent})
}

// flushPictures sends everything held, in the order it was produced, and tells each
// producer how its own picture fared. Drained under the lock before any send, so the
// second call — Finish and the driver's deferred drain both run on most paths — is a
// no-op rather than a duplicate send and a duplicated release.
//
// Nothing here may abort the rest. A failed send is logged and moved past, and a
// producer's callback runs behind a recover: the callbacks retire write-ahead-log
// records, and one panicking producer must not strand every OTHER picture in a
// drained-but-unsent list where no second flush will ever find them.
func (q *quietSink) flushPictures() {
	q.mu.Lock()
	held := q.held
	q.held = nil
	q.mu.Unlock()

	for _, p := range held {
		err := contract.DeliverPictureTo(q.inner, p.path, p.caption)
		if err != nil {
			slog.Warn("turn: held picture could not be sent", "path", p.path, "err", err)
		}
		q.release(p, err)
	}
}

// release runs one producer's completion callback, containing a panic in it.
func (q *quietSink) release(p heldPicture, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("turn: held picture's release callback panicked", "path", p.path, "panic", r)
		}
	}()
	if p.onSent != nil {
		p.onSent(err)
	}
}

// SendPhoto (contract.SinkPhotoSender): quiet is a policy about LIVE TEXT, not
// about the shape of an attachment's delivery. Without this pass-through the
// wrapper silently answers "I cannot send pictures" for every sink it wraps, and
// DeliverPictureTo degrades a looping turn's pictures to plain attachments —
// invisible, because the fallback is a legitimate send. Route through
// DeliverPictureTo rather than inner.SendPhoto so the inner sink's own
// can't-send-photos fallback stays written in exactly one place.
func (q *quietSink) SendPhoto(path, caption string) error {
	return contract.DeliverPictureTo(q.inner, path, caption)
}
func (q *quietSink) LastSent() string { return q.inner.LastSent() }

// BareProductFinish (contract.BareProductSink marker): ContentDelta above
// renders nothing, so Finish's full must be the bare product.
func (q *quietSink) BareProductFinish() {}

// silentPartialFinish (silentPartialSink marker): unlike the machine-facing
// bare sinks, this wrapper's Finish reaches a live human chat — a cancel/break
// partial that nobody saw must be dropped, never delivered as the product.
func (q *quietSink) silentPartialFinish() {}

var (
	_ contract.Sink              = (*quietSink)(nil)
	_ contract.BareProductSink   = (*quietSink)(nil)
	_ contract.SinkPhotoSender   = (*quietSink)(nil)
	_ contract.SinkPictureHolder = (*quietSink)(nil)
	_ silentPartialSink          = (*quietSink)(nil)
)
