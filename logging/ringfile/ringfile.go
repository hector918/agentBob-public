// Package ringfile is a size-bounded JSONL-style append log shared by
// the permissions judge log and the credentials broker call log. Each
// caller marshals its own record format and calls WriteRecord with the
// already-encoded bytes; ringfile owns the append/cap/trim/cached-fd
// machinery and the WARN-once-self-disable error contract.
//
// Design (REFACTOR 3.1): both audit streams previously
// carried a ~120 LOC writeRecord + trimLocked pair that was byte-
// equivalent except for slog WARN message wording. The duplication was
// a drift risk — any future fix to one would silently skip the other.
// This package extracts the shared machinery; per-domain JSON marshal,
// per-subsystem singleton plumbing, and per-subsystem clock injection
// (SetClock / nowFunc) stay in the caller packages, since clock choice
// is per-subsystem and ringfile intentionally knows nothing about time.
//
// Behaviour contract preserved verbatim from the original impls:
//
//   - Cached fd reuse (R4Y-17 fix): open once at Open,
//     reuse for every write, close+reopen during trim. Eliminates
//     per-write syscall overhead under the rich-plugin path where
//     every tool call hits Judge / every broker call hits the vault.
//   - Disabled sentinel: a WARN-once-then-self-disable contract — Open
//     failure or trim failure flips disabled=true and subsequent writes
//     short-circuit silently. Never returns error to caller / never
//     blocks the hot path.
//   - Record > maxBytes/2 defensive skip: a single record larger than
//     half the cap can't survive trim → WARN + skip, don't even try
//     to write.
//   - Trim strategy: rename base → .trim file via half-cut at byte
//     offset + ReadBytes('\n') to align, swap with os.Rename, then
//     re-open the cached fd on the new inode.
//   - Close-before-rename invariant: cached fd MUST be closed before
//     os.Rename runs, otherwise the next write would go to the
//     unlinked-but-still-open pre-trim file.
//   - 0o600 file mode (audit records carry identity attrs — sensitive).
package ringfile

import (
	"bufio"
	"io"
	"log/slog"
	"os"
	"sync"
)

// RingFile is a size-bounded JSONL-style append log. Construct via Open;
// the zero value is not usable. Safe for concurrent use — single mutex.
type RingFile struct {
	path      string
	maxBytes  int64
	subsystem string // slog tag prefix, e.g. "permissions: judge log"

	mu       sync.Mutex
	disabled bool
	// f is the cached append-mode file handle. Open once at Open(),
	// reused per WriteRecord, swapped during trim. Eliminates the
	// 3-syscall open/encode/close per record.
	f *os.File
	// n is the tracked byte size of the file: seeded from Stat wherever
	// f is (re)opened (Open, lazy reopen, trim), then bumped in memory
	// per write. Mirrors rotatingWriter in logging/10-slog.go and drops
	// the per-record fstat from the audit hot path. Valid only while
	// f != nil.
	n int64
}

// seedSize refreshes r.n from the cached handle. Caller must hold r.mu
// (or be inside Open before r escapes). Called wherever r.f is
// (re)opened so the tracked size stays in sync with the inode.
func (r *RingFile) seedSize() {
	r.n = 0
	if r.f == nil {
		return
	}
	if fi, err := r.f.Stat(); err == nil && fi != nil {
		r.n = fi.Size()
	}
}

// Open initialises a RingFile. path is the full file path, maxBytes is
// the soft cap (writes that would push past trigger an in-place trim of
// the oldest half), subsystem is the slog tag prefix used in WARN
// messages (e.g. "permissions: judge log" / "credentials: call log").
//
// Returns a usable *RingFile even on initial open failure — WARN is
// emitted and subsequent WriteRecord calls self-disable (best-effort
// contract; never block).
//
// path == "" or maxBytes <= 0 returns (nil, nil) — caller intentionally
// disabled logging; callers should nil-guard their *RingFile pointer.
func Open(path string, maxBytes int64, subsystem string) (*RingFile, error) {
	if path == "" || maxBytes <= 0 {
		return nil, nil
	}
	r := &RingFile{path: path, maxBytes: maxBytes, subsystem: subsystem}
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		r.f = f
		r.seedSize()
	} else {
		slog.Warn(subsystem+" initial open failed (logging will retry on first write)",
			"path", path, "err", err)
	}
	return r, nil
}

