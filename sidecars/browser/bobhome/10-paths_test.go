package bobhome

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestEnsureDirs_RestrictedModes pins the fix: EnsureDirs must create (and
// correct an already-loose) credentials/ + sandbox/ at 0o700, not the default
// 0o755 — otherwise the credentials subsystem's later 0o700 MkdirAll is a no-op
// against the pre-existing 0o755 dir and the secret-holding dir stays listable
// by other uids.
func TestEnsureDirs_RestrictedModes(t *testing.T) {
	home := t.TempDir()

	// Pre-create credentials/ at the loose 0o755 to exercise the in-place
	// Chmod correction (MkdirAll alone would leave it at 0o755).
	if err := os.MkdirAll(filepath.Join(home, "credentials"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Point Home() at our temp dir. Reset the sync.Once so the override wins.
	prevOverride := homeOverride
	homeOverride = home
	initOnce = sync.Once{}
	cachedHome, homeErr = "", nil
	t.Cleanup(func() {
		homeOverride = prevOverride
		initOnce = sync.Once{}
		cachedHome, homeErr = "", nil
	})

	if err := EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{"credentials", "sandbox"} {
		fi, err := os.Stat(filepath.Join(home, d))
		if err != nil {
			t.Fatalf("stat %s: %v", d, err)
		}
		if got := fi.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s mode = %o, want 0700 (secret dir must not be group/other-listable)", d, got)
		}
	}

	// A non-restricted standard dir stays at the default 0o755.
	fi, err := os.Stat(filepath.Join(home, "logs"))
	if err != nil {
		t.Fatalf("stat logs: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Fatalf("logs mode = %o, want 0755", got)
	}
}

// TestInit_ExportsBobHomeEnv pins the fix: Init() must export the resolved
// home as $BOB_HOME so os.ExpandEnv consumers (tools/policy's "$BOB_HOME/logs"
// denies) agree on it even when neither --home's caller nor the operator set
// the env var. Without the export, "$BOB_HOME/logs" expands to "/logs" and the
// logs/memory/sqlite-store/permissions denies silently no-op.
func TestInit_ExportsBobHomeEnv(t *testing.T) {
	home := t.TempDir()

	prevOverride := homeOverride
	prevEnv, hadEnv := os.LookupEnv("BOB_HOME")
	os.Unsetenv("BOB_HOME")
	homeOverride = home
	initOnce = sync.Once{}
	cachedHome, homeErr = "", nil
	t.Cleanup(func() {
		homeOverride = prevOverride
		initOnce = sync.Once{}
		cachedHome, homeErr = "", nil
		if hadEnv {
			os.Setenv("BOB_HOME", prevEnv)
		} else {
			os.Unsetenv("BOB_HOME")
		}
	})

	if err := Init(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("BOB_HOME"); got != home {
		t.Fatalf("BOB_HOME = %q, want %q (Init must export the resolved home)", got, home)
	}
	// The whole point: an ExpandEnv of a "$BOB_HOME/..." rule now resolves under
	// the real home rather than "/...".
	if got := os.ExpandEnv("$BOB_HOME/logs"); got != filepath.Join(home, "logs") {
		t.Fatalf("ExpandEnv($BOB_HOME/logs) = %q, want under resolved home", got)
	}
}

// TestDisplayCollapse_Boundary verifies that ~-collapsing only fires on a
// real path boundary (h == ud or h under ud/), not on a bare string prefix
// that would mis-fold sibling directories.
func TestDisplayCollapse_Boundary(t *testing.T) {
	sep := string(os.PathSeparator)
	home := sep + "home" + sep + "bob"

	cases := []struct {
		name string
		h    string
		ud   string
		want string
	}{
		{"exact home", home, home, "~"},
		{"subdir", home + sep + ".bob", home, "~" + sep + ".bob"},
		{"deep subdir", home + sep + ".bob" + sep + "logs", home, "~" + sep + ".bob" + sep + "logs"},
		// The bug: sibling dir sharing a string prefix must NOT collapse.
		{"sibling prefix not folded", sep + "home" + sep + "bobby", home, sep + "home" + sep + "bobby"},
		{"unrelated path", sep + "etc" + sep + "passwd", home, sep + "etc" + sep + "passwd"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayCollapse(tc.h, tc.ud); got != tc.want {
				t.Fatalf("displayCollapse(%q, %q) = %q, want %q", tc.h, tc.ud, got, tc.want)
			}
		})
	}
}
