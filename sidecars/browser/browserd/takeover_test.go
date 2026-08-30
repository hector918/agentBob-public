package browserd

// Chromium-free takeover-face tests (docs/browserd.md §4): the takeover face
// authenticates the SAME control-plane api key as every other endpoint (a
// missing / wrong Bearer is 401; the right Bearer passes through). A scope with
// no live browser then answers 503 (screencast) / 502 (input dispatch) ("no
// live session") — distinct from 401 — which is how these tests prove auth
// passed without chromium. browserd knows nothing about user business: there is
// no per-takeover ticket (retired) — per-takeover authorization is bob's job.

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/tools/browser"
)

// testAPIKey is the shared secret the takeover-face fixtures configure. bob
// reaches browserd's takeover face with this same control-plane key.
const testAPIKey = "s3cret"

// newTakeoverFixture builds a server whose control plane + takeover face both
// require testAPIKey, and returns its takeover handler.
func newTakeoverFixture(t *testing.T) http.Handler {
	t.Helper()
	t.Setenv("BROWSERD_API_KEY", "") // isolate from ambient env; cfg.APIKey drives it
	cfg := config.BrowserConfig{
		UserDataDirRoot: t.TempDir(),
		APIKey:          testAPIKey,
	}
	pool := browser.NewPool(cfg, config.FilesystemConfig{SandboxRoot: t.TempDir()}, false)
	t.Cleanup(pool.CloseAll)
	s := NewServer(pool, cfg)
	return s.TakeoverHandler()
}

// authedTakeoverReq builds a takeover-face request carrying the api-key Bearer.
func authedTakeoverReq(method, path string, body string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+testAPIKey)
	return r
}

// TestTakeoverRequiresAPIKey covers the api-key gate on the takeover face:
// missing / wrong Bearer → 401 on every endpoint, the same gate as the control
// plane (browserd's single trust boundary is "is this bob?").
func TestTakeoverRequiresAPIKey(t *testing.T) {
	takeover := newTakeoverFixture(t)
	cases := []struct {
		name, method, path, auth string
		want                     int
	}{
		{"screencast no key", http.MethodGet, "/takeover/screencast?scope=s1", "", http.StatusUnauthorized},
		{"screencast wrong key", http.MethodGet, "/takeover/screencast?scope=s1", "Bearer nope", http.StatusUnauthorized},
		{"screencast bare key", http.MethodGet, "/takeover/screencast?scope=s1", testAPIKey, http.StatusUnauthorized},
		{"input no key", http.MethodPost, "/takeover/input?scope=s1", "", http.StatusUnauthorized},
		{"input wrong key", http.MethodPost, "/takeover/input?scope=s1", "Bearer nope", http.StatusUnauthorized},
		{"tabs no key", http.MethodPost, "/takeover/tabs?scope=s1", "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{"kind":"text","text":"x"}`))
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		takeover.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

// TestTakeoverAuthedShape covers the post-auth request shape: a valid key with a
// missing scope is 400, a valid key + scope (no live chromium) reaches the
// handler (503 screencast / 502 input), and the wrong method is 405. None of
// these is 401 — proving the key authenticated.
func TestTakeoverAuthedShape(t *testing.T) {
	takeover := newTakeoverFixture(t)

	// Missing scope → 400 (not 401: the key authenticated, the request is malformed).
	rec := httptest.NewRecorder()
	takeover.ServeHTTP(rec, authedTakeoverReq(http.MethodGet, "/takeover/screencast", ""))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("screencast no scope: status = %d, want 400", rec.Code)
	}

	// Valid key + scope, no live chromium → 503, NOT 401.
	rec = httptest.NewRecorder()
	takeover.ServeHTTP(rec, authedTakeoverReq(http.MethodGet, "/takeover/screencast?scope=tg:dm:42", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("screencast authed, no live session: status = %d, want 503", rec.Code)
	}

	// Input: auth passes (the IME "text" kind decodes), dispatch then fails on
	// the missing live session → 502 (surfaces the real dispatch error to bob's
	// trusted proxy), NOT 401.
	rec = httptest.NewRecorder()
	takeover.ServeHTTP(rec, authedTakeoverReq(http.MethodPost, "/takeover/input?scope=tg:dm:42", `{"kind":"text","text":"你好"}`))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("input authed, no live session: status = %d, want 502", rec.Code)
	}

	// Wrong method → 405 (after auth).
	rec = httptest.NewRecorder()
	takeover.ServeHTTP(rec, authedTakeoverReq(http.MethodPost, "/takeover/screencast?scope=tg:dm:42", `{}`))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("screencast wrong method: status = %d, want 405", rec.Code)
	}
}

// TestTakeoverNoCORSHeaders confirms the takeover face advertises NO cross-origin
// allowance: it is reached only by bob server-to-server (the human's browser talks
// to bob's webui, never here), so there is no browser origin to grant. Removing the
// per-takeover ticket did NOT widen this — the face stays same-origin/no-CORS.
func TestTakeoverNoCORSHeaders(t *testing.T) {
	takeover := newTakeoverFixture(t)
	// Authed request from a browser-shaped Origin still gets no Allow-Origin back.
	req := authedTakeoverReq(http.MethodGet, "/takeover/screencast?scope=s1", "")
	req.Header.Set("Origin", "https://bob.example.com")
	rec := httptest.NewRecorder()
	takeover.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want empty (no CORS on the takeover face)", got)
	}
	// No OPTIONS preflight handler.
	req = authedTakeoverReq(http.MethodOptions, "/takeover/input", "")
	req.Header.Set("Origin", "https://bob.example.com")
	rec = httptest.NewRecorder()
	takeover.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("OPTIONS /takeover/input = %d, want 405", rec.Code)
	}
}

