package telegram

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"agentbob/contract"
)

// mediaGroupFlushTimeout — upper bound for waiting for an album's caption /
// remaining sibling updates. Telegram fans an album out as N separate
// Bot-API updates that share a media_group_id; the user's caption rides
// only on the first photo's update. In practice every sibling arrives
// within ~1s, but ~10s is a safe ceiling for network jitter / a missing
// caption-bearing update so we never strand a buffered album indefinitely.
//
// Exposed as a var (not a const) so tests can shorten it to ~100ms — the
// real timeout would make a 10s wait part of every test run.
var mediaGroupFlushTimeout = 10 * time.Second

// flushedStateTTL — how long we retain a groupState AFTER it has flushed
// so that a late sibling (telegram delivers album updates out-of-order;
// a sibling can land minutes after the captioned siblings already
// flushed) can still pick up the cached caption instead of arriving
// caption-less. 5 minutes is generously above the realistic delivery
// window without bloating the in-memory map on a busy chat — a typical
// chat sees a handful of albums per hour. After eviction a late sibling
// is treated as a fresh single-event album (the worst case is a
// missing caption on that one sibling, which downstream noCaptionStandIn
// covers).
//
// var (not const) so tests can shorten it without long real waits.
var flushedStateTTL = 5 * time.Minute

// mediaGroupBuffer accumulates the N separate Telegram updates that share
// a media_group_id and emits ONE event per sibling, each carrying the
// shared caption — NOT a single merged event with N attachments.
//
// Per-event emit rationale (reversal of the prior atomic-
// merge design): each sibling becoming its own turn gives the user
// crisply-bounded replies (one reply per photo) instead of one giant
// reply that the chat platform then has to slice into N messages,
// blurring which fragment belongs to which photo. The gateway then
// drains queued media-group events one-at-a-time (see
// pipeline-gateway/20-handle.go::drainSidAfterTurn) so the bounded-
// reply property survives all the way to the user.
//
// Flush triggers (whichever fires first):
//
//   - Caption arrival — a buffered group update carries a non-empty
//     Text: the caption is now known, so emit every buffered sibling
//     immediately, each with that caption stamped onto its .Text.
//   - Timeout — mediaGroupFlushTimeout fires for groups that never get
//     a caption (uncaptioned album) or whose caption-bearing update
//     was lost; siblings emit with empty .Text and downstream
//     noCaptionStandIn covers them.
//
// Post-flush, the groupState is RETAINED for flushedStateTTL with
// flushed=true so a late sibling (out-of-order delivery; minutes-late
// is observed in the wild) can still be stamped with the cached
// caption before being emitted. After the TTL the state is evicted
// and a late sibling is handled as a fresh single-event group.
//
// emit is the function that hands a finished MessageEvent to whatever
// runs next (production: a goroutine that pushes to the source's outChan;
// tests: a collector). It MUST be safe to call without the buffer's mu
// held — Submit / flushByTimeout / Close all release the lock before
// invoking emit so a back-pressured channel can never deadlock against
// another Submit waiting on mu.
type mediaGroupBuffer struct {
	mu     sync.Mutex
	groups map[string]*groupState
	emit   func(contract.MessageEvent)
	closed bool // set in Close; gates new TTL/flush timers so post-shutdown work is skipped
}

// groupState is the per-media-group accumulator.
//
// All fields are accessed only under mediaGroupBuffer.mu.
//
// Lifecycle:
//
//  1. First sibling arrives → state created, sib appended, timer armed.
//  2. More uncaptioned siblings arrive → appended to sibs.
//  3. Caption arrives OR timer fires → flushed=true, flushedAt=now,
//     sibs cleared (siblings already handed off to emit — accepted ones
//     downloaded + emitted, a dropped album's never), TTL eviction timer
//     armed (flushedStateTTL).
//  4. Late sibling arrives within TTL → emit immediately with cached
//     caption; state stays flushed.
//  5. TTL fires → state removed from b.groups.
type groupState struct {
	sibs      []bufferedSibling // buffered album siblings (event + deferred download)
	accepted  bool              // any sibling addressed the bot (group gate) — OR'd as siblings arrive; album dropped on flush if false
	caption   string            // non-empty once a captioned ev has been observed
	timer     *time.Timer
	ttlTimer  *time.Timer // armed post-flush; Close stops it so eviction can't fire past shutdown
	firstSeen time.Time
	flushed   bool      // set true after first flush; gates re-flush + selects late-sibling fast path
	flushedAt time.Time // when flush completed — for TTL eviction
}

