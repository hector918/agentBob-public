package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSave_PathsAndSize(t *testing.T) {
	st := New(t.TempDir())
	path, rel, n, err := st.Save("by_scope/k/attachments", "pic.jpg", strings.NewReader("hello"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("size = %d, want 5", n)
	}
	if !strings.HasSuffix(path, rel) {
		t.Fatalf("abs path %q should end with the sandbox-relative rel %q", path, rel)
	}
	// rel keeps only the LAST subdir segment: attachments/<date>/<file> — the
	// agent's sandbox is rooted at the session-scope dir, not the store base.
	if !strings.HasPrefix(rel, "attachments/") || !strings.HasSuffix(rel, "pic.jpg") {
		t.Fatalf("rel %q malformed (want attachments/<date>/<file>)", rel)
	}
}

func TestSave_RejectsTooLarge(t *testing.T) {
	st := New(t.TempDir())
	if _, _, _, err := st.Save("sub", "big.bin", strings.NewReader("0123456789"), 4); err == nil {
		t.Fatal("Save should reject input over the byte limit")
	}
}

// TestEnforceTotalCap_TodayExemptLocalMidnight locks the today-exemption to LOCAL
// midnight (B32): with time.Local pinned to +08, an over-cap sweep must never prune
// today's staging dir (an in-flight attachment lives there until PlaceAttachments
// moves it) — only the older dir is reclaimed. The pre-fix Truncate(24h) rounded to
// UTC midnight and could delete today's dir during 08:00–24:00 local.
func TestEnforceTotalCap_TodayExemptLocalMidnight(t *testing.T) {
	orig := time.Local
	time.Local = time.FixedZone("plus8", 8*60*60)
	defer func() { time.Local = orig }()

	base := t.TempDir()
	st := New(base)
	now := time.Now().In(time.Local)
	today := now.Format("2006-01-02")
	old := now.AddDate(0, 0, -5).Format("2006-01-02")

	write := func(day string, size int) {
		dir := filepath.Join(base, day)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f.bin"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(today, 1000)
	write(old, 1000)

	// Cap far below the 2000-byte total: without the exemption both dirs are
	// eligible. Today's dir must survive; only the old dir is reclaimed.
	removed, remaining := st.EnforceTotalCap(500)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the old dir)", removed)
	}
	if remaining != 1000 {
		t.Fatalf("remaining = %d, want 1000 (today's exempt dir)", remaining)
	}
	if _, err := os.Stat(filepath.Join(base, today)); err != nil {
		t.Fatalf("today's staging dir was pruned (local-midnight exemption broken): %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, old)); !os.IsNotExist(err) {
		t.Fatalf("old dir should have been removed, stat err = %v", err)
	}
}
