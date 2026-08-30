package logging

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRotatingWriter_RotateFailure_NoSelfDeadlock pins two invariants on the
// base→.1 rename-failure path: (1) the deadlock fix — rotate() must NOT log via
// slog while Write holds rw.mu (that would route back into rw.Write →
// rw.mu.Lock and self-deadlock, sync.Mutex being non-reentrant); and (2)
// decision ⑧ — the rename failure must KEEP the triggering line (write it to
// the still-open fd) rather than bubble an error that makes Write drop it.
func TestRotatingWriter_RotateFailure_NoSelfDeadlock(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "agent.log")
	bf, err := os.OpenFile(base, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bf.WriteString("seed"); err != nil {
		t.Fatal(err)
	}

	rw := &rotatingWriter{
		path:     base,
		maxBytes: 4, // tiny — next write trips rotate
		keep:     3,
		f:        bf,
		n:        4,
	}

	// Install rw as the process-wide slog sink: this is the configuration that
	// makes the deadlock possible. Restore afterwards.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(rw, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	// Force os.Rename(base, base+".1") to fail by removing the base file out
	// from under rotate (rename of a missing source errors). The .N chain
	// shifting tolerates missing files, so only the critical rename fails.
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}

	type res struct {
		n   int
		err error
	}
	done := make(chan res, 1)
	go func() {
		n, werr := rw.Write([]byte("trip-rotate\n"))
		done <- res{n, werr}
	}()

	select {
	case r := <-done:
		// Decision ⑧: rename failure keeps the line — Write must NOT error and
		// must report the bytes written (to the still-open, now-oversize fd).
		if r.err != nil {
			t.Fatalf("rename failure must keep the line, got Write error: %v", r.err)
		}
		if r.n != len("trip-rotate\n") {
			t.Fatalf("Write reported n=%d, want full line kept (%d)", r.n, len("trip-rotate\n"))
		}
		// And a backoff must be armed so the next oversize Write doesn't
		// re-attempt (and re-fail) the rename immediately.
		rw.mu.Lock()
		armed := !rw.rotateBackoffUntil.IsZero()
		rw.mu.Unlock()
		if !armed {
			t.Fatalf("rename failure must arm the rotate backoff")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Write deadlocked: rotate logged via slog back into rw.Write while holding rw.mu")
	}
}

// TestRotatingWriter_ReopenFailure_TriggeringLineReachesStderr pins the fix for
// the dropped-line bug: when rotate() renames the base file successfully but the
// subsequent reopen fails, it falls back to os.Stderr. The line that TRIGGERED
// this rotate must still be written to that stderr fallback — not dropped.
// Before the fix, rotate() returned the reopen error and Write bailed (then
// `return 0, rotErr`, since removed) before ever writing p, so exactly the
// rotate-triggering line vanished (only later lines reached stderr).
func TestRotatingWriter_ReopenFailure_TriggeringLineReachesStderr(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "agent.log")
	// Open a real base file as the current fd so rotate can Close + rename it.
	bf, err := os.OpenFile(base, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bf.WriteString("seed"); err != nil {
		t.Fatal(err)
	}

	// Capture os.Stderr via a pipe.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	// Force the reopen after rename to fail.
	origOpen := openFile
	openFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, errors.New("injected reopen failure")
	}
	defer func() { openFile = origOpen }()

	rw := &rotatingWriter{
		path:     base,
		maxBytes: 4, // tiny — the next write trips rotate
		keep:     3,
		f:        bf, // current fd is the real base file (rotate closes + renames it)
		n:        4,  // already at cap so the next Write triggers rotate
	}

	triggerLine := []byte("the-triggering-line\n")
	n, werr := rw.Write(triggerLine)
	if werr != nil {
		t.Fatalf("Write returned err=%v; the triggering line must still be written to stderr fallback", werr)
	}
	if n != len(triggerLine) {
		t.Fatalf("short write: n=%d want %d", n, len(triggerLine))
	}
	// rw.f must have fallen back to os.Stderr (the pipe we installed).
	if rw.f != os.Stderr {
		t.Fatalf("expected fallback to os.Stderr after reopen failure")
	}

	// Read what landed on the stderr pipe and confirm the triggering line is there.
	_ = w.Close()
	out, _ := io.ReadAll(r)
	os.Stderr = origStderr
	if got := string(out); !strings.Contains(got, "the-triggering-line") {
		t.Fatalf("triggering line was dropped — stderr fallback got %q", got)
	}
}

// TestRotatingWriter_PostFallbackToStderr_KeepsWriting pins BUG 1.1:
// once the writer has fallen back to os.Stderr (rw.f == os.Stderr,
// maxBytes == 0, n > 0 after the first stderr write), every
// subsequent Write must succeed end-to-end. Before the fix, Write's
// size guard re-entered rotate(), which then failed at os.Rename
// (base was already renamed) and returned (0, err) — slog's default
// handler silently drops per-Write errors, so the operator lost every
// line at exactly the wrong moment.
func TestRotatingWriter_PostFallbackToStderr_KeepsWriting(t *testing.T) {
	rw := &rotatingWriter{
		path:     "/tmp/bug11-fixture-does-not-exist",
		maxBytes: 0,
		keep:     3,
		f:        os.Stderr,
		n:        100, // simulate "already wrote at least once to stderr"
	}

	lines := [][]byte{
		[]byte("line one\n"),
		[]byte("line two\n"),
		[]byte("line three\n"),
	}
	for i, p := range lines {
		n, err := rw.Write(p)
		if err != nil {
			t.Fatalf("Write #%d returned err=%v (BUG 1.1 regression: stderr fallback re-entered rotate)", i+1, err)
		}
		if n != len(p) {
			t.Fatalf("Write #%d short-write: got n=%d want %d", i+1, n, len(p))
		}
	}
}

