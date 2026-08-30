package browserd

// Profile persistence face (docs/browserd.md P3 — 持久化 + 异机):
//
//	GET  /profiles               — list persisted vault profiles + checkout state
//	GET  /profile/{key}/export   — tar.gz of one profile dir (refused while checked out)
//	POST /profile/{key}/import   — restore one profile dir from a tar.gz body
//
// browserd's persistent volume holds the profile MASTER copies; bob keeps
// tar.gz backups (housekeeping sweep) and, in disaster recovery, pushes one
// back through /import. Control-plane only — private network, bob is the
// sole caller — so the size caps below are accident bounds, not a security
// boundary; the PATH validation, however, is load-bearing (a key or tar
// entry must never address outside the vault root).

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentbob/sidecars/browser/tools/browser"
)

// maxImportBytes caps the COMPRESSED import body. Profiles are typically tens
// of MB; 2 GiB is generous headroom that still stops an accidental endless
// stream from filling the disk through the request body.
const maxImportBytes = 2 << 30

// maxImportExtractedBytes caps the total bytes UNPACKED from an import — the
// decompression-side bound that pairs with maxImportBytes (a gzip body can
// expand far beyond its compressed size).
const maxImportExtractedBytes = 8 << 30

// profileIOLockPrefix prefixes the synthetic custodian scope export/import
// hold a profile under while reading/writing its files. Never a real
// session_scope. Each IO op mints a UNIQUE scope (prefix + random suffix) so a
// second concurrent export/import of the same key is rejected by the custodian
// (a different owner → fail-fast "busy") rather than reentering the same hold —
// the latter would let export read a half-swapped dir or let an early release
// drop the lock while another op is still mid-RemoveAll/Rename.
const profileIOLockPrefix = "browserd:profile-io#"

// newProfileIOScope mints a process-unique IO lock scope.
func newProfileIOScope() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a time-based suffix; uniqueness only needs to hold among
		// concurrent in-flight IO ops, and crypto/rand failing is near-impossible.
		return fmt.Sprintf("%s%d", profileIOLockPrefix, time.Now().UnixNano())
	}
	return profileIOLockPrefix + hex.EncodeToString(b[:])
}

// randSuffix returns a short random byte slice for naming process-local temp
// dirs uniquely (the set-aside dir on import). On the near-impossible
// crypto/rand failure it falls back to a time-based suffix.
func randSuffix() []byte {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	return b[:]
}

// vaultDir resolves key inside the vault root. Callers must have validated
// key via browser.ValidProfileVaultKey first.
func vaultDir(key string) string {
	return filepath.Join(browser.ProfileVaultRoot(), key)
}

// SweepStaleStaging removes leftover import staging dirs (".import-*") and
// set-aside-old dirs (".old-*") from the vault root. The profileImport defer
// cleans these on the live path, but an OOM/SIGKILL mid-import (mem_limit is
// the designed OOM backstop) leaves them stranded on the persistent volume —
// each up to hundreds of MB, unaddressable by export/import and skipped by the
// listing. Called once at startup; the "." prefix guarantees it can never
// touch a real profile dir (those are never dotfiles — ValidProfileVaultKey
// forbids a leading dot). Returns the count removed.
func SweepStaleStaging() (int, error) {
	root := browser.ProfileVaultRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, ".import-") && !strings.HasPrefix(name, ".old-") {
			continue
		}
		if rerr := os.RemoveAll(filepath.Join(root, name)); rerr != nil {
			slog.Warn("browserd: stale staging dir not removed", "name", name, "err", rerr)
			continue
		}
		removed++
	}
	return removed, nil
}

