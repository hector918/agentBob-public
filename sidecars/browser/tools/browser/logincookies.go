package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

// Login cookie sidecar (保存登陆, the no-close path).
//
// The disk write-back (writeProfileBack) can only ever carry cookies chromium has FLUSHED to
// its on-disk Cookies SQLite DB — which requires a graceful close, AND structurally EXCLUDES
// session cookies (no expiry → chromium never persists them). So a copy a human logged into
// could not be saved without (a) closing the live browser the worker still needs and (b)
// still losing any session-cookie login.
//
// This sidecar reads the FULL cookie set — session cookies included — straight from the LIVE
// browser's memory via CDP (storage.getCookies), with NO close, and persists it next to the
// member master. A re-seeded copy restores it via storage.setCookies. Cookies are the bulk of
// login state; the on-disk channel (writeProfileBack on natural close) still carries the rest
// (localStorage / IndexedDB / Service Worker).
//
// The internal control plane is the only caller — the agent-facing browser_cdp tool blocks
// Network.getAllCookies (actions.go) as an exfil vector; that gate is unaffected.

// loginCookiesPath is the sidecar file for a member master dir: a DOT-PREFIXED sibling in the
// vault root so ValidProfileVaultKey rejects it (never listed/exported as a profile, same
// trick .wb-staging / .import-* use) while sitting on the same filesystem as the master.
func loginCookiesPath(masterDir string) string {
	return filepath.Join(filepath.Dir(masterDir), "."+filepath.Base(masterDir)+".cookies.json")
}

// LoginCookiesArchiveName is the reserved tar-entry name profile export/import use to carry the
// sidecar (which lives OUTSIDE the profile dir) across machines, so a cross-machine restore
// keeps the session-cookie login too. Chromium never creates a file by this name, so it can't
// collide with real profile contents.
const LoginCookiesArchiveName = "__bob_login_cookies__.json"

// writeLoginCookiesFile installs raw sidecar bytes for a master atomically (temp+rename on the
// same filesystem → a reader sees the whole old or whole new file, never a torn one).
func writeLoginCookiesFile(masterDir string, data []byte) error {
	path := loginCookiesPath(masterDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("browser login cookies: mkdir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("browser login cookies: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("browser login cookies: install: %w", err)
	}
	return nil
}

// ReadLoginCookies returns a master's raw sidecar bytes, or (nil, nil) if none exists — the
// profile-export side bundles them into the archive under LoginCookiesArchiveName.
func ReadLoginCookies(masterDir string) ([]byte, error) {
	data, err := os.ReadFile(loginCookiesPath(masterDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// WriteLoginCookies installs raw sidecar bytes for a master — the profile-import side, restoring
// a bundled sidecar so a cross-machine import is not logged out of session-cookie sites.
func WriteLoginCookies(masterDir string, data []byte) error {
	return writeLoginCookiesFile(masterDir, data)
}

// captureLoginCookies reads every cookie (incl session cookies) from the LIVE browser at ctx
// and writes them to the master's sidecar (atomic temp+rename). No close — safe to call while
// the instance keeps running. Returns the cookie count written.
func captureLoginCookies(ctx context.Context, masterDir string) (int, error) {
	var cookies []*network.Cookie
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		cookies, e = storage.GetCookies().Do(ctx)
		return e
	})); err != nil {
		return 0, fmt.Errorf("browser login cookies: get: %w", err)
	}
	data, err := json.Marshal(cookies)
	if err != nil {
		return 0, fmt.Errorf("browser login cookies: marshal: %w", err)
	}
	if err := writeLoginCookiesFile(masterDir, data); err != nil {
		return 0, err
	}
	return len(cookies), nil
}

// injectLoginCookies restores the master's sidecar cookies into the LIVE browser at ctx (a
// freshly-seeded copy, after launch, before the first navigate). Missing sidecar is NOT an
// error (a never-saved master → the copy is simply logged out → login wall → handoff).
// Returns the cookie count injected.
func injectLoginCookies(ctx context.Context, masterDir string) (int, error) {
	data, err := os.ReadFile(loginCookiesPath(masterDir))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("browser login cookies: read: %w", err)
	}
	// Decode cookie-by-cookie (not into one []*network.Cookie): network.Cookie's enum fields
	// reject unknown values on Unmarshal, so a single future/unknown enum from a chromium
	// upgrade would otherwise fail the WHOLE batch → the copy comes up logged out. Skip the
	// odd undecodable cookie instead of losing every login.
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return 0, fmt.Errorf("browser login cookies: unmarshal: %w", err)
	}
	params := make([]*network.CookieParam, 0, len(raws))
	skipped := 0
	for _, raw := range raws {
		var c network.Cookie
		if err := json.Unmarshal(raw, &c); err != nil {
			skipped++
			continue
		}
		if p := cookieToParam(&c); p != nil {
			params = append(params, p)
		}
	}
	if skipped > 0 {
		slog.Warn("browser login cookies: skipped undecodable cookies on inject", "skipped", skipped, "kept", len(params))
	}
	if len(params) == 0 {
		return 0, nil
	}
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return storage.SetCookies(params).Do(ctx)
	})); err != nil {
		return 0, fmt.Errorf("browser login cookies: set: %w", err)
	}
	return len(params), nil
}

// RemoveLoginCookies drops a master's login-cookie sidecar. Called when the master dir is
// REPLACED out-of-band (profile import): the new dir carries its own cookies on disk, so a
// sidecar left over from the prior profile would inject mismatched cookies on the next seed.
// Missing sidecar is fine (no-op).
func RemoveLoginCookies(masterDir string) {
	if err := os.Remove(loginCookiesPath(masterDir)); err != nil && !os.IsNotExist(err) {
		slog.Warn("browser login cookies: remove sidecar failed", "master", masterDir, "err", err)
	}
}

// cookieToParam converts a read-back network.Cookie into the set-shape CookieParam. A SESSION
// cookie (no expiry) is restored as a session cookie by leaving Expires nil ("session cookie
// if not set" — the field that lets the round-trip carry what disk persistence cannot); a
// persistent cookie carries its absolute expiry forward. Returns nil for a cookie that can't
// be addressed (no domain → no way to scope the set).
func cookieToParam(c *network.Cookie) *network.CookieParam {
	host := strings.TrimPrefix(c.Domain, ".")
	if host == "" {
		return nil
	}
	p := &network.CookieParam{
		Name:         c.Name,
		Value:        c.Value,
		Path:         c.Path,
		Secure:       c.Secure,
		HTTPOnly:     c.HTTPOnly,
		SameSite:     c.SameSite,
		Priority:     c.Priority,
		SourceScheme: c.SourceScheme,
		SourcePort:   c.SourcePort,
		PartitionKey: c.PartitionKey,
	}
	// A leading-dot Domain is a DOMAIN cookie (valid across subdomains) → carry Domain forward.
	// No leading dot is a HOST-ONLY cookie — and __Host-/__Secure--prefixed cookies REQUIRE no
	// Domain attribute (chromium rejects a set that carries one). So restore those via a URL
	// instead of Domain, which makes chromium recreate them host-only. https for Secure cookies
	// (a __Host- cookie is always Secure); http otherwise.
	if strings.HasPrefix(c.Domain, ".") {
		p.Domain = c.Domain
	} else {
		scheme := "http"
		if c.Secure {
			scheme = "https"
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		p.URL = scheme + "://" + host + path
	}
	if !c.Session && c.Expires > 0 {
		t := cdp.TimeSinceEpoch(time.Unix(0, int64(c.Expires*float64(time.Second))))
		p.Expires = &t
	}
	return p
}