// TestTakeoverStreamLivesAcrossConnection pins the stream-lifetime semantics
// (takeover.go file header): once a stream is established it lives as long as
// the connection — there is no mid-stream re-auth (bob proxies it and the api
// key never leaves bob, so re-checking would only sever a healthy stream and
// bounce the human back to view mode mid-takeover).
func TestTakeoverStreamLivesAcrossConnection(t *testing.T) {
	t.Setenv("BROWSERD_API_KEY", "")
	cfg := config.BrowserConfig{UserDataDirRoot: t.TempDir(), APIKey: testAPIKey}
	pool := browser.NewPool(cfg, config.FilesystemConfig{SandboxRoot: t.TempDir()}, false)
	t.Cleanup(pool.CloseAll)
	s := NewServer(pool, cfg)
	// Frame-less fake source (the chromium-free seam): the streaming loop
	// then runs on heartbeats alone.
	s.startScreencast = func(string, string, func(browser.ScreencastFrame)) (func(), <-chan struct{}, string, error) {
		return func() {}, nil, "", nil
	}
	oldHB := takeoverHeartbeatInterval
	takeoverHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { takeoverHeartbeatInterval = oldHB })

	srv := httptest.NewServer(s.TakeoverHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/takeover/screencast?scope=tg:dm:42", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect status = %d, want 200", resp.StatusCode)
	}
	// Safety valve: if heartbeats never arrive, fail via the closed body
	// instead of hanging Scan until the test-runner timeout.
	guard := time.AfterFunc(10*time.Second, func() { resp.Body.Close() })
	defer guard.Stop()

	// 8 heartbeats ≈ 160ms: the stream stays healthy across many beats (no
	// mid-stream re-auth would sever it).
	sc := bufio.NewScanner(resp.Body)
	beats := 0
	for beats < 8 {
		if !sc.Scan() {
			t.Fatalf("stream ended after %d heartbeats — an established stream must outlive the connection", beats)
		}
		if strings.HasPrefix(sc.Text(), ":keepalive") {
			beats++
		}
	}
}