// lockProfileForIO checks the profile out under the synthetic IO scope and
// cross-checks the custodian snapshot, returning a release func on success
// and ok=false when the profile is busy. Two layers because the custodian is
// keyed by POOL key (ProfileName(raw identity)) while export/import address
// the VAULT dir (sanitized identity): the checkout makes the hold race-free
// for the common case (identity already filesystem-safe → pool key ==
// ProfileName(vault key)). The snapshot scan is a best-effort guard against an
// identity whose raw form sanitizes to this vault key under a DIFFERENT pool
// key — but it is only a point-in-time read, so a checkout under such an alias
// arriving AFTER the scan is not blocked. That window is purely theoretical
// (today's identities — ac_*/MemberID — are all filesystem-safe, so no alias
// pool key exists); a real fix would need the custodian keyed by vault key.
func (s *Server) lockProfileForIO(vaultKey string) (release func(), ok bool) {
	cust := s.pool.Custodian()
	poolKey := browser.ProfileName(vaultKey)
	ioScope := newProfileIOScope() // unique per op → concurrent IO ops exclude each other
	if !cust.Checkout(poolKey, ioScope) {
		return nil, false
	}
	rel := func() { cust.Release(poolKey, ioScope) }
	// Pin for the whole hold: an export/import legitimately outlives the
	// custodian's idle TTL (hundreds of MB, possibly over WAN), and an idle
	// reclaim mid-stream would free the lock and let chromium launch over the
	// dir being read/swapped. Release frees regardless of pin, so rel needs
	// no explicit unpin. Pin can only fail here on custodian shutdown (the
	// checkout above is ours and fresh) — fail closed.
	if !cust.Pin(poolKey, true) {
		rel()
		return nil, false
	}
	for _, c := range cust.Snapshot() {
		if c.Profile == poolKey && c.Owner == ioScope {
			continue // our own hold
		}
		if browser.ProfileVaultKeyFromPoolKey(c.Profile) == vaultKey {
			rel()
			return nil, false
		}
	}
	return rel, true
}

