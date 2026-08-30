package webui

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"agentbob/contract"
)

// refreshInterval is how often the poller re-polls every panel's State and
// republishes the cached snapshot. Read handlers only ever read the cache; they
// never trigger a fresh poll (mirrors the skeleton webui's tick model).
const refreshInterval = 3 * time.Second

// authSweepInterval is how often expired tokens/sessions are reaped. It runs on its
// own ticker (decoupled from panel polling); the cadence is non-critical because
// lookups enforce expiry independently — Sweep is pure map hygiene.
const authSweepInterval = 30 * time.Second

// server holds the live panel registry, the atomically-published snapshot cache,
// and the process start time. Handlers read the cache; the ticker writes it.
type server struct {
	reg         *registry
	auth        *Auth
	slash       contract.SlashRegistry             // for the dock command runner (/api/run)
	takeover    func() contract.BrowserTakeover    // lazy live browser handoff (resolved at request time)
	controlHold func() contract.BrowserControlHold // lazy human control-hold gate (browser yield)
	resume      func() contract.SessionResume      // lazy session wake (takeover hand-back)
	start       time.Time
	cached      atomic.Pointer[wireState]
}

// liveTakeover resolves the optional browser-handoff backend lazily (its provider
// Starts after webui). nil → the takeover endpoints answer "unavailable".
func (s *server) liveTakeover() contract.BrowserTakeover {
	if s.takeover == nil {
		return nil
	}
	return s.takeover()
}

// controlHoldReg / resumeSession resolve the optional hold gate + session-wake lazily
// (their providers Start after webui). nil → the hold endpoint degrades / no resume.
func (s *server) controlHoldReg() contract.BrowserControlHold {
	if s.controlHold == nil {
		return nil
	}
	return s.controlHold()
}

func (s *server) resumeSession() contract.SessionResume {
	if s.resume == nil {
		return nil
	}
	return s.resume()
}

// sessionCookie is the name of the management/capability session cookie.
const sessionCookie = "bob_webui"

// cookieOf returns the session cookie value, or "" if absent.
func cookieOf(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// isAdmin reports whether the request carries a live admin session.
func (s *server) isAdmin(r *http.Request) bool { return s.auth.ValidAdmin(cookieOf(r)) }

// wirePanel is one panel as it crosses to the browser: static meta + the live
// state polled this tick. JSON keys are Go-default (capitalized) — the frontend
// reads them as-is.
type wirePanel struct {
	ID        string
	Title     string
	Tier      string                 `json:",omitempty"`
	Upstreams []string               `json:",omitempty"`
	Settings  []contract.SettingSpec `json:",omitempty"`
	State     []contract.StateField
}

// wireState is the whole /api/state payload.
type wireState struct {
	Panels    []wirePanel
	UptimeSec int64
}

// refresh re-polls every panel's State (sequentially, on the single poller) and
// republishes the cache. A panel whose State closure panics is isolated (its fields
// drop to nil) so one bad module can't take down the poller or skip later panels.
func (s *server) refresh(ctx context.Context) {
	panels := s.reg.Panels()
	wps := make([]wirePanel, 0, len(panels))
	for _, p := range panels {
		wps = append(wps, wirePanel{
			ID:        p.ID,
			Title:     p.Title,
			Tier:      p.Tier,
			Upstreams: p.Upstreams,
			Settings:  p.Settings,
			State:     pollPanel(ctx, p),
		})
	}
	s.cached.Store(&wireState{Panels: wps, UptimeSec: int64(time.Since(s.start).Seconds())})
}

// pollPanel calls a panel's State with a panic guard: a panicking panel degrades to
// nil fields instead of taking down the poller (and skipping every later panel this
// tick). There is deliberately NO per-panel timeout / goroutine: one sequential
// poller drives all panels (one bucket). A panel whose State BLOCKS on a deadlock
// (in the polled module) parks the poller, so the cache stops refreshing and the
// whole dashboard goes stale — but HTTP handlers keep serving the last-published
// cache (s.cached) and never block. Isolation is between CONCERNS (panel polling vs
// the auth sweep), not between panels; a Go call you are blocked inside cannot be
// skipped anyway (only abandoned on another goroutine — the old fan-out that leaked).
func pollPanel(ctx context.Context, p contract.Panel) (fields []contract.StateField) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("webui: panel State panicked", "panel", p.ID, "panic", r)
			fields = nil
		}
	}()
	return p.State(ctx)
}

