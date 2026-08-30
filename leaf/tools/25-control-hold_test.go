package tools

import (
	"testing"
	"time"
)

// TestControlHolds pins the heartbeat lease: Set holds, a renewal extends it, a missed
// renewal lapses it, and Clear distinguishes a live hand-back from a lapsed/absent one.
func TestControlHolds(t *testing.T) {
	h := newControlHolds()
	cur := time.Unix(1000, 0)
	h.now = func() time.Time { return cur }

	if h.Held("s1") {
		t.Fatal("s1 held before any Set")
	}
	h.Set("s1")
	if !h.Held("s1") {
		t.Fatal("s1 not held after Set")
	}

	// heartbeat renew: 20s in, renew; 20s more (40s since first Set, 20s since renew) → still live.
	cur = cur.Add(20 * time.Second)
	h.Set("s1")
	cur = cur.Add(20 * time.Second)
	if !h.Held("s1") {
		t.Fatal("renewed hold lapsed early")
	}
	// no renewal for > TTL → lapses.
	cur = cur.Add(31 * time.Second)
	if h.Held("s1") {
		t.Fatal("hold did not lapse after the TTL")
	}

	// Clear of a live hold → wasLive true (an explicit hand-back); a second clear → false.
	h.Set("s2")
	if wasLive, _ := h.Clear("s2"); !wasLive {
		t.Fatal("Clear of a live hold must report wasLive=true")
	}
	if wasLive, _ := h.Clear("s2"); wasLive {
		t.Fatal("Clear of an already-released sid must be false")
	}
	// Clear of a LAPSED hold → false (a lapse is not a hand-back, so it wakes nothing).
	h.Set("s3")
	cur = cur.Add(31 * time.Second)
	if wasLive, _ := h.Clear("s3"); wasLive {
		t.Fatal("Clear of a lapsed hold must be false")
	}
}

// TestControlHoldResumeNote pins the resume note: escalate_to_coo records the worker sid
// for a principal-keyed hold, and the hand-back (Clear) names it so leaf/webui wakes the
// right worker — even though the hold itself is keyed by the principal, not the sid.
func TestControlHoldResumeNote(t *testing.T) {
	cur := time.Unix(0, 0)
	h := &controlHolds{ttl: defaultHoldTTL, now: func() time.Time { return cur }, holds: map[string]time.Time{}, resumes: map[string]resumeNote{}}

	// Worker w1 hands principal P off; the human takes control then hands back.
	h.NoteResume("P", "w1")
	h.Set("P")
	wasLive, resumeSid := h.Clear("P")
	if !wasLive || resumeSid != "w1" {
		t.Fatalf("hand-back must wake the noted worker, got wasLive=%v resumeSid=%q", wasLive, resumeSid)
	}
	// The note is consumed — a second Clear names nobody.
	if _, again := h.Clear("P"); again != "" {
		t.Fatalf("resume note must be consumed, got %q", again)
	}
	// A proactive /takeover (a live hold with NO note) → wasLive but no worker to wake.
	h.Set("Q")
	if wasLive, resumeSid := h.Clear("Q"); !wasLive || resumeSid != "" {
		t.Fatalf("noteless hand-back: wasLive=%v resumeSid=%q", wasLive, resumeSid)
	}
	// A stale note (handoff a human never took over) expires, never resurrecting a session.
	h.NoteResume("R", "w2")
	cur = cur.Add(resumeNoteTTL + time.Second)
	if _, resumeSid := h.Clear("R"); resumeSid != "" {
		t.Fatalf("expired note must not resume, got %q", resumeSid)
	}

	// Lapsed toggle-off must NOT consume the note (D17a): a control-toggle Clear after the
	// heartbeat lapsed (>30s blip / laptop sleep) is not a hand-back — it wakes nothing AND
	// leaves the note in place, so a control re-entry + live hand-back still wakes the worker.
	// (The old behavior consumed the note here, stranding the worker forever.)
	cur = time.Unix(2000, 0)
	h.NoteResume("S", "w3")
	if wasLive, resumeSid := h.Clear("S"); wasLive || resumeSid != "" {
		t.Fatalf("lapsed Clear must wake nothing and keep the note: got (%v, %q)", wasLive, resumeSid)
	}
	h.Set("S")
	if wasLive, resumeSid := h.Clear("S"); !wasLive || resumeSid != "w3" {
		t.Fatalf("note must survive a lapsed Clear for the retried hand-back: got (%v, %q)", wasLive, resumeSid)
	}

	// ConsumeResume — the 保存登陆 door: takes the note regardless of hold liveness,
	// exactly once; an expired note is gone.
	h.NoteResume("T", "w4") // no hold at all
	if sid := h.ConsumeResume("T"); sid != "w4" {
		t.Fatalf("ConsumeResume must take the note without a live hold, got %q", sid)
	}
	if sid := h.ConsumeResume("T"); sid != "" {
		t.Fatalf("ConsumeResume must consume exactly once, got %q", sid)
	}
	h.NoteResume("U", "w5")
	cur = cur.Add(resumeNoteTTL + time.Second)
	if sid := h.ConsumeResume("U"); sid != "" {
		t.Fatalf("ConsumeResume must not return an expired note, got %q", sid)
	}
}
