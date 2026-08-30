// Tests for the pick tie-break among equal-(prefer,priority) peers: in-flight
// load balancing first (spread work across equal-priority self-deployed models),
// then the soft cache affinity (own remembered entry first, then away from
// entries other live conversations occupy — 31-affinity.go), then Name asc as
// the deterministic fallback. White-box (package model) so a candidate's live
// inFlight and the affinity ledger can be set directly.
package model

import (
	"context"
	"testing"
	"time"

	"agentbob/contract"
)

// pair builds two equal-priority, equal-tag ("smart") llm entries a & b with the
// given in-flight loads, wrapped in a test pool.
func pair(t *testing.T, aInFlight, bInFlight int) (*MultiPool, *entryRow, *entryRow) {
	t.Helper()
	a := entry("a", 0, &fakeChatter{text: "a"})
	b := entry("b", 0, &fakeChatter{text: "b"})
	a.inFlight = aInFlight
	b.inFlight = bInFlight
	return newTestPool(a, b), a, b
}

// TestPickBalancesByInFlight: among equal (prefer,priority) peers the least-loaded
// backend wins — the core load-balancing goal.
func TestPickBalancesByInFlight(t *testing.T) {
	p, a, b := pair(t, 2, 0)
	defer p.Close()
	if got, err := p.pick(smartReq, nil); err != nil || got != b {
		t.Fatalf("least-loaded b must win (a=2,b=0), got %v err %v", got, err)
	}
	// Flip the load: a is now the idle one.
	a.inFlight, b.inFlight = 0, 3
	if got, err := p.pick(smartReq, nil); err != nil || got != a {
		t.Fatalf("least-loaded a must win (a=0,b=3), got %v err %v", got, err)
	}
}

// TestPickAffinityBreaksInFlightTie: on an EXACT in-flight tie the caller's own
// remembered backend (recorded on its AffinityKey) wins the tie — cache locality.
func TestPickAffinityBreaksInFlightTie(t *testing.T) {
	p, _, b := pair(t, 0, 0)
	defer p.Close()
	p.recordAffinity("s1", "b")
	req := contract.ModelRequest{Requires: []string{"smart"}, AffinityKey: "s1"}
	if got, err := p.pick(req, nil); err != nil || got != b {
		t.Fatalf("affinity to b must break the 0-0 tie, got %v err %v", got, err)
	}
}

// TestPickLoadOutranksAffinity: affinity is soft — a busier remembered backend is
// passed over for a less-loaded peer. Load balancing always outranks stickiness.
func TestPickLoadOutranksAffinity(t *testing.T) {
	p, a, _ := pair(t, 0, 1) // a idle, b (the remembered backend) busier
	defer p.Close()
	p.recordAffinity("s1", "b")
	req := contract.ModelRequest{Requires: []string{"smart"}, AffinityKey: "s1"}
	if got, err := p.pick(req, nil); err != nil || got != a {
		t.Fatalf("less-loaded a must beat busier affinity target b, got %v err %v", got, err)
	}
}

// TestPickAvoidsForeignOccupancy: a conversation with NO affinity of its own is
// steered toward the entry FEWER other live conversations sit on — here "a" is
// occupied by s1, so the newcomer lands on the otherwise Name-asc-losing "b"
// (protecting s1's warm cache AND spreading conversations across idle peers).
func TestPickAvoidsForeignOccupancy(t *testing.T) {
	p, a, b := pair(t, 0, 0)
	defer p.Close()
	p.recordAffinity("s1", "a")
	req := contract.ModelRequest{Requires: []string{"smart"}, AffinityKey: "s2"}
	if got, err := p.pick(req, nil); err != nil || got != b {
		t.Fatalf("newcomer must avoid s1-occupied a, got %v err %v", got, err)
	}
	// A KEYLESS request (a conversation's side-LLM call: salvage/compress/judge)
	// gets NO steering at all — neutral Name-asc. Its prompt shares no prefix
	// with any ledgered conversation, and repelling it off the entry its host
	// session was sized for is exactly the salvage-400 hazard (review).
	if got, err := p.pick(smartReq, nil); err != nil || got != a {
		t.Fatalf("keyless request must rank neutrally (Name asc → a), got %v err %v", got, err)
	}
}

