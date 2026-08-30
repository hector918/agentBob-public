package warrant

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCappedWriter: writes past the cap are dropped (bounded memory) while the total
// byte count survives for the truncation notice, and short output passes verbatim.
func TestCappedWriter(t *testing.T) {
	// Way over the cap, in many small writes (mirrors streamed command output).
	w := &cappedWriter{}
	const chunk = 4096
	writes := (execMaxOutput/chunk)*3 + 5 // comfortably > cap
	total := 0
	for i := 0; i < writes; i++ {
		n, err := w.Write([]byte(strings.Repeat("a", chunk)))
		if err != nil || n != chunk {
			t.Fatalf("Write = %d, %v; want %d, nil (must accept every byte)", n, err, chunk)
		}
		total += chunk
	}
	if len(w.buf) > execMaxOutput {
		t.Fatalf("stored %d bytes, want capped at %d (unbounded buffering = OOM)", len(w.buf), execMaxOutput)
	}
	if w.total != total {
		t.Fatalf("total counter = %d, want %d", w.total, total)
	}
	s, trunc := w.result()
	if !trunc {
		t.Fatal("result reported not-truncated for over-cap output")
	}
	if !strings.Contains(s, fmt.Sprintf("of %d bytes", total)) {
		t.Errorf("truncation notice missing total %d: %q", total, s[len(s)-80:])
	}

	// Under the cap: verbatim, no truncation.
	w2 := &cappedWriter{}
	_, _ = w2.Write([]byte("hello world"))
	if s2, trunc2 := w2.result(); trunc2 || s2 != "hello world" {
		t.Errorf("small output = %q, trunc=%v; want verbatim, false", s2, trunc2)
	}

	// The result must stay valid UTF-8 regardless of where the byte cap lands
	// relative to a multibyte rune — whether the cap slices a rune mid-way (drop
	// the partial rune) or lands exactly after a complete one (keep it intact).
	// '中' is 3 bytes (E4 B8 AD); sweep the cap across a rune boundary.
	for extra := 0; extra <= 4; extra++ {
		body := strings.Repeat("a", execMaxOutput-2+extra) + strings.Repeat("中", 4)
		w3 := &cappedWriter{}
		_, _ = w3.Write([]byte(body))
		s3, trunc3 := w3.result()
		if !trunc3 {
			t.Fatalf("extra=%d: expected truncation for over-cap body", extra)
		}
		kept := strings.TrimSuffix(s3[:strings.LastIndex(s3, "\n…[truncated")], "")
		if !utf8.ValidString(kept) {
			t.Errorf("extra=%d: result not valid UTF-8 (dangling rune byte): %q", extra, kept[max(0, len(kept)-8):])
		}
	}
}

// TestLocalExecDoesNotLeakSecrets locks the docs/security-assumptions.md §5.2
// invariant: the model-controlled shell must NOT inherit bob's process env (where
// DSN / API keys / bot tokens live). A planted secret env var must be absent from
// `env` inside the sandbox.
func TestLocalExecDoesNotLeakSecrets(t *testing.T) {
	t.Setenv("BOB_FAKE_TOKEN_TEST", "super-secret-value")
	ch, err := newLocalExecChannel("s1", t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	r, err := ch.Run(context.Background(), "env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Output, "super-secret-value") || strings.Contains(r.Output, "BOB_FAKE_TOKEN_TEST") {
		t.Fatalf("sandbox leaked a host env secret into the shell:\n%s", r.Output)
	}
	// The whitelist still provides the essentials.
	if !strings.Contains(r.Output, "HOME=") || !strings.Contains(r.Output, "PATH=") {
		t.Errorf("sandbox env missing essentials (HOME/PATH): %q", r.Output)
	}
}

// TestLocalExecRuns: commands run in the space dir, output + exit code come back.
func TestLocalExecRuns(t *testing.T) {
	ch, err := newLocalExecChannel("s1", t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	ctx := context.Background()

	r, err := ch.Run(ctx, "echo hello")
	if err != nil || !strings.Contains(r.Output, "hello") || r.ExitCode != 0 {
		t.Fatalf("echo = %+v / %v, want hello exit 0", r, err)
	}
	r, err = ch.Run(ctx, "false")
	if err != nil || r.ExitCode != 1 {
		t.Errorf("false = %+v / %v, want exit 1", r, err)
	}
	// stderr is captured too.
	r, _ = ch.Run(ctx, "echo oops >&2")
	if !strings.Contains(r.Output, "oops") {
		t.Errorf("stderr not captured: %q", r.Output)
	}
}

// TestExecSharesSpaceWithFile: a file written via the file channel is visible to
// the shell in the same space (the co-location the unified-space design promises).
func TestExecSharesSpaceWithFile(t *testing.T) {
	m := &Manager{home: t.TempDir()}
	m.policy.Store(emptyPolicy())
	ctx := context.Background()

	fc, err := m.File(ctx, "user", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := fc.Write(ctx, "note.txt", []byte("from-file")); err != nil {
		t.Fatal(err)
	}
	ec, err := m.Exec(ctx, "user", "shared") // nil pool → fresh
	if err != nil {
		t.Fatal(err)
	}
	defer ec.Close()
	r, err := ec.Run(ctx, "cat note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Output, "from-file") {
		t.Errorf("shell did not see the file written via the file channel: %q", r.Output)
	}
}
