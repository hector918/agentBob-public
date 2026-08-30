package stoma

import (
	"errors"
	"testing"

	"agentbob/contract"
)

func statusOf(ss []SourceStatus, name string) string {
	for _, s := range ss {
		if s.Name == name {
			return s.Status
		}
	}
	return "<absent>"
}

// TestHealthStopped pins F35: a clean io.EOF self-exit marks the source "stopped" (not
// green "ready" and not error "dead"); a later probe success must not resurrect it.
func TestHealthStopped(t *testing.T) {
	h := newHealthTable(map[string]contract.Source{"local": nil, "tg": nil})

	// Fresh sources read ready.
	if got := statusOf(h.snapshot(), "local"); got != "ready" {
		t.Fatalf("fresh source status = %q, want ready", got)
	}

	// EOF self-exit → stopped (no error), and a subsequent passing probe can't revive it.
	h.markStopped("local")
	if got := statusOf(h.snapshot(), "local"); got != "stopped" {
		t.Fatalf("after markStopped, status = %q, want stopped", got)
	}
	if tr := h.record("local", nil); tr != "" {
		t.Errorf("a passing probe on a stopped source must not transition, got %q", tr)
	}
	if got := statusOf(h.snapshot(), "local"); got != "stopped" {
		t.Fatalf("a stopped source must stay stopped through probes, got %q", got)
	}

	// dead wins over stopped (an error exit is a stronger signal than a clean one).
	h.markDead("tg", errors.New("boom"))
	h.markStopped("tg") // no-op once dead
	if got := statusOf(h.snapshot(), "tg"); got != "dead" {
		t.Fatalf("dead source must stay dead, got %q", got)
	}
}