// TestRotatingWriter_StderrFallback_RotateIsNoop pins the new
// invariant directly: rotate() must return without touching the
// filesystem when rw.f == os.Stderr. The fixture path is intentionally
// nonexistent — if rotate() actually tried os.Rename it would create /
// mutate state (and the checks below would catch the regression).
func TestRotatingWriter_StderrFallback_RotateIsNoop(t *testing.T) {
	rw := &rotatingWriter{
		path:     "/tmp/bug11-fixture-also-does-not-exist",
		maxBytes: 0,
		keep:     3,
		f:        os.Stderr,
		n:        42,
	}

	rw.rotate()

	// rotate() must not have re-opened or replaced rw.f.
	if rw.f != os.Stderr {
		t.Fatalf("rotate() mutated rw.f off os.Stderr — sentinel violated")
	}

	// Confirm rotate() didn't accidentally create the fixture file.
	if _, err := os.Stat(rw.path); !os.IsNotExist(err) {
		// Clean up regardless so we don't leave artifacts behind.
		_ = os.Remove(rw.path)
		t.Fatalf("rotate() touched the filesystem (created %q); want pure no-op", rw.path)
	}
}

// TestRotatingWriter_RenameFailBackoffGate pins decision ⑧'s backoff gate:
// after a rename failure, Write keeps appending to the current file and does
// NOT re-attempt rotation until rotateRetryBackoff elapses; once it does, a
// now-possible rename resumes rotation. Uses an injected clock.
func TestRotatingWriter_RenameFailBackoffGate(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "agent.log")
	bf, err := os.OpenFile(base, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bf.WriteString("seed"); err != nil {
		t.Fatal(err)
	}
	clk := time.Unix(1000, 0)
	rw := &rotatingWriter{path: base, maxBytes: 4, keep: 3, f: bf, n: 4, now: func() time.Time { return clk }}

	// Remove base so base→.1 rename fails on the first oversize write.
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Write([]byte("aaaa\n")); err != nil {
		t.Fatalf("first write must keep the line, got: %v", err)
	}
	if rw.rotateBackoffUntil.IsZero() {
		t.Fatal("backoff must be armed after rename failure")
	}

	// Recreate base so a rename WOULD now succeed. A second write while still
	// inside the backoff window must NOT rotate (no base.1 appears).
	if err := os.WriteFile(base, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Write([]byte("bbbb\n")); err != nil {
		t.Fatalf("second write (in backoff) must succeed, got: %v", err)
	}
	if _, err := os.Stat(base + ".1"); err == nil {
		t.Fatal("rotation must be skipped during the backoff window")
	}

	// Advance past the backoff — rotation resumes and the now-present base rolls.
	clk = clk.Add(rotateRetryBackoff + time.Second)
	if _, err := rw.Write([]byte("cccc\n")); err != nil {
		t.Fatalf("third write (post-backoff): %v", err)
	}
	if _, err := os.Stat(base + ".1"); err != nil {
		t.Fatalf("rotation must resume after the backoff elapses: %v", err)
	}
}

// TestSetup_TimestampsAreUTC pins the log timescale against the container's TZ.
// bob's other two log files (llm-requests.log, the warrant decision log) are
// hard-UTC; once the deploy sets TZ=America/New_York, agent.log following
// time.Local would put a 4/5-hour step between files an operator correlates
// line-by-line when something breaks. The "Z" is the assertion: RFC3339 spells
// the offset, so a local-time record would end in "-04:00" instead.
func TestSetup_TimestampsAreUTC(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata in this environment: %v", err)
	}
	saved := time.Local
	time.Local = ny
	defer func() { time.Local = saved }()
	// Setup installs a process-wide logger pointed at a temp dir this test is
	// about to delete — put the previous one back for the rest of the package.
	prev := slog.Default()
	defer slog.SetDefault(prev)

	dir := t.TempDir()
	l, err := Setup("file", dir, "info")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello")

	b, err := os.ReadFile(filepath.Join(dir, "agent.log"))
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	stamp, _, ok := strings.Cut(strings.TrimPrefix(line, "time="), " ")
	if !ok {
		t.Fatalf("unexpected log line shape: %q", line)
	}
	if !strings.HasSuffix(stamp, "Z") {
		t.Errorf("log stamp = %q, want a UTC instant ending in Z (the process is in %s)", stamp, ny)
	}
	if ts, perr := time.Parse(time.RFC3339Nano, stamp); perr != nil {
		t.Errorf("log stamp %q is not RFC3339: %v", stamp, perr)
	} else if _, off := ts.Zone(); off != 0 {
		t.Errorf("log stamp %q carries offset %ds, want 0", stamp, off)
	}
}