// TestTakeoverStreamEndsWhenSourceDies pins the frame-source-death contract
//: when the streamed instance is closed under an open takeover
// (purge / eviction / shutdown), startScreencast's `ended` channel closes
// and the SSE response must END — not keep heartbeating a frozen last frame.
// Bob's proxy turns this stream end into its own `done`, so the human's
// EventSource reconnects and hears "no live session".
func TestTakeoverStreamEndsWhenSourceDies(t *testing.T) {
	t.Setenv("BROWSERD_API_KEY", "")
	cfg := config.BrowserConfig{UserDataDirRoot: t.TempDir(), APIKey: testAPIKey}
	pool := browser.NewPool(cfg, config.FilesystemConfig{SandboxRoot: t.TempDir()}, false)
	t.Cleanup(pool.CloseAll)
	s := NewServer(pool, cfg)
	died := make(chan struct{})
	s.startScreencast = func(string, string, func(browser.ScreencastFrame)) (func(), <-chan struct{}, string, error) {
		return func() {}, died, "", nil
	}
	oldHB := takeoverHeartbeatInterval
	takeoverHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { takeoverHeartbeatInterval = oldHB })

	srv := httptest.NewServer(s.TakeoverHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/takeover/screencast?scope=tg:dm:42", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect status = %d, want 200", resp.StatusCode)
	}
	guard := time.AfterFunc(10*time.Second, func() { resp.Body.Close() })
	defer guard.Stop()

	sc := bufio.NewScanner(resp.Body)
	// Stream is healthy first: at least one heartbeat arrives.
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), ":keepalive") {
			break
		}
	}
	// Kill the frame source; the stream must end (Scan hits EOF) rather
	// than keep delivering heartbeats forever (the 10s guard above would
	// then close the body and the loop below would still terminate, but
	// only AFTER the deadline — distinguish via guard.Stop()'s result).
	close(died)
	for sc.Scan() {
	}
	if !guard.Stop() {
		t.Fatalf("stream did not end after the frame source died — it idled until the watchdog cut it")
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

// TestTakeoverEvictsPriorViewer locks the single-viewer rule: a second screencast
// for the same scope DISPLACES the first — the first receives a "~evicted" control
// line and ends (so its frontend stops instead of auto-reconnecting and ping-ponging),
// and the second proceeds. Two people can't watch one tab at once.
func TestTakeoverEvictsPriorViewer(t *testing.T) {
	t.Setenv("BROWSERD_API_KEY", "")
	cfg := config.BrowserConfig{UserDataDirRoot: t.TempDir(), APIKey: testAPIKey}
	pool := browser.NewPool(cfg, config.FilesystemConfig{SandboxRoot: t.TempDir()}, false)
	t.Cleanup(pool.CloseAll)
	s := NewServer(pool, cfg)
	// Frame-less source with a no-op stop + never-ending channel: the stream lives
	// until evicted (or ctx/idle/heartbeat), exercising the eviction path alone.
	s.startScreencast = func(string, string, func(browser.ScreencastFrame)) (func(), <-chan struct{}, string, error) {
		return func() {}, nil, "med", nil
	}
	oldHB := takeoverHeartbeatInterval
	takeoverHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { takeoverHeartbeatInterval = oldHB })

	srv := httptest.NewServer(s.TakeoverHandler())
	defer srv.Close()
	url := srv.URL + "/takeover/screencast?scope=tg:dm:42"

	get := func() (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		return http.DefaultClient.Do(req)
	}

	// Viewer 1 connects. The Do returns after headers, which the handler writes only
	// AFTER it registers itself — so viewer 1 is registered by the time this returns.
	resp1, err := get()
	if err != nil {
		t.Fatalf("connect 1: %v", err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("conn1 status = %d, want 200", resp1.StatusCode)
	}

	// Viewer 2 connects to the SAME scope → must evict viewer 1.
	resp2, err := get()
	if err != nil {
		t.Fatalf("connect 2: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("conn2 status = %d, want 200", resp2.StatusCode)
	}

	// Viewer 1 must receive ~evicted (then its stream ends).
	guard := time.AfterFunc(10*time.Second, func() { resp1.Body.Close() })
	defer guard.Stop()
	sc := bufio.NewScanner(resp1.Body)
	sawEvicted := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), "~evicted") {
			sawEvicted = true
			break
		}
	}
	if !sawEvicted {
		t.Fatal("viewer 1 was not evicted when viewer 2 connected to the same scope")
	}
}
