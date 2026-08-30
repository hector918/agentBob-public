package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/chromedp/cdproto/network"
)

// TestLoginCookiesPath pins the sidecar location: a DOT-PREFIXED sibling of the master dir, so
// the profile-vault key validator rejects it and it is never listed/exported as a profile.
func TestLoginCookiesPath(t *testing.T) {
	got := loginCookiesPath("/vault/browser_member42")
	want := filepath.Join("/vault", ".browser_member42.cookies.json")
	if got != want {
		t.Fatalf("loginCookiesPath = %q, want %q", got, want)
	}
	if base := filepath.Base(got); base[0] != '.' {
		t.Fatalf("sidecar base %q is not dot-prefixed (would be listed as a profile)", base)
	}
}

// TestCookieToParam pins the round-trip that carries what disk persistence cannot: a SESSION
// cookie (no expiry) must come back as a session cookie (Expires nil), while a persistent
// cookie carries its absolute expiry forward.
func TestCookieToParam(t *testing.T) {
	// Host-only session cookie (Domain has no leading dot): restored via URL, NOT Domain, so a
	// __Host-/__Secure- cookie (chromium rejects a Domain attribute on those) survives.
	session := &network.Cookie{Name: "auth", Value: "tok", Domain: "x.com", Path: "/", Session: true, Expires: -1, HTTPOnly: true, Secure: true}
	p := cookieToParam(session)
	if p.Expires != nil {
		t.Fatalf("session cookie must restore as session (Expires nil), got %v", p.Expires)
	}
	if p.Name != "auth" || p.Value != "tok" || !p.HTTPOnly || !p.Secure {
		t.Fatalf("session cookie fields not carried: %+v", p)
	}
	if p.Domain != "" {
		t.Fatalf("host-only cookie must NOT carry a Domain attribute, got %q", p.Domain)
	}
	if p.URL != "https://x.com/" {
		t.Fatalf("host-only Secure cookie URL = %q, want https://x.com/", p.URL)
	}

	// Domain cookie (leading dot): carry Domain forward, no URL.
	domainCk := cookieToParam(&network.Cookie{Name: "d", Value: "1", Domain: ".x.com", Path: "/", Session: true})
	if domainCk.Domain != ".x.com" || domainCk.URL != "" {
		t.Fatalf("domain cookie must keep Domain and set no URL, got domain=%q url=%q", domainCk.Domain, domainCk.URL)
	}

	// Non-secure host-only cookie → http URL.
	nonSecure := cookieToParam(&network.Cookie{Name: "n", Value: "1", Domain: "y.com", Path: "/a", Session: true})
	if nonSecure.URL != "http://y.com/a" {
		t.Fatalf("non-secure host-only URL = %q, want http://y.com/a", nonSecure.URL)
	}

	persistent := cookieToParam(&network.Cookie{Name: "remember", Value: "v", Domain: ".x.com", Path: "/", Session: false, Expires: 4102444800}) // 2100-01-01
	if persistent.Expires == nil {
		t.Fatalf("persistent cookie must carry an expiry")
	}
	if got := persistent.Expires.Time().Unix(); got != 4102444800 {
		t.Fatalf("persistent expiry = %d, want 4102444800", got)
	}

	// No domain → unaddressable → nil (skipped by inject).
	if cookieToParam(&network.Cookie{Name: "x", Value: "1", Session: true}) != nil {
		t.Fatalf("cookie with no domain must return nil")
	}
}

// TestInjectLoginCookiesMissingSidecar confirms a never-saved master is a no-op (not an error):
// the seeded copy is simply logged out → it hits the login wall → handoff.
func TestInjectLoginCookiesMissingSidecar(t *testing.T) {
	n, err := injectLoginCookies(nil, filepath.Join(t.TempDir(), "browser_never_saved"))
	if err != nil {
		t.Fatalf("missing sidecar must be a no-op, got err=%v", err)
	}
	if n != 0 {
		t.Fatalf("missing sidecar injected %d cookies, want 0", n)
	}
}

// TestLoginCookiesSidecarFormat pins the on-disk sidecar shape: capture marshals []*Cookie;
// inject's reader must round-trip it. (The chromium read/write halves are exercised on the box;
// this guards the serialization contract between the two ends.)
func TestLoginCookiesSidecarFormat(t *testing.T) {
	master := filepath.Join(t.TempDir(), "browser_m")
	// Real chromium cookies always carry these enums; a zero-value Priority/SourceScheme would
	// not round-trip (their UnmarshalJSON rejects "") — capture only ever serializes live ones.
	cookies := []*network.Cookie{{Name: "a", Value: "1", Domain: "x.com", Path: "/", Session: true, Priority: network.CookiePriorityMedium, SourceScheme: network.CookieSourceSchemeSecure}}
	data, err := json.Marshal(cookies)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(master), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loginCookiesPath(master), data, 0o600); err != nil {
		t.Fatal(err)
	}

	var back []*network.Cookie
	raw, err := os.ReadFile(loginCookiesPath(master))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("sidecar does not round-trip: %v", err)
	}
	if len(back) != 1 || back[0].Name != "a" || !back[0].Session {
		t.Fatalf("round-trip lost cookie data: %+v", back)
	}
}
