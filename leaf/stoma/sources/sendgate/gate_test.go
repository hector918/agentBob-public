package sendgate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// never is a classifier that recognises nothing — for tests that don't
// exercise the 429 path.
func never(error) (time.Duration, bool) { return 0, false }

// throttleAs builds a classifier that maps target to a fixed retry-after.
func throttleAs(target error, ra time.Duration) Classifier {
	return func(err error) (time.Duration, bool) {
		if errors.Is(err, target) {
			return ra, true
		}
		return 0, false
	}
}

// TestGate_SerialisesAllSends pins the single slot: concurrent Do invocations
// never overlap — the cross-sink serialisation that stops two concurrent turns
// from firing platform calls in parallel and overrunning the per-bot limit.
func TestGate_SerialisesAllSends(t *testing.T) {
	g := New(never)
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	call := func() error {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond) // hold long enough to detect overlap
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.Do(context.Background(), call)
		}()
	}
	wg.Wait()
	if maxInFlight != 1 {
		t.Fatalf("max concurrent sends through one gate = %d, want 1 (serialised)", maxInFlight)
	}
}

// TestGate_FloorPacesNextCall pins the shared cooldown: a 429 pushes the floor,
// and the NEXT call waits it out before dialing — a throttle one sink hits
// slows every other sink too. (retry-after > RelayClamp so the first call is
// handed back rather than relayed, isolating the floor behaviour.)
func TestGate_FloorPacesNextCall(t *testing.T) {
	rl := errors.New("429")
	g := New(func(err error) (time.Duration, bool) {
		if errors.Is(err, rl) {
			return 40 * time.Millisecond, true
		}
		return 0, false
	})
	// Exhaust the relay budget on an always-429 call so it returns throttled.
	if err := g.Do(context.Background(), func() error { return rl }); !errors.Is(err, rl) {
		t.Fatalf("throttled Do = %v, want the 429 back after the relay budget", err)
	}
	// The floor now sits ~40ms out; the next call must wait for it.
	start := time.Now()
	ran := false
	if err := g.Do(context.Background(), func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("second Do: err=%v ran=%v", err, ran)
	}
	if waited := time.Since(start); waited < 30*time.Millisecond {
		t.Fatalf("second call waited %v, want ≥ ~40ms cooldown (429 floor not honoured)", waited)
	}
}

