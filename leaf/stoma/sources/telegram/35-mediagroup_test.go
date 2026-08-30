// Tests for the source-layer Telegram media-group buffer.
//
// Behaviour reversal as of: the buffer NO LONGER merges
// siblings into one event. Each sibling emits as its own MessageEvent
// with the shared caption stamped onto .Text. The downstream gateway
// drains queued group siblings ONE turn at a time so each photo gets
// its own bounded reply. See sources/telegram/35-mediagroup.go for the
// design rationale.
//
// Invariants pinned here:
//
//   - Single events (MediaGroupID == "") flow through with zero
//     buffering — the non-album hot path MUST NOT incur added latency.
//   - Album events buffer until either the captioned sibling arrives
//     OR the timeout fires. Whichever flush triggers, ONE emit fires
//     per buffered sibling, each carrying the shared caption.
//   - Post-flush, the groupState is RETAINED for flushedStateTTL so a
//     late-arriving sibling picks up the cached caption and emits
//     immediately without another buffer cycle.
//   - After flushedStateTTL, the state is evicted; a late sibling
//     past the TTL is treated as a fresh single-event group.
//   - Two concurrent albums don't interfere with each other.
//   - Close() flushes any non-flushed groups so a Source shutdown
//     doesn't strand buffered albums. Already-flushed groups (TTL
//     state) are NOT re-emitted.
//   - No race under -race (Submit / Close / AfterFunc callback all
//     mutate the same group map under mu).
//
// The tests shorten mediaGroupFlushTimeout AND flushedStateTTL to a
// few hundred ms so a full run takes O(seconds) not O(minutes).
// Resetting them inside each test (t.Cleanup) keeps tests order-
// independent.
package telegram

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentbob/contract"
)

// TestMediaGroup_DeferredDownload pins L-D1: an album sibling's media download is DEFERRED
// to flush and runs ONLY if the album is accepted. An un-addressed (dropped) group album
// never fetches a photo; an accepted album downloads once per sibling and the attachments
// land on the emitted events.
func TestMediaGroup_DeferredDownload(t *testing.T) {
	shortenTimeout(t, 100*time.Millisecond)
	newDL := func(n *int32) func() []contract.Attachment {
		return func() []contract.Attachment {
			atomic.AddInt32(n, 1)
			return []contract.Attachment{{FileName: "x.jpg"}}
		}
	}

	t.Run("dropped album never downloads", func(t *testing.T) {
		c := newCollector()
		b := newMediaGroupBuffer(c.emit)
		var downloads int32
		dl := newDL(&downloads)
		// two un-addressed, uncaptioned siblings → flush by timeout → dropped.
		b.Submit(contract.MessageEvent{MediaGroupID: "g_drop", MessageID: "m1"}, false, dl)
		b.Submit(contract.MessageEvent{MediaGroupID: "g_drop", MessageID: "m2"}, false, dl)
		time.Sleep(250 * time.Millisecond) // past the 100ms timeout flush
		if c.count() != 0 {
			t.Fatalf("dropped album emitted %d events, want 0", c.count())
		}
		if n := atomic.LoadInt32(&downloads); n != 0 {
			t.Fatalf("dropped album ran %d downloads, want 0 (the deferred download must be skipped)", n)
		}
	})

	t.Run("accepted album downloads per sibling + attaches", func(t *testing.T) {
		c := newCollector()
		b := newMediaGroupBuffer(c.emit)
		var downloads int32
		dl := newDL(&downloads)
		b.Submit(contract.MessageEvent{MediaGroupID: "g_ok", MessageID: "m1"}, false, dl)              // uncaptioned, not addressed
		b.Submit(contract.MessageEvent{MediaGroupID: "g_ok", MessageID: "m2", Text: "look"}, true, dl) // captioned + addressed → flush now
		if !c.waitFor(2, time.Second) {
			t.Fatalf("accepted album emitted %d events, want 2", c.count())
		}
		if n := atomic.LoadInt32(&downloads); n != 2 {
			t.Fatalf("accepted album ran %d downloads, want 2 (one per sibling)", n)
		}
		for _, e := range c.snapshot() {
			if len(e.Attachments) != 1 || e.Attachments[0].FileName != "x.jpg" {
				t.Fatalf("emitted event missing the deferred-download attachment: %+v", e.Attachments)
			}
		}
	})
}

