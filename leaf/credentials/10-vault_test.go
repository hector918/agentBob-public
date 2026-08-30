package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnv(t *testing.T) {
	kind, data, err := parseEnv([]byte("# comment\nkind=ssh\nhost=h1\nuser=u1\n"))
	if err != nil || kind != "ssh" || data["host"] != "h1" || data["user"] != "u1" {
		t.Fatalf("parse = %q %v / %v", kind, data, err)
	}
	// First non-comment line must be kind=.
	if _, _, err := parseEnv([]byte("host=h\n")); err == nil {
		t.Error("missing leading kind= should error")
	}
	// A line without '=' is a loud error.
	if _, _, err := parseEnv([]byte("kind=ssh\nbadline\n")); err == nil {
		t.Error("a line missing '=' should error")
	}
	// '=' inside a value is preserved.
	_, d, _ := parseEnv([]byte("kind=dsn\ndsn=postgres://x?a=b&c=d\n"))
	if d["dsn"] != "postgres://x?a=b&c=d" {
		t.Errorf("'=' in value lost: %q", d["dsn"])
	}
}

// TestParseKindMatchesParseEnv pins the two parsers to the same kind verdict —
// namesByKind/kindOf uses the fast parseKind, broker.Build uses the full parseEnv,
// and they must never disagree on a file's kind (or by-kind resolution diverges
// from what Build actually loads).
func TestParseKindMatchesParseEnv(t *testing.T) {
	cases := [][]byte{
		[]byte("# comment\nkind=ssh\nhost=h1\nuser=u1\n"),
		[]byte("kind=wordpress\nbase_url=https://x\nwoo_ck=\nwp_pass=a b c\n"),
		[]byte("\n\n#lead\nkind=dsn\ndsn=postgres://x?a=b\n"),
	}
	for _, b := range cases {
		wantKind, _, wantErr := parseEnv(b)
		gotKind, gotErr := parseKind(b)
		if (gotErr == nil) != (wantErr == nil) || gotKind != wantKind {
			t.Errorf("parseKind=%q,%v  parseEnv=%q,%v  for %q", gotKind, gotErr, wantKind, wantErr, b)
		}
	}
	// Malformed files error in BOTH — including a bad line AFTER the kind line:
	// parseKind validates the whole file (not an early stop), else it would accept a
	// file parseEnv rejects and mis-classify it as a live candidate.
	for _, bad := range [][]byte{
		[]byte("host=h\n"),                  // no leading kind=
		[]byte("kind=\n"),                   // empty kind value
		[]byte("badline\n"),                 // no '='
		[]byte(""),                          // empty file
		[]byte("kind=wordpress\nbadline\n"), // malformed line AFTER kind= (the divergence case)
		[]byte("kind=ssh\n=novalue\n"),      // empty key after kind=
	} {
		_, kerr := parseKind(bad)
		_, _, eerr := parseEnv(bad)
		if (kerr == nil) != (eerr == nil) {
			t.Errorf("parseKind/parseEnv disagree on %q: parseKind err=%v parseEnv err=%v", bad, kerr, eerr)
		}
		if kerr == nil {
			t.Errorf("parseKind(%q) = nil err, want error", bad)
		}
	}
}

func TestVaultLoad(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "credentials")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "x.env"), []byte("kind=ssh\nhost=h\nuser=u\n"), 0o600)

	kind, data, err := load(home, "x")
	if err != nil || kind != "ssh" || data["user"] != "u" {
		t.Fatalf("load = %q %v / %v", kind, data, err)
	}
	// missing credential → error
	if _, _, err := load(home, "y"); err == nil {
		t.Error("load(y) should error (not found)")
	}
	// invalid name (path traversal) → error, never read
	if _, _, err := load(home, "../etc/passwd"); err == nil {
		t.Error("an invalid credential name must be rejected")
	}
}
