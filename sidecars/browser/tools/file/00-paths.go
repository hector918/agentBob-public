// Package file holds the filesystem tool group: read_file, search_files,
// patch (when slice 2 lands). Shared helpers — path validation, sandbox
// resolution, container detection, binary heuristic — live in this file.
//
// The path-validation algorithm is the audit-fixed version from
// docs/tools-design.md §2:
//  1. filepath.Abs + filepath.Clean
//  2. EvalSymlinks (for write-mode missing files, walk up to existing parent)
//  3. root check uses the REAL path (so a symlink dance can't escape)
//
// In native mode this is the only line of defense; in container mode the
// container itself is the primary boundary and the Go check is a sanity
// net (see §5.5).
package file

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentbob/sidecars/browser/bobhome"
	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/core"
)

// Mode is "read" or "write" — write requires a write_root or sandbox match.
type Mode string

const (
	ModeRead  Mode = "read"
	ModeWrite Mode = "write"
)

// SessionSandbox returns the per-session scratch directory — the
// model's "working home dir" for filesystem and exec tools. Both
// read_file / search_files / patch / execute_code / terminal use
// this one path as cwd + HOME + TMPDIR + the default read+write
// root. Auto-created by EnsureSessionSandbox.
//
// Layout (— added by_sessions_scope/ indexing segment):
//
//	<root>/by_sessions_scope/<session_scope>/                  # no bot
//	<root>/bots/<botID>/by_sessions_scope/<session_scope>/     # NamespaceByBot
//
// root = cfg.SandboxRoot when set, else $BOB_HOME/sandbox/.
//
// Keyed by session_scope (NOT sid) so the env persists across
// compression rollover and /new — model can pip install / clone /
// build inside a thread and the work survives generations. /new
// purges only via explicit SessionPurger (browser.Pool does this
// for chromedp; fs sandbox intentionally does not, to preserve
// venv / cache state).
//
// session_scope contains ':' which would confuse some shells / tools
// — it's replaced with '_' so the directory name is filesystem-safe.
func SessionSandbox(cfg config.FilesystemConfig, namespaceByBot bool, actx core.AgentCtx) string {
	root := cfg.SandboxRoot
	if root == "" {
		root = bobhome.Path("sandbox")
	}
	parts := []string{root}
	if namespaceByBot && actx.Event.BotID != "" {
		parts = append(parts, "bots", actx.Event.BotID)
	}
	parts = append(parts, ScopeRelDir(actx.SessionScope))
	return filepath.Join(parts...)
}

// ScopeRelDir is the single owner of the `by_sessions_scope/<sanitized>`
// path fragment that keys every per-session_scope on-disk area. Both the
// file/exec sandbox (SessionSandbox, above) and the browser pool's
// user-data-dir + screenshot dirs must produce the SAME fragment for the
// same scope, or two subsystems sharing a scope key would land in
// different directories (session-isolation invariant — see SanitizeKey).
// Centralizing ONLY this fragment lets the browser pool keep its own
// root + its own non-bot-namespaced layout while still agreeing on the
// scope segment.
func ScopeRelDir(scope string) string {
	return filepath.Join("by_sessions_scope", SanitizeKey(scope))
}

// SanitizeKey makes a session_scope filesystem-safe via a strict whitelist:
// only [A-Za-z0-9_.-] survives; anything else (control chars, null bytes,
// spaces, ':', '/', '\\', etc.) becomes '_'. Empty input -> "default".
// A leading '.' is rewritten to '_' to defang dot-prefixed traversal
// variants. Exported so EVERY caller across the codebase (file sandbox,
// browser user-data-dir + screenshot dir, attachment store in
// pipeline-startup) produces the SAME on-disk path under
// sandbox/by_sessions_scope/<sanitized>/ — sharing the same scope key
// MUST mean sharing the same directory or session-isolation is broken.
// Audit E6 — defends against path traversal if
// session_scope is attacker-influenced.
func SanitizeKey(s string) string {
	if s == "" {
		return "default"
	}
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			out = append(out, byte(r))
		default:
			out = append(out, '_')
		}
	}
	clean := string(out)
	if clean == "" || clean == "." || clean == ".." {
		return "default"
	}
	if clean[0] == '.' {
		clean = "_" + clean[1:]
	}
	return clean
}