// pollLoop is the single panel-polling worker: every interval it re-polls every panel
// and republishes the cache, until ctx is cancelled (module Stop).
func (s *server) pollLoop(ctx context.Context, interval time.Duration) {
	s.refresh(ctx) // initial fill, immediately but off the boot path
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refresh(ctx)
		}
	}
}

// authSweepLoop reaps expired tokens/sessions on its OWN ticker — a separate bucket
// from panel polling, so a stalled panel poll can never delay it. Correctness does
// NOT depend on the cadence: ValidAdmin/ValidSession enforce expiry on every lookup;
// Sweep is only map hygiene (evicting already-expired entries).
func (s *server) authSweepLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.auth.Sweep()
		}
	}
}

// routes builds the mux. Static assets + read-only state are open (AdminOnly
// panels redact); settings read/write and the dock command runner (/api/run)
// require an admin session; the /api/browser/* takeover endpoints are
// Authorize-gated per scope and answer 503 while the lazily-resolved backend
// is absent.
func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	// "/{$}" pins the index to exactly "/": the frontend has no client-side
	// routing (app.js only fetches registered endpoints), so a bare "GET /"
	// catch-all would 200+HTML every typo'd path — a mistyped /api/ URL then
	// surfaces as a confusing JSON parse error instead of a plain 404.
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /app.js", s.handleAppJS)
	// Public legal pages (no auth) — the Discord app's ToS / Privacy Policy URLs.
	mux.HandleFunc("GET /legal/terms-of-service", s.handleTOS)
	mux.HandleFunc("GET /legal/privacy-policy", s.handlePrivacy)
	mux.HandleFunc("GET /api/intro", s.handleIntro)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/token", s.handleToken)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/whoami", s.handleWhoami)
	mux.HandleFunc("POST /api/run", s.handleRun)
	mux.HandleFunc("GET /api/panel/{id}/setting/{sid}", s.handleSettingRead)
	mux.HandleFunc("POST /api/panel/{id}/setting/{sid}", s.handleSettingWrite)
	mux.HandleFunc("GET /api/panel/{id}/table/{fid}", s.handleTablePage)
	mux.HandleFunc("GET /api/panel/{id}/view/{vid}", s.handleView)
	// Live browser handoff — always registered (the backend resolves lazily, since its
	// provider Starts after webui); each handler answers 503 when it's absent.
	mux.HandleFunc("GET /api/browser/screencast", s.handleScreencast)
	mux.HandleFunc("POST /api/browser/input", s.handleBrowserInput)
	mux.HandleFunc("POST /api/browser/hold", s.handleBrowserHold)
	mux.HandleFunc("POST /api/browser/save", s.handleBrowserSave)
	mux.HandleFunc("GET /api/browser/live", s.handleBrowserLive)
	return mux
}

// handleView returns a parameterized read-only field set (Panel.View) for a drill/
// layer opened by a GraphAction.Open — e.g. an inbox's chat log. Same AdminOnly
// redaction as /api/state.
func (s *server) handleView(w http.ResponseWriter, r *http.Request) {
	p, ok := s.reg.panelByID(r.PathValue("id"))
	if !ok || p.View == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if p.AdminOnly && !s.isAdmin(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	fields, err := p.View(r.Context(), r.PathValue("vid"))
	if err != nil {
		http.Error(w, "view failed", http.StatusNotFound)
		slog.Warn("webui: panel view failed", "panel", p.ID, "view", r.PathValue("vid"), "err", err)
		return
	}
	writeJSON(w, fields)
}

// handleTablePage returns a later page of a paged table field. It enforces the
// same redaction as /api/state: an AdminOnly panel's pages require an admin
// session (a non-admin can't even see the panel).
func (s *server) handleTablePage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.reg.panelByID(r.PathValue("id"))
	if !ok || p.Page == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if p.AdminOnly && !s.isAdmin(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	limit := clampInt(r.URL.Query().Get("limit"), 50, 1, 200)
	offset := clampInt(r.URL.Query().Get("offset"), 0, 0, 1<<30)
	tp, err := p.Page(r.Context(), r.PathValue("fid"), limit, offset)
	if err != nil {
		http.Error(w, "page failed", http.StatusNotFound)
		slog.Warn("webui: table page failed", "panel", p.ID, "field", r.PathValue("fid"), "err", err)
		return
	}
	writeJSON(w, tp)
}

// clampInt parses s, falling back to def, clamped to [lo, hi].
func clampInt(s string, def, lo, hi int) int {
	n := def
	if s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			n = v
		}
	}
	if n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	return n
}

