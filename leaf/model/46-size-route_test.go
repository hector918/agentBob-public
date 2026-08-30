package model

import (
	"testing"
	"time"

	"agentbob/contract"
)

// sizedEntry builds a live LLM entry with a specific context window + priority.
func sizedEntry(name string, priority, ctxWindow int) *entryRow {
	return &entryRow{
		info: contract.ModelInfo{
			Name: name, Kind: contract.KindLLM, Model: name + "-m",
			Priority: priority, ContextWindow: ctxWindow, Tags: []string{"smart"},
		},
	}
}

// The picker is WINDOW-BLIND (hector): tags + priority + health
// decide the entry; context capacity is the turn's sizing concern AFTER the
// pick. A small-window entry with the higher priority wins the pick outright —
// an oversized prompt then 400s and the turn compacts to the winner's window.
func TestPick_WindowBlind(t *testing.T) {
	big := sizedEntry("big", 8, 131072)
	small := sizedEntry("small", 9, 32000)
	p := newTestPool(big, small)
	defer p.Close()

	row, err := p.pick(contract.ModelRequest{}, nil)
	if err != nil || row.info.Name != "small" {
		t.Fatalf("pick must be window-blind and follow priority: got %v err %v", row, err)
	}
}

// WindowFor returns the CONFIGURED winner's declared window — Prefer →
// priority → Name over enabled entries, blind to health and load.
func TestWindowFor_WinnersWindow(t *testing.T) {
	big := sizedEntry("big", 8, 131072)
	small := sizedEntry("small", 9, 32000)
	p := newTestPool(big, small)
	defer p.Close()

	if w := p.WindowFor(contract.ModelRequest{}); w != 32000 {
		t.Fatalf("WindowFor = %d, want the priority-9 winner's 32000", w)
	}
	// Prefer outranks priority — the sizing winner follows the request's tags.
	if w := p.WindowFor(contract.ModelRequest{Prefer: []string{"smart"}}); w != 32000 {
		// both carry "smart" (sizedEntry default) → tie → priority 9 wins
		t.Fatalf("WindowFor(Prefer smart) = %d, want 32000 (tie → priority)", w)
	}
	// Nothing enabled → 0 (caller skips compaction / uses its default).
	big.info.Priority, small.info.Priority = -10001, -10001
	if w := p.WindowFor(contract.ModelRequest{}); w != 0 {
		t.Fatalf("WindowFor with nothing enabled = %d, want 0", w)
	}
}

// Liveness accounting: ANY usable enabled entry of a kind keeps that kind
// alive — the window dimension is a per-turn sizing concern, not a liveness
// one.
func TestAllEntriesUnusable_AnyUsableCounts(t *testing.T) {
	big := sizedEntry("big", 10, 131072)
	small := sizedEntry("small", 2, 32000)
	big.deadUntil = time.Now().Add(time.Hour) // primary in cooldown
	p := newTestPool(big, small)
	defer p.Close()
	if p.allEntriesUnusable() {
		t.Fatal("a usable small-window entry keeps the kind alive")
	}

	// both down → all-dead.
	small.deadUntil = time.Now().Add(time.Hour)
	if !p.allEntriesUnusable() {
		t.Fatal("every entry in cooldown must read as all-dead")
	}

	// primary recovers → alive again.
	big.deadUntil = time.Time{}
	if p.allEntriesUnusable() {
		t.Fatal("a recovered entry means the pool is not all-dead")
	}
}

// TestWindowFor_StaticUnderHealthFlap (review round 2): the sizing winner is
// ranked on CONFIGURATION alone — a cooling blip on the primary must NOT flap
// the budget to a small sibling's window (that would proactively and
// irreversibly over-compact healthy history with no 400 ever fired).
func TestWindowFor_StaticUnderHealthFlap(t *testing.T) {
	big := sizedEntry("big", 8, 131072)
	small := sizedEntry("small", 2, 32000)
	p := newTestPool(big, small)
	defer p.Close()

	if w := p.WindowFor(contract.ModelRequest{}); w != 131072 {
		t.Fatalf("WindowFor = %d, want the configured winner's 131072", w)
	}
	// Primary cooling: the SERVING pick moves on, the SIZING answer must not.
	big.deadUntil = time.Now().Add(time.Hour)
	if w := p.WindowFor(contract.ModelRequest{}); w != 131072 {
		t.Fatalf("WindowFor under cooling = %d, want stable 131072 (health-blind sizing)", w)
	}
	// A disabled entry IS structural — it leaves the ranking.
	big.info.Priority = -10001
	if w := p.WindowFor(contract.ModelRequest{}); w != 32000 {
		t.Fatalf("WindowFor after disable = %d, want 32000", w)
	}
}
