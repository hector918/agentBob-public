package tools

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestInsightStoreRoundtrip exercises the INSIGHT.md persistence (docs/learn.md §6):
// put writes + updates in place, empty text removes the file, all reloads.
func TestInsightStoreRoundtrip(t *testing.T) {
	s := &insightStore{dir: filepath.Join(t.TempDir(), "insights", "tools")}

	if err := s.put("file_storage", "经验一"); err != nil {
		t.Fatalf("put insert: %v", err)
	}
	if err := s.put("file_storage", "经验二"); err != nil { // same tool updates in place
		t.Fatalf("put update: %v", err)
	}
	if err := s.put("web_search", "搜索经验"); err != nil {
		t.Fatalf("put other: %v", err)
	}

	all := s.all()
	if all["file_storage"] != "经验二" {
		t.Fatalf("insight = %q, want 经验二 (update in place)", all["file_storage"])
	}
	if all["web_search"] != "搜索经验" {
		t.Fatalf("web_search insight = %q", all["web_search"])
	}

	// Empty text removes the file — an integral rewrite that yields nothing leaves no
	// empty INSIGHT.md behind, and the tool drops out of all().
	if err := s.put("file_storage", "   "); err != nil {
		t.Fatalf("put empty: %v", err)
	}
	all = s.all()
	if _, ok := all["file_storage"]; ok {
		t.Fatal("empty insight should remove the file")
	}
	if all["web_search"] != "搜索经验" {
		t.Fatal("removing one insight must not touch the others")
	}
}

// TestInsightStoreMissingDir: all() on a never-created dir returns empty (first run
// before any tool has learned anything).
func TestInsightStoreMissingDir(t *testing.T) {
	s := &insightStore{dir: filepath.Join(t.TempDir(), "never-made")}
	if all := s.all(); len(all) != 0 {
		t.Fatalf("missing dir should be empty, got %v", all)
	}
}

// TestInsightStoreAllFailOpen: one unreadable insight file is skipped, NOT fatal —
// a per-file blip must not blank every tool's notes (a later distill would use the
// empty base and overwrite the intact on-disk INSIGHT.md) (F65).
func TestInsightStoreAllFailOpen(t *testing.T) {
	s := &insightStore{dir: filepath.Join(t.TempDir(), "insights", "tools")}
	if err := s.put("web_search", "搜索经验"); err != nil {
		t.Fatalf("put: %v", err)
	}
	// A dangling symlink is listed by ReadDir but fails ReadFile — a portable stand-in
	// for an EACCES/EIO blip on one file.
	if err := os.Symlink(filepath.Join(s.dir, "no-such-target"), filepath.Join(s.dir, "broken.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	all := s.all()
	if all["web_search"] != "搜索经验" {
		t.Fatalf("readable insight lost to a sibling's read failure: %v", all)
	}
	if _, ok := all["broken"]; ok {
		t.Fatal("unreadable insight should be skipped, not loaded")
	}
}

// TestFailureStoreTmpGrace: since() must not reap a FRESH .tmp (a concurrent
// record()'s WriteFile→Rename window) but still reaps a stale crash orphan (F64).
func TestFailureStoreTmpGrace(t *testing.T) {
	s := &toolFailureStore{base: t.TempDir()}
	s.record("web_search", toolFailure{Shape: "长度3", Error: "boom"})
	dir := s.dir("web_search")

	fresh := filepath.Join(dir, "9000000000000000000-ffff.json.tmp")
	if err := os.WriteFile(fresh, []byte("inflight"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "1000000000000000000-aaaa.json.tmp")
	if err := os.WriteFile(stale, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * tmpOrphanGrace)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}

	got := s.since("web_search", time.Time{})
	if len(got) != 1 || got[0].Error != "boom" {
		t.Fatalf("want the 1 committed record, got %+v", got)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a fresh .tmp (in-flight write) must survive since()")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale .tmp (crash orphan) should be reaped by since()")
	}
}

// TestSweepOrphans: the boot sweep drops INSIGHT.md files and .failures/ dirs of
// tools no longer in the catalog, and leaves live tools' state alone (F69).
func TestSweepOrphans(t *testing.T) {
	base := t.TempDir()
	store := &insightStore{dir: filepath.Join(base, "insights", "tools")}
	fail := &toolFailureStore{base: filepath.Join(base, "insights", "tools", ".failures")}

	if err := store.put("web_search", "live note"); err != nil {
		t.Fatal(err)
	}
	if err := store.put("force_ocr", "orphan note"); err != nil {
		t.Fatal(err)
	}
	fail.record("web_search", toolFailure{Error: "keep"})
	fail.record("force_ocr", toolFailure{Error: "orphan"})

	live := map[string]bool{safeToolName("web_search"): true}
	if n := store.sweepOrphans(live); n != 1 {
		t.Errorf("insight sweep removed %d files, want 1", n)
	}
	if n := fail.sweepOrphans(live); n != 1 {
		t.Errorf("failure sweep removed %d dirs, want 1", n)
	}

	all := store.all()
	if all["web_search"] != "live note" {
		t.Error("live tool's insight must survive the sweep")
	}
	if _, ok := all["force_ocr"]; ok {
		t.Error("removed tool's insight should be swept")
	}
	if got := fail.since("web_search", time.Time{}); len(got) != 1 {
		t.Errorf("live tool's failure records must survive, got %d", len(got))
	}
	if _, err := os.Stat(fail.dir("force_ocr")); !os.IsNotExist(err) {
		t.Error("removed tool's .failures dir should be swept")
	}
}
