package browserd

// Chromium-free handler tests: transport-level validation (bad body / bad
// method / unknown path) and the endpoints that never spawn an instance
// (/healthz, /purge on a cold scope, /snapshot's image-dir precondition).
// Anything that would AcquireRouted a live chromium is covered by the
// client↔fake-browserd tests in tools/browserremote instead.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/tools/browser"
)

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	cfg := config.BrowserConfig{UserDataDirRoot: t.TempDir()}
	pool := browser.NewPool(cfg, config.FilesystemConfig{SandboxRoot: t.TempDir()}, false)
	t.Cleanup(pool.CloseAll)
	return NewServer(pool, cfg).Handler()
}

func doReq(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, Response) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s %s: non-JSON response %q: %v", method, path, rec.Body.String(), err)
	}
	return rec, resp
}

func TestInvalidBodyIs400(t *testing.T) {
	h := newTestHandler(t)
	for _, path := range []string{"/navigate", "/click", "/snapshot", "/tab", "/purge"} {
		rec, resp := doReq(t, h, http.MethodPost, path, "{not json")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s with garbage body: status = %d, want 400", path, rec.Code)
		}
		if resp.OK || resp.Error == "" {
			t.Errorf("POST %s with garbage body: envelope = %+v, want ok=false with error", path, resp)
		}
	}
}

func TestNonPostIs405(t *testing.T) {
	h := newTestHandler(t)
	rec, resp := doReq(t, h, http.MethodGet, "/click", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /click: status = %d, want 405", rec.Code)
	}
	if resp.OK {
		t.Fatalf("GET /click: ok = true, want false")
	}
}

func TestHealthz(t *testing.T) {
	h := newTestHandler(t)
	rec, resp := doReq(t, h, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("GET /healthz: status=%d ok=%v, want 200 ok=true", rec.Code, resp.OK)
	}
	// healthz is GET-only.
	rec, _ = doReq(t, h, http.MethodPost, "/healthz", "{}")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz: status = %d, want 405", rec.Code)
	}
}

func TestPurge(t *testing.T) {
	h := newTestHandler(t)
	// Cold scope — PurgeSession is idempotent / no-op safe; no chromium.
	rec, resp := doReq(t, h, http.MethodPost, "/purge", `{"scope":"tg:dm:42"}`)
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("POST /purge: status=%d resp=%+v, want 200 ok=true", rec.Code, resp)
	}
	// Empty scope is an action-level error (200 + ok:false), not a crash.
	rec, resp = doReq(t, h, http.MethodPost, "/purge", `{"scope":""}`)
	if rec.Code != http.StatusOK || resp.OK || resp.Error == "" {
		t.Fatalf("POST /purge empty scope: status=%d resp=%+v, want 200 ok=false with error", rec.Code, resp)
	}
}

// TestLive covers POST /live — the no-live-browser guard's remote face
// (docs/remote-browser-handoff.md P1b). A cold scope has no live chromium →
// live:false (a normal 200 ok:true with a negative payload, NOT an error). The
// endpoint is read-only and never spawns an instance.
func TestLive(t *testing.T) {
	h := newTestHandler(t)
	rec, resp := doReq(t, h, http.MethodPost, "/live", `{"scope":"tg:dm:42"}`)
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("POST /live cold scope: status=%d resp=%+v, want 200 ok=true", rec.Code, resp)
	}
	var data LiveData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("POST /live: decode data %q: %v", string(resp.Data), err)
	}
	if data.Live {
		t.Fatalf("POST /live cold scope: live=true, want false (no chromium)")
	}

	// Empty scope is "nothing to take over" → live:false, not an error.
	rec, resp = doReq(t, h, http.MethodPost, "/live", `{"scope":""}`)
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("POST /live empty scope: status=%d resp=%+v, want 200 ok=true", rec.Code, resp)
	}
	_ = json.Unmarshal(resp.Data, &data)
	if data.Live {
		t.Fatalf("POST /live empty scope: live=true, want false")
	}

	// GET is rejected (POST-only handler).
	rec, _ = doReq(t, h, http.MethodGet, "/live", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /live: status = %d, want 405", rec.Code)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/no-such", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /no-such: status = %d, want 404", rec.Code)
	}
}