// TestGate_RelaysThrottledCall pins the relay: a call that 429s once and then
// succeeds is delivered by the gate itself — the caller sees nil, not the 429.
func TestGate_RelaysThrottledCall(t *testing.T) {
	rl := errors.New("429")
	g := New(throttleAs(rl, 5*time.Millisecond))
	calls := 0
	err := g.Do(context.Background(), func() error {
		calls++
		if calls == 1 {
			return rl
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("relayed Do: err=%v calls=%d, want nil after 2 dials", err, calls)
	}
}

// TestGate_RelayBudget pins the bound: an always-throttled call dials exactly
// RelayAttempts times, then the 429 comes back (floor stays pushed).
func TestGate_RelayBudget(t *testing.T) {
	rl := errors.New("429")
	g := New(throttleAs(rl, time.Millisecond))
	calls := 0
	if err := g.Do(context.Background(), func() error { calls++; return rl }); !errors.Is(err, rl) {
		t.Fatalf("exhausted Do = %v, want the 429 back", err)
	}
	if calls != RelayAttempts {
		t.Fatalf("dials = %d, want %d", calls, RelayAttempts)
	}
}

// TestGate_LongRetryAfterHandsBack pins the clamp: a retry-after past RelayClamp
// is never sat out slot-held — the 429 returns after ONE dial.
func TestGate_LongRetryAfterHandsBack(t *testing.T) {
	rl := errors.New("429")
	g := New(throttleAs(rl, RelayClamp+time.Second))
	calls := 0
	if err := g.Do(context.Background(), func() error { calls++; return rl }); !errors.Is(err, rl) {
		t.Fatalf("long-ask Do = %v, want the 429 back", err)
	}
	if calls != 1 {
		t.Fatalf("dials = %d, want 1 (no relay past the clamp)", calls)
	}
}

// TestGate_NonThrottleReturnsImmediately pins the 429-only rule: any other
// error goes straight back — a timeout MAY have delivered, so the gate must
// never redial it.
func TestGate_NonThrottleReturnsImmediately(t *testing.T) {
	boom := errors.New("boom")
	g := New(never)
	calls := 0
	if err := g.Do(context.Background(), func() error { calls++; return boom }); !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want boom after exactly 1 dial", err, calls)
	}
}

// TestGate_CtxCancelDuringFloorWait pins the ctx-aware floor: cancelled while
// parked on the cooldown → ctx.Err() WITHOUT dialing.
func TestGate_CtxCancelDuringFloorWait(t *testing.T) {
	g := New(never)
	g.notBefore = time.Now().Add(5 * time.Second) // long cooldown
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	called := false
	if err := g.Do(ctx, func() error { called = true; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Do err = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("wire call ran despite ctx cancellation during the cooldown wait")
	}
}

// TestGate_ExpiredCtxNeverDials pins timeliness: a deadline that lapses while
// queued for the slot means the send is dropped, not fired late (the typing-
// indicator / reaction case — callers bound their ctx to declare urgency).
func TestGate_ExpiredCtxNeverDials(t *testing.T) {
	g := New(never)
	release := make(chan struct{})
	go g.Do(context.Background(), func() error { <-release; return nil }) // park the slot
	time.Sleep(5 * time.Millisecond)                                      // let the holder acquire

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	called := false
	done := make(chan error, 1)
	go func() { done <- g.Do(ctx, func() error { called = true; return nil }) }()
	time.Sleep(40 * time.Millisecond) // deadline lapses while queued
	close(release)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued-past-deadline Do = %v, want DeadlineExceeded", err)
	}
	if called {
		t.Fatal("send fired after its deadline lapsed in the queue")
	}
}

// TestRelay mirrors the gate's relay semantics without the slot: one 429 then
// success → delivered; always-429 → RelayAttempts dials then the error.
func TestRelay(t *testing.T) {
	rl := errors.New("429")
	classify := throttleAs(rl, time.Millisecond)

	calls := 0
	if err := Relay(context.Background(), classify, func() error {
		calls++
		if calls == 1 {
			return rl
		}
		return nil
	}); err != nil || calls != 2 {
		t.Fatalf("relay: err=%v calls=%d, want nil after 2 dials", err, calls)
	}

	calls = 0
	if err := Relay(context.Background(), classify, func() error { calls++; return rl }); !errors.Is(err, rl) || calls != RelayAttempts {
		t.Fatalf("relay exhaustion: err=%v calls=%d, want 429 after %d dials", err, calls, RelayAttempts)
	}

	boom := errors.New("boom")
	calls = 0
	if err := Relay(context.Background(), classify, func() error { calls++; return boom }); !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("relay non-429: err=%v calls=%d, want boom after 1 dial", err, calls)
	}
}

// TestRelay_LongRetryAfterClampsAndRetries pins the L-D3 guarantee: unlike
// Gate.Do (which hands a long ask back to free its slot), Relay holds no slot,
// so a retry-after past RelayClamp is clamped and the dial still retried — a
// one-off notice must not drop because the server asked for a minute.
func TestRelay_LongRetryAfterClampsAndRetries(t *testing.T) {
	rl := errors.New("429")
	// Classifier asks for far past the clamp; wait is clamped, retry proceeds.
	// (Keep the test fast: run with a goroutine-free cancelable ctx and a call
	// that succeeds on dial 2 — the clamped wait is 15s, too slow to sit out,
	// so bound the wall time via a short deadline and assert on dials instead.)
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := Relay(ctx, throttleAs(rl, RelayClamp+time.Minute), func() error {
		calls++
		return rl
	})
	// The clamped wait (15s) outlives the 50ms deadline → ctx error, but the
	// point is the dial was NOT abandoned pre-wait: exactly one dial happened
	// and Relay chose to wait (clamped) rather than hand the 429 back.
	if calls != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("long-ask relay: calls=%d err=%v, want 1 dial then a clamped ctx-aware wait", calls, err)
	}
}

// TestRelay_CtxCancelDuringWait pins the ctx-aware backoff: cancelled while
// waiting out a retry-after, Relay returns ctx.Err() without redialing.
func TestRelay_CtxCancelDuringWait(t *testing.T) {
	rl := errors.New("429")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	calls := 0
	err := Relay(ctx, throttleAs(rl, 5*time.Second), func() error { calls++; return rl })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancel during relay wait: err=%v calls=%d, want context.Canceled after 1 dial", err, calls)
	}
}
