// Tests for the model pool's "fully unavailable" admin alert — the
// false→true transition (≥1 usable entry → every entry dead/paused)
// must page the admin EXACTLY ONCE per outage, and the latch must
// re-arm when any entry becomes usable again so a later outage pages
// afresh.
//
// White-box (package model) — recordError / checkPoolLiveness and the
// allDeadNotified latch are unexported.
package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agentbob/contract"
)

// fakeAdminLine is an in-memory contract.AdminLine that records every
// Notify call so the test can assert exactly how many all-dead alerts
// fired. Mutex-guarded — the heartbeat goroutine never touches it in
// these tests, but recordError / checkPoolLiveness can be reached from
// the streamOneWatch pump goroutine in production, so keep it concurrency-safe.
type fakeAdminLine struct {
	mu    sync.Mutex
	calls []string // one entry per Notify, "<origin>: <rendered text>"
}

func (f *fakeAdminLine) Notify(_ context.Context, _ contract.AdminLevel, origin, format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = args
	f.calls = append(f.calls, origin+": "+format)
}

func (f *fakeAdminLine) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// waitForCount polls count() until it reaches want or the deadline
// expires. Used by tests that fire Notify via notifyAdminAsync (D8,
//) — the goroutine indirection means count reads racy
// just after the trip, even though the value is deterministically
// either now or in a few microseconds.
func (f *fakeAdminLine) waitForCount(want int, within time.Duration) int {
	deadline := time.Now().Add(within)
	for {
		if got := f.count(); got >= want {
			return got
		}
		if time.Now().After(deadline) {
			return f.count()
		}
		time.Sleep(time.Millisecond)
	}
}

// killEntry drives an entry to "dead" via consecutiveFailThreshold
// back-to-back errors (the low-traffic consecutive-fail rule — no
// dependency on wall-clock spacing).
func killEntry(p *MultiPool, row *entryRow) {
	boom := errors.New("backend down")
	for i := 0; i < consecutiveFailThreshold; i++ {
		p.recordError(context.Background(), row, boom)
	}
}

func TestAllDeadTransitionFiresOnce(t *testing.T) {
	a := entry("a", 3, &fakeChatter{})
	b := entry("b", 0, &fakeChatter{})
	p := newTestPool(a, b)
	defer p.Close()
	line := &fakeAdminLine{}
	p.SetAdminLineResolver(func() contract.AdminLine { return line })

	// Kill entry a — b is still usable, so the pool is NOT all-dead.
	killEntry(p, a)
	if a.IsEligible() {
		t.Fatal("entry a should be dead after consecutive failures")
	}
	if got := line.count(); got != 0 {
		t.Fatalf("pool still has a usable entry — no admin alert expected, got %d", got)
	}

	// Kill entry b too — now EVERY entry is dead: one alert fires.
	killEntry(p, b)
	if got := line.waitForCount(1, time.Second); got != 1 {
		t.Fatalf("all-dead transition must fire exactly one admin alert, got %d", got)
	}

	// Further failures while already all-dead must NOT re-page (the latch).
	killEntry(p, a)
	killEntry(p, b)
	// Brief settle so any erroneous extra goroutine has a chance to deliver
	// before we re-check — the latch must hold.
	time.Sleep(20 * time.Millisecond)
	if got := line.count(); got != 1 {
		t.Fatalf("all-dead alert must fire ONCE per outage, got %d after repeated failures", got)
	}
}

// TestAllDeadLatchResetsOnRecovery: when an entry comes back from the
// dead, the all-dead latch must re-arm so a fresh outage pages again.
// We use the heartbeat path (clear deadUntil under the row mutex to
// simulate cooldown expiry) rather than Resume — D10 split
// Resume into pause-only, so it no longer doubles as a generic "fresh
// start" gesture for cooling state.
func TestAllDeadLatchResetsOnRecovery(t *testing.T) {
	a := entry("a", 3, &fakeChatter{})
	b := entry("b", 0, &fakeChatter{})
	p := newTestPool(a, b)
	defer p.Close()
	line := &fakeAdminLine{}
	p.SetAdminLineResolver(func() contract.AdminLine { return line })

	// Drive the pool fully dead — one alert.
	killEntry(p, a)
	killEntry(p, b)
	if got := line.waitForCount(1, time.Second); got != 1 {
		t.Fatalf("expected one all-dead alert, got %d", got)
	}

	// Simulate cooldown expiry / heartbeat recovery on entry a — its
	// deadUntil clears, it becomes eligible again. The all-dead latch
	// must re-arm on the next checkPoolLiveness call.
	a.mu.Lock()
	a.deadUntil = time.Time{}
	a.errTimes = nil
	a.consecutiveFails = 0
	a.mu.Unlock()
	p.checkPoolLiveness()
	if !a.IsEligible() {
		t.Fatal("entry a should be usable after liveness reset")
	}

	// Re-kill entry a. b is still dead from the first outage, so a's
	// death is the false→true transition again — the re-armed latch
	// pages the admin a SECOND time.
	killEntry(p, a)
	if got := line.waitForCount(2, time.Second); got != 2 {
		t.Fatalf("a second outage after recovery must fire a second alert, got %d total", got)
	}
}