// shortenTimeout swaps mediaGroupFlushTimeout to d for the test's
// duration; t.Cleanup restores the original. ALL tests in this file
// use it — even those that don't expect to hit the timeout, so a
// surprise timeout fires in O(ms) instead of O(10s).
func shortenTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := mediaGroupFlushTimeout
	mediaGroupFlushTimeout = d
	t.Cleanup(func() { mediaGroupFlushTimeout = prev })
}

// shortenTTL swaps flushedStateTTL to d for the test's duration. Tests
// that probe the late-sibling / eviction window use it; the rest leave
// it at the production default (large enough that none of the test's
// ~ms-scale waits could ever cross it).
func shortenTTL(t *testing.T, d time.Duration) {
	t.Helper()
	prev := flushedStateTTL
	flushedStateTTL = d
	t.Cleanup(func() { flushedStateTTL = prev })
}

// collector wraps a slice of emitted events behind a mutex so the
// AfterFunc-driven flush callback (different goroutine) doesn't race
// the test goroutine's assertions.
type collector struct {
	mu   sync.Mutex
	evs  []contract.MessageEvent
	cond *sync.Cond
}

func newCollector() *collector {
	c := &collector{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *collector) emit(ev contract.MessageEvent) {
	c.mu.Lock()
	c.evs = append(c.evs, ev)
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.evs)
}

func (c *collector) snapshot() []contract.MessageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]contract.MessageEvent, len(c.evs))
	copy(out, c.evs)
	return out
}

// waitFor blocks until count() >= n or timeout. Returns true on success.
func (c *collector) waitFor(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.evs) < n {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		// cond.Wait can't take a timeout directly — spin via a sentinel
		// goroutine to wake us when the deadline passes.
		timer := time.AfterFunc(remaining, c.cond.Broadcast)
		c.cond.Wait()
		timer.Stop()
	}
	return true
}

// TestMediaGroup_SingleEv_NoGroupID_EmitsImmediately — the non-album
// hot path bypasses every buffer concern with zero latency. Pins
// that a single ev never lingers in b.groups (it never enters).
func TestMediaGroup_SingleEv_NoGroupID_EmitsImmediately(t *testing.T) {
	shortenTimeout(t, 100*time.Millisecond)
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	ev := contract.MessageEvent{
		Source:    "telegram",
		MessageID: "m1",
		Text:      "hello",
	}
	b.Submit(ev, true, nil)

	if c.count() != 1 {
		t.Fatalf("expected 1 emit, got %d", c.count())
	}
	got := c.snapshot()[0]
	if got.MessageID != "m1" || got.Text != "hello" {
		t.Errorf("unexpected forwarded ev: %+v", got)
	}
	// The buffer must not retain anything for a non-group ev.
	b.mu.Lock()
	if len(b.groups) != 0 {
		t.Errorf("non-group ev leaked into b.groups: %d entries", len(b.groups))
	}
	b.mu.Unlock()
}

