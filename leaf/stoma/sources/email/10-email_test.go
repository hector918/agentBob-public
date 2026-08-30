package email

import (
	"container/list"
	"fmt"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// The dedup ring only suppresses re-emit when it covers the ENTIRE processed
// set. pollOnce clamps each poll's unseen set via clampUIDs (newest first) for
// exactly that reason: without the clamp, unseen = cap+1 starts an eviction
// domino that re-emits (and re-replies to) the whole backlog every poll under
// mark_seen:false (F26). This pins the invariant against the REAL clamp (not a
// test-local copy): a stable over-cap unseen set emits every message exactly
// once.
func TestDedup_ClampedPollNeverReEmits(t *testing.T) {
	s := &Source{dedupRing: list.New(), dedupSet: make(map[string]struct{}, dedupCap)}
	total := dedupCap + 50
	all := make([]imap.UID, total)
	for i := range all {
		all[i] = imap.UID(i + 1) // ascending, like UID SEARCH results
	}
	poll := func() int {
		uids := s.clampUIDs(all)
		emitted := 0
		for _, uid := range uids {
			if !s.markSeenAndCheck(fmt.Sprintf("id-%d@example.com", uid)) {
				emitted++
			}
		}
		return emitted
	}
	if got := poll(); got != dedupCap {
		t.Fatalf("first poll should emit the newest %d messages, emitted %d", dedupCap, got)
	}
	for i := 2; i <= 3; i++ {
		if got := poll(); got != 0 {
			t.Fatalf("poll %d re-emitted %d messages — ring eviction domino", i, got)
		}
	}
}

// clampUIDs keeps an under-cap set untouched and clamps an over-cap set to the
// newest dedupCap uids (the ascending tail).
func TestClampUIDs(t *testing.T) {
	s := &Source{}
	small := []imap.UID{1, 2, 3}
	if got := s.clampUIDs(small); len(got) != 3 || got[0] != 1 {
		t.Fatalf("under-cap set must pass through unchanged, got %v", got)
	}
	big := make([]imap.UID, dedupCap+7)
	for i := range big {
		big[i] = imap.UID(i + 1)
	}
	got := s.clampUIDs(big)
	if len(got) != dedupCap {
		t.Fatalf("clamped length = %d, want %d", len(got), dedupCap)
	}
	if got[0] != 8 || got[len(got)-1] != imap.UID(dedupCap+7) {
		t.Fatalf("clamp must keep the newest tail: got [%d..%d], want [8..%d]",
			got[0], got[len(got)-1], dedupCap+7)
	}
}
