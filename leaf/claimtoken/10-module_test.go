package claimtoken

import (
	"testing"
	"time"
)

// Mint → Verify (non-consuming, repeatable) → Consume → gone.
func TestMintVerifyConsume(t *testing.T) {
	m := New()
	tok := m.Mint("gate-admit", "payload-x", time.Minute)

	// Verify does NOT consume — a transient retry can Verify again.
	for i := 0; i < 2; i++ {
		kind, pl, ok := m.Verify(tok)
		if !ok || kind != "gate-admit" || pl.(string) != "payload-x" {
			t.Fatalf("Verify #%d = %q,%v,%v; want gate-admit,payload-x,true", i, kind, pl, ok)
		}
	}
	m.Consume(tok)
	if _, _, ok := m.Verify(tok); ok {
		t.Fatal("Verify after Consume must be false")
	}
	m.Consume(tok) // idempotent
}

// An expired token is not returned and is reaped on Verify.
func TestExpiry(t *testing.T) {
	m := New()
	tok := m.Mint("k", 1, -time.Second) // already expired
	if _, _, ok := m.Verify(tok); ok {
		t.Fatal("expired token must not Verify")
	}
	m.mu.Lock()
	_, present := m.toks[tok]
	m.mu.Unlock()
	if present {
		t.Fatal("expired token must be reaped on Verify")
	}
}

// sweep drops expired entries, keeps live ones.
func TestSweep(t *testing.T) {
	m := New()
	live := m.Mint("k", "a", time.Minute)
	dead := m.Mint("k", "b", -time.Second)
	m.sweep()
	if _, _, ok := m.Verify(live); !ok {
		t.Fatal("sweep dropped a live token")
	}
	m.mu.Lock()
	_, deadPresent := m.toks[dead]
	m.mu.Unlock()
	if deadPresent {
		t.Fatal("sweep kept an expired token")
	}
}

// Empty token is always a miss; missing token too.
func TestMissAndEmpty(t *testing.T) {
	m := New()
	if _, _, ok := m.Verify(""); ok {
		t.Fatal("empty token must miss")
	}
	if _, _, ok := m.Verify("nope"); ok {
		t.Fatal("unknown token must miss")
	}
	m.Consume("") // no panic
}

// Distinct mints get distinct tokens (random).
func TestDistinctTokens(t *testing.T) {
	m := New()
	a := m.Mint("k", 1, time.Minute)
	b := m.Mint("k", 1, time.Minute)
	if a == b || a == "" {
		t.Fatalf("tokens must be distinct + non-empty, got %q %q", a, b)
	}
}
