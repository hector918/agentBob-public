// Busy-backend queueing (hector): a backend that answers "busy /
// not here right now" — 429 / 502 / 503 / 504, or a connection that never
// completed — is a QUEUE condition, not an outage. The pool waits it out on the
// last-resort candidate (withBusyRetry) instead of declaring the kind
// unavailable, and the waiting itself is bounded: a kind's wait queue holds
// queueDepthFactor × its total concurrency, past which callers are told the
// queue is full (contract.ErrModelQueueFull) and the admin is paged once.
//
// Real incident behind these tests: 23:48:20, the sole kind:ocr
// entry got one nginx 502 while its sidecar restarted, the pool declared "no
// fallback left", and the model quietly answered with a vision description
// instead of a transcription. The same kind served fine 70s later.
//
// White-box (package model).
package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"agentbob/contract"
)

// shrinkBusyRetry collapses the retry budget + backoff to test speed.
func shrinkBusyRetry(t *testing.T) {
	t.Helper()
	oldBudget, oldBackoff := busyRetryBudget, busyRetryBackoff
	busyRetryBudget = 200 * time.Millisecond
	busyRetryBackoff = []time.Duration{2 * time.Millisecond}
	t.Cleanup(func() { busyRetryBudget, busyRetryBackoff = oldBudget, oldBackoff })
}

// ocrEntry builds an eligible kind:ocr entry with a bounded semaphore — the
// shape of the real single-candidate sidecar kinds (concurrency: 1).
func ocrEntry(name string, c contract.Chatter, slots, priority int) *entryRow {
	return &entryRow{
		info: contract.ModelInfo{
			Name: name, Kind: contract.KindOCR, Model: name + "-model", Priority: priority,
		},
		chatter: c,
		sem:     make(chan struct{}, slots),
	}
}

var ocrReq = contract.ModelRequest{Kind: contract.KindOCR}

// gateway502 is the exact error text the rapidocr provider produces when the
// sidecar's fronting nginx has no upstream (the observed incident).
func gateway502() error {
	return fmt.Errorf("rapidocr: 1/1 image(s) failed (sidecar likely unhealthy): rapidocr: HTTP 502: <html>\r\n<head><title>502 Bad Gateway</title></head>\r\n</html>")
}

func healthOf(row *entryRow) (fails int, dead bool) {
	row.mu.Lock()
	defer row.mu.Unlock()
	return row.consecutiveFails, !row.deadUntil.IsZero() && row.deadUntil.After(time.Now())
}

// TestBusyRetryAbsorbsTransientBlip: the sole candidate 502s twice, then
// serves. The call must SUCCEED (not fail the tool), and the absorbed blip must
// leave no health damage — a restart window is not a strike against the entry.
func TestBusyRetryAbsorbsTransientBlip(t *testing.T) {
	shrinkBusyRetry(t)
	ch := &fakeChatter{text: "transcribed", chatErrs: []error{gateway502(), gateway502()}}
	row := ocrEntry("pp-ocrv6", ch, 1, 0)
	p := newTestPool(row)
	defer p.Close()

	resp, err := p.Chat(context.Background(), ocrReq, nil)
	if err != nil {
		t.Fatalf("a two-blip 502 must be absorbed, got %v", err)
	}
	if resp.Content != "transcribed" {
		t.Errorf("content = %q, want the real transcription", resp.Content)
	}
	if calls, _ := ch.counts(); calls != 3 {
		t.Errorf("chat calls = %d, want 3 (two blips + the served retry)", calls)
	}
	if fails, dead := healthOf(row); fails != 0 || dead {
		t.Errorf("absorbed blip must not count against health: consecutiveFails=%d dead=%v", fails, dead)
	}
}

// TestBusyRetryOnlyOnLastResort: while a healthy peer remains, a busy entry is
// abandoned for the peer rather than queued on — failing over is both faster
// and healthier than waiting.
func TestBusyRetryOnlyOnLastResort(t *testing.T) {
	shrinkBusyRetry(t)
	busy := &fakeChatter{chatErr: gateway502()}
	good := &fakeChatter{text: "served-by-peer"}
	busyRow := ocrEntry("busy", busy, 1, 10) // higher priority → picked first
	goodRow := ocrEntry("good", good, 1, 5)
	p := newTestPool(busyRow, goodRow)
	defer p.Close()

	resp, err := p.Chat(context.Background(), ocrReq, nil)
	if err != nil {
		t.Fatalf("must fail over to the healthy peer, got %v", err)
	}
	if resp.Content != "served-by-peer" {
		t.Errorf("content = %q, want the peer's answer", resp.Content)
	}
	if calls, _ := busy.counts(); calls != 1 {
		t.Errorf("busy entry chat calls = %d, want 1 (no queueing while a peer exists)", calls)
	}
}

