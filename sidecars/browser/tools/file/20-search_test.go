package file

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/core"
	"agentbob/sidecars/browser/tools/policy"
)

// TestSearchFilesSymlinkEscape is a security regression test: a symlink
// placed inside an allowed sandbox root that points OUT of the sandbox
// must NOT have its content scanned/returned. A model can create such a
// link via the terminal tool (`ln -s /secret leak.txt`), so search_files
// must resolve each matched symlink and refuse targets that escape the
// search root. Real (non-symlink) files in the sandbox must still scan.
func TestSearchFilesSymlinkEscape(t *testing.T) {
	cfg := config.FilesystemConfig{SandboxRoot: t.TempDir()}
	actx := core.AgentCtx{SessionScope: "telegram:dm:test"}
	sandbox := SessionSandbox(cfg, false, actx)
	if err := os.MkdirAll(sandbox, 0o755); err != nil {
		t.Fatalf("mkdir sandbox: %v", err)
	}

	const secretMarker = "TOPSECRET_SENTINEL_VALUE"
	const realMarker = "LEGIT_SENTINEL_VALUE"

	// A real, in-sandbox file that should be found.
	realPath := filepath.Join(sandbox, "real.txt")
	if err := os.WriteFile(realPath, []byte("hello "+realMarker+" world\n"), 0o644); err != nil {
		t.Fatalf("write real file: %v", err)
	}

	// An out-of-sandbox secret + a symlink to it INSIDE the sandbox.
	outside := t.TempDir() // distinct tree, not under sandbox
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("oops "+secretMarker+" leaked\n"), 0o644); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	linkPath := filepath.Join(sandbox, "leak.txt")
	if err := os.Symlink(secretPath, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	tool := SearchFiles(cfg, policy.NewFromConfig(config.PolicyConfig{}), false)

	// Content-search for ANY of the two markers (regex alternation).
	args, _ := json.Marshal(map[string]any{
		"root":    sandbox,
		"pattern": secretMarker + "|" + realMarker,
		"kind":    "content",
	})
	res, err := tool.Run(context.Background(), actx, args)
	if err != nil {
		t.Fatalf("search Run error: %v", err)
	}

	body := res.Envelope()
	if strings.Contains(body, secretMarker) {
		t.Errorf("symlink escape: out-of-sandbox secret %q leaked into search results:\n%s", secretMarker, body)
	}
	if !strings.Contains(body, realMarker) {
		t.Errorf("happy path broken: in-sandbox file marker %q not found in results:\n%s", realMarker, body)
	}
}

// TestScanContentOversizedLine pins the fix: a single line over the scanner's
// per-line ceiling makes bufio.Scanner stop with ErrTooLong. scanContent must
// report incomplete=true so search_files surfaces truncated:true rather than
// silently dropping that line and everything after it while claiming a complete
// scan.
func TestScanContentOversizedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minified.js")

	// Line 1: a normal match. Line 2: an oversized single line that also
	// matches. Line 3 must go unscanned once line 2 trips ErrTooLong.
	big := "needle" + strings.Repeat("x", 70*1024)
	content := "first needle here\n" + big + "\nlast needle line\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	re := regexp.MustCompile("needle")
	hits, incomplete, err := scanContent(path, re, 100)
	if err != nil {
		t.Fatalf("scanContent: %v", err)
	}
	if !incomplete {
		t.Fatalf("incomplete = false, want true (oversized line must flag a truncated scan)")
	}
	// The first (in-buffer) line should still be returned.
	if len(hits) == 0 || hits[0].lineNumber != 1 {
		t.Fatalf("expected the first line to still match; got %+v", hits)
	}
}