// TestAllDeadPerKindIndependent: with a single non-kind-keyed latch, a second
// kind's outage that begins while a first kind is already latched was never
// paged. Kill the llm kind (alert #1), then — while llm is still dead — kill the
// ocr kind: its usable→dead transition must page independently (alert #2).
func TestAllDeadPerKindIndependent(t *testing.T) {
	llm := entry("llm", 0, &fakeChatter{})
	ocr := entry("ocr", 0, &fakeChatter{})
	ocr.info.Kind = contract.KindOCR
	p := newTestPool(llm, ocr)
	defer p.Close()
	line := &fakeAdminLine{}
	p.SetAdminLineResolver(func() contract.AdminLine { return line })

	// llm kind fully dead — ocr still healthy, so exactly one alert (kind llm).
	killEntry(p, llm)
	if got := line.waitForCount(1, time.Second); got != 1 {
		t.Fatalf("llm-kind outage must fire one alert, got %d", got)
	}

	// ocr kind now dies while llm is still latched — a per-kind latch must page
	// again; a single global bool would swallow this.
	killEntry(p, ocr)
	if got := line.waitForCount(2, time.Second); got != 2 {
		t.Fatalf("a second kind's outage overlapping the first must page again, got %d total", got)
	}
}

func TestAllDeadNilAdminLineSafe(t *testing.T) {
	// No admin line wired — the all-dead transition must be a silent
	// no-op, never a nil-deref panic.
	a := entry("a", 0, &fakeChatter{})
	p := newTestPool(a)
	defer p.Close()
	killEntry(p, a) // would panic if checkPoolLiveness didn't nil-guard
}

// TestKindExhaustionPagesWhileEntryStaysHealthy is the regression for the ASR
// outage of: a kind whose ONLY entry fails every call it ever gets
// was never paged, because the alert rode checkPoolLiveness, which runs only
// once RecordError marks an entry NEWLY DEAD — and the cooling breaker counts
// CALLS (consecutiveFailThreshold), so a rarely-called kind never reaches it.
// The pool considered a 100%-failing backend healthy and said nothing.
//
// A single failing call must now page on candidate EXHAUSTION alone, with the
// entry still eligible (i.e. cooling deliberately did NOT trip).
func TestKindExhaustionPagesWhileEntryStaysHealthy(t *testing.T) {
	asr := entry("asr-only", 0, &fakeChatter{chatErr: errors.New("500: model failed to load")})
	asr.info.Kind = contract.KindASR
	p := newTestPool(asr)
	defer p.Close()
	line := &fakeAdminLine{}
	p.SetAdminLineResolver(func() contract.AdminLine { return line })

	asrReq := contract.ModelRequest{Kind: contract.KindASR}
	if _, err := p.Chat(context.Background(), asrReq, nil); err == nil {
		t.Fatal("the only ASR entry errors — Chat must fail")
	}
	// The whole point: ONE failure is below consecutiveFailThreshold, so the
	// entry is untouched by cooling. Under the old wiring this meant silence.
	if !asr.IsEligible() {
		t.Fatal("one failure must not cool the entry — this test would prove nothing otherwise")
	}
	if got := line.waitForCount(1, time.Second); got != 1 {
		t.Fatalf("a kind with no fallback left must page on the first exhaustion, got %d", got)
	}

	// Latched: a second exhaustion inside the same outage must not re-page.
	if _, err := p.Chat(context.Background(), asrReq, nil); err == nil {
		t.Fatal("expected the second call to fail too")
	}
	time.Sleep(20 * time.Millisecond) // let any erroneous extra delivery land
	if got := line.count(); got != 1 {
		t.Fatalf("exhaustion alert must fire ONCE per outage, got %d", got)
	}
}