// TestBusyRetryGivesUpAfterBudget: a backend that never comes back must still
// produce a hard failure once the budget elapses — "keep queueing" is bounded,
// not forever — and the entry takes exactly ONE health strike for the whole
// sequence, so a persistent outage still cools it at the normal rate.
func TestBusyRetryGivesUpAfterBudget(t *testing.T) {
	shrinkBusyRetry(t)
	ch := &fakeChatter{chatErr: gateway502()}
	row := ocrEntry("pp-ocrv6", ch, 1, 0)
	p := newTestPool(row)
	defer p.Close()

	_, err := p.Chat(context.Background(), ocrReq, nil)
	if !errors.Is(err, ErrNoModelAvailable) {
		t.Fatalf("a backend that never returns must end as ErrNoModelAvailable, got %v", err)
	}
	calls, _ := ch.counts()
	if calls < 2 {
		t.Errorf("chat calls = %d, want the request to have retried before giving up", calls)
	}
	if fails, _ := healthOf(row); fails != 1 {
		t.Errorf("consecutiveFails = %d, want exactly 1 (one request = one strike, however many retries)", fails)
	}
}

// TestQueueCapacityIsTwiceConcurrency pins the depth rule: 2 × the kind's total
// configured concurrency, summed over enabled entries, health-blind.
func TestQueueCapacityIsTwiceConcurrency(t *testing.T) {
	p := newTestPool(ocrEntry("a", &fakeChatter{}, 1, 0), ocrEntry("b", &fakeChatter{}, 3, 0))
	defer p.Close()
	if got := p.queueCapacity(contract.KindOCR); got != 8 {
		t.Errorf("queueCapacity = %d, want 8 (2 × (1+3))", got)
	}
	if got := p.queueCapacity(contract.KindLLM); got != 0 {
		t.Errorf("queueCapacity for a kind with no entries = %d, want 0", got)
	}
}

// TestQueueFullRejectsPastCap: with the single slot busy and the queue at its
// cap, the next caller is told the QUEUE is full (a busy backend) rather than
// being parked in an unbounded line.
func TestQueueFullRejectsPastCap(t *testing.T) {
	ch := &fakeChatter{text: "ok"}
	row := ocrEntry("pp-ocrv6", ch, 1, 0) // cap 1 slot → queue holds 2
	p := newTestPool(row)
	defer p.Close()

	// Occupy the only slot, so every further caller has to queue.
	row.sem <- struct{}{}

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Chat(ctx, ocrReq, nil)
		}()
	}
	waitQueueDepth(t, p, contract.KindOCR, 2)

	// Queue is at cap (2 = 2 × 1 slot) — the third caller is rejected.
	_, err := p.Chat(context.Background(), ocrReq, nil)
	if !errors.Is(err, contract.ErrModelQueueFull) {
		t.Fatalf("past-cap caller must get ErrModelQueueFull, got %v", err)
	}

	cancel()
	wg.Wait()
	<-row.sem
}

// waitQueueDepth blocks until the kind's wait queue holds `want` requests.
func waitQueueDepth(t *testing.T, p *MultiPool, kind string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.queueMu.Lock()
		got := p.queueDepth[kind]
		p.queueMu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("wait queue never reached depth %d", want)
}

// TestIsTransientBackendErr pins the classification boundary — everything the
// pool is willing to WAIT on, and everything it must not.
func TestIsTransientBackendErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"sidecar gateway 502", gateway502(), true},
		{"chat client 503", fmt.Errorf("POST http://x/v1/chat → 503 Service Unavailable: upstream"), true},
		{"rate limited 429", fmt.Errorf("POST http://x/v1/chat → 429 Too Many Requests: slow down"), true},
		{"connection refused", fmt.Errorf(`Post "http://x/ocr": dial tcp 1.2.3.4:11500: connect: connection refused`), true},
		{"eof mid-restart", fmt.Errorf(`Post "http://x/ocr": EOF`), true},
		{"backend 500", fmt.Errorf("POST http://x/v1/chat → 500 Internal Server Error: boom"), false},
		{"auth revoked", fmt.Errorf("POST http://x/v1/chat → 401 Unauthorized: bad key"), false},
		{"context overflow", fmt.Errorf("POST http://x/v1/chat → 400 Bad Request: maximum context length"), false},
		{"caller cancelled", context.Canceled, false},
		{"committed stream", fmt.Errorf("%w: broken pipe", ErrStreamCommitted), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTransientBackendErr(c.err); got != c.want {
				t.Errorf("isTransientBackendErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