// handleState returns the cached snapshot. AdminOnly panels are redacted for a
// caller without a live admin session.
func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	st := s.cached.Load()
	if st == nil {
		st = &wireState{}
	}
	admin := s.isAdmin(r)
	out := *st
	visible := make([]wirePanel, 0, len(out.Panels))
	for _, p := range out.Panels {
		if !admin && panelAdminOnly(s.reg, p.ID) {
			continue // redacted until authenticated
		}
		visible = append(visible, p)
	}
	out.Panels = visible
	writeJSON(w, out)
}

// handleToken consumes a pasted one-time token and, on success, sets the session
// cookie. Body is the raw token string.
func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	tok, err := readBody(r, 256)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cookie, ok := s.auth.SubmitToken(strings.TrimSpace(tok))
	if !ok {
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: cookie, Path: "/",
		// Secure is OFF: the server itself is always plain HTTP (TLS lives in an
		// optional fronting tunnel). Secure:true would make the browser DROP the
		// cookie on the documented plain-HTTP LAN deployment — burning the
		// one-time token on every login with {ok:true} and a dashboard that
		// stays locked. HttpOnly+SameSite=Strict remain; the non-loopback bind
		// warning (00-module.go) names the plaintext-cookie risk.
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	writeJSON(w, map[string]bool{"ok": true})
}

// handleLogout ends the caller's session and clears the cookie.
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(cookieOf(r))
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, map[string]bool{"ok": true})
}

// handleWhoami returns the caller's coverage ("*" admin / "<scope>" / "").
func (s *server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"coverage": s.auth.Coverage(cookieOf(r))})
}

// handleSettingRead returns a panel setting's current value (admin only).
func (s *server) handleSettingRead(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	p, ok := s.reg.panelByID(r.PathValue("id"))
	if !ok || p.Read == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, err := p.Read(r.Context(), r.PathValue("sid"))
	if err != nil {
		http.Error(w, "read failed", http.StatusNotFound)
		slog.Warn("webui: setting read failed", "panel", p.ID, "setting", r.PathValue("sid"), "err", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// handleSettingWrite applies a panel setting (admin only). The body is the new
// value; the result is the uniform WriteResult shape the frontend renders.
func (s *server) handleSettingWrite(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	p, ok := s.reg.panelByID(r.PathValue("id"))
	if !ok || p.Apply == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, err := readBody(r, 256*1024)
	if err != nil {
		writeJSON(w, contract.WriteResult{Phase: "validate", Error: "failed to read request body (too large or malformed)"})
		return
	}
	writeJSON(w, p.Apply(r.Context(), r.PathValue("sid"), body))
}

func (s *server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(pageHTML))
}

func (s *server) handleAppJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(appJS)
}

// handleTOS / handlePrivacy serve the Discord app's legal pages — PUBLIC (no auth)
// so Discord and users can fetch them.
func (s *server) handleTOS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(tosHTML)
}

func (s *server) handlePrivacy(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(privacyHTML)
}

func (s *server) handleIntro(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(introMD))
}

// panelAdminOnly reports whether the live panel with this id is AdminOnly.
func panelAdminOnly(reg *registry, id string) bool {
	p, ok := reg.panelByID(id)
	return ok && p.AdminOnly
}

// readBody reads up to max bytes from the request body. It uses MaxBytesReader
// (not io.LimitReader) so an over-cap body is an error, never a silent prefix
// that could pass validation and corrupt a file on write.
func readBody(r *http.Request, max int64) (string, error) {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, max))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("webui: encode response failed", "err", err)
	}
}
