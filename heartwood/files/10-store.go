// Package files stores captured inbound attachments. The store is a
// scope-AGNOSTIC staging bucket rooted at $BOB_HOME/sandbox/: sources
// save incoming bytes here before the session scope is known, and the
// submit pipeline's placement step (PlaceAttachments) later moves
// accepted files into the resolved scope's space
// (<spaces>/<space>/inbox/), rewriting Attachment.Path space-relative.
// Staging layout:
//
//	$BOB_HOME/sandbox/<source>/<YYYY-MM-DD>/<file>
//
// The "subdir" passed to Save (e.g. "telegram") is chosen by the source
// caller — files just owns the date sub-bucket + size-capped write. The
// recursive sweep walks <base>/ looking for any directory whose basename
// parses as YYYY-MM-DD and prunes by mtime / global cap, so leftover
// staged files (failed or never-reached placements) age out under the
// same date-and-cap policy regardless of subdir layout. SweepSpaces
// (20-sweeper.go) separately age-prunes the per-space trees under
// $BOB_HOME/spaces (whole tree for working spaces; inbox/ only for
// company_ shared stores).
package files

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrTooLarge is returned by Save when the input exceeds the byte limit.
var ErrTooLarge = errors.New("file exceeds the size limit")

// Store writes attachments under a base directory (the sandbox root).
type Store struct{ base string }

// New builds a store rooted at baseDir. Production wiring passes
// $BOB_HOME/sandbox; the staging subdir (typically the source name) is
// then provided on each Save call.
func New(baseDir string) *Store { return &Store{base: baseDir} }

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func safeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = unsafeName.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._-")
	if name == "" {
		name = "file"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

// Save copies up to max bytes from r into
//
//	<base>/<subdir>/<YYYY-MM-DD>/<unixNanos>-<8hex>-<safeName>
//
// and returns the absolute path (Attachment.LocalPath, consumed by the
// placement step), a relative handle
// (<last-subdir-segment>/<YYYY-MM-DD>/<filename>) that callers stash in
// Attachment.Path as a staging-side placeholder — PlaceAttachments
// overwrites it with the space-relative "inbox/<name>" on success — and
// the bytes written. ErrTooLarge if the input exceeds max (the partial
// file is removed). subdir is the scope-agnostic staging bucket the
// caller computed (typically the source name, e.g. "telegram").
func (s *Store) Save(subdir, name string, r io.Reader, max int64) (path, relPath string, size int64, err error) {
	day := time.Now().Format("2006-01-02")
	dir := filepath.Join(s.base, subdir, day)
	// R4Y-3 defense-in-depth: verify the cleaned path still
	// lives under s.base — filepath.Join cleans away ".." segments, so a
	// malicious subdir like "../../etc" would escape the sandbox root.
	// Callers pass a scope-agnostic staging-bucket string (e.g. the source
	// name), so this guard ensures no caller can write outside base.
	cleanBase := filepath.Clean(s.base)
	cleanDir := filepath.Clean(dir)
	if cleanDir != cleanBase && !strings.HasPrefix(cleanDir, cleanBase+string(filepath.Separator)) {
		return "", "", 0, fmt.Errorf("files.Save: subdir %q escapes base %q", subdir, s.base)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", 0, err
	}
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	fname := fmt.Sprintf("%d-%s-%s", time.Now().UnixNano(), hex.EncodeToString(rb[:]), safeName(name))
	path = filepath.Join(dir, fname)
	// Relative handle: drop everything in subdir EXCEPT its last segment.
	// This is only a staging-side placeholder for Attachment.Path — the
	// placement step rewrites it space-relative when the file is accepted.
	relPath = filepath.Join(filepath.Base(subdir), day, fname)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return "", "", 0, err
	}

	limit := max
	if limit <= 0 {
		limit = 25 << 20
	}
	// R5B-2: hard-cap disk usage at `limit` bytes per attempt
	// — never read past it. CopyN copies up to `limit` bytes and returns
	// nil on success (full limit transferred) or io.EOF if r ran out
	// earlier. We then probe for ONE more byte; if the probe succeeds the
	// source is over the limit and we abort. This caps DoS at exactly
	// `limit` bytes vs the previous read-limit+1 form which could already
	// have drained a full extra byte to disk.
	//
	// R5B-1: close BEFORE remove on the error path (Windows
	// won't unlink an open file; on linux it's harmless but consistent).
	// Log unexpected remove failures (not IsNotExist — that's fine, the
	// file may never have existed if OpenFile partially failed upstream).
	cleanup := func(reason error) error {
		_ = f.Close()
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			slog.Warn("files.Save: remove partial file failed",
				"path", path, "reason", reason, "remove_err", rmErr)
		}
		return reason
	}
	n, cerr := io.CopyN(f, r, limit)
	if cerr != nil && cerr != io.EOF {
		return "", "", 0, cleanup(cerr)
	}
	if cerr == nil {
		// Full `limit` bytes consumed — check whether r has more.
		var probe [1]byte
		switch _, perr := io.ReadFull(r, probe[:]); perr {
		case nil:
			return "", "", 0, cleanup(ErrTooLarge)
		case io.EOF, io.ErrUnexpectedEOF:
			// Exactly `limit` bytes — acceptable.
		default:
			return "", "", 0, cleanup(perr)
		}
	}
	if err := f.Close(); err != nil {
		return "", "", 0, cleanup(err)
	}
	return path, relPath, n, nil
}