// TestAPIKeyAuth covers the optional control-plane bearer key (A1): with a key
// configured, a missing or wrong token is 401 and the right token passes
// through to the handler; without a key the plane stays open. All chromium-free
// (cold /purge is a no-op 200 ok).
func TestAPIKeyAuth(t *testing.T) {
	t.Setenv("BROWSERD_API_KEY", "") // isolate from any ambient env
	cfg := config.BrowserConfig{UserDataDirRoot: t.TempDir(), APIKey: "s3cret"}
	pool := browser.NewPool(cfg, config.FilesystemConfig{SandboxRoot: t.TempDir()}, false)
	t.Cleanup(pool.CloseAll)
	srv := NewServer(pool, cfg)
	if !srv.APIKeyConfigured() {
		t.Fatal("APIKeyConfigured() = false, want true")
	}
	h := srv.Handler()

	call := func(authHeader string) (int, Response) {
		req := httptest.NewRequest(http.MethodPost, "/purge", strings.NewReader(`{"scope":"tg:dm:42"}`))
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var resp Response
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}

	// Missing token → 401.
	if code, resp := call(""); code != http.StatusUnauthorized || resp.OK {
		t.Errorf("no token: status=%d ok=%v, want 401 ok=false", code, resp.OK)
	}
	// Wrong token → 401.
	if code, resp := call("Bearer nope"); code != http.StatusUnauthorized || resp.OK {
		t.Errorf("wrong token: status=%d ok=%v, want 401 ok=false", code, resp.OK)
	}
	// Malformed (no "Bearer " prefix) → 401.
	if code, _ := call("s3cret"); code != http.StatusUnauthorized {
		t.Errorf("bare key (no Bearer prefix): status=%d, want 401", code)
	}
	// Correct token → passes through to the handler (cold purge = 200 ok).
	if code, resp := call("Bearer s3cret"); code != http.StatusOK || !resp.OK {
		t.Errorf("correct token: status=%d resp=%+v, want 200 ok=true", code, resp)
	}

	// No key configured → open plane, no header needed (the default newTestHandler).
	open := newTestHandler(t)
	rec, resp := doReq(t, open, http.MethodPost, "/purge", `{"scope":"tg:dm:42"}`)
	if rec.Code != http.StatusOK || !resp.OK {
		t.Errorf("open plane no header: status=%d resp=%+v, want 200 ok=true", rec.Code, resp)
	}
}

// TestAPIKeyEnvOverridesConfig confirms $BROWSERD_API_KEY wins over the config
// field (BrowserdAPIKeyEff precedence).
func TestAPIKeyEnvOverridesConfig(t *testing.T) {
	t.Setenv("BROWSERD_API_KEY", "from-env")
	cfg := config.BrowserConfig{UserDataDirRoot: t.TempDir(), APIKey: "from-config"}
	pool := browser.NewPool(cfg, config.FilesystemConfig{SandboxRoot: t.TempDir()}, false)
	t.Cleanup(pool.CloseAll)
	h := NewServer(pool, cfg).Handler()

	req := httptest.NewRequest(http.MethodPost, "/purge", strings.NewReader(`{"scope":"s"}`))
	req.Header.Set("Authorization", "Bearer from-config") // the LOSER
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("config key while env set: status=%d, want 401 (env wins)", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/purge", strings.NewReader(`{"scope":"s"}`))
	req.Header.Set("Authorization", "Bearer from-env")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("env key: status=%d, want 200", rec.Code)
	}
}

func TestProfilesSnapshot(t *testing.T) {
	cfg := config.BrowserConfig{UserDataDirRoot: t.TempDir()}
	pool := browser.NewPool(cfg, config.FilesystemConfig{SandboxRoot: t.TempDir()}, false)
	t.Cleanup(pool.CloseAll)
	h := NewServer(pool, cfg).Handler()

	// Cold pool → ok with an empty list (read-only, never spawns chromium).
	rec, resp := doReq(t, h, http.MethodPost, "/profiles", "{}")
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("POST /profiles cold: status=%d envelope=%+v, want 200 ok", rec.Code, resp)
	}
	var data ProfilesData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(data.Profiles) != 0 {
		t.Fatalf("cold profiles = %+v, want empty", data.Profiles)
	}

	// Seed one checkout (+pin) straight on the custodian — chromium-free.
	if !pool.Custodian().Checkout("browser_alice", "tg:dm:42") {
		t.Fatal("seed checkout failed")
	}
	if !pool.Custodian().Pin("browser_alice", true) {
		t.Fatal("seed pin failed")
	}
	_, resp = doReq(t, h, http.MethodPost, "/profiles", "{}")
	if !resp.OK {
		t.Fatalf("POST /profiles: envelope=%+v, want ok", resp)
	}
	data = ProfilesData{}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(data.Profiles) != 1 {
		t.Fatalf("profiles = %+v, want exactly the seeded checkout", data.Profiles)
	}
	p := data.Profiles[0]
	if p.Key != "browser_alice" || p.Owner != "tg:dm:42" || !p.Pinned {
		t.Errorf("profile = %+v, want {browser_alice tg:dm:42 pinned}", p)
	}
}
