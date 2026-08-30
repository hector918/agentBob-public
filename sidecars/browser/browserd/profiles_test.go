package browserd

// P3 profile persistence face tests — all chromium-free: export/import
// round-trip, the checked-out 409 guard, key validation against traversal,
// and tar-entry escape rejection. The vault root lives under the package's
// pinned temp $BOB_HOME (TestMain), shared across tests, so every test uses
// its own unique profile keys.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"agentbob/sidecars/browser/bobhome"
	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/tools/browser"
)

// TestMain pins a temp $BOB_HOME for the whole package BEFORE anything
// resolves bobhome (which caches the home once) — same pattern as the
// tools/browser package. The profile vault root derives from it.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "browserd-test-home")
	if err != nil {
		panic(err)
	}
	bobhome.SetHome(home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

func newProfilesServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	cfg := config.BrowserConfig{UserDataDirRoot: t.TempDir()}
	pool := browser.NewPool(cfg, config.FilesystemConfig{SandboxRoot: t.TempDir()}, false)
	t.Cleanup(pool.CloseAll)
	s := NewServer(pool, cfg)
	return s, s.Handler()
}

// mkProfile materialises a fake profile dir under the vault root: a nested
// SQLite-ish file, a top-level file, an internal symlink, and the stale
// chromium singleton lock (which export must skip).
func mkProfile(t *testing.T, key string) string {
	t.Helper()
	dir := filepath.Join(browser.ProfileVaultRoot(), key)
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Default", "Cookies"), []byte("cookie-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Local State"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("Local State", filepath.Join(dir, "lnk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("host-123", filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProfileExportImportRoundTrip(t *testing.T) {
	_, h := newProfilesServer(t)
	mkProfile(t, "rt_src")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/profile/rt_src/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("export: content-type = %q", ct)
	}
	archive := rec.Body.Bytes()

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/profile/rt_dst/import", bytes.NewReader(archive)))
	if rec.Code != http.StatusOK {
		t.Fatalf("import: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	dst := filepath.Join(browser.ProfileVaultRoot(), "rt_dst")
	got, err := os.ReadFile(filepath.Join(dst, "Default", "Cookies"))
	if err != nil || string(got) != "cookie-bytes" {
		t.Fatalf("imported Cookies = %q, err = %v", got, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "lnk")); err != nil || target != "Local State" {
		t.Fatalf("imported symlink = %q, err = %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "SingletonLock")); !os.IsNotExist(err) {
		t.Fatalf("SingletonLock should be skipped on export; lstat err = %v", err)
	}
	// Import over an EXISTING profile replaces it wholesale.
	if err := os.WriteFile(filepath.Join(dst, "stale-extra"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/profile/rt_dst/import", bytes.NewReader(archive)))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-import: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(dst, "stale-extra")); !os.IsNotExist(err) {
		t.Fatalf("re-import should replace the dir; stale file lstat err = %v", err)
	}
}

// TestProfileExportImportCarriesLoginSidecar pins the cross-machine sidecar bundling: export
// folds the login-cookie sidecar (a sibling of the dir) into the archive, import restores it as
// a sibling (not inside the dir), and a sidecar-less archive drops a stale sidecar.
func TestProfileExportImportCarriesLoginSidecar(t *testing.T) {
	_, h := newProfilesServer(t)
	src := mkProfile(t, "sc_src")
	cookieJSON := []byte(`[{"name":"sid","value":"abc"}]`)
	if err := browser.WriteLoginCookies(src, cookieJSON); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/profile/sc_src/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	archive := rec.Body.Bytes()

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/profile/sc_dst/import", bytes.NewReader(archive)))
	if rec.Code != http.StatusOK {
		t.Fatalf("import: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	dst := filepath.Join(browser.ProfileVaultRoot(), "sc_dst")
	got, err := browser.ReadLoginCookies(dst)
	if err != nil || string(got) != string(cookieJSON) {
		t.Fatalf("imported sidecar = %q, err = %v", got, err)
	}
	// The reserved archive entry must NOT survive inside the profile dir.
	if _, err := os.Lstat(filepath.Join(dst, browser.LoginCookiesArchiveName)); !os.IsNotExist(err) {
		t.Fatalf("reserved archive entry leaked into the profile dir; lstat err = %v", err)
	}

	// Importing a sidecar-LESS archive over the existing profile drops the stale sidecar.
	mkProfile(t, "sc_plain")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/profile/sc_plain/export", nil))
	plain := rec.Body.Bytes()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/profile/sc_dst/import", bytes.NewReader(plain)))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-import: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got, _ := browser.ReadLoginCookies(dst); got != nil {
		t.Fatalf("sidecar-less import must drop the stale sidecar, got %q", got)
	}
}

func TestProfileExportImportBusy409(t *testing.T) {
	s, h := newProfilesServer(t)
	mkProfile(t, "busy_key")
	if !s.pool.Custodian().Checkout(browser.ProfileName("busy_key"), "scope-A") {
		t.Fatal("test checkout failed")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/profile/busy_key/export", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("export while checked out: status = %d, want 409", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/profile/busy_key/import", bytes.NewReader(nil)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("import while checked out: status = %d, want 409", rec.Code)
	}

	// Release → export works again (and the IO lock is itself released after).
	s.pool.Custodian().Release(browser.ProfileName("busy_key"), "scope-A")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/profile/busy_key/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export after release: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !s.pool.Custodian().Checkout(browser.ProfileName("busy_key"), "scope-B") {
		t.Fatal("IO lock leaked: profile still held after export finished")
	}
}

// TestProfileIOLockPinned — the IO hold must be PINNED for its whole
// duration: an export/import can legitimately outlive the custodian's idle
// TTL, and an un-pinned checkout would be freed by the reclaim sweep
// mid-stream, letting chromium launch over the dir being read/swapped.
// (Pinned-exempt-from-reclaim itself is covered by the custodian's own
// tests; here we only assert the IO lock actually uses it.)
func TestProfileIOLockPinned(t *testing.T) {
	s, _ := newProfilesServer(t)
	mkProfile(t, "pin_key")

	release, ok := s.lockProfileForIO("pin_key")
	if !ok {
		t.Fatal("lockProfileForIO failed on a free profile")
	}
	poolKey := browser.ProfileName("pin_key")
	pinned := false
	for _, c := range s.pool.Custodian().Snapshot() {
		if c.Profile == poolKey {
			pinned = c.Pinned
		}
	}
	if !pinned {
		t.Fatal("IO hold is not pinned — idle reclaim could free it mid export/import")
	}
	release()
	for _, c := range s.pool.Custodian().Snapshot() {
		if c.Profile == poolKey {
			t.Fatal("IO hold survived its release")
		}
	}
}

// TestProfileIOLockMutualExclusion pins the fix for the reentrant IO lock:
// two concurrent export/import ops on the SAME key must exclude each other.
// With a fixed lock scope they were mutually reentrant (the custodian grants
// reentry to the same owner), so a second op acquired the lock while the first
// still held it — and the first's release dropped the lock out from under the
// second. A unique per-op scope makes the second a "busy" fail-fast.
func TestProfileIOLockMutualExclusion(t *testing.T) {
	s, _ := newProfilesServer(t)
	mkProfile(t, "excl_key")

	release1, ok := s.lockProfileForIO("excl_key")
	if !ok {
		t.Fatal("first lockProfileForIO failed on a free profile")
	}
	if _, ok2 := s.lockProfileForIO("excl_key"); ok2 {
		release1()
		t.Fatal("second concurrent IO lock on the same key was granted — locks are reentrant")
	}
	// After the first releases, a fresh op may acquire.
	release1()
	release2, ok3 := s.lockProfileForIO("excl_key")
	if !ok3 {
		t.Fatal("IO lock not acquirable after the prior holder released")
	}
	release2()
}

func TestProfileKeyValidationRejectsTraversal(t *testing.T) {
	_, h := newProfilesServer(t)
	for _, raw := range []string{"%2e%2e", "..%2Fx", "a%3Ab"} { // "..", "../x", "a:b"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/profile/"+raw+"/export", nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("export key %q: status = %d, want 400", raw, rec.Code)
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/profile/"+raw+"/import", bytes.NewReader(nil)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("import key %q: status = %d, want 400", raw, rec.Code)
		}
	}
	// Missing profile (valid key, no dir) → 404, not 400.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/profile/no_such_profile/export", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("export missing profile: status = %d, want 404", rec.Code)
	}
}

// gzTar builds a tar.gz with the given entries (write fn per entry).
func gzTar(t *testing.T, write func(tw *tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarFile(t *testing.T, tw *tar.Writer, name, content string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, content); err != nil {
		t.Fatal(err)
	}
}

func TestProfileImportRejectsTarEscape(t *testing.T) {
	_, h := newProfilesServer(t)

	// Entry name escaping the target dir.
	archive := gzTar(t, func(tw *tar.Writer) { tarFile(t, tw, "../evil.txt", "x") })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/profile/esc_a/import", bytes.NewReader(archive)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("escaping entry name: status = %d, want 400", rec.Code)
	}
	if _, err := os.Lstat(filepath.Join(browser.ProfileVaultRoot(), "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("escaping entry was written; lstat err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(browser.ProfileVaultRoot(), "esc_a")); !os.IsNotExist(err) {
		t.Fatalf("failed import must not install the profile dir; lstat err = %v", err)
	}

	// Symlink whose target escapes.
	archive = gzTar(t, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "lnk", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}); err != nil {
			t.Fatal(err)
		}
	})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/profile/esc_b/import", bytes.NewReader(archive)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("escaping symlink: status = %d, want 400", rec.Code)
	}

	// Absolute symlink target.
	archive = gzTar(t, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "lnk", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
			t.Fatal(err)
		}
	})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/profile/esc_c/import", bytes.NewReader(archive)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("absolute symlink: status = %d, want 400", rec.Code)
	}

	// Sanity: a tame archive with a "./"-rooted name (GNU tar style) imports.
	archive = gzTar(t, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
			t.Fatal(err)
		}
		tarFile(t, tw, "./ok.txt", "fine")
	})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/profile/esc_ok/import", bytes.NewReader(archive)))
	if rec.Code != http.StatusOK {
		t.Fatalf("tame archive: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(browser.ProfileVaultRoot(), "esc_ok", "ok.txt")); err != nil || string(got) != "fine" {
		t.Fatalf("tame archive content = %q, err = %v", got, err)
	}
}

