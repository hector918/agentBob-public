package browser

import (
	"os"
	"path/filepath"
	"testing"

	"agentbob/sidecars/browser/config"
)

// TestLaunchFailurePreservesPersistentProfile is the load-bearing data-loss
// guard: when chromium fails to launch, a PERSISTENT (profile) data dir — the
// user's saved login — must be PRESERVED, while a legacy ephemeral dir is still
// cleaned up. The regression this pins: a transient launch failure (e.g. a stale
// SingletonLock from an unclean prior exit) used to wipe the whole profile dir,
// destroying the login. A bogus exec path forces chromedp.Run to fail fast.
func TestLaunchFailurePreservesPersistentProfile(t *testing.T) {
	p := NewPool(config.BrowserConfig{ExecPath: "/nonexistent/chromium-bin"}, config.FilesystemConfig{}, false)
	t.Cleanup(p.CloseAll)

	withSentinel := func() string {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "Cookies"), []byte("saved-login"), 0o600); err != nil {
			t.Fatal(err)
		}
		return d
	}

	// Persistent profile path: a launch failure must leave the dir + login intact.
	pdir := withSentinel()
	if _, err := p.acquireKeyed("browser_acct", pdir, true, "", false); err == nil {
		t.Fatal("expected a launch failure with a bogus exec path")
	}
	if _, err := os.Stat(filepath.Join(pdir, "Cookies")); err != nil {
		t.Fatalf("DATA LOSS: persistent profile wiped on launch failure: %v", err)
	}

	// Legacy ephemeral path: a launch failure removes the throwaway dir (unchanged).
	ldir := withSentinel()
	if _, err := p.acquireKeyed("scope_legacy", ldir, false, "", false); err == nil {
		t.Fatal("expected a launch failure with a bogus exec path")
	}
	if _, err := os.Stat(ldir); !os.IsNotExist(err) {
		t.Fatalf("legacy ephemeral dir should be removed on launch failure; stat err = %v", err)
	}
}

// TestClearStaleProfileLocks: the singleton lock files (incl. the SingletonLock
// symlink) are removed; a missing file is a no-op; unrelated profile data is kept.
func TestClearStaleProfileLocks(t *testing.T) {
	d := t.TempDir()
	// SingletonLock is normally a symlink; a dangling symlink must still be cleared.
	if err := os.Symlink("somehost-12345", filepath.Join(d, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"SingletonSocket", "SingletonCookie", "Cookies"} {
		if err := os.WriteFile(filepath.Join(d, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clearStaleProfileLocks(d)

	for _, n := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		if _, err := os.Lstat(filepath.Join(d, n)); !os.IsNotExist(err) {
			t.Errorf("%s should have been cleared; lstat err = %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(d, "Cookies")); err != nil {
		t.Errorf("non-lock profile data must be preserved: %v", err)
	}
	clearStaleProfileLocks(d) // idempotent — missing files are a no-op
}