// bufferedSibling is one album sibling held in a group: its event plus a DEFERRED
// download closure. The download runs ONLY at flush, and only if the album is ACCEPTED,
// so an un-addressed group album's photos are never fetched (L-D1). It is network I/O,
// so emitSiblings invokes it in the lock-free epilogue — NEVER under b.mu. nil download =
// a non-media sibling, or attachments already on ev (the non-album fast path).
type bufferedSibling struct {
	ev       contract.MessageEvent
	download func() []contract.Attachment
}

// newMediaGroupBuffer wires a buffer around an emit callback. The
// callback receives ONE event per album sibling (NOT a merged event)
// and MUST NOT block on the buffer's mu (Submit / flushByTimeout /
// Close drop the lock before calling).
func newMediaGroupBuffer(emit func(contract.MessageEvent)) *mediaGroupBuffer {
	return &mediaGroupBuffer{
		groups: make(map[string]*groupState),
		emit:   emit,
	}
}

// Submit is the only entry point for inbound events. Single events (no
// media_group_id) take a zero-latency fast path: emit straight through
// without ever touching the lock state. Group events accumulate in
// buffer; the captioned-ev arrival or the timeout flushes them.
//
// Late-sibling fast path: if the group has already flushed but still
// lives within its flushedStateTTL window, the new sibling is stamped
// with the cached caption and emitted immediately — no second buffer
// cycle, no waiting for another timeout.
//
// Concurrency: invoked from the Telegram onUpdate goroutine (one per
// inbound update — the SDK fan-out is parallel). Multiple concurrent
// Submits for different groups serialise briefly through mu but do real
// work (emit / store mutate) lock-free.
// accepted reports whether the sender addressed the bot (group @-mention / reply,
// or a DM). For an album it is OR'd across siblings: the @mention rides only the
// captioned sibling, so the WHOLE album is accepted iff ANY sibling addressed the
// bot — the onUpdate group gate defers album siblings here instead of dropping the
// caption-less ones. A non-album event is already gated upstream and emits as-is.
func (b *mediaGroupBuffer) Submit(ev contract.MessageEvent, accepted bool, download func() []contract.Attachment) {
	if ev.MediaGroupID == "" {
		// Fast path: non-album event bypasses every buffer concern with
		// zero latency. The vast majority of Telegram traffic is single
		// messages — they MUST NOT incur lock contention here. download is
		// nil here (a non-album event downloaded upstream); kept for symmetry.
		if download != nil {
			ev.Attachments = append(ev.Attachments, download()...)
		}
		b.emit(ev)
		return
	}

	b.mu.Lock()
	g, ok := b.groups[ev.MediaGroupID]
	if ok && g.flushed {
		// Late-sibling fast path: a captioned/timeout flush already ran
		// for this group; we kept the state alive for flushedStateTTL
		// specifically so this sibling can pick up the cached caption.
		// Stamp + emit + done — but only if the group is ADDRESSED (an
		// unaddressed album was dropped at flush; its late siblings drop too,
		// so their deferred download never runs). The late sibling's OWN
		// accepted flag still ORs in: the @mention rides only the captioned
		// sibling, which can itself be the late arrival (out-of-order
		// delivery) — dropping it because its caption-less siblings timed
		// out unaddressed would lose the one message that explicitly
		// addressed the bot. It revives the group so even-later siblings
		// survive too, and caches its caption for them. The siblings already
		// dropped at the timeout flush are unrecoverable (their deferred
		// downloads never ran) — accepted loss.
		g.accepted = g.accepted || accepted
		if g.caption == "" && strings.TrimSpace(ev.Text) != "" {
			g.caption = ev.Text
		}
		caption := g.caption
		groupAccepted := g.accepted
		b.mu.Unlock()
		if !groupAccepted {
			return
		}
		if caption != "" && ev.Text == "" {
			ev.Text = caption
		}
		b.emitSiblings([]bufferedSibling{{ev: ev, download: download}})
		return
	}
	if !ok {
		if b.closed {
			// Buffer already shutting down — refuse new groups so we
			// don't arm timers whose callbacks would fire past Close.
			b.mu.Unlock()
			return
		}
		g = &groupState{firstSeen: time.Now()}
		b.groups[ev.MediaGroupID] = g
		gid := ev.MediaGroupID // capture for the closure
		g.timer = time.AfterFunc(mediaGroupFlushTimeout, func() {
			b.flushByTimeout(gid)
		})
	}
	g.sibs = append(g.sibs, bufferedSibling{ev: ev, download: download})
	g.accepted = g.accepted || accepted // group accepted iff ANY sibling addressed the bot

	// A captioned ev means the user's text for this album is known —
	// flush right away. Stop the timer first so its callback won't
	// race and trigger a second flush (prepareLocked's flushed guard
	// would no-op it anyway, but stopping is cleaner).
	// A whitespace-only caption is not a real caption: trimming keeps the
	// has-caption check consistent with the onUpdate drop guard so a blank
	// caption neither flushes the album early nor gets stamped onto every
	// sibling's .Text (downstream noCaptionStandIn covers the empty case).
	captioned := strings.TrimSpace(ev.Text) != ""
	var toEmit []bufferedSibling
	if captioned {
		g.caption = ev.Text
		g.timer.Stop()
		toEmit = b.prepareLocked(ev.MediaGroupID, false)
	}
	b.mu.Unlock()

	// emit + the deferred download MUST run lock-free — a back-pressured channel send (or a
	// download's network I/O) would otherwise deadlock against any concurrent Submit on mu.
	b.emitSiblings(toEmit)
}

