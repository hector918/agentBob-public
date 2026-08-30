package browser

import (
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeClock is an advanceable clock for deterministic Reclaim tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// recordingCloser records which profile keys got closed.
type recordingCloser struct {
	mu     sync.Mutex
	closed []string
}

func (r *recordingCloser) close(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = append(r.closed, key)
}
func (r *recordingCloser) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.closed...)
	sort.Strings(out)
	return out
}

func newTestCustodian(t *testing.T, ttl time.Duration) (*ProfileCustodian, *fakeClock, *recordingCloser) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	cl := &recordingCloser{}
	c := NewProfileCustodian(cl.close, ttl, clk.now)
	t.Cleanup(c.Close)
	return c, clk, cl
}

// TestCustodian_CheckoutExclusivity pins the core "one owner per profile, others
// fail-fast" semantics, plus a scope owning several distinct profiles.
func TestCustodian_CheckoutExclusivity(t *testing.T) {
	c, _, _ := newTestCustodian(t, 5*time.Minute)

	// Free profile → granted to scope A.
	if !c.Checkout("ac_hector", "scopeA") {
		t.Fatal("first checkout of a free profile must be granted")
	}
	// Same scope re-checkout (next turn of the same task) → still granted.
	if !c.Checkout("ac_hector", "scopeA") {
		t.Fatal("same-scope re-checkout must be granted (head-to-tail hold)")
	}
	// Different scope wants the same profile → fail-fast.
	if c.Checkout("ac_hector", "scopeB") {
		t.Fatal("a different scope must fail-fast while scopeA holds the profile")
	}
	// A scope may own multiple DISTINCT profiles (two people in one group chat).
	if !c.Checkout("ac_alice", "scopeA") {
		t.Fatal("scopeA must be able to hold a second, distinct profile")
	}
	snap := c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d checkouts, want 2", len(snap))
	}
}

// TestCustodian_ReleaseScope frees every profile a scope owns and closes their
// instances; another scope can then claim them.
func TestCustodian_ReleaseScope(t *testing.T) {
	c, _, cl := newTestCustodian(t, 5*time.Minute)
	c.Checkout("ac_hector", "scopeA")
	c.Checkout("ac_alice", "scopeA")
	c.Checkout("ac_bob", "scopeB") // owned by a different scope — must NOT be freed

	if n := c.ReleaseScope("scopeA"); n != 2 {
		t.Fatalf("ReleaseScope freed %d, want 2", n)
	}
	if got := cl.snapshot(); len(got) != 2 || got[0] != "ac_alice" || got[1] != "ac_hector" {
		t.Fatalf("closed instances = %v, want [ac_alice ac_hector]", got)
	}
	// scopeB's profile survives; scopeC can now grab a freed one.
	if !c.Checkout("ac_hector", "scopeC") {
		t.Fatal("a freed profile must be grantable to a new scope")
	}
	if c.Checkout("ac_bob", "scopeC") {
		t.Fatal("scopeB's profile must still be held (only scopeA was released)")
	}
}

// TestCustodian_ReclaimIdle pins the 5-min backstop: untouched checkouts are
// reclaimed, freshly-touched ones survive, and a same-scope re-Checkout (the
// per-action touch path) resets the clock.
func TestCustodian_ReclaimIdle(t *testing.T) {
	c, clk, cl := newTestCustodian(t, 5*time.Minute)
	c.Checkout("ac_hector", "scopeA")
	c.Checkout("ac_alice", "scopeA")

	// 3 min in: touch hector only (re-checkout refreshes lastTouched).
	// Nothing is idle past 5 min yet.
	clk.advance(3 * time.Minute)
	c.Checkout("ac_hector", "scopeA")
	if n := c.Reclaim(); n != 0 {
		t.Fatalf("reclaim at 3min = %d, want 0 (nothing idle past 5min)", n)
	}

	// +4 min (alice untouched 7 min → idle; hector touched 4 min ago → fresh).
	clk.advance(4 * time.Minute)
	if n := c.Reclaim(); n != 1 {
		t.Fatalf("reclaim = %d, want 1 (alice idle, hector fresh)", n)
	}
	if got := cl.snapshot(); len(got) != 1 || got[0] != "ac_alice" {
		t.Fatalf("reclaimed-closed = %v, want [ac_alice]", got)
	}
	// hector still held.
	if c.Checkout("ac_hector", "scopeOther") {
		t.Fatal("hector was touched recently — must still be held, not reclaimed")
	}
}

// TestCustodian_PinExemptsReclaim pins that a handoff-pinned profile is never
// idle-reclaimed (human logging in may take minutes), and unpinning re-arms it.
func TestCustodian_PinExemptsReclaim(t *testing.T) {
	c, clk, _ := newTestCustodian(t, 5*time.Minute)
	c.Checkout("ac_hector", "scopeA")
	if !c.Pin("ac_hector", true) {
		t.Fatal("pin on a checked-out profile must succeed")
	}
	// Way past idleTTL, but pinned → not reclaimed.
	clk.advance(30 * time.Minute)
	if n := c.Reclaim(); n != 0 {
		t.Fatalf("reclaim of a pinned profile = %d, want 0", n)
	}
	// Unpin → now reclaimable.
	c.Pin("ac_hector", false)
	if n := c.Reclaim(); n != 1 {
		t.Fatalf("reclaim after unpin = %d, want 1", n)
	}
	// Pin on an unknown profile reports false.
	if c.Pin("ac_nobody", true) {
		t.Fatal("pin on an unknown profile must return false")
	}
}