// TestProfileImportNoSetAsideLeak — importing OVER an existing profile must
// leave no ".old-*" set-aside dir behind on success. The set-aside is part of
// the install-before-destroy swap (move old aside → rename new in → remove
// old); a leaked ".old-*" would accumulate on the persistent vault volume.
func TestProfileImportNoSetAsideLeak(t *testing.T) {
	_, h := newProfilesServer(t)
	mkProfile(t, "swap_src")
	mkProfile(t, "swap_dst") // pre-existing dst, exercises the set-aside branch

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/profile/swap_src/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	archive := rec.Body.Bytes()

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/profile/swap_dst/import", bytes.NewReader(archive)))
	if rec.Code != http.StatusOK {
		t.Fatalf("import over existing: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The new profile is live and no ".old-*" staging dir lingers.
	got, err := os.ReadFile(filepath.Join(browser.ProfileVaultRoot(), "swap_dst", "Default", "Cookies"))
	if err != nil || string(got) != "cookie-bytes" {
		t.Fatalf("imported Cookies = %q, err = %v", got, err)
	}
	entries, err := os.ReadDir(browser.ProfileVaultRoot())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && (filepathHasPrefix(e.Name(), ".old-") || filepathHasPrefix(e.Name(), ".import-")) {
			t.Fatalf("set-aside/staging dir leaked after successful import: %q", e.Name())
		}
	}
}

// TestSweepStaleStaging — leftover ".import-*"/".old-*" dirs (a crash mid-
// import bypasses the defer cleanup) are reaped at startup, and real profile
// dirs are left untouched.
func TestSweepStaleStaging(t *testing.T) {
	newProfilesServer(t) // pins the vault root
	root := browser.ProfileVaultRoot()
	mkProfile(t, "keepme")
	for _, stale := range []string{".import-x-123", ".old-y-456"} {
		if err := os.MkdirAll(filepath.Join(root, stale, "Default"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	n, err := SweepStaleStaging()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("SweepStaleStaging removed %d, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(root, "keepme", "Default", "Cookies")); err != nil {
		t.Fatalf("real profile must survive the sweep: %v", err)
	}
	for _, stale := range []string{".import-x-123", ".old-y-456"} {
		if _, err := os.Stat(filepath.Join(root, stale)); !os.IsNotExist(err) {
			t.Fatalf("stale dir %q not removed: stat err = %v", stale, err)
		}
	}
}

func filepathHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestVaultProfilesListing(t *testing.T) {
	s, h := newProfilesServer(t)
	mkProfile(t, "list_free")
	mkProfile(t, "list_busy")
	if !s.pool.Custodian().Checkout(browser.ProfileName("list_busy"), "scope-L") {
		t.Fatal("test checkout failed")
	}
	// A crashed-import temp dir must not be listed.
	if err := os.MkdirAll(filepath.Join(browser.ProfileVaultRoot(), ".import-list_free-123"), 0o700); err != nil {
		t.Fatal(err)
	}

	rec, resp := doReq(t, h, http.MethodGet, "/profiles", "")
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("GET /profiles: status = %d, envelope = %+v", rec.Code, resp)
	}
	var data VaultProfilesData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range data.Profiles {
		got[p.Key] = p.CheckedOut
	}
	if co, ok := got["list_free"]; !ok || co {
		t.Fatalf("list_free: present=%v checked_out=%v, want present & free", ok, co)
	}
	if co, ok := got["list_busy"]; !ok || !co {
		t.Fatalf("list_busy: present=%v checked_out=%v, want present & busy", ok, co)
	}
	for k := range got {
		if !browser.ValidProfileVaultKey(k) {
			t.Fatalf("listing leaked a non-vault-key entry: %q", k)
		}
	}
}