// Base returns this store's root directory (for log lines).
func (s *Store) Base() string { return s.base }

// dateDir is one date-named subdir collected by the recursive walk.
type dateDir struct {
	abs  string    // absolute path
	day  time.Time // parsed YYYY-MM-DD
	size int64
}

// findDateDirs walks <base> and returns every directory whose basename
// parses as YYYY-MM-DD. computeSize controls whether each entry's
// total byte count is populated (skipped when caller doesn't need it
// — e.g. SweepOlderThan only needs the mtime/path).
func (s *Store) findDateDirs(computeSize bool) []dateDir {
	var out []dateDir
	_ = filepath.WalkDir(s.base, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr — best-effort walk; permission denied etc. shouldn't abort sweep
		}
		base := filepath.Base(p)
		day, perr := time.ParseInLocation("2006-01-02", base, time.Local) // parse in the SAME tz the name was formatted in (line 74 uses local time.Now)
		if perr != nil {
			return nil
		}
		dd := dateDir{abs: p, day: day}
		if computeSize {
			dd.size = dirSize(p)
		}
		out = append(out, dd)
		return filepath.SkipDir // date subdirs don't contain nested date dirs; skip descent
	})
	return out
}

// SweepOlderThan walks every <date> directory under base (regardless
// of how deep — covers the staging tree <base>/<source>/<date>/ and
// any other subdir layout the caller uses), and removes ones whose
// date is older than `days` days ago. Returns how many directories
// were removed.
func (s *Store) SweepOlderThan(days int) int {
	if days <= 0 {
		return 0
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	removed := 0
	for _, dd := range s.findDateDirs(false) {
		if dd.day.Before(cutoff) {
			// os.RemoveAll returns nil for an already-missing path, so a
			// nil result alone does NOT prove we removed anything. Only
			// count a dir we confirmed existed before the call.
			existed := dirExists(dd.abs)
			if os.RemoveAll(dd.abs) == nil && existed {
				removed++
			}
		}
	}
	return removed
}

// EnforceTotalCap walks every <date> directory under base, sums their
// total size, and removes the oldest (across the whole tree) until
// the remaining total is at or below maxBytes. Returns (removed dirs,
// final size). maxBytes <= 0 is a no-op.
func (s *Store) EnforceTotalCap(maxBytes int64) (int, int64) {
	if maxBytes <= 0 {
		return 0, 0
	}
	dirs := s.findDateDirs(true)
	var total int64
	for _, d := range dirs {
		total += d.size
	}
	// Oldest first (chronological).
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].day.Before(dirs[j].day) })
	removed := 0
	// TODAY is exempt: a just-Saved staging file lives in today's date-dir until
	// PlaceAttachments moves it at submit — capping it away would kill an
	// in-flight attachment (dead Path at placement). The cap's job is long-term
	// growth, so it accepts at most one day of bounded overage instead.
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	for total > maxBytes && len(dirs) > 0 {
		d := dirs[0]
		dirs = dirs[1:]
		if !d.day.Before(today) {
			break // reached today's dir (dirs are oldest-first) — stop here
		}
		// os.RemoveAll returns nil for an already-missing path. Only
		// credit the reclaimed bytes (and bump `removed`) when the dir
		// actually existed and was removed — otherwise `total` would be
		// driven below the real on-disk size and we'd stop too early
		// (no-op delete "freeing" space) or, on a genuine RemoveAll
		// failure, keep deleting NEWER dirs to compensate for bytes we
		// never reclaimed. On a real failure we leave `total` intact and
		// simply move on to the next-oldest dir.
		existed := dirExists(d.abs)
		if err := os.RemoveAll(d.abs); err == nil && existed {
			removed++
			total -= d.size
		}
	}
	return removed, total
}

// dirExists reports whether path names an existing directory entry. Used
// by the sweep/cap paths to distinguish a genuine removal from a no-op
// RemoveAll on an already-gone path (both return nil).
func dirExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func dirSize(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, e := d.Info(); e == nil {
			n += fi.Size()
		}
		return nil
	})
	return n
}
