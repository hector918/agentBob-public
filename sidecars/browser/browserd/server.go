package browserd

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/tools/browser"
)

// maxBodyBytes bounds a control-plane request body. Requests are small
// (typed text tops out at a few KB); 1 MB is generous headroom that still
// stops an accidental megapayload from buffering unbounded.
const maxBodyBytes = 1 << 20

// Server is the browserd control plane: one *browser.Pool (the single owner
// of every chromium instance + profile checkout state) plus the BrowserConfig
// driving per-action timeouts. Handlers are thin shells over the existing
// actions — decode, route via Pool.AcquireRouted, call, envelope.
type Server struct {
	pool *browser.Pool
	cfg  config.BrowserConfig
	// apiKey is the OPTIONAL shared secret (A1). Empty = the control plane AND
	// takeover face are open (the legacy behaviour, guarded only by network
	// isolation); non-empty = every request to either face must carry
	// `Authorization: Bearer <apiKey>`. Resolved once at construction from
	// config.BrowserConfig.BrowserdAPIKeyEff ($BROWSERD_API_KEY > api_key).
	// browserd authenticates BOB with this key and knows nothing about user
	// business — per-takeover authorization is bob's job (docs/browserd.md §4).
	apiKey string
	// startScreencast is takeoverScreencast's frame-source seam — production
	// wires pool.StartScreencastForScope (NewServer); tests swap in a fake so
	// the SSE streaming loop is exercisable without chromium.
	startScreencast func(scope string, quality string, cb func(browser.ScreencastFrame)) (stop func(), ended <-chan struct{}, effTier string, err error)

	// lastInput tracks the last human input time per takeover scope so an ABANDONED
	// takeover (tab left open, human gone) closes itself: an open SSE keeps the copy pinned
	// (exempt from idle-reclaim), so without this a walked-away takeover leaks the copy
	// forever. The screencast loop closes the stream after takeoverInputIdleTTL of no input.
	inputMu   sync.Mutex
	lastInput map[string]time.Time

	// takeoverConns enforces ONE live takeover stream per scope. A new screencast for a
	// scope that already has one EVICTS the prior (closes its evict chan → it emits
	// ~evicted and returns), then waits on the prior's done chan so the prior has fully
	// released the pool before this one acquires (so this stream comes in as the FIRST
	// on the tab and applies its own tier). Guarded by takeoverMu.
	takeoverMu    sync.Mutex
	takeoverConns map[string]*takeoverConn
}

// takeoverConn is one live takeover SSE for a scope. evict is closed by a SUCCESSOR
// to displace this stream (last-viewer-wins); done is closed when this stream's
// handler has returned and released the pool, so the successor can acquire as first.
type takeoverConn struct {
	evict chan struct{}
	done  chan struct{}
}

// takeoverInputIdleTTL closes a takeover stream after this long with no mouse/keyboard
// input — an abandoned takeover (watching counts as idle). The controlled-close fallback
// then writes the copy back to the master.
const takeoverInputIdleTTL = 5 * time.Minute

// touchInput records human input (or stream start) for scope's takeover idle clock.
func (s *Server) touchInput(scope string) {
	s.inputMu.Lock()
	if s.lastInput == nil {
		s.lastInput = map[string]time.Time{}
	}
	s.lastInput[scope] = time.Now()
	s.inputMu.Unlock()
}

// inputIdle returns how long scope's takeover has had no input.
func (s *Server) inputIdle(scope string) time.Duration {
	s.inputMu.Lock()
	t, ok := s.lastInput[scope]
	s.inputMu.Unlock()
	if !ok {
		return 0
	}
	return time.Since(t)
}

// NewServer wraps pool + cfg. The pool is constructed WITHOUT a profile gate
// — authorization is bob's job; browserd only executes pre-gated routes. The
// takeover face (docs/browserd.md §4) authenticates the same control-plane api
// key as every other endpoint; per-takeover authorization stays in bob.
func NewServer(pool *browser.Pool, cfg config.BrowserConfig) *Server {
	return &Server{
		pool:            pool,
		cfg:             cfg,
		apiKey:          cfg.BrowserdAPIKeyEff(),
		startScreencast: pool.StartScreencastForScope,
		takeoverConns:   map[string]*takeoverConn{},
	}
}

// APIKeyConfigured reports whether the control plane requires a bearer key —
// cmd/browserd uses it to emit the no-auth startup WARN when false.
func (s *Server) APIKeyConfigured() bool { return s.apiKey != "" }

// Handler returns the control-plane mux. Mount behind a private-network
// listener only — the control plane carries NO transport security (§6 of
// docs/browserd.md). When tools.browser.api_key / $BROWSERD_API_KEY is set,
// requireAPIKey gates every endpoint on `Authorization: Bearer <key>`; when
// unset, the plane is open (network isolation is the only guard, hence the
// startup WARN cmd/browserd emits).
func (s *Server) Handler() http.Handler {
	return s.requireAPIKey(s.controlMux())
}