// EnsureSessionSandbox creates the per-session sandbox dir if missing
// and marks it used (MarkSandboxUsed). Tools should call this once at
// the start of any operation that uses the sandbox.
func EnsureSessionSandbox(cfg config.FilesystemConfig, namespaceByBot bool, actx core.AgentCtx) (string, error) {
	dir := SessionSandbox(cfg, namespaceByBot, actx)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create sandbox %s: %w", dir, err)
	}
	MarkSandboxUsed(dir)
	return dir, nil
}

// SpaceRelDir is the single owner of the `spaces/<sanitized>` path fragment
// that keys every named file_storage space — symmetric to ScopeRelDir for the
// per-session sandbox. Routing the name through SanitizeKey is what makes the
// space name traversal-safe and is also why distinct IDs can't sanitize-
// collide into the same directory (see SanitizeKey + docs/spaces.md §2).
//
// Note the fragment is FLAT: "resale" → spaces/resale, "resale/alice" →
// spaces/resale_alice (the '/' sanitizes to '_'). Two distinct names are two
// independent top-level dirs — this is what gives "private corners" for free
// (docs/spaces.md §2 / D7): nobody who passes "resale" can ls into
// "resale_alice".
func SpaceRelDir(name string) string {
	return filepath.Join("spaces", SanitizeKey(name))
}

// SpaceDir returns the absolute path of named file_storage space `name`,
// rooted the same way SessionSandbox is (cfg.SandboxRoot, else
// $BOB_HOME/sandbox/). Deliberately NOT bot-namespaced: spaces are shared
// across bots (the whole point of name-addressing is colleagues seeing the
// same files), so there is no bots/<botID> segment.
func SpaceDir(cfg config.FilesystemConfig, name string) string {
	root := cfg.SandboxRoot
	if root == "" {
		root = bobhome.Path("sandbox")
	}
	return filepath.Join(root, SpaceRelDir(name))
}

// EnsureSpaceDir creates the space dir if missing and returns it. Unlike
// EnsureSessionSandbox there is no MarkSandboxUsed: spaces persist by design
// (the idle sweep keys off the by_sessions_scope fragment and never touches
// spaces/ — docs/spaces.md §8, cleanup is manual).
func EnsureSpaceDir(cfg config.FilesystemConfig, name string) (string, error) {
	dir := SpaceDir(cfg, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create space %s: %w", dir, err)
	}
	return dir, nil
}

// ResolveSpacePath validates a space-relative `input` against space `name`'s
// root and returns the canonical (symlink-resolved) path to operate on. It
// reuses the EXACT same real-path pipeline as ResolvePath (resolveReal +
// isUnder), so the symlink-escape defense is identical: a symlink inside the
// space pointing OUT resolves to a real path outside spaces/<name>/ and is
// rejected by isUnder(real, spaceRoot). Relative paths and "~" resolve against
// the space root (NOT the session sandbox), which is why this can't just defer
// to ResolvePath. mode=ModeWrite enables the walk-up-to-existing-parent block
// for not-yet-created files (e.g. put creating a new file in the space).
func ResolveSpacePath(cfg config.FilesystemConfig, name, input string, mode Mode) (canonical string, err error) {
	spaceRoot := SpaceDir(cfg, name)
	real, err := resolveReal(spaceRoot, input, mode)
	if err != nil {
		return "", err
	}
	if isUnder(real, spaceRoot) {
		return real, nil
	}
	return "", fmt.Errorf("path %q escapes the storage space", input)
}