// TestMediaGroup_AlbumWithCaptionLater_FlushesPerEvOnCaption — the
// classic Telegram order: the captioned ev arrives LAST. Each
// uncaptioned sibling buffers; the captioned arrival triggers
// per-sibling emit, each carrying the shared caption on .Text. Pins
// that NO emit happens before the caption arrives.
func TestMediaGroup_AlbumWithCaptionLater_FlushesPerEvOnCaption(t *testing.T) {
	shortenTimeout(t, 5*time.Second) // generous — must NOT trip in this test
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-1"
	const caption = "describe these"
	// Two uncaptioned siblings + a captioned one.
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, true, nil)
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m2", Attachments: []contract.Attachment{{FileName: "b.jpg"}}}, true, nil)
	// Buffer must still hold the group; nothing emitted.
	if c.count() != 0 {
		t.Fatalf("expected 0 emits before caption, got %d", c.count())
	}
	// Caption arrives — must trigger 3 per-sibling emits, each with
	// the same caption.
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m3", Text: caption, Attachments: []contract.Attachment{{FileName: "c.jpg"}}}, true, nil)
	if c.count() != 3 {
		t.Fatalf("expected 3 per-ev emits after caption, got %d", c.count())
	}
	evs := c.snapshot()
	// Every ev must carry the shared caption + its own single attachment.
	for i, e := range evs {
		if e.Text != caption {
			t.Errorf("ev[%d] caption mismatch: got %q want %q", i, e.Text, caption)
		}
		if len(e.Attachments) != 1 {
			t.Errorf("ev[%d] expected 1 attachment (per-ev emit, not merged), got %d", i, len(e.Attachments))
		}
		// MediaGroupID must be preserved so the gateway can detect it
		// and apply the drain-one-at-a-time policy.
		if e.MediaGroupID != gid {
			t.Errorf("ev[%d] MediaGroupID dropped: got %q want %q", i, e.MediaGroupID, gid)
		}
	}
	// Specifically: original m1+m2 had no Text, the caption was on m3 —
	// they should all now show `caption` as Text.
	msgIDs := map[string]string{evs[0].MessageID: evs[0].Text, evs[1].MessageID: evs[1].Text, evs[2].MessageID: evs[2].Text}
	for _, id := range []string{"m1", "m2", "m3"} {
		if msgIDs[id] != caption {
			t.Errorf("ev with id %q should carry caption %q; got %q", id, caption, msgIDs[id])
		}
	}
	// State must still exist (flushed=true, TTL retained) so a late
	// sibling can still pick up the cached caption.
	b.mu.Lock()
	g, present := b.groups[gid]
	if !present {
		t.Errorf("group %q evicted immediately after flush — late siblings would lose the cached caption", gid)
	} else {
		if !g.flushed {
			t.Errorf("group %q present but flushed=false after flush", gid)
		}
		if g.caption != caption {
			t.Errorf("cached caption mismatch: got %q want %q", g.caption, caption)
		}
	}
	b.mu.Unlock()
}

// TestMediaGroup_AlbumCaptionFirst_FlushesOnCaption — when the
// captioned ev is the FIRST update of the album, the buffer flushes
// immediately with just that one ev. Subsequent siblings then take
// the late-sibling path (state still present with flushed=true) and
// pick up the cached caption.
func TestMediaGroup_AlbumCaptionFirst_FlushesOnCaption(t *testing.T) {
	shortenTimeout(t, 200*time.Millisecond)
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-cf"
	const caption = "leading caption"
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Text: caption, Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, true, nil)
	// The first ev IS captioned — must flush immediately, just that one.
	if c.count() != 1 {
		t.Fatalf("expected immediate flush on captioned first ev, got %d emits", c.count())
	}
	got := c.snapshot()[0]
	if got.Text != caption {
		t.Errorf("caption mismatch: got %q want %q", got.Text, caption)
	}
	if len(got.Attachments) != 1 {
		t.Errorf("expected 1 attachment in initial flush, got %d", len(got.Attachments))
	}
}

// TestMediaGroup_AlbumNoCaption_FlushesPerEvOnTimeout — siblings
// without a captioned ev sit in the buffer until mediaGroupFlushTimeout,
// then emit ONE-PER-SIBLING with empty .Text (downstream noCaptionStandIn
// covers them). Pins the safety net: no album is stranded indefinitely.
func TestMediaGroup_AlbumNoCaption_FlushesPerEvOnTimeout(t *testing.T) {
	shortenTimeout(t, 80*time.Millisecond)
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-nc"
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, true, nil)
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m2", Attachments: []contract.Attachment{{FileName: "b.jpg"}}}, true, nil)

	if c.count() != 0 {
		t.Fatalf("expected 0 emits before timeout, got %d", c.count())
	}

	if !c.waitFor(2, 2*time.Second) {
		t.Fatalf("timeout flush did not fire within budget")
	}
	if c.count() != 2 {
		t.Fatalf("timeout should emit ONE event per sibling (2), got %d", c.count())
	}
	evs := c.snapshot()
	for i, e := range evs {
		if e.Text != "" {
			t.Errorf("ev[%d]: expected empty caption on timeout flush, got %q", i, e.Text)
		}
		if len(e.Attachments) != 1 {
			t.Errorf("ev[%d]: expected 1 attachment per emit, got %d", i, len(e.Attachments))
		}
	}
}