// TestCustodian_PinRefcount pins that Pin is a refcount, not a boolean: with
// two concurrent takeover streams on one profile, the first stream stopping
// must NOT strip the still-live second stream's reclaim exemption. Also pins
// the floor-at-0: an unmatched unpin must not leave a sticky negative count
// that swallows a later real pin.
func TestCustodian_PinRefcount(t *testing.T) {
	c, clk, _ := newTestCustodian(t, 5*time.Minute)
	c.Checkout("ac_hector", "scopeA")
	c.Pin("ac_hector", true)  // stream 1
	c.Pin("ac_hector", true)  // stream 2
	c.Pin("ac_hector", false) // stream 1 stops
	clk.advance(30 * time.Minute)
	if n := c.Reclaim(); n != 0 {
		t.Fatalf("reclaim with one stream still pinned = %d, want 0", n)
	}
	c.Pin("ac_hector", false) // stream 2 stops — last hold gone
	if n := c.Reclaim(); n != 1 {
		t.Fatalf("reclaim after last unpin = %d, want 1", n)
	}

	// Floor at 0: unpin-before-pin (release+re-checkout shuffle) must not
	// underflow; the subsequent pin/unpin pair still nets to reclaimable.
	c.Checkout("ac_x", "scopeB")
	c.Pin("ac_x", false)
	c.Pin("ac_x", true)
	c.Pin("ac_x", false)
	clk.advance(30 * time.Minute)
	if n := c.Reclaim(); n != 1 {
		t.Fatalf("reclaim after balanced pin/unpin = %d, want 1", n)
	}
}

// TestCustodian_ConcurrentCheckout exercises the actor's core claim — that
// concurrent callers are serialised — under -race: N goroutines race to check
// out ONE free profile from N distinct scopes; exactly one must win.
func TestCustodian_ConcurrentCheckout(t *testing.T) {
	c, _, _ := newTestCustodian(t, 5*time.Minute)
	const N = 32
	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // line them all up to maximise the race
			if c.Checkout("ac_shared", "scope"+strconv.Itoa(i)) {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent checkout of one free profile: %d winners, want exactly 1", wins)
	}
}

// TestCustodian_CallsAfterCloseDontHang pins the engineered shutdown safety: a
// call racing/after Close() returns the sentinel promptly instead of wedging on
// the readerless reqs channel (the handleWg-drain hazard the post() guard fixes).
func TestCustodian_CallsAfterCloseDontHang(t *testing.T) {
	cl := &recordingCloser{}
	c := NewProfileCustodian(cl.close, time.Minute, (&fakeClock{t: time.Unix(1, 0)}).now)
	c.Close()

	done := make(chan struct{})
	go func() {
		// None of these may block now that the actor is gone.
		if c.Checkout("p", "s") {
			t.Error("Checkout after Close should return false")
		}
		if c.Pin("p", true) {
			t.Error("Pin after Close should return false")
		}
		if c.ReleaseScope("s") != 0 {
			t.Error("ReleaseScope after Close should return 0")
		}
		if c.Reclaim() != 0 {
			t.Error("Reclaim after Close should return 0")
		}
		if c.Snapshot() != nil {
			t.Error("Snapshot after Close should return nil")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a call after Close() hung — post() quit-guard regressed")
	}
}

// TestCustodian_CloserPanicSurvives pins the load-bearing fix: a panicking closer
// must NOT wedge the actor — the triggering call still returns and subsequent
// calls keep working.
func TestCustodian_CloserPanicSurvives(t *testing.T) {
	panicker := func(key string) {
		if key == "ac_bomb" {
			panic("boom")
		}
	}
	c := NewProfileCustodian(panicker, time.Minute, (&fakeClock{t: time.Unix(1, 0)}).now)
	t.Cleanup(c.Close)

	c.Checkout("ac_bomb", "scopeA")
	done := make(chan int, 1)
	go func() { done <- c.ReleaseScope("scopeA") }() // closer panics inside run()
	select {
	case n := <-done:
		if n != 1 {
			t.Fatalf("ReleaseScope freed %d, want 1 (panic swallowed)", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReleaseScope hung — closer panic wedged the actor")
	}
	// Actor must still be alive and serving.
	if !c.Checkout("ac_ok", "scopeB") {
		t.Fatal("custodian wedged after a closer panic — subsequent Checkout hung/failed")
	}
}

// TestCustodian_ReclaimBoundary pins the exact TTL edge: age == idleTTL is NOT
// reclaimed (strictly-older-than semantics); one tick past is.
func TestCustodian_ReclaimBoundary(t *testing.T) {
	c, clk, _ := newTestCustodian(t, 5*time.Minute)
	c.Checkout("ac_x", "scopeA")
	clk.advance(5 * time.Minute) // age == idleTTL exactly
	if n := c.Reclaim(); n != 0 {
		t.Fatalf("reclaim at age==TTL = %d, want 0 (strictly-older-than)", n)
	}
	clk.advance(time.Nanosecond)
	if n := c.Reclaim(); n != 1 {
		t.Fatalf("reclaim at age>TTL = %d, want 1", n)
	}
}

// TestCustodian_ReleaseEmptyScope — releasing a scope that owns nothing is a
// no-op returning 0.
func TestCustodian_ReleaseEmptyScope(t *testing.T) {
	c, _, _ := newTestCustodian(t, 5*time.Minute)
	if n := c.ReleaseScope("nobody"); n != 0 {
		t.Fatalf("ReleaseScope(nobody) = %d, want 0", n)
	}
}
