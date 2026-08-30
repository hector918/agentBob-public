package skills

import (
	"path/filepath"
	"testing"
	"time"

	"agentbob/contract"
)

// TestSkillFailureStore: RecordFailure rings snapshots as files (failure-only, with
// .Snapshot set), readable via since(); the watermark sweep deletes consumed files.
func TestSkillFailureStore(t *testing.T) {
	s := &skillFailureStore{base: t.TempDir()}
	s.RecordFailure("ultrasound", "失败轨迹 A")
	s.RecordFailure("ultrasound", "失败轨迹 B")
	s.RecordFailure("", "ignored")      // blank skill → no-op
	s.RecordFailure("ultrasound", "  ") // blank snapshot → no-op

	got := s.since("ultrasound", time.Time{})
	if len(got) != 2 {
		t.Fatalf("want 2 snapshots, got %d", len(got))
	}
	for _, g := range got {
		if g.OK {
			t.Fatal("a failure snapshot must be OK=false")
		}
		if g.Snapshot == "" {
			t.Fatalf("snapshot trace malformed: %+v", g)
		}
	}

	// Watermark sweep: since(>= newest mtime) consumes + DELETES the files.
	maxAt := got[len(got)-1].At
	if g := s.since("ultrasound", maxAt); len(g) != 0 {
		t.Fatalf("since(watermark) should be empty (swept), got %d", len(g))
	}
	if g := s.since("ultrasound", time.Time{}); len(g) != 0 {
		t.Fatal("swept snapshot files should be gone")
	}
	// An unrecorded skill has no traces.
	if g := s.since("other", time.Time{}); len(g) != 0 {
		t.Fatal("unrecorded skill must have no traces")
	}
}

// TestSkillInsightStore exercises INSIGHT.md persistence (docs/learn.md §6): per-origin
// path, atomic put, empty-removes, all() keys by (origin,name) so a same-named pair
// stays DISTINCT (no name collapse — the catalog owns the external-overrides-internal
// precedence), delete clears only the named origin.
func TestSkillInsightStore(t *testing.T) {
	s := &skillInsightStore{base: filepath.Join(t.TempDir(), "skills")}

	if err := s.put(contract.OriginInternal, "ocr_report", "内置经验"); err != nil {
		t.Fatalf("put internal: %v", err)
	}
	if err := s.put(contract.OriginExternal, "ultrasound", "外置经验"); err != nil {
		t.Fatalf("put external: %v", err)
	}

	all := s.all()
	if all[addendaKey{contract.OriginInternal, "ocr_report"}] != "内置经验" ||
		all[addendaKey{contract.OriginExternal, "ultrasound"}] != "外置经验" {
		t.Fatalf("all = %v", all)
	}

	// A same-named internal + external pair stays DISTINCT (keyed by origin) — the
	// store no longer collapses them; precedence is the catalog's job at lookup time.
	if err := s.put(contract.OriginInternal, "dup", "内置版"); err != nil {
		t.Fatal(err)
	}
	if err := s.put(contract.OriginExternal, "dup", "外置版"); err != nil {
		t.Fatal(err)
	}
	all = s.all()
	if all[addendaKey{contract.OriginInternal, "dup"}] != "内置版" {
		t.Fatalf("internal dup = %q, want 内置版", all[addendaKey{contract.OriginInternal, "dup"}])
	}
	if all[addendaKey{contract.OriginExternal, "dup"}] != "外置版" {
		t.Fatalf("external dup = %q, want 外置版", all[addendaKey{contract.OriginExternal, "dup"}])
	}

	// Empty rewrite removes the file → drops out of all().
	if err := s.put(contract.OriginExternal, "ultrasound", "  "); err != nil {
		t.Fatal(err)
	}
	all = s.all()
	if _, ok := all[addendaKey{contract.OriginExternal, "ultrasound"}]; ok {
		t.Fatal("empty insight should remove the file")
	}

	// delete clears ONLY the named origin's file — the same-named other origin survives.
	if err := s.delete(contract.OriginExternal, "dup"); err != nil {
		t.Fatal(err)
	}
	all = s.all()
	if _, ok := all[addendaKey{contract.OriginExternal, "dup"}]; ok {
		t.Fatal("delete should remove the external insight")
	}
	if all[addendaKey{contract.OriginInternal, "dup"}] != "内置版" {
		t.Fatal("delete(external) must not touch the same-named internal insight")
	}
	if all[addendaKey{contract.OriginInternal, "ocr_report"}] != "内置经验" {
		t.Fatal("delete must not touch other skills")
	}
}
