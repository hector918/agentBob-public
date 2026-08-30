package warrant

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 45-exechome-sweep.go — retention sweep for $BOB_HOME/exec-home/ (persistent
// disk state → trunk Housekeeper, per the Housekeeper-persistent-only rule).
//
// exec-home/<space>/ is the PRIVATE HOME/TMPDIR vended to local exec channels
// (40-channel.go) so dotdirs, package caches and mktemp litter stay out of the
// user-visible space dir. That privacy cuts both ways: neither the model nor the
// user can see the dir, so without this sweep it grows unbounded for the life of
// the deployment. Rules, most to least aggressive:
//
//   - ORPHAN: an exec-home/<name> whose spaces/<name> sibling no longer exists
//     is removed whole — the space is gone, its shell home has no owner.
//   - CACHE: inside a live space, the contents of the well-known regenerable
//     cache dirs (cachePruneDirs) are age-pruned by mtime — a cold cache is
//     re-downloaded on next use, slower but never wrong.
//   - TMP LITTER: TMPDIR is the exec-home root itself, so non-dot top-level
//     entries (mktemp files/dirs) are removed whole once their own mtime ages
//     out.
//   - Everything else — .local, .cargo, other dotdirs — is INSTALL STATE and is
//     never touched: a `pip install --user` tree keeps its install-time mtime
//     forever, so age-pruning it would part-delete a still-working toolchain
//     (wrong, not slow).
//
// The tree is MODEL-WRITABLE (it is the exec channel's $HOME), so the walk pins
// directory fds with os.OpenRoot exactly like files/20-sweeper.go::sweepSpaceInbox
// — a path-string walk would let a dir be swapped for a symlink between the
// check and the remove, unlinking files outside exec-home.

const (
	execHomeRetention   = 30 * 24 * time.Hour
	execHomeSweepPeriod = 7 * 24 * time.Hour // firstRunDelay makes long periods restart-safe
)

// cachePruneDirs are the top-level dirs inside a space's exec-home whose
// CONTENTS are regenerable caches, safe to age-prune.
var cachePruneDirs = map[string]bool{
	".cache": true, // XDG cache: pip, uv, go-build, ...
	".npm":   true, // npm _cacache
	".pip":   true, // legacy pip cache location
	"tmp":    true,
	".tmp":   true,
}

// sweepExecHome applies the rules above once. Returns entries removed.
// Best-effort throughout: an unreadable entry is skipped, not fatal.
func sweepExecHome(ctx context.Context, home string, cutoff time.Time) int {
	root, err := os.OpenRoot(filepath.Join(home, "exec-home"))
	if err != nil {
		return 0 // no exec-home yet (or unreadable) — nothing to sweep
	}
	defer root.Close()
	entries, err := readDirRoot(root, ".")
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if ctx.Err() != nil {
			return removed
		}
		if !e.IsDir() { // symlink or stray file at the root — not ours to judge
			continue
		}
		// ORPHAN: exec-home names mirror spaces/ names (both sanitizeSpace'd at
		// creation), so a missing spaces/<name> means the space was torn down.
		if _, serr := os.Stat(filepath.Join(home, "spaces", e.Name())); os.IsNotExist(serr) {
			if root.RemoveAll(e.Name()) == nil {
				removed++
			}
			continue
		}
		spaceRoot, oerr := root.OpenRoot(e.Name())
		if oerr != nil {
			continue
		}
		removed += pruneSpaceExecHome(ctx, spaceRoot, cutoff)
		spaceRoot.Close()
	}
	return removed
}

// pruneSpaceExecHome applies the CACHE + TMP-LITTER rules to one live space's
// pinned exec-home root.
func pruneSpaceExecHome(ctx context.Context, r *os.Root, cutoff time.Time) int {
	entries, err := readDirRoot(r, ".")
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if ctx.Err() != nil {
			return removed
		}
		name := e.Name()
		switch {
		case cachePruneDirs[name] && e.IsDir():
			sub, oerr := r.OpenRoot(name)
			if oerr != nil {
				continue
			}
			removed += pruneTreeOld(ctx, sub, ".", cutoff)
			sub.Close()
		case strings.HasPrefix(name, "."):
			// Install state (.local, .cargo, ...) — never touched.
		default:
			// TMPDIR litter at the root: whole-entry removal by its own mtime.
			// (A fresh mktemp dir is protected by its young mtime — see the
			// age guard, which covers dirs here too.)
			fi, lerr := r.Lstat(name)
			if lerr == nil && fi.ModTime().Before(cutoff) {
				if r.RemoveAll(name) == nil {
					removed++
				}
			}
		}
	}
	return removed
}

// pruneTreeOld age-prunes regular files under dir (relative to the pinned root
// r), then removes dirs left empty — but only dirs whose own mtime aged out, so
// a just-created mktemp-style dir is never yanked from under a running command.
// Symlinks are left alone (removing one is pointless; following one is the
// escape this walk exists to prevent). Returns entries removed; the bool
// reports whether dir ended up empty.
func pruneTreeOld(ctx context.Context, r *os.Root, dir string, cutoff time.Time) int {
	removed, _ := pruneDirOld(ctx, r, dir, cutoff)
	return removed
}

func pruneDirOld(ctx context.Context, r *os.Root, dir string, cutoff time.Time) (int, bool) {
	entries, err := readDirRoot(r, dir)
	if err != nil {
		return 0, false
	}
	removed, empty := 0, true
	for _, e := range entries {
		if ctx.Err() != nil {
			return removed, false
		}
		p := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			n, subEmpty := pruneDirOld(ctx, r, p, cutoff)
			removed += n
			if subEmpty {
				if fi, lerr := r.Lstat(p); lerr == nil && fi.ModTime().Before(cutoff) && r.Remove(p) == nil {
					removed++
					continue
				}
			}
			empty = false
		case e.Type().IsRegular():
			fi, lerr := r.Lstat(p)
			if lerr == nil && fi.ModTime().Before(cutoff) && r.Remove(p) == nil {
				removed++
				continue
			}
			empty = false
		default:
			empty = false // symlink / device / socket — left alone
		}
	}
	return removed, empty
}

// readDirRoot lists one dir relative to the pinned root.
func readDirRoot(r *os.Root, dir string) ([]os.DirEntry, error) {
	d, err := r.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.ReadDir(-1)
}