// vaultProfiles — GET /profiles — lists every persisted profile dir under the
// vault root with its live checkout state. Never spawns chromium. Entries
// whose name is not a valid vault key (e.g. an in-flight ".import-*" temp
// dir) are skipped — they are not addressable by export/import anyway.
func (s *Server) vaultProfiles(w http.ResponseWriter, r *http.Request) {
	busy := map[string]bool{}
	for _, c := range s.pool.Custodian().Snapshot() {
		if vk := browser.ProfileVaultKeyFromPoolKey(c.Profile); vk != "" {
			busy[vk] = true
		}
	}
	entries, err := os.ReadDir(browser.ProfileVaultRoot())
	if err != nil && !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, Response{Error: "read vault root: " + err.Error()})
		return
	}
	out := make([]VaultProfile, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !browser.ValidProfileVaultKey(e.Name()) {
			continue
		}
		out = append(out, VaultProfile{Key: e.Name(), CheckedOut: busy[e.Name()]})
	}
	data, merr := json.Marshal(VaultProfilesData{Profiles: out})
	if merr != nil {
		writeJSON(w, http.StatusInternalServerError, Response{Error: "marshal payload: " + merr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, Response{OK: true, Data: data})
}

// profileExport — GET /profile/{key}/export — streams the profile dir as a
// tar.gz. Refused with 409 while the profile is checked out: a live chromium
// holds its SQLite databases mid-write, so a copy taken then would be
// inconsistent (docs/browserd.md §3 — export only after idle-close). The IO
// lock also keeps it that way for the duration of the stream.
func (s *Server) profileExport(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !browser.ValidProfileVaultKey(key) {
		writeJSON(w, http.StatusBadRequest, Response{Error: "invalid profile key"})
		return
	}
	info, err := os.Stat(vaultDir(key))
	if err != nil || !info.IsDir() {
		writeJSON(w, http.StatusNotFound, Response{Error: "no such profile"})
		return
	}
	release, ok := s.lockProfileForIO(key)
	if !ok {
		writeJSON(w, http.StatusConflict, Response{Error: "profile is checked out (chromium may be live) — retry after it goes idle"})
		return
	}
	defer release()
	// Bundle the login-cookie sidecar (a sibling of the dir, holding session cookies that the
	// on-disk Cookies DB structurally can't) so a cross-machine restore keeps that login too.
	var extras []tarExtra
	if b, rerr := browser.ReadLoginCookies(vaultDir(key)); rerr != nil {
		slog.Warn("browserd: profile export could not read login-cookie sidecar (continuing without it)", "key", key, "err", rerr)
	} else if b != nil {
		extras = append(extras, tarExtra{name: browser.LoginCookiesArchiveName, data: b})
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", key+".tar.gz"))
	if err := tarGzDir(w, vaultDir(key), extras...); err != nil {
		// Status/headers already streamed, so the only honest signal left is
		// severing the connection: http.ErrAbortHandler drops it without the
		// terminating chunk, making the client's download fail ("unexpected
		// EOF") instead of completing cleanly — a plain return would let a
		// truncated archive replace the client's previous good backup.
		slog.Warn("browserd: profile export stream failed", "key", key, "err", err)
		panic(http.ErrAbortHandler)
	}
}

// profileImport — POST /profile/{key}/import — restores a profile dir from a
// tar.gz body (disaster recovery; bob keeps the backups). The archive is
// unpacked into a temp dir under the vault root first and swapped in with a
// rename only on full success, so a truncated upload can never leave a
// half-written profile behind. Same 409 rule as export: never touch a
// checked-out profile.
func (s *Server) profileImport(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !browser.ValidProfileVaultKey(key) {
		writeJSON(w, http.StatusBadRequest, Response{Error: "invalid profile key"})
		return
	}
	release, ok := s.lockProfileForIO(key)
	if !ok {
		writeJSON(w, http.StatusConflict, Response{Error: "profile is checked out (chromium may be live) — retry after it goes idle"})
		return
	}
	defer release()

	root := browser.ProfileVaultRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		writeJSON(w, http.StatusInternalServerError, Response{Error: "create vault root: " + err.Error()})
		return
	}
	// The "." prefix makes the temp dir invalid as a vault key, so a crashed
	// import can never be listed/exported as a real profile; one stranded by a
	// crash mid-import is reaped at the next startup by SweepStaleStaging.
	tmp, err := os.MkdirTemp(root, ".import-"+key+"-")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, Response{Error: "create staging dir: " + err.Error()})
		return
	}
	defer os.RemoveAll(tmp) // no-op after the success rename

	if err := untarGz(http.MaxBytesReader(w, r.Body, maxImportBytes), tmp); err != nil {
		writeJSON(w, http.StatusBadRequest, Response{Error: "unpack: " + err.Error()})
		return
	}
	// Pull the bundled login-cookie sidecar OUT of the unpacked dir before the swap (it must
	// land as a SIBLING of the master, not inside it). Restored to its sibling path after the
	// dir is installed; a (nil) absence means an old/sidecar-less archive → the stale sidecar
	// is dropped below instead.
	var sidecar []byte
	if b, rerr := os.ReadFile(filepath.Join(tmp, browser.LoginCookiesArchiveName)); rerr == nil {
		sidecar = b
		_ = os.Remove(filepath.Join(tmp, browser.LoginCookiesArchiveName))
	}
	// Swap in install-before-destroy: move the old profile aside (not delete
	// it) BEFORE renaming the freshly-unpacked dir into place, so a failed
	// install leaves the old profile recoverable. Plain RemoveAll(dst) +
	// Rename(tmp,dst) would, on a Rename error, leave the key with NO profile
	// at all (old gone, new not installed). The ".old-" prefix keeps the
	// saved-aside dir out of the vault-key namespace (same rule as ".import-").
	dst := vaultDir(key)
	var saved string // non-empty once the old profile has been moved aside
	if _, statErr := os.Stat(dst); statErr == nil {
		saved = filepath.Join(root, ".old-"+key+"-"+hex.EncodeToString(randSuffix()))
		if err := os.Rename(dst, saved); err != nil {
			writeJSON(w, http.StatusInternalServerError, Response{Error: "set aside old profile: " + err.Error()})
			return
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Install failed: put the old profile back so the key is not left empty.
		if saved != "" {
			if rerr := os.Rename(saved, dst); rerr != nil {
				slog.Error("browserd: profile import failed AND old profile could not be restored",
					"key", key, "saved", saved, "install_err", err, "restore_err", rerr)
			}
		}
		writeJSON(w, http.StatusInternalServerError, Response{Error: "install profile: " + err.Error()})
		return
	}
	if saved != "" {
		if err := os.RemoveAll(saved); err != nil {
			// New profile is already live; the stale set-aside copy is harmless
			// but should not linger silently.
			slog.Warn("browserd: stale set-aside profile not removed after import", "key", key, "saved", saved, "err", err)
		}
	}
	// Restore the bundled sidecar as a sibling of the freshly-installed master; if the archive
	// carried none (old export), drop any sidecar from the PRIOR profile so a re-seed can't
	// inject the old profile's session cookies over the imported login.
	if sidecar != nil {
		if werr := browser.WriteLoginCookies(dst, sidecar); werr != nil {
			slog.Warn("browserd: profile import could not restore login-cookie sidecar", "key", key, "err", werr)
		}
	} else {
		browser.RemoveLoginCookies(dst)
	}
	writeJSON(w, http.StatusOK, Response{OK: true})
}

