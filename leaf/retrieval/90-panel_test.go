package retrieval

import (
	"testing"
	"time"
)

// Depth alone is never a warning — the outbox fills between ticks by design.
// What the panel must catch is a head that stopped moving, or a failing drain.
func TestOutboxStatusIgnoresDepthAlone(t *testing.T) {
	busy := outboxStats{Rows: 400, OldestAge: 3 * time.Second}
	if got := outboxStatus(busy, ""); got != "" {
		t.Fatalf("a deep but moving queue must not be coloured, got %q", got)
	}
	stale := outboxStats{Rows: 1, OldestAge: 26 * time.Hour}
	if got := outboxStatus(stale, ""); got != "warn" {
		t.Fatalf("stale head: want warn, got %q", got)
	}
	// A live drain error outranks age: the queue may look fresh because rows
	// keep arriving while none leave.
	if got := outboxStatus(busy, "leaf 500"); got != "down" {
		t.Fatalf("failing drain: want down, got %q", got)
	}
}

func TestOldestTextEmptyQueueHasNoAge(t *testing.T) {
	if got := oldestText(outboxStats{}); got != "—" {
		t.Fatalf("empty outbox: want dash, got %q", got)
	}
	if got := oldestText(outboxStats{Rows: 2, OldestAge: 90 * time.Second}); got != "1m30s" {
		t.Fatalf("want 1m30s, got %q", got)
	}
}

func TestDrainTextAndStatus(t *testing.T) {
	if got, want := drainText(""), "ok"; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if got, want := drainText("boom"), "failing"; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if got, want := drainStatus(""), "ok"; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if got, want := drainStatus("boom"), "down"; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

// The feeder clears the error on the first success, so a recovered feed never
// shows a stale reason next to a healthy queue.
func TestDrainErrRoundTrip(t *testing.T) {
	var m Module
	if got := m.drainErr(); got != "" {
		t.Fatalf("before the first tick the feed is not failing, got %q", got)
	}
	m.lastDrainErr.Store("leaf 500")
	if got := m.drainErr(); got != "leaf 500" {
		t.Fatalf("want the stored error, got %q", got)
	}
	m.lastDrainErr.Store("")
	if got := m.drainErr(); got != "" {
		t.Fatalf("recovery must clear the reason, got %q", got)
	}
}
