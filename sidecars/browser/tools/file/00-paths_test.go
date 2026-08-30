package file

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/core"
)

// TestResolvePathRelativeToSandbox checks that a model-supplied relative
// path — or a leading "~" — resolves against the per-session sandbox (the
// model's working dir), not the bob process cwd. Escape attempts and
// absolute paths outside every allowed root are still rejected.
func TestResolvePathRelativeToSandbox(t *testing.T) {
	cfg := config.FilesystemConfig{SandboxRoot: t.TempDir()}
	actx := core.AgentCtx{SessionScope: "telegram:dm:test"}
	sandbox := SessionSandbox(cfg, false, actx)
	if err := os.MkdirAll(sandbox, 0o755); err != nil {
		t.Fatalf("mkdir sandbox: %v", err)
	}
	realSandbox, err := filepath.EvalSymlinks(sandbox)
	if err != nil {
		t.Fatalf("evalsymlinks sandbox: %v", err)
	}
	underSandbox := func(p string) bool {
		return strings.HasPrefix(p, realSandbox+string(filepath.Separator))
	}

	// A relative path resolves under the sandbox.
	got, err := ResolvePath(cfg, false, actx, "attachments/foo.epub", ModeWrite)
	if err != nil {
		t.Fatalf("relative path rejected: %v", err)
	}
	if !underSandbox(got) {
		t.Errorf("relative path resolved to %q; want under sandbox %q", got, realSandbox)
	}

	// A leading "~" means the sandbox.
	got, err = ResolvePath(cfg, false, actx, "~/bar.txt", ModeWrite)
	if err != nil {
		t.Fatalf("tilde path rejected: %v", err)
	}
	if !underSandbox(got) {
		t.Errorf("~ path resolved to %q; want under sandbox %q", got, realSandbox)
	}

	// A ".." escape is still rejected — it leaves every allowed root.
	if _, err := ResolvePath(cfg, false, actx, "../../../../../../../../etc/passwd", ModeWrite); err == nil {
		t.Error(".. escape should be rejected")
	}

	// An absolute path outside every allowed root is still rejected.
	if _, err := ResolvePath(cfg, false, actx, "/etc/hostname", ModeRead); err == nil {
		t.Error("absolute path outside roots should be rejected")
	}
}

// TestEnsureSessionSandboxBumpsMtime pins the "mtime == last use" contract
// the idle-sandbox sweep (pipeline-startup sweepBySessionScopeDir) depends
// on: re-entering an EXISTING sandbox must bump the scope dir's own mtime.
// MkdirAll alone does not touch an existing dir, and routine deep writes
// bump only leaf dirs — without the explicit MarkSandboxUsed touch an
// active scope's root mtime would freeze at creation and the sweep would
// reap it (attachments + chromedp login state) after sandboxTTL.
func TestEnsureSessionSandboxBumpsMtime(t *testing.T) {
	cfg := config.FilesystemConfig{SandboxRoot: t.TempDir()}
	actx := core.AgentCtx{SessionScope: "telegram:dm:mtime"}
	dir, err := EnsureSessionSandbox(cfg, false, actx)
	if err != nil {
		t.Fatalf("EnsureSessionSandbox: %v", err)
	}
	// Backdate the dir to simulate a scope created long ago.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := EnsureSessionSandbox(cfg, false, actx); err != nil {
		t.Fatalf("EnsureSessionSandbox (existing): %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().After(old.Add(time.Hour)) {
		t.Fatalf("mtime not bumped on reuse: got %v, still ~%v", info.ModTime(), old)
	}
}

// TestResolveSpacePathConfinement checks that ResolveSpacePath confines
// space-relative paths to spaces/<SanitizeKey(name)>/ — relative paths resolve
// against the space root (not the session sandbox), and .. / absolute /
// symlink escapes are rejected by the same real-path isUnder defense
// ResolvePath uses.
func TestResolveSpacePathConfinement(t *testing.T) {
	root := t.TempDir()
	cfg := config.FilesystemConfig{SandboxRoot: root}
	spaceRoot, err := EnsureSpaceDir(cfg, "acme")
	if err != nil {
		t.Fatalf("EnsureSpaceDir: %v", err)
	}
	realSpace, err := filepath.EvalSymlinks(spaceRoot)
	if err != nil {
		t.Fatalf("evalsymlinks space: %v", err)
	}
	underSpace := func(p string) bool {
		return p == realSpace || strings.HasPrefix(p, realSpace+string(filepath.Separator))
	}

	// A relative path resolves under the space root (write mode → walk-up
	// allows a not-yet-existing file).
	got, err := ResolveSpacePath(cfg, "acme", "reports/q3.pdf", ModeWrite)
	if err != nil {
		t.Fatalf("relative path rejected: %v", err)
	}
	if !underSpace(got) {
		t.Errorf("relative path resolved to %q; want under space %q", got, realSpace)
	}

	// ".." escape is rejected.
	if _, err := ResolveSpacePath(cfg, "acme", "../..", ModeRead); err == nil {
		t.Errorf("expected .. escape to be rejected")
	}
	// An absolute path outside the space is rejected.
	if _, err := ResolveSpacePath(cfg, "acme", filepath.Join(root, "secret"), ModeWrite); err == nil {
		t.Errorf("expected absolute escape to be rejected")
	}

	// A symlink inside the space pointing OUT is caught by the real-path check.
	if err := os.Symlink(root, filepath.Join(spaceRoot, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := ResolveSpacePath(cfg, "acme", "escape/secret", ModeRead); err == nil {
		t.Errorf("expected symlink escape to be rejected")
	}

	// "resale" and "resale/alice" are distinct, non-overlapping space roots.
	a := SpaceDir(cfg, "resale")
	b := SpaceDir(cfg, "resale/alice")
	if a == b || strings.HasPrefix(b, a+string(filepath.Separator)) {
		t.Errorf("private corner not isolated: %q vs %q", a, b)
	}
}