// MarkSandboxUsed bumps the sandbox dir's own mtime to now. The idle
// sweep (pipeline-startup sweepBySessionScopeDir) reads the scope dir's
// mtime as "last use", but every routine write lands DEEP inside it
// (attachments/<date>/, skills/ symlinks, chromedp/ profile data) and
// only bumps the leaf dir — without an explicit touch the root mtime
// freezes at creation and an active scope gets reaped after sandboxTTL.
// Every per-turn / per-attachment entry point must call this so the
// sweep's mtime check really means "last use". Best-effort: a missing
// dir or failed utimes just leaves the dir eligible for the sweep.
func MarkSandboxUsed(dir string) {
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
}

// resolveReal turns `input` into the canonical, symlink-resolved ("real")
// path it should operate on, resolving relative paths / "~" / "~/..." against
// `base`. It does NOT apply any root allowlist — the caller is responsible for
// the isUnder(real, root) check that is the load-bearing symlink-escape
// defense (see ResolvePath / ResolveSpacePath). Factoring this out lets the
// per-session sandbox (ResolvePath) and a named space (ResolveSpacePath) share
// the exact same relative-base + EvalSymlinks + write-mode walk-up algorithm,
// so neither can be subtly weaker than the other.
//
// For non-existent paths in write mode it walks up to the nearest existing
// parent and resolves THAT, so a not-yet-created file inherits its parent's
// real location.
func resolveReal(base, input string, mode Mode) (real string, err error) {
	if strings.TrimSpace(input) == "" {
		return "", errors.New("empty path")
	}
	in := input
	switch {
	case in == "~":
		in = base
	case strings.HasPrefix(in, "~/"):
		in = filepath.Join(base, in[2:])
	case !filepath.IsAbs(in):
		in = filepath.Join(base, in)
	}
	abs, err := filepath.Abs(in)
	if err != nil {
		return "", fmt.Errorf("absolutize %q: %w", input, err)
	}
	lexical := filepath.Clean(abs)

	real, evalErr := filepath.EvalSymlinks(lexical)
	if evalErr != nil {
		if !errors.Is(evalErr, os.ErrNotExist) || mode != ModeWrite {
			return "", fmt.Errorf("resolve %s: %w", lexical, evalErr)
		}
		parent := filepath.Dir(lexical)
		for {
			if rp, e := filepath.EvalSymlinks(parent); e == nil {
				rest := strings.TrimPrefix(lexical, parent)
				real = filepath.Join(rp, rest)
				break
			}
			next := filepath.Dir(parent)
			if next == parent {
				real = lexical // last resort — nothing in the chain exists
				break
			}
			parent = next
		}
	}
	return real, nil
}