// TestPickLoadOutranksForeignOccupancy: the hold is soft — when the unoccupied
// peer is busier, the newcomer still lands on the occupied-but-idle entry.
// "About to be full" beats the soft hold.
func TestPickLoadOutranksForeignOccupancy(t *testing.T) {
	p, a, _ := pair(t, 0, 2) // a idle (occupied by s1), b busy
	defer p.Close()
	p.recordAffinity("s1", "a")
	req := contract.ModelRequest{Requires: []string{"smart"}, AffinityKey: "s2"}
	if got, err := p.pick(req, nil); err != nil || got != a {
		t.Fatalf("idle-but-occupied a must beat busy b, got %v err %v", got, err)
	}
}

// TestPickOwnAffinityOutranksForeignOccupancy: when my remembered entry is also
// occupied by someone else, MY affinity still wins — my cache is there too.
func TestPickOwnAffinityOutranksForeignOccupancy(t *testing.T) {
	p, a, _ := pair(t, 0, 0)
	defer p.Close()
	p.recordAffinity("s1", "a")
	p.recordAffinity("s2", "a") // both conversations live on a
	req := contract.ModelRequest{Requires: []string{"smart"}, AffinityKey: "s2"}
	if got, err := p.pick(req, nil); err != nil || got != a {
		t.Fatalf("own affinity to a must outrank s1's occupancy of a, got %v err %v", got, err)
	}
}

// TestAffinityTTLExpires: past the TTL a record loses both effects (stickiness
// and the foreign hold) and is pruned from the ledger.
func TestAffinityTTLExpires(t *testing.T) {
	p, a, _ := pair(t, 0, 0)
	defer p.Close()
	p.recordAffinity("s1", "b")
	p.affinityMu.Lock()
	p.affinity["s1"] = affinityRec{entry: "b", lastUsed: time.Now().Add(-affinityTTL - time.Minute)}
	p.affinityMu.Unlock()
	// Own stickiness gone → full tie falls to Name asc (a).
	req := contract.ModelRequest{Requires: []string{"smart"}, AffinityKey: "s1"}
	if got, err := p.pick(req, nil); err != nil || got != a {
		t.Fatalf("expired affinity must not stick, got %v err %v", got, err)
	}
	p.affinityMu.Lock()
	_, still := p.affinity["s1"]
	p.affinityMu.Unlock()
	if still {
		t.Fatal("expired record must be pruned from the ledger")
	}
}

// TestAffinityRecordedOnServe: the failover driver books (key → served entry) on a
// real successful Chat, so the NEXT turn of the same session sticks — the
// cross-turn half of the design (the ledger outlives any one turn's state).
func TestAffinityRecordedOnServe(t *testing.T) {
	p, _, b := pair(t, 0, 0)
	defer p.Close()
	// Steer the first serve onto b via a pre-existing foreign hold on a.
	p.recordAffinity("other", "a")
	req := contract.ModelRequest{Requires: []string{"smart"}, AffinityKey: "s1"}
	if _, err := p.Chat(context.Background(), req, []contract.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	own, _ := p.affinityFor("s1")
	if own != "b" {
		t.Fatalf("serve must record s1 → b, got %q", own)
	}
	// Drop the foreign hold: s1 must now stick to b on its own record alone.
	p.affinityMu.Lock()
	delete(p.affinity, "other")
	p.affinityMu.Unlock()
	if got, err := p.pick(req, nil); err != nil || got != b {
		t.Fatalf("next pick must stick to recorded b, got %v err %v", got, err)
	}
}

// TestPickNameFallbackDeterministic: no affinity + a full tie (same load) falls to
// Name asc, so the pick stays deterministic.
func TestPickNameFallbackDeterministic(t *testing.T) {
	p, a, _ := pair(t, 0, 0)
	defer p.Close()
	if got, err := p.pick(smartReq, nil); err != nil || got != a {
		t.Fatalf("full tie must fall to Name asc (a<b), got %v err %v", got, err)
	}
}
