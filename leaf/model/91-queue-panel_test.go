package model

import (
	"testing"

	"agentbob/contract"
)

// A kind whose waiters all left must not linger as a zero row: the panel treats
// a non-empty Queues slice as "somebody is waiting" and colours it warn.
func TestQueueSnapshotSkipsEmptyKinds(t *testing.T) {
	p := &MultiPool{queueDepth: map[string]int{"llm": 0, "vl": 2}}
	got := p.queueSnapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 kind, got %d (%+v)", len(got), got)
	}
	if got[0].Kind != "vl" || got[0].Waiting != 2 {
		t.Fatalf("want vl waiting 2, got %+v", got[0])
	}
	// No entry table loaded → no cap to report, and 0 must read as "uncapped"
	// rather than "capacity zero" (which would render as vl 2/0).
	if got[0].Capacity != 0 {
		t.Fatalf("want uncapped, got %d", got[0].Capacity)
	}
}

func TestQueueSnapshotEmptyIsNil(t *testing.T) {
	p := &MultiPool{}
	if got := p.queueSnapshot(); got != nil {
		t.Fatalf("resting pool must report no queues, got %+v", got)
	}
}

func TestQueueSnapshotSortsKinds(t *testing.T) {
	p := &MultiPool{queueDepth: map[string]int{"vl": 1, "asr": 1, "llm": 3}}
	got := p.queueSnapshot()
	want := []string{"asr", "llm", "vl"}
	if len(got) != len(want) {
		t.Fatalf("want %d kinds, got %+v", len(want), got)
	}
	for i, kind := range want {
		if got[i].Kind != kind {
			t.Fatalf("position %d: want %q, got %q (%+v)", i, kind, got[i].Kind, got)
		}
	}
}

func TestWaitingTextNamesTheKinds(t *testing.T) {
	if got := waitingText(nil); got != "0" {
		t.Fatalf("empty queue: want %q, got %q", "0", got)
	}
	qs := []contract.QueueInfo{
		{Kind: "llm", Waiting: 3, Capacity: 8},
		{Kind: "vl", Waiting: 1}, // uncapped → no /N
	}
	if got, want := waitingText(qs), "4 (llm 3/8, vl 1)"; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestWaitingStatus(t *testing.T) {
	if got := waitingStatus(nil); got != "" {
		t.Fatalf("nobody waiting must not be coloured, got %q", got)
	}
	if got := waitingStatus([]contract.QueueInfo{{Kind: "llm", Waiting: 2, Capacity: 8}}); got != "warn" {
		t.Fatalf("want warn, got %q", got)
	}
	// A latched full queue is callers being REJECTED, not merely waiting.
	full := []contract.QueueInfo{{Kind: "llm", Waiting: 2}, {Kind: "vl", Waiting: 8, Capacity: 8, Full: true}}
	if got := waitingStatus(full); got != "down" {
		t.Fatalf("want down, got %q", got)
	}
}
