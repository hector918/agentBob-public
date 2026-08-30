package imagecreate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWALRoundTrip(t *testing.T) {
	w := newWAL(t.TempDir())
	w.claim(walRecord{PromptID: "job-1", Entry: "comfy-klein", Scope: "telegram:dm:7", Style: "comfyui-klein", Prompt: "a car"})

	got := w.list()
	if len(got) != 1 {
		t.Fatalf("list = %d records, want 1", len(got))
	}
	if got[0].Entry != "comfy-klein" || got[0].Scope != "telegram:dm:7" {
		t.Errorf("record = %+v, lost fields needed for recovery", got[0])
	}

	w.drop("job-1")
	if n := len(w.list()); n != 0 {
		t.Errorf("list = %d records after drop, want 0", n)
	}
}

// Delivery is the only thing that clears a record, so a record with no id (which
// could never be recovered) must not be written at all.
func TestWALIgnoresEmptyPromptID(t *testing.T) {
	dir := t.TempDir()
	w := newWAL(dir)
	w.claim(walRecord{Entry: "comfy-klein"})
	if n := len(w.list()); n != 0 {
		t.Errorf("wrote a record with no prompt id (%d found)", n)
	}
}

// A crash between write and rename leaves a .tmp behind. Left alone those pile up
// forever in a directory nothing else ever cleans.
func TestWALReapsTempFiles(t *testing.T) {
	dir := t.TempDir()
	w := newWAL(dir)
	w.claim(walRecord{PromptID: "job-1"})
	if err := os.WriteFile(filepath.Join(w.dir, "orphan.json.tmp"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := len(w.list()); n != 1 {
		t.Errorf("list = %d, want only the real record", n)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "orphan.json.tmp")); !os.IsNotExist(err) {
		t.Error("leftover .tmp survived a list")
	}
}

// A record that cannot be parsed can never be recovered — chasing it every two
// minutes forever is worse than forgetting it.
func TestWALDropsCorruptRecords(t *testing.T) {
	dir := t.TempDir()
	w := newWAL(dir)
	w.claim(walRecord{PromptID: "job-1"})
	bad := filepath.Join(w.dir, "corrupt.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := len(w.list()); n != 1 {
		t.Errorf("list = %d, want the corrupt file skipped", n)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Error("corrupt record survived a list")
	}
}

// A missing directory is the normal resting state (nothing in flight), not an
// error worth logging on every sweep.
func TestWALListsNothingWhenUnused(t *testing.T) {
	if n := len(newWAL(t.TempDir()).list()); n != 0 {
		t.Errorf("list = %d on a fresh home, want 0", n)
	}
	if newWAL("") != nil {
		t.Error("newWAL(\"\") should yield no wal — there is nowhere to write")
	}
	var nilWAL *wal
	nilWAL.claim(walRecord{PromptID: "x"}) // must not panic
	nilWAL.drop("x")
	if nilWAL.isOwned("x") {
		t.Error("a wal with nowhere to write claims ownership of a record it never stored")
	}
	if got := nilWAL.list(); got != nil {
		t.Errorf("nil wal list = %v, want nil", got)
	}
}

// The prompt id comes from the backend and lands in a filename. It is a uuid in
// practice, but it crosses a trust boundary, so traversal must be impossible
// rather than merely unlikely.
func TestSafeNameContainsPathEscapes(t *testing.T) {
	for _, in := range []string{"../../etc/passwd", "a/b", `..\..\win`, "x\x00y"} {
		got := safeName(in)
		if strings.ContainsAny(got, `/\`) || strings.Contains(got, "\x00") {
			t.Errorf("safeName(%q) = %q — still contains a separator", in, got)
		}
	}
	if got := safeName(strings.Repeat("a", 500)); len(got) > 128 {
		t.Errorf("safeName did not bound length: %d", len(got))
	}
	if got := safeName("job-1_A"); got != "job-1_A" {
		t.Errorf("safeName mangled a normal id: %q", got)
	}
}

// A record written by one process must be readable by the next one — that is the
// entire point, so the on-disk shape is worth pinning.
func TestWALRecordSurvivesProcessBoundary(t *testing.T) {
	dir := t.TempDir()
	newWAL(dir).claim(walRecord{
		PromptID: "job-9", Entry: "comfy-anima", Scope: "telegram:group:-100",
		Sid: "sid-1", Style: "comfyui-anima-hq", Prompt: "1girl", Created: 1700000000,
	})
	got := newWAL(dir).list() // a fresh wal, as after a restart
	if len(got) != 1 {
		t.Fatalf("list = %d records, want 1", len(got))
	}
	r := got[0]
	if r.PromptID != "job-9" || r.Entry != "comfy-anima" || r.Scope != "telegram:group:-100" ||
		r.Style != "comfyui-anima-hq" || r.Created != 1700000000 {
		t.Errorf("record = %+v, want every recovery field intact", r)
	}
}

func TestInconclusiveCoversEveryWayOfNotFindingOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"backend polled, still rendering", errTimeout{}, true},
		// The probe's own deadline, which can fire before the backend is ever asked:
		// an image entry runs one job at a time, so a probe issued while a generation
		// holds the slot expires in the pool's queue. Reading that as terminal is what
		// told a user "没画成" about a picture that was on its way.
		{"probe deadline, wrapped", fmt.Errorf("pool: %w", context.DeadlineExceeded), true},
		{"probe deadline, flattened to a string", errBareDeadline{}, true},
		{"turn cancelled", context.Canceled, true},
		{"backend verdict", errOther{}, false},
		{"nil", nil, false},
	} {
		if got := inconclusive(tc.err); got != tc.want {
			t.Errorf("%s: inconclusive(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

type errBareDeadline struct{}

func (errBareDeadline) Error() string { return "context deadline exceeded" }

type errTimeout struct{}

func (errTimeout) Error() string {
	return "comfyui: generation did not finish in time: context deadline exceeded"
}

type errOther struct{}

func (errOther) Error() string { return "comfyui: this size is too large for the engine" }