// WriteRecord appends one record (raw bytes — caller has already
// marshalled JSON / done any per-domain formatting). The record gets a
// trailing '\n' appended internally. Best-effort: never returns error;
// errors WARN once + self-disable.
func (r *RingFile) WriteRecord(rec []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disabled {
		return
	}
	rec = append(rec, '\n')

	if int64(len(rec)) > r.maxBytes/2 {
		// Single record bigger than half the cap can't survive trim;
		// shouldn't happen for audit records but defensive.
		slog.Warn(r.subsystem+" record larger than half the cap; skipping",
			"rec_bytes", len(rec), "max_bytes", r.maxBytes)
		return
	}

	// Use the tracked size for the cap check (seeded from Stat wherever
	// the handle is opened, bumped per write — no fstat on the hot
	// path). Falls back to os.Stat when the handle isn't open yet (init
	// failed) or after a trim swap that didn't reopen.
	sz := r.n
	if r.f == nil {
		sz = 0
		if fi, _ := os.Stat(r.path); fi != nil {
			sz = fi.Size()
		}
	}

	if sz+int64(len(rec)) > r.maxBytes {
		if terr := r.trimLocked(); terr != nil {
			slog.Warn(r.subsystem+" trim failed; disabling further captures",
				"path", r.path, "err", terr)
			r.disabled = true
			if r.f != nil {
				_ = r.f.Close()
				r.f = nil
			}
			return
		}
	}

	// Re-open lazily if the cached handle isn't usable (init failed or
	// trim closed-without-reopen). One slow path; subsequent writes
	// reuse the new handle.
	if r.f == nil {
		f, oerr := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if oerr != nil {
			slog.Warn(r.subsystem+" open failed; disabling further captures",
				"path", r.path, "err", oerr)
			r.disabled = true
			return
		}
		r.f = f
		r.seedSize()
	}
	if n, werr := r.f.Write(rec); werr != nil {
		// Match the trim/open paths and the package's WARN-once-then-
		// self-disable contract: a write failure (invalidated fd, full
		// disk) would otherwise WARN on every subsequent record. Disable
		// after one WARN; drop the dead handle.
		slog.Warn(r.subsystem+" write failed; disabling further captures",
			"path", r.path, "err", werr)
		r.disabled = true
		_ = r.f.Close()
		r.f = nil
	} else {
		r.n += int64(n)
	}
}

// Close releases the cached fd. Idempotent. After Close, WriteRecord
// is a silent no-op (disabled=true).
func (r *RingFile) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	r.disabled = true
}

// IsDisabled reports whether the ring file has flipped into the
// self-disabled state (Open failure, trim failure, write failure, or
// explicit Close). Exposed for tests that need to assert the
// best-effort contract took effect; production callers should treat
// WriteRecord as fire-and-forget regardless.
func (r *RingFile) IsDisabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.disabled
}

// trimLocked drops the oldest ~half of the file. Caller must hold r.mu.
// Runs ≈ once per (maxBytes/2) bytes written so the amortised cost is
// negligible. Closes + re-opens the cached file handle so subsequent
// WriteRecord calls land on the new inode (the old one would still be
// referenced by the rename target).
func (r *RingFile) trimLocked() error {
	// Close the cached append handle before we rewrite the file —
	// otherwise our next write would go to the unlinked-but-still-open
	// pre-trim file and never reach the trimmed one.
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}

	src, err := os.Open(r.path)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	cutAt := fi.Size() / 2
	if cutAt <= 0 {
		// Nothing to trim; re-open the cached handle on the way out so
		// WriteRecord can resume on the existing file.
		f, oerr := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if oerr == nil {
			r.f = f
			r.seedSize()
		}
		return nil
	}
	if _, err := src.Seek(cutAt, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReader(src)
	if _, err := br.ReadBytes('\n'); err != nil && err != io.EOF {
		return err
	}
	tmp := r.path + ".trim"
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, br); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Re-open the cached handle pointing at the freshly trimmed file
	// so subsequent WriteRecord calls go to the right inode.
	f, oerr := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if oerr != nil {
		return oerr
	}
	r.f = f
	r.seedSize()
	return nil
}
