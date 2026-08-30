package browser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/tools/file"
)

// TestSweepDataDirs pins the browserd-side legacy data-dir reaper (#26 in the
// accumulation inventory). A scope dir older than the TTL with no live session
// is removed; a fresh one and one belonging to a live session are spared.
func TestSweepDataDirs(t *testing.T) {
	root := t.TempDir()
	p := NewPool(config.BrowserConfig{UserDataDirRoot: root}, config.FilesystemConfig{}, false)

	byScope := filepath.Join(root, "by_sessions_scope")
	mkDir := func(scope string, age time.Duration) string {
		dir := filepath.Join(byScope, file.SanitizeKey(scope))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(dir, mt, mt); err != nil {
			t.Fatalf("chtimes %s: %v", dir, err)
		}
		return dir
	}

	staleDir := mkDir("tg:group:stale", 48*time.Hour)
	freshDir := mkDir("tg:group:fresh", time.Minute)
	liveScope := "tg:group:live"
	liveDir := mkDir(liveScope, 48*time.Hour) // old but has a live session

	// A live session keyed by the live scope must spare its dir even when old.
	p.sessions[liveScope] = &Session{}

	n := p.SweepDataDirs(24 * time.Hour)
	if n != 1 {
		t.Fatalf("SweepDataDirs removed %d, want 1 (stale only)", n)
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Error("stale data dir must be removed")
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Error("fresh data dir must survive")
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Error("live-session data dir must survive even when old")
	}
}