// TestMediaGroup_MultipleGroupsConcurrent_Isolated — two albums in
// flight at the same time MUST NOT interfere with each other. Group A
// flushes via caption (2 siblings → 2 per-ev emits); group B flushes
// via timeout (1 sibling → 1 emit). Each ev carries only its own
// sibling's attachments.
func TestMediaGroup_MultipleGroupsConcurrent_Isolated(t *testing.T) {
	shortenTimeout(t, 150*time.Millisecond)
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gidA, gidB = "alb-A", "alb-B"
	// Submit interleaved across both groups.
	b.Submit(contract.MessageEvent{MediaGroupID: gidA, MessageID: "a1", Attachments: []contract.Attachment{{FileName: "a1.jpg"}}}, true, nil)
	b.Submit(contract.MessageEvent{MediaGroupID: gidB, MessageID: "b1", Attachments: []contract.Attachment{{FileName: "b1.jpg"}}}, true, nil)
	b.Submit(contract.MessageEvent{MediaGroupID: gidA, MessageID: "a2", Text: "caption A", Attachments: []contract.Attachment{{FileName: "a2.jpg"}}}, true, nil)
	// A flushed on caption — 2 per-ev emits; B still buffered.
	if c.count() != 2 {
		t.Fatalf("expected 2 emits (A's 2 siblings) before B timeout, got %d", c.count())
	}
	if !c.waitFor(3, 2*time.Second) {
		t.Fatalf("B timeout flush did not fire")
	}
	evs := c.snapshot()
	if len(evs) != 3 {
		t.Fatalf("expected 3 total emits (2 from A + 1 from B), got %d", len(evs))
	}
	// Group A's siblings (caption stamped) vs group B's (empty caption).
	// Each ev keeps its MediaGroupID so we can route by it.
	aCount, bCount := 0, 0
	for _, e := range evs {
		switch e.MediaGroupID {
		case gidA:
			aCount++
			if e.Text != "caption A" {
				t.Errorf("A ev caption mismatch: %q", e.Text)
			}
			if len(e.Attachments) != 1 {
				t.Errorf("A ev attachment count: got %d want 1 (per-ev emit)", len(e.Attachments))
			}
			if !(e.Attachments[0].FileName == "a1.jpg" || e.Attachments[0].FileName == "a2.jpg") {
				t.Errorf("A ev has unexpected attachment: %+v", e.Attachments)
			}
		case gidB:
			bCount++
			if e.Text != "" {
				t.Errorf("B ev should have empty caption (timeout flush), got %q", e.Text)
			}
			if len(e.Attachments) != 1 || e.Attachments[0].FileName != "b1.jpg" {
				t.Errorf("B ev attachments wrong: %+v", e.Attachments)
			}
		default:
			t.Errorf("unknown group id on emitted ev: %q", e.MediaGroupID)
		}
	}
	if aCount != 2 || bCount != 1 {
		t.Errorf("group split wrong: A=%d (want 2), B=%d (want 1)", aCount, bCount)
	}
}