// isSingletonLockFile reports whether rel (a path relative to the profile
// root) is one of chromium's singleton launch-lock files. They are skipped on
// export: in an unchecked-out profile they are stale by definition, the
// SingletonSocket is a unix socket tar can't meaningfully carry, and
// clearStaleProfileLocks would delete them on the next launch anyway.
func isSingletonLockFile(rel string) bool {
	switch rel {
	case "SingletonLock", "SingletonSocket", "SingletonCookie":
		return true
	}
	return false
}

// tarExtra is a synthetic file (not on disk under dir) added to the archive — used to carry
// the login-cookie sidecar, which lives OUTSIDE the profile dir, across machines.
type tarExtra struct {
	name string
	data []byte
}

// tarGzDir streams dir as a gzip'd tar onto w: directories, regular files and
// symlinks, with paths relative to dir. Other file types (sockets, devices)
// are skipped — chromium state that matters (SQLite, LevelDB, prefs) is plain
// files. Symlinks are stored as symlinks (chromium uses them, e.g. the
// singleton lock family — though those exact ones are skipped, see
// isSingletonLockFile). Any extras are appended as top-level regular-file entries.
func tarGzDir(w io.Writer, dir string, extras ...tarExtra) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." || isSingletonLockFile(rel) {
			return nil
		}
		mode := info.Mode()
		var link string
		if mode&os.ModeSymlink != 0 {
			l, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			link = l
		} else if !mode.IsRegular() && !mode.IsDir() {
			return nil // socket / fifo / device — skip
		}
		hdr, herr := tar.FileInfoHeader(info, link)
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(rel)
		if mode.IsDir() {
			hdr.Name += "/"
		}
		if werr := tw.WriteHeader(hdr); werr != nil {
			return werr
		}
		if mode.IsRegular() {
			f, oerr := os.Open(path)
			if oerr != nil {
				return oerr
			}
			_, cerr := io.Copy(tw, f)
			f.Close()
			if cerr != nil {
				return cerr
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, e := range extras {
		hdr := &tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(e.data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// untarGz unpacks a tar.gz stream into dst, refusing any entry that would
// land outside it: entry names must be local (no absolute paths, no ".."
// escape — filepath.IsLocal), and a symlink's target must stay local after
// joining with the link's own directory. Total extracted bytes are bounded by
// maxImportExtractedBytes. Only dirs, regular files and symlinks are created;
// anything else is skipped.
func untarGz(r io.Reader, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("not a gzip stream: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(hdr.Name)
		// A dir entry's trailing separator is fine; strip it before the
		// locality check so "sub/" doesn't fail on the empty last segment.
		name = filepath.Clean(name)
		if name == "." {
			continue // the root dir itself ("./" in tar-created archives)
		}
		if !filepath.IsLocal(name) {
			return fmt.Errorf("tar entry escapes target dir: %q", hdr.Name)
		}
		path := filepath.Join(dst, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			perm := hdr.FileInfo().Mode().Perm()
			if perm == 0 {
				perm = 0o600
			}
			f, oerr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
			if oerr != nil {
				return oerr
			}
			n, cerr := io.Copy(f, io.LimitReader(tr, maxImportExtractedBytes-total+1))
			f.Close()
			if cerr != nil {
				return cerr
			}
			total += n
			if total > maxImportExtractedBytes {
				return fmt.Errorf("archive exceeds extracted-size cap (%d bytes)", int64(maxImportExtractedBytes))
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) ||
				!filepath.IsLocal(filepath.Join(filepath.Dir(name), filepath.FromSlash(hdr.Linkname))) {
				return fmt.Errorf("tar symlink escapes target dir: %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, path); err != nil {
				return err
			}
		default:
			// hardlinks / char devices / fifos — nothing chromium needs.
		}
	}
}