// requireAPIKey wraps next so that, when the api key is configured, every
// request must present `Authorization: Bearer <key>` (constant-time compared) —
// missing/malformed/wrong → 401. When no key is configured it is a pass-through
// (network isolation is the only guard). Both the control plane (Handler) AND
// the takeover face (TakeoverHandler) are wrapped by this: browserd's single
// trust boundary is "is this bob?" — per-takeover authorization is bob's job.
func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	if s.apiKey == "" {
		return next
	}
	want := []byte("Bearer " + s.apiKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, Response{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// controlMux builds the bare control-plane routing (no auth wrapper).
func (s *Server) controlMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/navigate", handle(s.navigate))
	mux.HandleFunc("/snapshot", handle(s.snapshot))
	mux.HandleFunc("/click", handle(s.click))
	mux.HandleFunc("/type", handle(s.typeText))
	mux.HandleFunc("/press", handle(s.press))
	mux.HandleFunc("/scroll", handle(s.scroll))
	mux.HandleFunc("/back", handle(s.back))
	mux.HandleFunc("/dialog", handle(s.dialog))
	mux.HandleFunc("/console", handle(s.console))
	mux.HandleFunc("/get-images", handle(s.getImages))
	mux.HandleFunc("/vision", handle(s.vision))
	mux.HandleFunc("/tab", handle(s.tab))
	mux.HandleFunc("/purge", handle(s.purge))
	mux.HandleFunc("/save", handle(s.saveLogin))
	mux.HandleFunc("/live", handle(s.live))
	mux.HandleFunc("GET /debug/copies", s.debugCopies) // test hook (docs/wip-browser-profile-identity.md)
	// /profiles is method-split: POST = live checkout snapshot (P2 takeover
	// picker feed), GET = persisted vault listing (P3 backup feed). The
	// export/import pair completes the P3 persistence face (profiles.go).
	mux.HandleFunc("POST /profiles", handle(s.profiles))
	mux.HandleFunc("GET /profiles", s.vaultProfiles)
	mux.HandleFunc("GET /profile/{key}/export", s.profileExport)
	mux.HandleFunc("POST /profile/{key}/import", s.profileImport)
	return mux
}

// healthz reports liveness AND echoes the config browserd actually resolved,
// so a deploy can detect drift from bob's own config.yaml (the two processes
// read separate files; docs/browserd.md). page_timeout_seconds is the one that
// bites: if browserd runs a slower timeout than bob's client deadline, bob
// reports a transport error while browserd is still working. No handshake is
// forced — this is a passive diagnostic surface.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, Response{Error: "GET only"})
		return
	}
	data, err := json.Marshal(HealthData{PageTimeoutSeconds: s.cfg.PageTimeoutSecondsEff()})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, Response{Error: "marshal health: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, Response{OK: true, Data: data})
}