// TestMediaGroup_CloseFlushesPending — Close() at Source shutdown
// must drain every non-flushed buffered group so albums in flight
// don't get stranded in memory. Each pending group flushes as a
// per-sibling emit (caption may be empty).
func TestMediaGroup_CloseFlushesPending(t *testing.T) {
	shortenTimeout(t, 5*time.Second) // long — Close must fire before timeout
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid1, gid2 = "alb-c1", "alb-c2"
	b.Submit(contract.MessageEvent{MediaGroupID: gid1, MessageID: "x1", Attachments: []contract.Attachment{{FileName: "x1.jpg"}}}, true, nil)
	b.Submit(contract.MessageEvent{MediaGroupID: gid1, MessageID: "x2", Attachments: []contract.Attachment{{FileName: "x2.jpg"}}}, true, nil)
	b.Submit(contract.MessageEvent{MediaGroupID: gid2, MessageID: "y1", Attachments: []contract.Attachment{{FileName: "y1.jpg"}}}, true, nil)
	if c.count() != 0 {
		t.Fatalf("expected 0 emits pre-Close, got %d", c.count())
	}
	b.Close()
	// gid1 had 2 siblings, gid2 had 1 — Close emits one per sibling.
	if c.count() != 3 {
		t.Fatalf("expected Close to emit per-sibling (3), got %d", c.count())
	}
	// All groups must transition to flushed=true (state retained for
	// late-sibling TTL but evs cleared).
	b.mu.Lock()
	for gid, g := range b.groups {
		if !g.flushed {
			t.Errorf("Close left group %q with flushed=false", gid)
		}
		if len(g.sibs) != 0 {
			t.Errorf("Close left group %q with %d residual siblings", gid, len(g.sibs))
		}
	}
	b.mu.Unlock()
}

// TestMediaGroup_FlushedThenClose_NoRedundantFlush — Close MUST NOT
// re-emit siblings of an already-flushed group. The flushed=true
// guard inside prepareLocked is the safety net; this test pins the
// Close-path skip so future refactors don't accidentally double-emit.
func TestMediaGroup_FlushedThenClose_NoRedundantFlush(t *testing.T) {
	shortenTimeout(t, 5*time.Second) // generous — caption-flush is the path here
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-noreflush"
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, true, nil)
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m2", Text: "hi", Attachments: []contract.Attachment{{FileName: "b.jpg"}}}, true, nil)
	if c.count() != 2 {
		t.Fatalf("expected 2 per-ev emits after caption, got %d", c.count())
	}
	b.Close()
	if c.count() != 2 {
		t.Errorf("Close re-emitted siblings of an already-flushed group: %d emits (want 2)", c.count())
	}
}

// TestMediaGroup_LateSibling_GetsCachedCaption — a sibling that
// arrives AFTER the captioned-flush ran (within flushedStateTTL)
// must pick up the cached caption and emit immediately, NOT spin up
// a new buffer cycle. Pins the late-sibling fast path that the
// per-ev-emit reversal explicitly enables.
func TestMediaGroup_LateSibling_GetsCachedCaption(t *testing.T) {
	shortenTimeout(t, 80*time.Millisecond)
	shortenTTL(t, 500*time.Millisecond) // long enough for the late sibling to land
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-late"
	const caption = "describe these"
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Text: caption, Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, true, nil)
	if c.count() != 1 {
		t.Fatalf("expected first flush on captioned ev, got %d", c.count())
	}
	// A short pause to simulate the out-of-order delivery — still well
	// within flushedStateTTL.
	time.Sleep(20 * time.Millisecond)
	// Late sibling — must emit IMMEDIATELY with cached caption (no
	// second timeout wait).
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m_late", Attachments: []contract.Attachment{{FileName: "late.jpg"}}}, true, nil)
	if c.count() != 2 {
		t.Fatalf("late-sibling fast-path did not emit immediately: count=%d", c.count())
	}
	evs := c.snapshot()
	late := evs[1]
	if late.MessageID != "m_late" {
		t.Errorf("late emit's MessageID wrong: %q", late.MessageID)
	}
	if late.Text != caption {
		t.Errorf("late sibling should pick up cached caption %q, got %q", caption, late.Text)
	}
	if len(late.Attachments) != 1 || late.Attachments[0].FileName != "late.jpg" {
		t.Errorf("late sibling attachments wrong: %+v", late.Attachments)
	}
}

