package browser

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Per-session worker COPY support (browser profile identity, Phase 2 / docs/
// wip-browser-profile-identity.md). A worker turn runs on a READ-ONLY copy of its
// principal's master profile dir — its own chromium instance + its own dir — so
// concurrent same-principal turns never collide on the one custodian-exclusive vault
// instance. The copy is discarded on close (the worker never writes the login back; only
// the in-place login session a human takes over does). The master itself stays the vault
// dir (ProfileDir(principal)); copyProfileDir seeds a fresh per-session copy from it.

// copyCacheDirNames are profile sub-DIRS skipped entirely when copying: heavy and fully
// regenerable, never login-bearing. Matched by base name at any depth (chromium nests
// some under Default/).
var copyCacheDirNames = map[string]bool{
	"Cache": true, "Code Cache": true, "GPUCache": true, "ShaderCache": true,
	"GrShaderCache": true, "DawnCache": true, "DawnGraphiteCache": true,
	"DawnWebGPUCache": true, "component_crx_cache": true, "extensions_crx_cache": true,
	// chromium nests the service-worker caches as "Service Worker/CacheStorage" +
	// "Service Worker/ScriptCache" — match by their (nested) base names so they're
	// skipped, while keeping "Service Worker/Database" (the registration that some
	// sites' auth depends on).
	"CacheStorage": true, "ScriptCache": true,
}

// copySingletonNames are chromium's process-singleton lock files — a copy must NOT carry
// a live lock (it would make the fresh instance think another chromium owns the dir).
var copySingletonNames = map[string]bool{
	"SingletonLock": true, "SingletonSocket": true, "SingletonCookie": true,
}

// copyProfileDir copies a browser profile dir (src → dst) for a read-only per-session
// worker copy. It skips the singleton lock files (the copy must be lock-free) and the
// regenerable cache dirs (so the copy is cheap — only login-bearing state: Cookies,
// Local Storage, IndexedDB, Service Worker, Network, Preferences, Local State travels).
//
// src not existing is NOT an error: a never-logged-in principal has no master yet → dst
// is left as a fresh empty 0700 dir (chromium initialises it; the worker is simply not
// logged in and will hit the login wall → handoff). dst is created if absent; callers
// pass a fresh path (the pool RemoveAll's a stale copy before seeding).
func copyProfileDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("browser copy: mkdir %s: %w", dst, err)
	}
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil // no master yet → empty copy
	}
	if err != nil {
		return fmt.Errorf("browser copy: stat src %s: %w", src, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("browser copy: src %s is not a directory", src)
	}
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		base := filepath.Base(rel)
		if fi.IsDir() {
			if copyCacheDirNames[base] {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		if copySingletonNames[base] || strings.HasPrefix(base, ".org.chromium.Chromium.") {
			return nil // lock files + chromium temp scratch — never copy
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil // skip symlinks (cache/socket artifacts); login state is regular files
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, filepath.Join(dst, rel), fi.Mode().Perm())
	})
}

// writebackStaging is a DOT-PREFIXED dir inside the vault root holding write-back temps.
// Dot-prefixed so ValidProfileVaultKey rejects it → it is NEVER listed/exported as a
// profile (the same trick .import-*/.old-* use). A sibling of the masters (same FS → atomic
// rename). ReconcileWritebacks recovers an interrupted swap from here at startup.
func writebackStaging(dst string) string { return filepath.Join(filepath.Dir(dst), ".wb-staging") }

// writeProfileBack copies a worker COPY dir (src) back to a MEMBER master dir (dst) — the
// one path that updates a master (保存登陆 / a controlled-close fallback). Builds the new
// master in .wb-staging (off the listed namespace) via copyProfileDir (login state incl
// SQLite WAL/journal, skipping cache + locks), then install-before-destroy swap (old aside,
// new in, drop old) so a concurrent seed — serialised by the per-member master lock — reads
// either the whole old or whole new master, never a half. A crash in the swap window leaves
// the prior master in .wb-staging/<base>.old; ReconcileWritebacks restores it at next start.
func writeProfileBack(src, dst string) (int64, error) {
	stage := writebackStaging(dst)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return 0, fmt.Errorf("browser writeback: mkdir staging: %w", err)
	}
	base := filepath.Base(dst)
	tmp := filepath.Join(stage, base+".tmp")
	old := filepath.Join(stage, base+".old")
	_ = os.RemoveAll(tmp)
	_ = os.RemoveAll(old)
	if err := copyProfileDir(src, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return 0, fmt.Errorf("browser writeback: build tmp: %w", err)
	}
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			_ = os.RemoveAll(tmp)
			return 0, fmt.Errorf("browser writeback: set aside old master: %w", err)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Rename(old, dst) // best-effort restore
		_ = os.RemoveAll(tmp)
		return 0, fmt.Errorf("browser writeback: install new master: %w", err)
	}
	_ = os.RemoveAll(old)
	return dirSize(dst), nil
}

// ReconcileWritebacks (called at startup) recovers a master whose write-back was interrupted
// mid-swap — its prior copy sits in .wb-staging/<base>.old while the master dir is missing —
// then clears the staging area. vaultRoot is ProfileVaultRoot().
func ReconcileWritebacks(vaultRoot string) {
	stage := filepath.Join(vaultRoot, ".wb-staging")
	entries, err := os.ReadDir(stage)
	if err != nil {
		return // no staging → nothing to do
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".old") {
			continue
		}
		dst := filepath.Join(vaultRoot, strings.TrimSuffix(name, ".old"))
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			if rerr := os.Rename(filepath.Join(stage, name), dst); rerr == nil {
				slog.Warn("browser writeback: recovered an interrupted master from staging", "master", dst)
				continue
			}
		}
	}
	_ = os.RemoveAll(stage) // clear leftovers (recovered .old already moved out)
}

// dirSize sums regular-file bytes under dir (best-effort; for the writeback size hook).
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// copyFile copies one regular file src → dst with perm, creating parent dirs.
func copyFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
