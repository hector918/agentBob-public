package file

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/core"
	"agentbob/sidecars/browser/tools/policy"
)

// newPatchActx mirrors paths_test's setup: a per-test sandbox + an
// AgentCtx that ResolvePath joins to. Returns the tool + the canonical
// sandbox path so tests can assert against it.
func newPatchActx(t *testing.T) (core.Tool, core.AgentCtx, string) {
	t.Helper()
	cfg := config.FilesystemConfig{SandboxRoot: t.TempDir()}
	actx := core.AgentCtx{SessionScope: "telegram:dm:test"}
	sandbox := SessionSandbox(cfg, false, actx)
	if err := os.MkdirAll(sandbox, 0o755); err != nil {
		t.Fatalf("mkdir sandbox: %v", err)
	}
	pol := policy.NewFromConfig(config.PolicyConfig{})
	return Patch(cfg, pol, false), actx, sandbox
}

// TestPatch_AppendCreatesFile — `append` with a non-existent path
// creates the file (parent dir auto-created), seeds it with the
// content, and reports new_file=true.
func TestPatch_AppendCreatesFile(t *testing.T) {
	tool, actx, sandbox := newPatchActx(t)
	args, _ := json.Marshal(map[string]string{"path": "orc.md", "append": "# ORC\n\nbody\n"})
	res, err := tool.Run(context.Background(), actx, args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Envelope(), `new_file\":true`) {
		t.Errorf("expected new_file=true, got: %s", res.Envelope())
	}
	body, err := os.ReadFile(filepath.Join(sandbox, "orc.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "# ORC\n\nbody\n" {
		t.Errorf("file body wrong: %q", body)
	}
}

// TestPatch_AppendExtendsExisting — second `append` adds to the file
// without losing the first content. Multi-turn workflow.
func TestPatch_AppendExtendsExisting(t *testing.T) {
	tool, actx, sandbox := newPatchActx(t)
	first, _ := json.Marshal(map[string]string{"path": "doc.md", "append": "# §1\nfirst\n"})
	if _, err := tool.Run(context.Background(), actx, first); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	second, _ := json.Marshal(map[string]string{"path": "doc.md", "append": "\n# §2\nsecond\n"})
	res, err := tool.Run(context.Background(), actx, second)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !strings.Contains(res.Envelope(), `new_file\":false`) {
		t.Errorf("second call should report new_file=false, got: %s", res.Envelope())
	}
	body, _ := os.ReadFile(filepath.Join(sandbox, "doc.md"))
	want := "# §1\nfirst\n\n# §2\nsecond\n"
	if string(body) != want {
		t.Errorf("concatenation wrong:\n got: %q\nwant: %q", body, want)
	}
}

// TestPatch_AppendPreservesCRLF — file with CRLF line endings keeps
// CRLF after append (consistency with diff path's existing CRLF
// preservation).
func TestPatch_AppendPreservesCRLF(t *testing.T) {
	tool, actx, sandbox := newPatchActx(t)
	path := filepath.Join(sandbox, "crlf.txt")
	if err := os.WriteFile(path, []byte("line1\r\nline2\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"path": "crlf.txt", "append": "appended\n"})
	if _, err := tool.Run(context.Background(), actx, args); err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, _ := os.ReadFile(path)
	want := "line1\r\nline2\r\nappended\r\n"
	if string(body) != want {
		t.Errorf("CRLF not preserved:\n got: %q\nwant: %q", body, want)
	}
}

// TestPatch_AppendAndDiffMutuallyExclusive — both fields populated →
// the tool refuses the call (envelope, not Run error — model should
// see + recover).
func TestPatch_AppendAndDiffMutuallyExclusive(t *testing.T) {
	tool, actx, _ := newPatchActx(t)
	args, _ := json.Marshal(map[string]string{
		"path":   "x.md",
		"diff":   "@@ -0,0 +1,1 @@\n+x\n",
		"append": "y\n",
	})
	res, err := tool.Run(context.Background(), actx, args)
	if err != nil {
		t.Fatalf("Run shouldn't hard-error, got: %v", err)
	}
	if !res.IsErr() || !strings.Contains(res.Error, "mutually exclusive") {
		t.Errorf("expected mutual-exclusion error result, got: %s", res.Envelope())
	}
}

// TestPatch_NeitherDiffNorAppend — both missing → Run-level error
// (invalid input).
func TestPatch_NeitherDiffNorAppend(t *testing.T) {
	tool, actx, _ := newPatchActx(t)
	args, _ := json.Marshal(map[string]string{"path": "x.md"})
	_, err := tool.Run(context.Background(), actx, args)
	if err == nil {
		t.Fatal("expected error when neither diff nor append given, got nil")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error doesn't mention required: %v", err)
	}
}

// TestPatch_AppendEscapesSandbox — relative path that climbs above
// sandbox via .. is rejected by ResolvePath, no file written.
func TestPatch_AppendEscapesSandbox(t *testing.T) {
	tool, actx, sandbox := newPatchActx(t)
	args, _ := json.Marshal(map[string]string{"path": "../../../escape.txt", "append": "secret\n"})
	res, err := tool.Run(context.Background(), actx, args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsErr() {
		t.Errorf("expected error result on path escape, got: %s", res.Envelope())
	}
	// And no file was written anywhere above sandbox.
	parents := []string{filepath.Dir(filepath.Dir(filepath.Dir(sandbox))), filepath.Dir(filepath.Dir(sandbox))}
	for _, parent := range parents {
		if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
			t.Errorf("escape.txt leaked to %s", parent)
		}
	}
}

// TestPatch_AppendEmptyStringFails — `append: ""` is treated as
// "missing field"; one of diff/append must be provided. Confirms the
// trim discipline: pure newline `"\n"` IS a valid append (paragraph
// break in append-chunks workflow); empty string is not.
func TestPatch_AppendEmptyStringFails(t *testing.T) {
	tool, actx, _ := newPatchActx(t)
	args, _ := json.Marshal(map[string]string{"path": "x.md", "append": ""})
	_, err := tool.Run(context.Background(), actx, args)
	if err == nil {
		t.Fatal("expected error on empty append, got nil")
	}
}

// TestPatch_AppendNewlineOnly — pure "\n" payload is accepted (used
// as a paragraph break between chunks in the long-content workflow).
func TestPatch_AppendNewlineOnly(t *testing.T) {
	tool, actx, sandbox := newPatchActx(t)
	first, _ := json.Marshal(map[string]string{"path": "x.md", "append": "para1\n"})
	if _, err := tool.Run(context.Background(), actx, first); err != nil {
		t.Fatalf("first: %v", err)
	}
	second, _ := json.Marshal(map[string]string{"path": "x.md", "append": "\n"})
	if _, err := tool.Run(context.Background(), actx, second); err != nil {
		t.Fatalf("second (newline-only): %v", err)
	}
	third, _ := json.Marshal(map[string]string{"path": "x.md", "append": "para2\n"})
	if _, err := tool.Run(context.Background(), actx, third); err != nil {
		t.Fatalf("third: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(sandbox, "x.md"))
	if string(body) != "para1\n\npara2\n" {
		t.Errorf("paragraph break not preserved: %q", body)
	}
}

// TestPatch_DiffStillWorks — sanity check the diff mode (existing
// behavior) still creates a new file via @@ -0,0 +1,N @@.
func TestPatch_DiffStillWorks(t *testing.T) {
	tool, actx, sandbox := newPatchActx(t)
	args, _ := json.Marshal(map[string]string{
		"path": "diff.md",
		"diff": "@@ -0,0 +1,2 @@\n+line A\n+line B\n",
	})
	res, err := tool.Run(context.Background(), actx, args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Envelope(), `new_file\":true`) {
		t.Errorf("expected new_file=true, got: %s", res.Envelope())
	}
	body, _ := os.ReadFile(filepath.Join(sandbox, "diff.md"))
	if string(body) != "line A\nline B\n" {
		t.Errorf("diff mode body wrong: %q", body)
	}
}

// TestPatch_AppendCRLFPayloadNoDoubleEncoding — BUG fix (// review): existing CRLF file + payload that ALSO contains \r\n was
// double-encoded into broken \r\r\n. Verifies the normalize-payload-
// first fix in runAppend.
func TestPatch_AppendCRLFPayloadNoDoubleEncoding(t *testing.T) {
	tool, actx, sandbox := newPatchActx(t)
	path := filepath.Join(sandbox, "crlf.txt")
	if err := os.WriteFile(path, []byte("A\r\nB\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Payload from a Windows paste — already has \r\n.
	args, _ := json.Marshal(map[string]string{"path": "crlf.txt", "append": "C\r\nD\n"})
	if _, err := tool.Run(context.Background(), actx, args); err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, _ := os.ReadFile(path)
	want := "A\r\nB\r\nC\r\nD\r\n"
	if string(body) != want {
		t.Errorf("CRLF payload double-encoded:\n got: %q\nwant: %q", body, want)
	}
	// Sanity-check: no \r\r\n anywhere.
	if strings.Contains(string(body), "\r\r\n") {
		t.Errorf("found \\r\\r\\n in result: %q", body)
	}
}

// TestPatch_SerializeIsTrue — pinned contract after the
// review: append mode's read-modify-write makes parallel patches on
// the same path lose writes silently. Serialize=true is the
// non-negotiable guard. If a future change flips this back to false,
// add per-path locking first.
func TestPatch_SerializeIsTrue(t *testing.T) {
	tool, _, _ := newPatchActx(t)
	if !tool.Serialize() {
		t.Error("patch must Serialize=true to prevent lost-write races on the append path")
	}
}
