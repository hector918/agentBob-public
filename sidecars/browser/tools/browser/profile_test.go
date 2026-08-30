package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbob/sidecars/browser/bobhome"
)

// TestMain pins a temp $BOB_HOME for the whole package BEFORE any test triggers
// bobhome.Init() (which caches the home once). Without this, a per-test SetHome
// is silently ignored when another browser test resolves the home first. Also
// keeps the package's tests off the real ~/.bob.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "browser-test-home")
	if err != nil {
		panic(err)
	}
	bobhome.SetHome(home) // before any Init() → takes effect
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

func TestProfileName(t *testing.T) {
	if got := ProfileName("ac_hector"); got != "browser_ac_hector" {
		t.Fatalf("ProfileName = %q, want browser_ac_hector", got)
	}
}

func TestSanitizeProfileID(t *testing.T) {
	cases := map[string]string{
		"ac_hector":        "ac_hector",
		"feishu:ou_abc.12": "feishu_ou_abc.12", // ':' → '_', dots kept
		"../etc/passwd":    "_etc_passwd",      // '/' → '_', leading dots trimmed → traversal-safe
		"..":               "",                 // pure-dots → rejected
		".":                "",                 // pure-dots → rejected
		"":                 "",
		"  ":               "",
		"a/b\\c":           "a_b_c",
	}
	for in, want := range cases {
		if got := sanitizeProfileID(in); got != want {
			t.Fatalf("sanitizeProfileID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestProfileDir pins the vault-rooted path, 0o700 mode, idempotent create, and
// the empty/invalid-identity error. Uses a temp BOB_HOME via SetHome.
func TestProfileDir(t *testing.T) {
	home := bobhome.Home() // the temp home set in TestMain

	dir, err := ProfileDir("ac_hector")
	if err != nil {
		t.Fatalf("ProfileDir: %v", err)
	}
	want := filepath.Join(home, "credentials", "browser-profiles", "ac_hector")
	if dir != want {
		t.Fatalf("ProfileDir = %q, want %q (under the credentials vault root)", dir, want)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat profile dir: %v", err)
	}
	if !fi.IsDir() {
		t.Fatal("profile path is not a directory")
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("profile dir perm = %o, want 0700", perm)
	}
	// Idempotent: second call returns the same path, no error.
	if dir2, err := ProfileDir("ac_hector"); err != nil || dir2 != dir {
		t.Fatalf("second ProfileDir = %q/%v, want %q/nil", dir2, err, dir)
	}
	// A sanitized identity can't escape the profiles root.
	esc, err := ProfileDir("../../escape")
	if err != nil {
		t.Fatalf("ProfileDir(traversal): %v", err)
	}
	root := filepath.Join(home, "credentials", "browser-profiles")
	if !strings.HasPrefix(esc, root+string(os.PathSeparator)) {
		t.Fatalf("traversal identity escaped the root: %q not under %q", esc, root)
	}
	// Empty / pure-dot identity → error, no dir.
	if _, err := ProfileDir("  "); err == nil {
		t.Fatal("ProfileDir(\"  \") must error (empty after sanitize)")
	}
}
