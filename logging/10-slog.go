// Package logging configures the process-wide slog logger, with a size-based
// rotating file writer so logs stay bounded on long-running gateways.
// (Per-scope filtering — §19.5 — is still a TODO.)
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Setup configures and installs the default slog logger.
//
//	target: "stdout" | "file" | "both"
//	dir:    log directory (used for "file"/"both") — agent.log lives here
//	level:  "debug" | "info" | "warn" | "error"
//
// Rotation size/retention are owned by newRotatingWriter's built-in defaults
// (10 MiB × 3); they were never config-driven (the sole caller passed 0,0), so the
// params are gone.
func Setup(target, dir, level string) (*slog.Logger, error) {
	// Unknown enum values (config typos: "wran", "fle", …) fall back to the
	// defaults below, but loudly — the warning is deferred until the new
	// logger is installed so it lands on the configured sink instead of
	// vanishing with the pre-Setup default.
	var unknownLevel, unknownTarget bool

	lvl := slog.LevelInfo
	switch level {
	case "", "info":
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		unknownLevel = true
	}

	// R5C-4: "stdout" target now actually writes to
	// os.Stdout (matches the docstring). Default stays os.Stderr (the
	// conventional log destination — won't intermix with chatter that
	// might one day go to stdout).
	var w io.Writer = os.Stderr
	switch target {
	case "stdout":
		w = os.Stdout
	case "file", "both":
		rw, err := newRotatingWriter(filepath.Join(dir, "agent.log"), 0, 0) // 0,0 → newRotatingWriter's built-in defaults (10 MiB × 3)
		if err != nil {
			return nil, err
		}
		if target == "both" {
			w = io.MultiWriter(os.Stderr, rw)
		} else {
			w = rw
		}
	default:
		if target != "" {
			unknownTarget = true
		}
	}

	l := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl, ReplaceAttr: utcTime}))
	slog.SetDefault(l)
	if unknownLevel {
		l.Warn("unknown logging level — falling back to info", "level", level)
	}
	if unknownTarget {
		l.Warn("unknown logging target — falling back to stderr", "target", target)
	}
	return l, nil
}

// utcTime pins every record's timestamp to UTC, whatever the container's TZ is.
//
// bob writes three log files and slog was the only one following time.Local:
// llm-requests.log (leaf/model/providers) and the warrant decision log are both
// hard-UTC already. Once the deploy sets TZ, correlating one turn across the
// three — the first move when something breaks in production — would otherwise
// mean mentally adding the local offset to exactly one of them. RFC3339 ends
// each line's stamp in "Z", so the scale is stated on every line rather than
// being a thing you have to remember.
//
// Panels are the deliberate opposite (clock.Stamp renders local): logs get
// correlated with each other, panels get read by a person standing in a
// timezone.
func utcTime(groups []string, a slog.Attr) slog.Attr {
	// Top level only — a caller's own attr that happens to be keyed "time"
	// inside a group is their data, not the record stamp.
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.TimeValue(a.Value.Time().UTC())
	}
	return a
}

// rotateRetryBackoff is how long Write keeps appending to the current
// (oversize) file after a rename failure before re-attempting the rotation.
// Bounds the failed-rename stderr noise and the per-line retry cost while a
// transient condition (directory permissions, a held lock) clears.
const rotateRetryBackoff = 30 * time.Second

// rotatingWriter appends to `path`, rotating to `path.1`, `path.2`, … when the
// current file exceeds maxBytes, keeping at most `keep` rotated files.
type rotatingWriter struct {
	path     string
	maxBytes int64
	keep     int

	mu sync.Mutex
	f  *os.File
	n  int64 // bytes in the current file
	// rotateBackoffUntil suppresses rotation retries until this time after a
	// rename failure (decision ⑧). Zero = no backoff active. Guarded by mu.
	rotateBackoffUntil time.Time
	now                func() time.Time // injectable for tests; defaults to time.Now
}

