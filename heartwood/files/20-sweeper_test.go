package files

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentbob/contract"
)

// writeDateDir creates <base>/<subdir>/<YYYY-MM-DD>/<file> for a given day with
// `size` bytes, returning the date-dir path.
func writeDateDir(t *testing.T, base, subdir string, day time.Time, file string, size int) string {
	t.Helper()
	dir := filepath.Join(base, subdir, day.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSweepOlderThan(t *testing.T) {
	base := t.TempDir()
	st := New(base)
	now := time.Now()

	oldDir := writeDateDir(t, base, "by_scope/k/attachments", now.AddDate(0, 0, -10), "old.bin", 1)
	freshDir := writeDateDir(t, base, "by_scope/k/attachments", now.AddDate(0, 0, -1), "fresh.bin", 1)
	// today's dir is also kept (cutoff is `days` ago; today is not "before" it)
	todayDir := writeDateDir(t, base, "by_scope/k/attachments", now, "today.bin", 1)

	removed := st.SweepOlderThan(7)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the 10-day-old dir)", removed)
	}
	if dirExists(oldDir) {
		t.Error("past-cutoff date dir should have been removed")
	}
	if !dirExists(freshDir) || !dirExists(todayDir) {
		t.Error("within-cutoff date dirs must be kept")
	}

	// days<=0 is a no-op.
	if n := st.SweepOlderThan(0); n != 0 {
		t.Fatalf("SweepOlderThan(0) = %d, want 0 (disabled)", n)
	}
}

func TestEnforceTotalCap(t *testing.T) {
	base := t.TempDir()
	st := New(base)
	now := time.Now()

	// Three date dirs, 100 bytes each = 300 total. Oldest first removed.
	writeDateDir(t, base, "by_scope/k/attachments", now.AddDate(0, 0, -3), "a.bin", 100)
	writeDateDir(t, base, "by_scope/k/attachments", now.AddDate(0, 0, -2), "b.bin", 100)
	writeDateDir(t, base, "by_scope/k/attachments", now.AddDate(0, 0, -1), "c.bin", 100)

	// Cap at 150: must remove the two oldest (a, b) → 100 remains.
	removed, final := st.EnforceTotalCap(150)
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if final != 100 {
		t.Fatalf("final size = %d, want 100", final)
	}

	// maxBytes<=0 is a no-op.
	if n, _ := st.EnforceTotalCap(0); n != 0 {
		t.Fatalf("EnforceTotalCap(0) removed %d, want 0 (disabled)", n)
	}
}

// write creates <path> with content and sets its mtime to `age` days ago.
func writeAged(t *testing.T, path string, ageDays int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm := time.Now().AddDate(0, 0, -ageDays)
	if err := os.Chtimes(path, tm, tm); err != nil {
		t.Fatal(err)
	}
}

// TestSweepSpaces_CompanyProtected_WorkingSwept exercises both policies in one run:
// a company_ shared space (root files protected, inbox/ pruned) and a per-scope working
// space (whole tree pruned, incl. model-created subdirs; fresh files kept).
func TestSweepSpaces_CompanyProtected_WorkingSwept(t *testing.T) {
	root := t.TempDir()

	// company_ shared store: the root deliverable must SURVIVE; its inbox/ ages out.
	coRootFile := filepath.Join(root, "company_co_1", "products", "report.md")
	coInboxOld := filepath.Join(root, "company_co_1", contract.InboxSubdir, "att.bin")
	writeAged(t, coRootFile, 90) // very old, but a shared deliverable → protected
	writeAged(t, coInboxOld, 10)

	// per-scope WORKING space: everything old goes, incl. screenshots/ and a model-made dir.
	ws := filepath.Join(root, "discord_group_1")
	wsRootOld := filepath.Join(ws, "口红.md")
	wsShotOld := filepath.Join(ws, "screenshots", "vision-1.png")
	wsDeepOld := filepath.Join(ws, "downloads", "sub", "old.bin") // dir the model invented
	wsFresh := filepath.Join(ws, "screenshots", "vision-2.png")
	writeAged(t, wsRootOld, 10)
	writeAged(t, wsShotOld, 10)
	writeAged(t, wsDeepOld, 10)
	writeAged(t, wsFresh, 1)

	removed := SweepSpaces(context.Background(), root, 7)
	if removed != 4 { // coInboxOld + wsRootOld + wsShotOld + wsDeepOld
		t.Fatalf("removed = %d, want 4", removed)
	}
	// company_ root protected; its inbox pruned.
	if !fileExists(coRootFile) {
		t.Error("company_ shared deliverable at the root must be protected")
	}
	if fileExists(coInboxOld) {
		t.Error("company_ inbox/ file should have been pruned")
	}
	// working space: old files everywhere gone, fresh kept.
	for _, p := range []string{wsRootOld, wsShotOld, wsDeepOld} {
		if fileExists(p) {
			t.Errorf("working-space stale file should have been pruned: %s", p)
		}
	}
	if !fileExists(wsFresh) {
		t.Error("working-space fresh file must be kept")
	}

	// retentionDays<=0 disables.
	if n := SweepSpaces(context.Background(), root, 0); n != 0 {
		t.Fatalf("SweepSpaces(_, 0) = %d, want 0 (disabled)", n)
	}
}

// TestSweepSpaces_RemovesAgedEmptyDir locks the emptied-dir reclaim branch: a working
// space's subdir that is already empty AND whose own mtime aged out is removed (counted);
// a fresh empty dir is kept (its young mtime protects it — no mid-write yank).
func TestSweepSpaces_RemovesAgedEmptyDir(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "discord_group_2")

	staleEmpty := filepath.Join(ws, "staleempty")
	freshEmpty := filepath.Join(ws, "freshempty")
	if err := os.MkdirAll(staleEmpty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(freshEmpty, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -10)
	if err := os.Chtimes(staleEmpty, old, old); err != nil {
		t.Fatal(err)
	}

	removed := SweepSpaces(context.Background(), root, 7)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (the aged empty dir)", removed)
	}
	if dirExists(staleEmpty) {
		t.Error("aged empty dir should have been reclaimed")
	}
	if !dirExists(freshEmpty) {
		t.Error("fresh empty dir must be kept (young mtime guards mid-write dirs)")
	}
}

// TestSweepSpaces_SkipsSymlinks: the recursive working-space walk must never follow a
// symlink (deleting its target) — the TOCTOU/escape guard.
func TestSweepSpaces_SkipsSymlinks(t *testing.T) {
	root := t.TempDir()

	// A file outside any space, targeted by a symlink placed inside a working space.
	outside := filepath.Join(root, "secret.txt")
	writeAged(t, outside, 30)

	ws := filepath.Join(root, "telegram_dm_9", "sub")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "link.bin")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	SweepSpaces(context.Background(), root, 7)
	if !fileExists(outside) {
		t.Error("a symlink target outside the space must never be followed/deleted")
	}
	if !fileExists(link) {
		t.Error("the symlink itself must be left alone (not removed as a regular file)")
	}
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