// TestMediaGroup_LateSibling_AfterTimeoutFlush_GetsEmptyCaption —
// a timeout-flushed group has no caption (g.caption stays ""); a
// late sibling within TTL still hits the flushed fast path but its
// .Text remains empty (downstream noCaptionStandIn covers). Pins
// that the late-sibling path doesn't accidentally stamp the wrong
// caption.
func TestMediaGroup_LateSibling_AfterTimeoutFlush_GetsEmptyCaption(t *testing.T) {
	shortenTimeout(t, 60*time.Millisecond)
	shortenTTL(t, 500*time.Millisecond)
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-late-timeout"
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, true, nil)
	if !c.waitFor(1, 2*time.Second) {
		t.Fatalf("timeout flush did not fire")
	}
	if c.snapshot()[0].Text != "" {
		t.Fatalf("setup: timeout-flushed ev should have empty caption")
	}
	// Late sibling — fast path; caption still empty.
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m_late", Text: "", Attachments: []contract.Attachment{{FileName: "late.jpg"}}}, true, nil)
	if c.count() != 2 {
		t.Fatalf("late sibling did not emit immediately: count=%d", c.count())
	}
	if c.snapshot()[1].Text != "" {
		t.Errorf("late sibling should keep empty caption (timeout-flushed group), got %q", c.snapshot()[1].Text)
	}
}

// TestMediaGroup_TTLEviction_LateSiblingTreatedAsNew — after
// flushedStateTTL expires the groupState is evicted. A sibling that
// arrives past the TTL is therefore treated as a fresh single-event
// album: it starts a new buffer cycle and flushes via the new
// cycle's timeout. Pins that the TTL eviction actually fires + that
// the late-sibling fast path correctly bypasses for post-TTL evs.
func TestMediaGroup_TTLEviction_LateSiblingTreatedAsNew(t *testing.T) {
	shortenTimeout(t, 60*time.Millisecond)
	shortenTTL(t, 120*time.Millisecond) // short enough to expire within test budget
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-ttl"
	const caption = "first round"
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Text: caption, Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, true, nil)
	if c.count() != 1 {
		t.Fatalf("expected first flush, got %d", c.count())
	}
	// Wait past the TTL window so the eviction AfterFunc fires.
	time.Sleep(250 * time.Millisecond)
	// State should be gone.
	b.mu.Lock()
	_, stillThere := b.groups[gid]
	b.mu.Unlock()
	if stillThere {
		t.Fatalf("groupState should have been evicted after TTL")
	}
	// Late sibling — buffer is now empty for this gid, so it starts
	// a fresh cycle. The new cycle has no caption → flushes via the
	// new timeout, NOT immediately.
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m_late", Attachments: []contract.Attachment{{FileName: "late.jpg"}}}, true, nil)
	if c.count() != 1 {
		t.Fatalf("post-TTL late sibling should not fast-path emit; count=%d", c.count())
	}
	// Wait for the new cycle's timeout.
	if !c.waitFor(2, 2*time.Second) {
		t.Fatalf("post-TTL new-cycle timeout did not fire")
	}
	late := c.snapshot()[1]
	if late.Text != "" {
		t.Errorf("post-TTL ev should have empty caption (no cached caption), got %q", late.Text)
	}
	if len(late.Attachments) != 1 || late.Attachments[0].FileName != "late.jpg" {
		t.Errorf("post-TTL ev attachments wrong: %+v", late.Attachments)
	}
}

// TestMediaGroup_CaptionPreservedOnOriginalCaptionedEv — when the
// captioned sibling is the LAST to arrive, the prior uncaptioned
// siblings' .Text was "". After flush, EVERY emit (including the
// originally-captioned one) must carry the caption — overwriting
// the captioned one's Text with the same string is a no-op but
// pins that the per-sibling stamp is unconditional, so a future
// refactor doesn't accidentally treat the captioned sibling
// specially.
func TestMediaGroup_CaptionPreservedOnOriginalCaptionedEv(t *testing.T) {
	shortenTimeout(t, 5*time.Second)
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-stamp"
	const caption = "the caption"
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, true, nil)
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m2", Text: caption, Attachments: []contract.Attachment{{FileName: "b.jpg"}}}, true, nil)

	if c.count() != 2 {
		t.Fatalf("expected 2 emits, got %d", c.count())
	}
	for i, e := range c.snapshot() {
		if e.Text != caption {
			t.Errorf("ev[%d] missing caption: got %q want %q", i, e.Text, caption)
		}
	}
}