// emitSiblings runs each sibling's DEFERRED download (lock-free network I/O — it MUST NOT
// be invoked while holding b.mu) and emits the event. A nil download means the attachments
// are already on the event (non-album fast path) or the sibling carried no media. (L-D1)
func (b *mediaGroupBuffer) emitSiblings(sibs []bufferedSibling) {
	for _, s := range sibs {
		if s.download != nil {
			s.ev.Attachments = append(s.ev.Attachments, s.download()...)
		}
		b.emit(s.ev)
	}
}

// flushByTimeout fires from the per-group AfterFunc when no captioned
// ev has arrived inside mediaGroupFlushTimeout. The group has whatever
// siblings landed — each emits with an empty caption (downstream falls
// back to noCaptionStandIn) so the user still gets a reply per photo.
func (b *mediaGroupBuffer) flushByTimeout(gid string) {
	b.mu.Lock()
	toEmit := b.prepareLocked(gid, true)
	b.mu.Unlock()
	b.emitSiblings(toEmit)
}

// prepareLocked emits ONE event per buffered sibling (NOT a single
// merged event) and marks the group as flushed, retaining the state
// for flushedStateTTL so a late sibling can pick up the cached caption.
// Returns the slice the CALLER must emit in the lock-free epilogue.
//
// Returns nil (and is a no-op) if the group is already gone (race with
// Close) or already flushed (race between caption-arrival and timeout
// — both paths call prepareLocked; the second sees flushed=true and
// bails). Must be called with b.mu held; the caller is responsible for
// releasing the lock BEFORE invoking emit on the result.
//
// Per-sibling fields (chat, user, reply pointers, attachments) come
// straight from the original ev — telegram puts the same routing info
// on every sibling so each is independently dispatchable. Text is set
// to g.caption (which is the captioned sibling's Text when one
// arrived, "" on timeout). The original Text of an uncaptioned
// sibling is empty; the captioned sibling already had this caption as
// its Text, so overwriting is a no-op for it. This keeps every
// emitted ev's Text consistent across siblings, which is what the
// downstream agent expects when it processes them as separate turns
// sharing context.
func (b *mediaGroupBuffer) prepareLocked(gid string, byTimeout bool) []bufferedSibling {
	g, ok := b.groups[gid]
	if !ok || g.flushed {
		return nil
	}
	g.flushed = true
	g.flushedAt = time.Now()
	if g.timer != nil {
		g.timer.Stop()
	}

	// Capture siblings to emit + clear the slice so a subsequent Close
	// can't re-emit them (the state stays in b.groups for TTL late-
	// sibling handling, but it no longer owns the events).
	sibs := g.sibs
	g.sibs = nil

	// Group gate (deferred from onUpdate for albums): emit the siblings only if the album
	// addressed the bot. An unaddressed group album is dropped here — its siblings (and
	// their DEFERRED downloads, never run) are simply not carried into out — but the state
	// still flips to flushed + arms TTL below, so late siblings drop too. (A DM album is
	// always accepted; see Submit's accepted arg.)
	out := make([]bufferedSibling, 0, len(sibs))
	if g.accepted {
		for _, s := range sibs {
			if g.caption != "" {
				s.ev.Text = g.caption
			}
			out = append(out, s)
		}
	}

	// Arm TTL eviction. Best-effort — process restart drops the in-
	// memory state and a late sibling arriving in the gap will be
	// treated as a fresh single-event group (worst case: missing
	// caption on that one sibling). Tracked on g.ttlTimer so Close
	// can Stop it and not leak buffer refs past Source shutdown.
	if !b.closed {
		g.ttlTimer = time.AfterFunc(flushedStateTTL, func() {
			b.mu.Lock()
			// Only delete if it's still the same flushed state we just
			// produced — a paranoid guard against a future code path that
			// might recreate the entry.
			if cur, stillThere := b.groups[gid]; stillThere && cur == g {
				delete(b.groups, gid)
			}
			b.mu.Unlock()
		})
	}

	if byTimeout {
		slog.Info("telegram: media-group flushed by timeout",
			"group_id", gid,
			"n_evs", len(out),
			"have_caption", g.caption != "",
			"waited", time.Since(g.firstSeen))
	}
	return out
}