// handle wraps one typed endpoint func into an http.HandlerFunc:
// POST-only, body decode (4xx on garbage), then the action — whose error is
// an ACTION-level failure carried as HTTP 200 + ok:false (it is a normal
// model-facing error: timeout, missing selector, busy profile). A nil data
// return omits the Data field.
func handle[T any](fn func(ctx context.Context, req *T) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, Response{Error: "POST only"})
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, Response{Error: "read body: " + err.Error()})
			return
		}
		var req T
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, Response{Error: "invalid request body: " + err.Error()})
			return
		}
		data, err := fn(r.Context(), &req)
		if err != nil {
			writeJSON(w, http.StatusOK, Response{Error: err.Error()})
			return
		}
		resp := Response{OK: true}
		if data != nil {
			raw, merr := json.Marshal(data)
			if merr != nil {
				writeJSON(w, http.StatusInternalServerError, Response{Error: "marshal payload: " + merr.Error()})
				return
			}
			resp.Data = raw
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func writeJSON(w http.ResponseWriter, status int, resp Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// --- endpoint bodies ---------------------------------------------------------

func (s *Server) timeout() int { return s.cfg.PageTimeoutSecondsEff() }

func (s *Server) navigate(ctx context.Context, req *NavigateReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	res, err := browser.Navigate(ctx, sess, req.URL, s.timeout())
	if err != nil {
		return nil, err
	}
	// Persist the URL for auto-resume on re-spawn — same call the in-process
	// tool makes (tools.go browserNavigate.Run); state stays browserd-side.
	// RecordNav writes the auto-resume marker into the session dataDir (vault
	// for a profile session) and the scope-level used-marker.
	s.pool.RecordNav(sess, req.Scope, res.URL)
	return res, nil
}

func (s *Server) snapshot(ctx context.Context, req *SnapshotReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	// Bytes mode (screenshotDir ""): format=image PNGs travel back in the
	// response and BOB writes them into its own sandbox — the same two-step
	// shape as /vision. browserd never touches a bob-side path again (the
	// old screenshot_dir contract silently wrote to browserd's local
	// filesystem on multi-host deploys).
	return browser.Snapshot(ctx, sess, "",
		browser.SnapshotOpts{Format: req.Format, Selector: req.Selector}, s.timeout())
}

func (s *Server) click(ctx context.Context, req *ClickReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	return nil, browser.Click(ctx, sess, browser.Locator{Ref: req.Ref, Selector: req.Selector}, s.timeout())
}

func (s *Server) typeText(ctx context.Context, req *TypeReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	return nil, browser.Type(ctx, sess, browser.Locator{Ref: req.Ref, Selector: req.Selector}, req.Text, req.Replace, s.timeout())
}

func (s *Server) press(ctx context.Context, req *PressReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	return nil, browser.Press(ctx, sess, req.Key, browser.Locator{Ref: req.Ref, Selector: req.Selector}, s.timeout())
}

func (s *Server) scroll(ctx context.Context, req *ScrollReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	return nil, browser.Scroll(ctx, sess, browser.ScrollOpts{Direction: req.Direction, Pixels: req.Pixels, Selector: req.Selector}, s.timeout())
}

func (s *Server) back(ctx context.Context, req *BackReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	return browser.Back(ctx, sess, s.timeout())
}

func (s *Server) dialog(_ context.Context, req *DialogReq) (any, error) {
	// Acquire-then-prime mirrors the in-process tool (browserDialog.Run):
	// the acquire materialises the session + refreshes lastUsed; the reply
	// is primed on the acquired session directly (a scope-keyed Pool lookup
	// would miss profile-path sessions, which are keyed by ProfileName).
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	sess.SetDialogReply(req.Accept, req.PromptText)
	return nil, nil
}

func (s *Server) console(_ context.Context, req *ConsoleReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	return ConsoleData{Entries: sess.ConsoleSnapshot(req.Level, req.MaxEntries)}, nil
}

func (s *Server) getImages(ctx context.Context, req *GetImagesReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	imgs, err := browser.GetImages(ctx, sess, req.Selector, req.MaxImages, s.timeout())
	if err != nil {
		return nil, err
	}
	return ImagesData{Images: imgs}, nil
}

func (s *Server) vision(ctx context.Context, req *VisionReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	png, err := browser.Screenshot(ctx, sess, s.timeout())
	if err != nil {
		return nil, err
	}
	return VisionData{ImageB64: base64.StdEncoding.EncodeToString(png), ByteCount: len(png)}, nil
}

func (s *Server) cdp(ctx context.Context, req *CDPReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	// The read-only CDP method allowlist is enforced inside CDPExec — it
	// rides along to browserd unchanged (defense stays with the executor).
	return browser.CDPExec(ctx, sess, req.Method, req.Params, s.timeout())
}

func (s *Server) tab(_ context.Context, req *TabReq) (any, error) {
	sess, err := s.pool.AcquireRouted(req.Scope, req.ProfileKey, req.Mode)
	if err != nil {
		return nil, err
	}
	return browser.RunTab(sess, req.Action, req.Index)
}

func (s *Server) purge(_ context.Context, req *PurgeReq) (any, error) {
	if strings.TrimSpace(req.Scope) == "" {
		return nil, fmt.Errorf("scope is required")
	}
	s.pool.PurgeSession(req.Scope)
	return nil, nil
}

// saveLogin (保存登陆) captures the live worker copy's login into its member master sidecar
// WITHOUT closing it — the worker keeps driving the same logged-in browser.
func (s *Server) saveLogin(_ context.Context, req *SaveReq) (any, error) {
	if strings.TrimSpace(req.Scope) == "" {
		return nil, fmt.Errorf("scope is required")
	}
	return nil, s.pool.SaveLogin(req.Scope)
}

// debugCopies is a TEST HOOK (no auth beyond the control-plane network): dumps the live
// worker copies + their master targets + write-back markers, so a remote test can verify
// the seed → takeover → 保存登陆 → master flow without shell access to the box.
func (s *Server) debugCopies(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{OK: true, Data: jsonRaw(s.pool.DebugCopies())})
}

// jsonRaw marshals v, returning null on error (debug hook only).
func jsonRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// live — POST /live — reports whether the scope has a live, take-over-able
// chromium instance (Pool.LiveForScope). Read-only; never spawns chromium.
// Backs bob's /takeover no-live-browser guard (docs/remote-browser-handoff.md
// P1b). An empty scope is "nothing to take over" → live:false, not an error,
// so the guard's negative branch fires cleanly.
func (s *Server) live(_ context.Context, req *LiveReq) (any, error) {
	return LiveData{Live: s.pool.LiveForScope(req.Scope)}, nil
}

// profiles — POST /profiles — lists the live profile checkouts (custodian
// snapshot). Read-only; never spawns chromium. Feeds bob's admin takeover
// picker in remote mode (docs/browserd.md P2).
func (s *Server) profiles(_ context.Context, _ *ProfilesReq) (any, error) {
	snap := s.pool.Custodian().Snapshot()
	out := make([]ProfileInfo, 0, len(snap))
	for _, c := range snap {
		out = append(out, ProfileInfo{Key: c.Profile, Owner: c.Owner, Pinned: c.Pinned})
	}
	return ProfilesData{Profiles: out}, nil
}