// ResolvePath validates `input` against the configured allowed roots and
// returns the canonical path (post-symlink, post-Clean) the tool should
// operate on. mode controls whether write_roots are required.
//
// See docs/tools-design.md §2 for the algorithm spec.
func ResolvePath(cfg config.FilesystemConfig, namespaceByBot bool, actx core.AgentCtx, input string, mode Mode) (canonical string, err error) {
	// A model-supplied relative path — or a leading "~" — resolves against
	// the per-session sandbox (the model's working dir: the cwd of
	// terminal / execute_code, its HOME, and the default read+write root).
	// filepath.Abs alone would join a relative path to the bob *process*
	// cwd, which is NOT the sandbox — see the SessionSandbox doc above.
	sandbox := SessionSandbox(cfg, namespaceByBot, actx)
	real, err := resolveReal(sandbox, input, mode)
	if err != nil {
		return "", err
	}

	// NOTE: an earlier draft refused outright when real != lexical. That
	// was too strict — on macOS /var, /etc, /tmp are all symlinks to
	// /private/..., so even allowed paths fail. The actual symlink-leak
	// attack (a symlink inside an allowed root pointing OUT) is caught by
	// the root-check below, which uses the REAL path — that is the sole,
	// and sufficient, symlink-escape defense.

	readRoots := append([]string{sandbox}, cfg.ReadRoots...)
	readRoots = append(readRoots, cfg.ReadWriteRoots...)
	writeRoots := append([]string{sandbox}, cfg.ReadWriteRoots...)

	if mode == ModeWrite {
		for _, r := range writeRoots {
			if isUnder(real, expandTilde(r)) {
				return real, nil
			}
		}
		// Not under any write root. Positive-only error: just say WHERE writes
		// work, so the model retries with a valid location rather than a "you
		// did it wrong" framing. (The path being in a READ root vs nowhere made
		// no difference to the message, so there's no branch on it.)
		return "", fmt.Errorf("writable locations: a relative path like %q (auto-resolves to this session's sandbox %s); or an absolute path under read_write_roots %v",
			filepath.Base(real), sandbox, cfg.ReadWriteRoots)
	}

	for _, r := range readRoots {
		if isUnder(real, expandTilde(r)) {
			return real, nil
		}
	}
	return "", fmt.Errorf("readable locations: a relative path like %q (auto-resolves to this session's sandbox %s); or an absolute path under read_roots %v / read_write_roots %v",
		filepath.Base(real), sandbox, cfg.ReadRoots, cfg.ReadWriteRoots)
}

// isUnder reports whether `path` (already symlink-resolved) is at or under
// `root`. We also EvalSymlinks the root — config paths are typically
// lexical (e.g. "/var/folders/..." on macOS), but the path we're checking
// is real ("/private/var/folders/..."), so we must resolve both before
// comparing or every macOS /var/etc/tmp path would fail the prefix check.
//
// A trailing separator is added to root before the prefix check so e.g.
// "/foo" doesn't match "/foobar".
func isUnder(path, root string) bool {
	rabs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rclean := filepath.Clean(rabs)
	if rreal, err := filepath.EvalSymlinks(rclean); err == nil {
		rclean = rreal
	}
	if path == rclean {
		return true
	}
	return strings.HasPrefix(path, rclean+string(filepath.Separator))
}

// expandTilde swaps a leading "~" / "~/" for $HOME. Convenient for config
// paths the operator types as ~/Projects/foo. Doesn't handle ~user (rare).
func expandTilde(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// ContainerRuntime is what DetectContainer reports.
type ContainerRuntime string

const (
	RuntimeNative ContainerRuntime = "native"
	RuntimeDocker ContainerRuntime = "docker"
	RuntimePodman ContainerRuntime = "podman"
	RuntimeOther  ContainerRuntime = "container" // unknown but evidently containerized
)

// DetectContainer returns whether bob is running inside a container, and
// which runtime if known. Heuristics (any one match → containerized):
//   - $BOB_RUNTIME=container (operator override)
//   - /.dockerenv exists (Docker)
//   - /run/.containerenv exists (Podman)
//   - /proc/1/cgroup contains docker / containerd / kubepods / crio
func DetectContainer() ContainerRuntime {
	if os.Getenv("BOB_RUNTIME") == "container" {
		return RuntimeOther
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return RuntimeDocker
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return RuntimePodman
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		switch {
		case strings.Contains(s, "docker"):
			return RuntimeDocker
		case strings.Contains(s, "containerd"), strings.Contains(s, "kubepods"), strings.Contains(s, "crio"):
			return RuntimeOther
		}
	}
	return RuntimeNative
}

// IsBinary returns true if the first up-to-binarySniffBytes bytes of the
// file look binary (any null byte → binary; otherwise not). Cheap, ~99%
// accurate. Used by read_file and search_files (content-mode skip). EOF
// during the sniff is fine — whatever bytes we got is enough.
func IsBinary(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, binarySniffBytes)
	n, _ := f.Read(buf)
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return true, nil
		}
	}
	return false, nil
}

const binarySniffBytes = 8 * 1024
