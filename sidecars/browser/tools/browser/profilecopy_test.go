package browser

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCopyProfileDir pins the worker-copy seed: login-bearing state copies; singleton
// locks + cache dirs do not; a missing master yields an empty (not failed) copy.
func TestCopyProfileDir(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "copy")

	// A realistic-ish profile: login state under Default/, a cache dir, a singleton lock.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "Default", "Local Storage"), 0o700))
	must(os.MkdirAll(filepath.Join(src, "Default", "Cache"), 0o700)) // skipped
	must(os.MkdirAll(filepath.Join(src, "GPUCache"), 0o700))         // skipped
	must(os.WriteFile(filepath.Join(src, "Default", "Cookies"), []byte("cookie-db"), 0o600))
	must(os.WriteFile(filepath.Join(src, "Default", "Local Storage", "leveldb.ldb"), []byte("ls"), 0o600))
	must(os.WriteFile(filepath.Join(src, "Local State"), []byte("state"), 0o600))
	must(os.WriteFile(filepath.Join(src, "Default", "Cache", "data_0"), []byte("cache"), 0o600))
	must(os.WriteFile(filepath.Join(src, "SingletonLock"), []byte("lock"), 0o600))

	if err := copyProfileDir(src, dst); err != nil {
		t.Fatalf("copyProfileDir: %v", err)
	}

	// login state present
	for _, rel := range []string{"Default/Cookies", "Default/Local Storage/leveldb.ldb", "Local State"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("expected %s copied: %v", rel, err)
		}
	}
	// cache + lock absent
	for _, rel := range []string{"Default/Cache", "Default/Cache/data_0", "GPUCache", "SingletonLock"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); !os.IsNotExist(err) {
			t.Errorf("expected %s NOT copied, err=%v", rel, err)
		}
	}
	// content integrity
	if b, _ := os.ReadFile(filepath.Join(dst, "Default", "Cookies")); string(b) != "cookie-db" {
		t.Errorf("cookie content mismatch: %q", b)
	}

	// missing master → empty copy, no error
	empty := filepath.Join(t.TempDir(), "empty")
	if err := copyProfileDir(filepath.Join(src, "does-not-exist"), empty); err != nil {
		t.Fatalf("missing master must not error: %v", err)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("missing master must yield empty copy, got %d entries", len(entries))
	}
}

// TestWriteProfileBack pins the 保存登陆 path: a human-logged-in copy replaces the master
// (new cookies + localStorage), cache does NOT reach the master, the swap is atomic (no
// .wb-tmp/.wb-old left), and a copy reseeded afterward carries the new login.
func TestWriteProfileBack(t *testing.T) {
	w := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	master := filepath.Join(t.TempDir(), "master")
	copyDir := filepath.Join(t.TempDir(), "copy")

	w(filepath.Join(master, "Default", "Cookies"), "old") // base master
	if err := copyProfileDir(master, copyDir); err != nil {
		t.Fatal(err)
	}
	w(filepath.Join(copyDir, "Default", "Cookies"), "new-login") // human logged in
	w(filepath.Join(copyDir, "Default", "Local Storage", "ls.ldb"), "token")
	w(filepath.Join(copyDir, "Default", "Cache", "blob"), "junk") // cache must NOT travel

	n, err := writeProfileBack(copyDir, master)
	if err != nil || n <= 0 {
		t.Fatalf("writeProfileBack: n=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(master, "Default", "Cookies")); string(b) != "new-login" {
		t.Errorf("master cookie not updated: %q", b)
	}
	if _, err := os.Stat(filepath.Join(master, "Default", "Local Storage", "ls.ldb")); err != nil {
		t.Errorf("localStorage not written back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(master, "Default", "Cache", "blob")); !os.IsNotExist(err) {
		t.Errorf("cache must NOT reach master, err=%v", err)
	}
	stage, base := writebackStaging(master), filepath.Base(master)
	for _, sfx := range []string{".tmp", ".old"} {
		if _, err := os.Stat(filepath.Join(stage, base+sfx)); !os.IsNotExist(err) {
			t.Errorf("leftover %s%s after atomic swap", base, sfx)
		}
	}
	// a fresh copy now carries the new login
	reseed := filepath.Join(t.TempDir(), "reseed")
	if err := copyProfileDir(master, reseed); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(reseed, "Default", "Cookies")); string(b) != "new-login" {
		t.Errorf("reseeded copy lacks the new login: %q", b)
	}
}
