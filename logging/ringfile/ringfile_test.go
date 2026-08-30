package ringfile

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRingFile_BasicAppend covers the happy path: Open + two writes
// + Close, then read the file back and assert two newline-terminated
// records survived in order.
func TestRingFile_BasicAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "basic.log")
	r, err := Open(path, 1<<20, "test: basic")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if r == nil {
		t.Fatal("Open returned nil RingFile for non-empty path / positive cap")
	}
	r.WriteRecord([]byte("alpha"))
	r.WriteRecord([]byte("beta"))
	r.Close()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	want := "alpha\nbeta\n"
	if string(body) != want {
		t.Errorf("file content mismatch:\nwant %q\ngot  %q", want, string(body))
	}
}

// TestRingFile_TrimsAtCap drives enough writes to force a trim, then
// asserts the file size shrank below the cap and the most recent record
// is still present (the oldest half was the one dropped).
func TestRingFile_TrimsAtCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trim.log")
	// maxBytes=200 → record size budget is 100 (maxBytes/2 oversize
	// guard). Use 40-byte records; ~5 records fit before a trim fires.
	const maxBytes int64 = 200
	r, err := Open(path, maxBytes, "test: trim")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := bytes.Repeat([]byte("x"), 39) // +1 newline = 40
	// Write enough to guarantee at least one trim cycle.
	for i := 0; i < 20; i++ {
		// Encode an index so we can locate "newest" record.
		line := append([]byte{}, rec...)
		line[0] = byte('A' + (i % 26))
		r.WriteRecord(line)
	}
	// Final, identifiable record we expect to survive any trim.
	r.WriteRecord([]byte("FINAL_RECORD_MARKER"))
	r.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() > maxBytes {
		t.Errorf("after trim file size %d exceeds cap %d", fi.Size(), maxBytes)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(body), "FINAL_RECORD_MARKER") {
		t.Errorf("trim dropped the most recent record; file body:\n%s", body)
	}
}

// TestRingFile_OversizeRecordSkipped asserts a single record larger
// than maxBytes/2 is silently dropped (defensive guard so trim doesn't
// loop forever trying to make room).
func TestRingFile_OversizeRecordSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversize.log")
	r, err := Open(path, 100, "test: oversize")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.WriteRecord(bytes.Repeat([]byte("y"), 200))
	r.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("oversize record was not skipped; file size = %d", fi.Size())
	}
}

// TestRingFile_DisableOnInitOpenFailure passes a path inside a
// non-existent directory; OpenFile fails → RingFile lands in disabled
// state and subsequent WriteRecord is a silent no-op.
func TestRingFile_DisableOnInitOpenFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-subdir", "ring.log")
	r, err := Open(path, 1<<20, "test: init-fail")
	if err != nil {
		t.Fatalf("Open should not return error even on open failure, got %v", err)
	}
	if r == nil {
		t.Fatal("Open returned nil despite non-empty path / positive cap; contract is to return a usable disabled RingFile")
	}
	// First WriteRecord will try to lazy-reopen, also fail, and then
	// flip disabled=true.
	r.WriteRecord([]byte("should-be-dropped"))
	if !r.IsDisabled() {
		t.Errorf("expected ringfile to self-disable after open failure")
	}
	// Second WriteRecord is a no-op via the disabled short-circuit.
	r.WriteRecord([]byte("also-dropped"))
	// And the file genuinely never showed up.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file to not exist; stat err = %v", err)
	}
}

// TestRingFile_DisableOnWriteFailure asserts a write failure (here: the
// underlying fd is invalidated out from under the RingFile) flips
// disabled=true after a single WARN, matching the package's
// WARN-once-then-self-disable contract (same as the trim/open paths).
func TestRingFile_DisableOnWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writefail.log")
	r, err := Open(path, 1<<20, "test: write-fail")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// First write opens the cached handle.
	r.WriteRecord([]byte("alpha"))
	if r.IsDisabled() {
		t.Fatal("ringfile disabled after a healthy write")
	}
	// Invalidate the cached fd so the next Write fails, but keep r.f
	// non-nil so WriteRecord goes straight to the Write (no lazy reopen).
	r.mu.Lock()
	if r.f != nil {
		_ = r.f.Close()
	}
	r.mu.Unlock()

	r.WriteRecord([]byte("beta")) // Write on a closed fd → error
	if !r.IsDisabled() {
		t.Fatal("expected ringfile to self-disable after a write failure")
	}
	// Subsequent writes are silent no-ops via the disabled short-circuit.
	r.WriteRecord([]byte("gamma"))
}

// TestRingFile_ConcurrentWrites fires N goroutines each calling
// WriteRecord K times. Run with -race to catch any unsynchronised
// access. Asserts the final file has exactly N*K newline-terminated
// records.
func TestRingFile_ConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.log")
	// Large cap so no trim fires during the test → exact record count
	// is the only thing we need to verify.
	r, err := Open(path, 1<<24, "test: concurrent")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const (
		N = 16
		K = 100
	)
	var wg sync.WaitGroup
	wg.Add(N)
	for g := 0; g < N; g++ {
		go func(id int) {
			defer wg.Done()
			payload := []byte("g=" + string(rune('a'+id)))
			for i := 0; i < K; i++ {
				r.WriteRecord(payload)
			}
		}(g)
	}
	wg.Wait()
	r.Close()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := bytes.Count(body, []byte("\n"))
	if got != N*K {
		t.Errorf("concurrent writes: want %d newlines, got %d", N*K, got)
	}
}

// TestRingFile_CloseIsIdempotent makes sure double-Close doesn't panic
// and the second call is a no-op (logger stays disabled).
func TestRingFile_CloseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "close.log")
	r, err := Open(path, 1<<20, "test: close")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.WriteRecord([]byte("one"))
	r.Close()
	// Second Close — must not panic, must keep disabled flag.
	r.Close()
	if !r.IsDisabled() {
		t.Errorf("expected RingFile to remain disabled after second Close")
	}
	// And a WriteRecord after Close is silently dropped.
	r.WriteRecord([]byte("ghost"))
	body, _ := os.ReadFile(path)
	if !bytes.Equal(body, []byte("one\n")) {
		t.Errorf("post-close write leaked into file: %q", body)
	}
}

// TestRingFile_NilOnDisabledConfig asserts the explicit
// caller-disabled signal (empty path or maxBytes <= 0) returns
// (nil, nil) so callers can nil-guard.
func TestRingFile_NilOnDisabledConfig(t *testing.T) {
	r, err := Open("", 1<<20, "test: nilcfg")
	if err != nil || r != nil {
		t.Errorf("Open(\"\", ...) = (%v, %v); want (nil, nil)", r, err)
	}
	r, err = Open(filepath.Join(t.TempDir(), "x.log"), 0, "test: nilcfg")
	if err != nil || r != nil {
		t.Errorf("Open(.., 0, ..) = (%v, %v); want (nil, nil)", r, err)
	}
}