func newRotatingWriter(path string, maxBytes int64, keep int) (*rotatingWriter, error) {
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	if keep <= 0 {
		keep = 3
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	rw := &rotatingWriter{path: path, maxBytes: maxBytes, keep: keep, now: time.Now}
	if err := rw.open(); err != nil {
		return nil, err
	}
	return rw, nil
}

// openFile is the indirection point for opening the log file. It defaults to
// os.OpenFile and exists so tests can inject a reopen failure (the
// rename-succeeds-but-reopen-fails branch of rotate() is otherwise only
// reachable on a read-only directory / full disk).
var openFile = os.OpenFile

// clock returns rw.now() or time.Now when now is nil (test literals that build
// the struct directly don't set it; newRotatingWriter always does).
func (rw *rotatingWriter) clock() time.Time {
	if rw.now != nil {
		return rw.now()
	}
	return time.Now()
}

func (rw *rotatingWriter) open() error {
	f, err := openFile(rw.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	rw.f = f
	if fi, e := f.Stat(); e == nil {
		rw.n = fi.Size()
	} else {
		rw.n = 0
	}
	return nil
}

func (rw *rotatingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	// Rotate when oversize — but not while a rename-failure backoff is active
	// (decision ⑧): during the backoff window keep appending to the current
	// file rather than re-attempting (and re-failing) the rename on every
	// line. The file may briefly exceed maxBytes; that beats going silent.
	if rw.n+int64(len(p)) > rw.maxBytes && rw.n > 0 && !rw.clock().Before(rw.rotateBackoffUntil) {
		rw.rotate()
	}
	n, err := rw.f.Write(p)
	rw.n += int64(n)
	return n, err
}

// rotate never fails from Write's perspective — every failure path recovers
// in place (rename-failure backoff, stderr fallback) so Write always proceeds
// to rw.f.Write(p) and never drops a line (decision ⑧). Hence no error return:
// the never-drop-a-line invariant is structural.
func (rw *rotatingWriter) rotate() {
	// BUG 1.1 fix: post-fallback to stderr, rotate is a
	// no-op. There is no on-disk file to rename, and the size guard in
	// Write would otherwise re-enter rotate every call (rw.n > 0 after
	// the first stderr write, plus rw.maxBytes == 0). Returning early
	// here lets Write proceed to rw.f.Write(p) so the line lands on
	// stderr.
	if rw.f == os.Stderr {
		return
	}

	// R5C-3: three sub-fixes:
	//   (a) recover from errors in place so Write never drops the line
	//       (originally "bubble errors to Write"; the error return was
	//       removed once every path became recover-in-place).
	//   (b) on open() failure after fallback to os.Stderr, rw.f is set
	//       to os.Stderr — that's the sentinel the short-circuit at
	//       the top of rotate() checks. maxBytes=0 + n=0 are
	//       belt-and-suspenders to keep size accounting sane.
	//   (c) on os.Rename failure for the base file, log via slog.Error
	//       and KEEP using the existing rw.f if it's still open —
	//       don't close it before rename succeeds.

	// Shift the .N → .N+1 chain first; these are tolerable to fail
	// individually (the file just stays at its current rotation
	// index; next rotate will retry).
	_ = os.Remove(fmt.Sprintf("%s.%d", rw.path, rw.keep))
	for i := rw.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", rw.path, i), fmt.Sprintf("%s.%d", rw.path, i+1))
	}

	// Critical step: rename base → .1. If THIS fails (disk full,
	// permission, target already exists & locked on win), keep the
	// existing fd alive — the file just stays oversized until next
	// Write triggers another attempt. Don't close before rename.
	if err := os.Rename(rw.path, rw.path+".1"); err != nil {
		// Decision ⑧: KEEP the line. The old fd is still open and
		// writable — the rename just couldn't roll the file. Bubbling the
		// error up would make Write drop this line, so a persistent rename
		// failure (e.g. dir perms tightened, fd still good) would silently
		// blackhole the entire file log — the worst outcome on a target=file
		// deploy where nobody reads stderr. Instead arm a backoff (Write
		// skips rotation until it elapses) and return so Write appends to
		// the current fd. The file may exceed maxBytes during the window;
		// that violates the soft size cap but never loses lines.
		//
		// Must NOT use slog here: rotate() runs while Write holds rw.mu, and
		// when rw is the installed slog sink the slog.Error would route back
		// into rw.Write → rw.mu.Lock → self-deadlock (sync.Mutex is not
		// reentrant). The stderr notice fires ~once per backoff cycle (Write
		// gates re-entry on rotateBackoffUntil), not per line.
		fmt.Fprintf(os.Stderr, "logging: rotate rename failed — appending to current file, backing off %s (path=%s err=%v)\n", rotateRetryBackoff, rw.path, err)
		rw.rotateBackoffUntil = rw.clock().Add(rotateRetryBackoff)
		return
	}
	rw.rotateBackoffUntil = time.Time{} // rename succeeded — clear any prior backoff

	// Rename succeeded — safe to close the old fd and open a fresh
	// base file.
	_ = rw.f.Close()
	if err := rw.open(); err != nil {
		// Last resort: keep writing to stderr so we don't lose
		// everything. rw.f = os.Stderr is the sentinel rotate()
		// checks at its top to short-circuit (BUG 1.1 fix); the
		// maxBytes=0 + n=0 reset is belt-and-suspenders to keep size
		// accounting sane on the stderr path.
		// As above: no slog while holding rw.mu — write to stderr directly to
		// avoid the reentrant-lock deadlock.
		fmt.Fprintf(os.Stderr, "logging: reopen after rotate failed — falling back to stderr permanently (path=%s err=%v)\n", rw.path, err)
		rw.f = os.Stderr
		rw.n = 0
		rw.maxBytes = 0
		// Fall through — Write proceeds to rw.f.Write(p) so the line that
		// triggered this rotate still lands on the stderr fallback instead
		// of being dropped. Otherwise the comment's promise to "keep
		// writing to stderr so we don't lose everything" would silently
		// skip this very line; only later lines would reach stderr.
	}
}