// TestMediaGroup_WhitespaceCaptionNotTreatedAsCaption — a sibling whose
// Text is only whitespace MUST NOT count as a real caption: it must not
// flush the album early, and the blank string must not be stamped onto
// the siblings. The group flushes via timeout instead, with empty Text.
func TestMediaGroup_WhitespaceCaptionNotTreatedAsCaption(t *testing.T) {
	shortenTimeout(t, 150*time.Millisecond)
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-ws"
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, true, nil)
	// Whitespace-only "caption" — must NOT flush the album.
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m2", Text: "   \n\t ", Attachments: []contract.Attachment{{FileName: "b.jpg"}}}, true, nil)

	if c.count() != 0 {
		t.Fatalf("whitespace caption flushed album early: got %d emits, want 0", c.count())
	}

	// Timeout flushes both siblings with NO group caption. The blank
	// "caption" must not have been stored as g.caption, so it is never
	// stamped onto the OTHER sibling (m1 stays empty). Each sibling keeps
	// only its own original Text — m2's raw whitespace is left as-is, m1
	// stays empty.
	if !c.waitFor(2, time.Second) {
		t.Fatalf("timeout flush did not produce 2 emits; got %d", c.count())
	}
	byID := map[string]string{}
	for _, e := range c.snapshot() {
		byID[e.MessageID] = e.Text
	}
	if byID["m1"] != "" {
		t.Errorf("m1 (uncaptioned sibling) must stay empty — blank caption was wrongly stamped: got %q", byID["m1"])
	}
}

// TestMediaGroup_RealCaptionStillFlushes — a non-whitespace caption must
// still flush the album immediately (guards against over-trimming).
func TestMediaGroup_RealCaptionStillFlushes(t *testing.T) {
	shortenTimeout(t, 5*time.Second) // must NOT trip
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-real"
	const caption = "  describe these  " // surrounding whitespace, real content
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, true, nil)
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m2", Text: caption, Attachments: []contract.Attachment{{FileName: "b.jpg"}}}, true, nil)

	if c.count() != 2 {
		t.Fatalf("real caption did not flush album: got %d emits, want 2", c.count())
	}
	// The caption is stamped verbatim (the buffer doesn't trim the stored
	// caption — only the has-caption decision is trimmed).
	for i, e := range c.snapshot() {
		if e.Text != caption {
			t.Errorf("ev[%d] caption mismatch: got %q want %q", i, e.Text, caption)
		}
	}
}

// A GROUP album where no sibling addressed the bot (accepted=false throughout) is
// dropped at flush — δ-2: caption-less siblings are deferred to the buffer instead
// of dropped at the per-message gate, and the whole album is gated here.
func TestMediaGroup_UnaddressedGroupAlbum_Dropped(t *testing.T) {
	shortenTimeout(t, 5*time.Second)
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)
	const gid = "alb-unaddressed"
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, false, nil)
	// A captioned sibling triggers the flush, but it still didn't address the bot.
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m2", Text: "just chatting", Attachments: []contract.Attachment{{FileName: "b.jpg"}}}, false, nil)
	if c.count() != 0 {
		t.Fatalf("an unaddressed group album must emit nothing, got %d", c.count())
	}
}

