package browser

import (
	"testing"

	"agentbob/sidecars/browser/config"
)

// TestLiveForScope_AgreesWithScreencastLookup pins that Pool.LiveForScope makes
// the EXACT same scope→live-instance judgement StartScreencast does: resolve via
// liveKeyForScope, then hit the in-memory sessions map. The /takeover guard
// (docs/remote-browser-handoff.md P1b) rests on this agreement — a guard that
// disagreed with the screencast path would either deny a takeover that would
// work or admit one onto a black screen. No chromium: we seed p.sessions
// directly (the same approach as screencast_test.go's Session-field tests).
func TestLiveForScope_AgreesWithScreencastLookup(t *testing.T) {
	p := NewPool(config.BrowserConfig{UserDataDirRoot: t.TempDir()},
		config.FilesystemConfig{SandboxRoot: t.TempDir()}, false)
	// Bare &Session{} stubs carry nil ctx-cancel funcs, so CloseAll would panic
	// tearing them down. Drop them ourselves at teardown instead — this test
	// never spawns real chromium.
	t.Cleanup(func() {
		p.mu.Lock()
		p.sessions = map[string]*Session{}
		p.mu.Unlock()
	})

	const scope = "tg:dm:99"

	// Cold: no instance → not live, and the screencast lookup misses the same key.
	if p.LiveForScope(scope) {
		t.Fatalf("LiveForScope on a cold scope = true, want false")
	}

	// Seed a live session under the legacy key (the scope itself, since no
	// custodian checkout is owned — liveKeyForScope falls back to the scope).
	key := p.liveKeyForScope(scope)
	if key != scope {
		t.Fatalf("liveKeyForScope(%q) = %q, want the scope itself (no checkout owned)", scope, key)
	}
	p.mu.Lock()
	p.sessions[key] = &Session{}
	p.mu.Unlock()

	if !p.LiveForScope(scope) {
		t.Fatalf("LiveForScope with a seeded session = false, want true")
	}

	// Removing the live instance flips it back to not-live (the idle-close case
	// the guard must catch: disk state may linger but no chromium is up).
	p.mu.Lock()
	delete(p.sessions, key)
	p.mu.Unlock()
	if p.LiveForScope(scope) {
		t.Fatalf("LiveForScope after instance removal = true, want false")
	}
}

// TestLiveForScope_ProfilePathKey pins that when the scope owns a custodian
// checkout, LiveForScope resolves to the PROFILE key (not the scope) — the same
// resolution StartScreencastForScope uses. A live instance keyed by the profile
// must count as live for the scope.
func TestLiveForScope_ProfilePathKey(t *testing.T) {
	p := NewPool(config.BrowserConfig{UserDataDirRoot: t.TempDir()},
		config.FilesystemConfig{SandboxRoot: t.TempDir()}, false)
	t.Cleanup(func() {
		p.mu.Lock()
		p.sessions = map[string]*Session{}
		p.mu.Unlock()
	})

	const scope = "tg:dm:1"
	const profile = "alice-profile"

	if !p.custodian.Checkout(profile, scope) {
		t.Fatalf("custodian.Checkout(%q,%q) failed", profile, scope)
	}

	// liveKeyForScope now resolves to the profile key, not the scope.
	if got := p.liveKeyForScope(scope); got != profile {
		t.Fatalf("liveKeyForScope(%q) = %q, want profile key %q", scope, got, profile)
	}

	// A live instance keyed by the SCOPE must NOT count — the profile path is
	// the resolved key, and LiveForScope must agree with screencast on that.
	p.mu.Lock()
	p.sessions[scope] = &Session{}
	p.mu.Unlock()
	if p.LiveForScope(scope) {
		t.Fatalf("LiveForScope = true with an instance under the SCOPE key, but the resolved key is the profile — want false")
	}

	// A live instance under the PROFILE key counts.
	p.mu.Lock()
	p.sessions[profile] = &Session{}
	p.mu.Unlock()
	if !p.LiveForScope(scope) {
		t.Fatalf("LiveForScope = false with an instance under the resolved profile key, want true")
	}
}
