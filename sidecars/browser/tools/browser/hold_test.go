package browser

// Control-hold (takeover yield+resume) tests — all chromium-free: a held
// scope makes AcquireFor yield BEFORE any routing/spawn, so the gate is
// exercisable headless, like TestAcquireFor_Busy.

import (
	"context"
	"testing"
	"time"

	"agentbob/sidecars/browser/core"
)

// newHoldFixture builds a registry on the advanceable fakeClock shared with
// the custodian tests (custodian_test.go).
func newHoldFixture(ttl time.Duration) (*ControlHolds, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	return NewControlHolds(ttl, clk.now), clk
}

// TestHoldSetClearTake pins the core lifecycle: Set → held; explicit Clear →
// released AND a one-shot resume note parked; second Take is empty.
func TestHoldSetClearTake(t *testing.T) {
	h, _ := newHoldFixture(30 * time.Second)
	const scope = "telegram:dm:1"

	if h.Held(scope) {
		t.Fatal("fresh registry must hold nothing")
	}
	h.Set(scope)
	if !h.Held(scope) {
		t.Fatal("Set must establish the hold")
	}
	if _, ok := h.TakeResumeNote(scope); ok {
		t.Fatal("no resume note may exist while the hold is live")
	}

	h.Clear(scope)
	if h.Held(scope) {
		t.Fatal("Clear must release the hold")
	}
	note, ok := h.TakeResumeNote(scope)
	if !ok || note == "" {
		t.Fatalf("explicit Clear must park a resume note; got (%q, %v)", note, ok)
	}
	if _, ok := h.TakeResumeNote(scope); ok {
		t.Fatal("resume note must be one-shot")
	}
}

// TestHoldTTLExpiry pins the heartbeat-lease contract: a hold not renewed
// within the TTL lapses on its own, and a TTL expiry parks NO resume note
// (the human may just have lost connectivity — the model resumes silently).
func TestHoldTTLExpiry(t *testing.T) {
	h, clk := newHoldFixture(30 * time.Second)
	const scope = "telegram:dm:1"

	h.Set(scope)
	clk.advance(31 * time.Second)
	if h.Held(scope) {
		t.Fatal("an unrenewed hold must lapse after the TTL")
	}
	if _, ok := h.TakeResumeNote(scope); ok {
		t.Fatal("TTL expiry must NOT park a resume note")
	}
	// Clearing a lapsed hold parks nothing either (the explicit-release
	// nudge only fires on a LIVE hand-back).
	h.Clear(scope)
	if _, ok := h.TakeResumeNote(scope); ok {
		t.Fatal("Clear after TTL expiry must NOT park a resume note")
	}
}

// TestHoldHeartbeatRenewal pins that periodic Set re-assertion keeps the
// hold alive past the bare TTL.
func TestHoldHeartbeatRenewal(t *testing.T) {
	h, clk := newHoldFixture(30 * time.Second)
	const scope = "telegram:dm:1"

	h.Set(scope)
	for i := 0; i < 6; i++ { // 6 × 20s = 2 min, each beat inside the lease
		clk.advance(20 * time.Second)
		if !h.Held(scope) {
			t.Fatalf("hold lapsed despite in-lease heartbeats (beat %d)", i)
		}
		h.Set(scope)
	}
	clk.advance(31 * time.Second) // stop renewing → lapse
	if h.Held(scope) {
		t.Fatal("hold must lapse once heartbeats stop")
	}
}

// TestHoldResumeNoteTTL pins the unbounded-growth guard: an unconsumed
// resume note is dropped after resumeNoteTTL.
func TestHoldResumeNoteTTL(t *testing.T) {
	h, clk := newHoldFixture(30 * time.Second)
	const scope = "telegram:dm:1"

	h.Set(scope)
	h.Clear(scope)
	clk.advance(resumeNoteTTL + time.Second)
	if _, ok := h.TakeResumeNote(scope); ok {
		t.Fatal("a stale resume note must expire instead of parking forever")
	}
}

// TestHoldNilSafety: every method must tolerate a nil receiver (wired-less
// test paths) and the empty scope.
func TestHoldNilSafety(t *testing.T) {
	var h *ControlHolds
	h.Set("s")
	h.Clear("s")
	if h.Held("s") {
		t.Fatal("nil registry must report not-held")
	}
	if _, ok := h.TakeResumeNote("s"); ok {
		t.Fatal("nil registry must have no resume note")
	}
	h2, _ := newHoldFixture(0)
	h2.Set("")
	if h2.Held("") {
		t.Fatal("empty scope must never be held")
	}
}

// TestAcquireForHeldYields pins the model-side gate: while the scope is
// human-held, AcquireFor yields with ErrHeldByHuman (whose text IS the
// model-visible envelope) without touching routing/custodian/chromium; after
// the hold is cleared the same call proceeds past the gate.
func TestAcquireForHeldYields(t *testing.T) {
	ctx := context.Background()
	p := newTestPool(t)
	h, _ := newHoldFixture(30 * time.Second)
	p.SetControlHolds(h)
	p.SetProfileGate(fakeGate{allow: map[string]bool{"browser-profile": true}})
	actx := core.AgentCtx{AccountID: "ac_hector", SessionScope: "scope-held"}

	h.Set("scope-held")
	if _, err := p.AcquireFor(ctx, actx); err != ErrHeldByHuman {
		t.Fatalf("held scope: AcquireFor err = %v, want ErrHeldByHuman", err)
	}
	// The yield must not have checked the profile out (no custodian side
	// effects while held) — another scope can claim it freely.
	if !p.custodian.Checkout("browser_ac_hector", "scope-other") {
		t.Fatal("held yield must not check the profile out")
	}

	// Other scopes are unaffected by this scope's hold: a busy fail-fast
	// (not a hold yield) is what a competing scope still sees.
	_, err := p.AcquireFor(ctx, core.AgentCtx{AccountID: "ac_hector", SessionScope: "scope-third"})
	if err == nil || err == ErrHeldByHuman {
		t.Fatalf("unheld scope must not see the hold envelope; got %v", err)
	}

	// Clear → the gate opens: the call now proceeds to checkout and fails
	// only on the pre-existing busy hold, NOT on ErrHeldByHuman.
	h.Clear("scope-held")
	_, err = p.AcquireFor(ctx, actx)
	if err == ErrHeldByHuman {
		t.Fatal("cleared scope must pass the hold gate")
	}
	if err == nil {
		t.Fatal("expected the busy fail-fast (profile held by scope-other)")
	}
}