// Close flushes every pending non-flushed group — last-chance delivery
// so a Source shutdown doesn't strand buffered albums in memory (the
// rest of the pipeline would never know to look for them). Callers
// invoke this after the source stops pumping new events; subsequent
// Submits would just buffer forever otherwise.
//
// Each non-flushed group flushes as a timeout-style per-sibling emit
// (caption may be empty). Already-flushed groups (live for late-
// sibling TTL) are skipped — their siblings already left the buffer.
func (b *mediaGroupBuffer) Close() {
	b.mu.Lock()
	b.closed = true
	ids := make([]string, 0, len(b.groups))
	for gid, g := range b.groups {
		if g.flushed {
			continue // already emitted; TTL state — nothing more to flush
		}
		ids = append(ids, gid)
	}
	var allSibs []bufferedSibling
	for _, gid := range ids {
		allSibs = append(allSibs, b.prepareLocked(gid, true)...)
	}
	// Stop every timer (flush + TTL) so no AfterFunc callback fires
	// after Source.Run returns holding buffer refs.
	for gid, g := range b.groups {
		if g.timer != nil {
			g.timer.Stop()
		}
		if g.ttlTimer != nil {
			g.ttlTimer.Stop()
		}
		delete(b.groups, gid)
	}
	b.mu.Unlock()
	b.emitSiblings(allSibs)
}
