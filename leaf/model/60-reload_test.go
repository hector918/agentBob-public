package model

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// The webui's "config" stat reads ReloadInfo.LastMtime to answer "did my
// models.yaml edit take effect". That has to be the config the pool is
// SERVING, not the mtime the watcher merely noticed: an edit that fails to
// load (bad base_url, unknown provider — buildEntry rejects plenty that
// Validate accepts) must keep the stat pointing at the config still in
// service, with the failure reaching the operator as an admin page.
//
// The watcher's own baseline still advances on failure, on purpose — that is
// what stops a broken file from being retried, and admin-paged, every check
// interval. The two accounts diverging IS the failure state.
func TestSnapshotInfoTracksServedConfigNotSightings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	if err := os.WriteFile(path, []byte("entries: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Explicit mtimes: a fast test can write twice inside one filesystem
	// timestamp tick, and this test is entirely about telling them apart.
	stamp := func(t *testing.T, at time.Time) time.Time {
		t.Helper()
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.ModTime()
	}
	t1 := stamp(t, time.Now().Add(-3*time.Hour))

	// Priority below minEnabledPriority → buildEntry skips the live probe, so
	// the rebuild never touches the network.
	var loadErr atomic.Pointer[error]
	reloadFn := func() ([]Entry, []FallbackRule, error) {
		if p := loadErr.Load(); p != nil {
			return nil, nil, *p
		}
		return []Entry{{
			Name: "a", Provider: "openai", Model: "m",
			BaseURL: "http://127.0.0.1:1", Priority: -20000,
		}}, nil, nil
	}

	var state atomic.Pointer[poolState]
	state.Store(&poolState{byName: map[string]*entryRow{}})
	paged := make(chan error, 4)
	r := NewConfigReloader(&state, reloadFn, path, ReloaderHooks{
		NotifyAsyncError: func(err error) { paged <- err },
	})

	// Nothing has loaded through the reloader yet — the stat must not claim
	// the file it merely stat'd at construction.
	if got := r.SnapshotInfo().LastMtime; !got.IsZero() {
		t.Fatalf("LastMtime before any load = %v, want zero", got)
	}

	if err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := r.SnapshotInfo().LastMtime; !got.Equal(t1) {
		t.Fatalf("after a successful load LastMtime = %v, want the served config's mtime %v", got, t1)
	}

	// A broken edit lands. MaybeReload sees it, dispatches, and the load fails.
	boom := errors.New("entries[0] (a): base_url not set and provider \"nope\" has no preset")
	loadErr.Store(&boom)
	t2 := stamp(t, time.Now().Add(-2*time.Hour))
	r.unthrottleForTest()
	r.MaybeReload()
	select {
	case <-paged:
	case <-time.After(2 * time.Second):
		t.Fatal("a failed hot-reload must page the admin")
	}
	if got := r.SnapshotInfo().LastMtime; !got.Equal(t1) {
		t.Errorf("after a FAILED load LastMtime = %v, want the still-served config's mtime %v", got, t1)
	}
	// The watcher, separately, has consumed that mtime: the broken file is not
	// re-read (and not re-paged) until it changes again.
	r.configMu.Lock()
	seen := r.lastMtime
	r.configMu.Unlock()
	if !seen.Equal(t2) {
		t.Errorf("watcher baseline = %v, want the failed edit's mtime %v (else it retries every check)", seen, t2)
	}

	// The operator fixes the file: a new mtime, a load that succeeds, and the
	// stat catches up.
	loadErr.Store(nil)
	swapped := make(chan struct{}, 1)
	r.hooks.PostSwap = func() { swapped <- struct{}{} }
	t3 := stamp(t, time.Now().Add(-1*time.Hour))
	r.unthrottleForTest()
	r.MaybeReload()
	select {
	case <-swapped:
	case <-time.After(2 * time.Second):
		t.Fatal("the fixed config never swapped in")
	}
	if got := r.SnapshotInfo().LastMtime; !got.Equal(t3) {
		t.Errorf("after the fix LastMtime = %v, want %v", got, t3)
	}
}

// unthrottleForTest clears the per-interval stat throttle so a test can drive
// consecutive MaybeReload calls without sleeping through configCheckInterval.
func (r *ConfigReloader) unthrottleForTest() {
	r.configMu.Lock()
	r.lastCheck = time.Time{}
	r.configMu.Unlock()
}