// TestKindExhaustionLatchResetsOnSuccess: only a real served call re-arms the
// page. (A reachability Ping must not — an ASR/OCR shim answers /v1/models
// while its model is unloadable, which is exactly how the outage
// stayed invisible.)
func TestKindExhaustionLatchResetsOnSuccess(t *testing.T) {
	fake := &fakeChatter{
		text:     "ok",
		chatErrs: []error{errors.New("boom"), nil, errors.New("boom again")},
	}
	asr := entry("asr-only", 0, fake)
	asr.info.Kind = contract.KindASR
	p := newTestPool(asr)
	defer p.Close()
	line := &fakeAdminLine{}
	p.SetAdminLineResolver(func() contract.AdminLine { return line })

	asrReq := contract.ModelRequest{Kind: contract.KindASR}
	if _, err := p.Chat(context.Background(), asrReq, nil); err == nil {
		t.Fatal("call 1 is scripted to fail")
	}
	if got := line.waitForCount(1, time.Second); got != 1 {
		t.Fatalf("first exhaustion must page, got %d", got)
	}

	// Call 2 succeeds — the kind is serving again, so the latch clears.
	if _, err := p.Chat(context.Background(), asrReq, nil); err != nil {
		t.Fatalf("call 2 is scripted to succeed: %v", err)
	}

	// Call 3 fails: a fresh outage after a recovery must page again.
	if _, err := p.Chat(context.Background(), asrReq, nil); err == nil {
		t.Fatal("call 3 is scripted to fail")
	}
	if got := line.waitForCount(2, time.Second); got != 2 {
		t.Fatalf("an outage after a served call must page afresh, got %d total", got)
	}
}

// TestKindExhaustionDoesNotDoublePageWithLivenessAlert: when the attempt that
// exhausts a kind is also the one that tips its last entry into cooling, the
// liveness latch and the exhaustion latch describe the SAME outage. One page.
func TestKindExhaustionDoesNotDoublePageWithLivenessAlert(t *testing.T) {
	asr := entry("asr-only", 0, &fakeChatter{chatErr: errors.New("backend down")})
	asr.info.Kind = contract.KindASR
	p := newTestPool(asr)
	defer p.Close()
	line := &fakeAdminLine{}
	p.SetAdminLineResolver(func() contract.AdminLine { return line })

	asrReq := contract.ModelRequest{Kind: contract.KindASR}
	// consecutiveFailThreshold calls: the last one both exhausts the kind AND
	// marks the sole entry dead, driving both alert paths in one request.
	for i := 0; i < consecutiveFailThreshold; i++ {
		if _, err := p.Chat(context.Background(), asrReq, nil); err == nil {
			t.Fatalf("call %d must fail", i+1)
		}
	}
	if asr.IsEligible() {
		t.Fatal("the entry should now be cooling — both paths must have been live")
	}
	time.Sleep(30 * time.Millisecond)
	if got := line.count(); got != 1 {
		t.Fatalf("one outage must produce exactly one page, got %d", got)
	}
}

// TestKindExhaustionIgnoresRequestFaults: a bad REQUEST is not an outage. A
// content-level 4xx and a tool-args-truncated cascade both fail every candidate
// identically — the very shape that reaches the exhaustion branch — and
// RecordError already refuses to cool an entry for either. The page must apply
// the same filter, or one oversized/malformed request cries outage over a
// perfectly healthy pool.
func TestKindExhaustionIgnoresRequestFaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"content-level 4xx", errors.New("POST http://x/v1/chat/completions → 413 Payload Too Large: too big")},
		{"tool-args truncated", errors.New("failed to parse tool call arguments: invalid json: unexpected end of JSON input")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := entry("a", 3, &fakeChatter{chatErr: tc.err})
			b := entry("b", 0, &fakeChatter{chatErr: tc.err})
			p := newTestPool(a, b)
			defer p.Close()
			line := &fakeAdminLine{}
			p.SetAdminLineResolver(func() contract.AdminLine { return line })

			// Both candidates reject it → the kind IS exhausted for this
			// request, but the fault is the request's, not the pool's.
			if _, err := p.Chat(context.Background(), smartReq, nil); err == nil {
				t.Fatal("every candidate rejects this request — Chat must fail")
			}
			time.Sleep(20 * time.Millisecond) // let any erroneous page land
			if got := line.count(); got != 0 {
				t.Fatalf("a request fault must not page as an outage, got %d alert(s)", got)
			}
		})
	}
}