// The album is accepted iff ANY sibling addressed the bot — telegram puts the
// @mention only on the captioned sibling, so a caption-less first sibling must not
// sink the whole album.
func TestMediaGroup_AcceptedIfAnySiblingAddressed(t *testing.T) {
	shortenTimeout(t, 5*time.Second)
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)
	const gid = "alb-mixed"
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1", Attachments: []contract.Attachment{{FileName: "a.jpg"}}}, false, nil)
	// The captioned sibling DID address the bot → whole album accepted, all emit.
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m2", Text: "@bot describe", Attachments: []contract.Attachment{{FileName: "b.jpg"}}}, true, nil)
	if c.count() != 2 {
		t.Fatalf("an album with one addressed sibling must emit all siblings, got %d", c.count())
	}
}

// The "album accepted iff ANY sibling addressed the bot" invariant must hold
// across the timeout-flush boundary too: telegram delivers album updates
// out-of-order, so the captioned (@-mentioning) sibling can land AFTER its
// caption-less siblings already timeout-flushed as unaddressed. Its own
// accepted flag must OR into the flushed state — not be discarded by the
// late-sibling fast path — so the one message that explicitly addressed the
// bot still emits (with its download), and it revives the group for
// even-later siblings, caching its caption for them.
func TestMediaGroup_LateAddressedSibling_AfterUnaddressedTimeoutFlush_Emits(t *testing.T) {
	shortenTimeout(t, 60*time.Millisecond)
	shortenTTL(t, 2*time.Second)
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-late-addressed"
	var downloads int32
	dl := func() []contract.Attachment {
		atomic.AddInt32(&downloads, 1)
		return []contract.Attachment{{FileName: "late.jpg"}}
	}
	// Caption-less, un-addressed group siblings → timeout flush drops the album.
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1"}, false, nil)
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m2"}, false, nil)
	time.Sleep(200 * time.Millisecond) // past the timeout flush
	if c.count() != 0 {
		t.Fatalf("unaddressed album must be dropped at flush, got %d emits", c.count())
	}
	// The captioned + addressed sibling arrives late, within the TTL window.
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m3", Text: "describe these"}, true, dl)
	if c.count() != 1 {
		t.Fatalf("late addressed sibling must emit immediately, count=%d", c.count())
	}
	got := c.snapshot()[0]
	if got.MessageID != "m3" || got.Text != "describe these" {
		t.Errorf("late addressed sibling emitted wrong ev: id=%q text=%q", got.MessageID, got.Text)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].FileName != "late.jpg" {
		t.Errorf("late addressed sibling's deferred download must run: %+v", got.Attachments)
	}
	if n := atomic.LoadInt32(&downloads); n != 1 {
		t.Errorf("deferred download ran %d times, want 1", n)
	}
	// The acceptance revives the group: an even-later caption-less sibling
	// emits too, stamped with the now-cached caption.
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m4"}, false, nil)
	if c.count() != 2 {
		t.Fatalf("post-revival late sibling must emit, count=%d", c.count())
	}
	if got := c.snapshot()[1]; got.Text != "describe these" {
		t.Errorf("post-revival sibling should carry the cached caption, got %q", got.Text)
	}
}

// A late sibling of a dropped album that ITSELF didn't address the bot still
// drops — the revival above is strictly gated on the late sibling's own
// accepted flag.
func TestMediaGroup_LateUnaddressedSibling_OfDroppedAlbum_StillDropped(t *testing.T) {
	shortenTimeout(t, 60*time.Millisecond)
	shortenTTL(t, 2*time.Second)
	c := newCollector()
	b := newMediaGroupBuffer(c.emit)

	const gid = "alb-late-unaddressed"
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m1"}, false, nil)
	time.Sleep(200 * time.Millisecond) // past the timeout flush → dropped
	var downloads int32
	dl := func() []contract.Attachment {
		atomic.AddInt32(&downloads, 1)
		return nil
	}
	b.Submit(contract.MessageEvent{MediaGroupID: gid, MessageID: "m2"}, false, dl)
	if c.count() != 0 {
		t.Fatalf("late unaddressed sibling of a dropped album must not emit, got %d", c.count())
	}
	if n := atomic.LoadInt32(&downloads); n != 0 {
		t.Errorf("dropped late sibling ran %d downloads, want 0", n)
	}
}
