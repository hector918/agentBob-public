package browser

import (
	"context"
	"testing"
	"time"
)

// TestSweepIdle pins the idle-browser sweep (#23 in the accumulation
// inventory): instances whose lastUsed is older than the TTL are closed +
// dropped from the pool; fresher ones survive. Fake sessions (real cancel
// funcs, no chromium) exercise the bookkeeping without launching a browser.
func TestSweepIdle(t *testing.T) {
	mk := func(lastUsed time.Time) *Session {
		_, c1 := context.WithCancel(context.Background())
		_, c2 := context.WithCancel(context.Background())
		return &Session{lastUsed: lastUsed, allocCancel: c1, browserCxl: c2}
	}
	p := &Pool{sessions: map[string]*Session{
		"stale":     mk(time.Now().Add(-2 * time.Hour)),
		"fresh":     mk(time.Now()),
		"just-over": mk(time.Now().Add(-61 * time.Minute)),
	}}

	n := p.SweepIdle(time.Hour)
	if n != 2 {
		t.Fatalf("SweepIdle closed %d, want 2 (stale + just-over)", n)
	}
	if _, ok := p.sessions["stale"]; ok {
		t.Error("stale instance must be closed + removed")
	}
	if _, ok := p.sessions["just-over"]; ok {
		t.Error("just-over-TTL instance must be closed + removed")
	}
	if _, ok := p.sessions["fresh"]; !ok {
		t.Error("fresh instance must survive the sweep")
	}
	// Idempotent: a second sweep with nothing stale returns 0.
	if n2 := p.SweepIdle(time.Hour); n2 != 0 {
		t.Fatalf("second SweepIdle = %d, want 0", n2)
	}
}

// TestSweepIdleSkipsPinnedProfile pins the takeover exemption (// review D1): a handoff-pinned profile is a human mid-takeover — its lastUsed
// goes stale (takeover input doesn't refresh it, and a control-hold stops the
// model from refreshing it), but the sweep must not close chromium under the
// human. Unpin → the next sweep reaps it normally.
func TestSweepIdleSkipsPinnedProfile(t *testing.T) {
	mk := func(lastUsed time.Time) *Session {
		_, c1 := context.WithCancel(context.Background())
		_, c2 := context.WithCancel(context.Background())
		return &Session{lastUsed: lastUsed, allocCancel: c1, browserCxl: c2}
	}
	p := &Pool{sessions: map[string]*Session{
		"held-profile": mk(time.Now().Add(-2 * time.Hour)),
		"stale":        mk(time.Now().Add(-2 * time.Hour)),
	}}
	p.custodian = NewProfileCustodian(p.CloseInstance, time.Hour, nil)
	defer p.custodian.Close()
	if !p.custodian.Checkout("held-profile", "scope-a") {
		t.Fatal("checkout failed")
	}
	if !p.custodian.Pin("held-profile", true) {
		t.Fatal("pin failed")
	}

	if n := p.SweepIdle(time.Hour); n != 1 {
		t.Fatalf("SweepIdle closed %d, want 1 (only the unpinned stale one)", n)
	}
	if _, ok := p.sessions["held-profile"]; !ok {
		t.Error("pinned profile must survive the sweep (human mid-takeover)")
	}
	if _, ok := p.sessions["stale"]; ok {
		t.Error("unpinned stale instance must still be reaped")
	}

	p.custodian.Pin("held-profile", false)
	if n := p.SweepIdle(time.Hour); n != 1 {
		t.Fatalf("post-unpin SweepIdle closed %d, want 1", n)
	}
}

// TestSweepIdleSkipsActiveTakeover pins the LEGACY-key exemption: a scope-keyed
// instance owns no custodian checkout, so Pin can't protect it — the pool-side
// takeoverActive count (live screencast streams) must exempt it from the sweep
// the same way a pin exempts a profile. When the stream ends (count removed),
// the next sweep reaps it normally.
func TestSweepIdleSkipsActiveTakeover(t *testing.T) {
	mk := func(lastUsed time.Time) *Session {
		_, c1 := context.WithCancel(context.Background())
		_, c2 := context.WithCancel(context.Background())
		return &Session{lastUsed: lastUsed, allocCancel: c1, browserCxl: c2}
	}
	p := &Pool{
		sessions: map[string]*Session{
			"telegram:dm:42": mk(time.Now().Add(-2 * time.Hour)), // human mid-takeover
			"stale":          mk(time.Now().Add(-2 * time.Hour)),
		},
		takeoverActive: map[string]int{"telegram:dm:42": 1},
	}

	if n := p.SweepIdle(time.Hour); n != 1 {
		t.Fatalf("SweepIdle closed %d, want 1 (only the non-takeover stale one)", n)
	}
	if _, ok := p.sessions["telegram:dm:42"]; !ok {
		t.Error("takeover-active legacy instance must survive the sweep")
	}

	delete(p.takeoverActive, "telegram:dm:42") // stream stopped
	if n := p.SweepIdle(time.Hour); n != 1 {
		t.Fatalf("post-takeover SweepIdle closed %d, want 1", n)
	}
}

// TestLruKeyLockedSkipsExempt pins the eviction-side mirror of the sweep
// exemption: the LRU pick must never land on a pinned-profile key or a
// takeover-active key — evicting one closes chromium under the human's hands —
// and returns "" (caller temporarily exceeds the cap) when everything is exempt.
func TestLruKeyLockedSkipsExempt(t *testing.T) {
	mk := func(lastUsed time.Time) *Session { return &Session{lastUsed: lastUsed} }
	p := &Pool{
		sessions: map[string]*Session{
			"browser_ac_hector": mk(time.Now().Add(-3 * time.Hour)), // oldest, but pinned
			"telegram:dm:42":    mk(time.Now().Add(-2 * time.Hour)), // next, but takeover-active
			"plain":             mk(time.Now().Add(-1 * time.Hour)),
		},
		takeoverActive: map[string]int{"telegram:dm:42": 1},
	}
	pinned := map[string]bool{"browser_ac_hector": true}

	p.mu.Lock()
	got := p.lruKeyLocked(pinned)
	p.mu.Unlock()
	if got != "plain" {
		t.Fatalf("lruKeyLocked = %q, want plain (pinned + takeover-active skipped)", got)
	}

	delete(p.sessions, "plain")
	p.mu.Lock()
	got = p.lruKeyLocked(pinned)
	p.mu.Unlock()
	if got != "" {
		t.Fatalf("lruKeyLocked with all exempt = %q, want \"\" (exceed cap, never evict a takeover)", got)
	}
}
