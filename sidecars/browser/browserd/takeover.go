package browserd

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agentbob/sidecars/browser/tools/browser"
)

// Takeover face (docs/browserd.md §4 — takeover proxied through bob): served
// to BOB ONLY. The human's browser talks to bob's webui
// (/api/browser/screencast + /input, one origin, session cookie); bob's
// webui proxies those onto this face server-to-server.
//
// Auth = the control-plane API key ($BROWSERD_API_KEY), the SAME static shared
// secret every other browserd endpoint uses (TakeoverHandler is wrapped by
// requireAPIKey in NewServer's wiring). browserd is a dumb service: it
// authenticates bob via the api key and knows NOTHING about user business.
// The per-takeover AUTHORIZATION — which human may take over which browser —
// lives entirely in bob (leaf/webui Authorize(cookie, scope)), which runs
// BEFORE bob ever reaches this face. The api key is server-to-server and never
// leaves bob (bob's webui proxies the stream — the human's browser connects to
// bob, never here), so it is never exposed to a browser. The per-takeover
// ticket browserd used to verify was retired (it taught a dumb service about
// user business); see the §4 removal note in docs/browserd.md.
//
// Serve this handler on its OWN listener (cmd/browserd --takeover-listen) so
// the long-lived SSE streams never tie up the control plane. Like the
// control plane, it must only be reachable by bob — never exposed publicly.

// takeoverHeartbeatInterval paces the screencast SSE keepalive comments — a
// static page produces no frames, and without periodic writes the write
// deadline would kill a healthy stream. A var (not const) so tests can shrink
// it to exercise the streaming loop quickly.
var takeoverHeartbeatInterval = 10 * time.Second

// TakeoverHandler returns the takeover mux: GET /takeover/screencast (SSE) +
// POST /takeover/input + POST /takeover/tabs. The mux is wrapped by
// requireAPIKey so every takeover request authenticates the control-plane api
// key (the same Bearer secret as the control plane); per-takeover authorization
// is bob's job and has already happened before bob reaches this face.
func (s *Server) TakeoverHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/takeover/screencast", s.takeoverScreencast)
	mux.HandleFunc("/takeover/input", s.takeoverInput)
	mux.HandleFunc("/takeover/tabs", s.takeoverTabs)
	return s.requireAPIKey(mux)
}

// takeoverScope extracts the target scope from the query. Writes the error
// response itself; ok=false means the handler must return. The CALLER's identity
// (bob) is already authenticated by requireAPIKey on the mux; this only validates
// the request shape — WHICH browser to drive. Per-human authorization happened in
// bob (Authorize(cookie, scope)) before bob ever proxied here.
func (s *Server) takeoverScope(w http.ResponseWriter, r *http.Request) (scope string, ok bool) {
	scope = strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		http.Error(w, "missing scope", http.StatusBadRequest)
		return "", false
	}
	return scope, true
}

// takeoverScreencast — GET /takeover/screencast?scope=… — streams
// the live browser owned by `scope` as Server-Sent-Events (one
// `data: <base64-jpeg>` per frame), consumed by bob's webui proxy which
// relays the frames to the human. Mirrors bob's webui handleBrowserScreencast
// with the same load-bearing details:
//
//   - the screencast cb runs on the browser drainer goroutine and only
//     forwards frames onto `frames`; THIS handler goroutine is the sole
//     writer of the ResponseWriter (no cross-goroutine writes);
//   - defer stop() — unpins the profile + stops the CDP stream when the
//     client disconnects;
//   - per-frame write deadline overrides the server's WriteTimeout (which
//     would otherwise kill the stream) and bounds a dead client;
//   - 10s heartbeat keeps a static page (no frames) from tripping the
//     write timeout.
//
// The takeover listener mounts this mux directly (no buffering recover
// wrapper), so Flush is available; net/http still recovers a per-request
// panic, worst case a dropped stream.
func (s *Server) takeoverScreencast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.takeoverScope(w, r)
	if !ok {
		return
	}
	// The api key was authenticated by requireAPIKey on the takeover mux; this
	// stream lives as long as the connection (no mid-stream re-auth — bob proxies
	// it and the key never leaves bob).
	rc := http.NewResponseController(w)

	// q is the requested bandwidth tier ("high"|"med"|"low"; empty → med). It is a
	// REQUEST — the pool clamps it down under load (ResolveCastTier) to protect the box.
	quality := r.URL.Query().Get("q")

	// Single-viewer enforcement: a scope has AT MOST one live takeover stream. Evict any
	// prior viewer (it emits ~evicted + stops, so it never auto-reconnects and ping-pongs)
	// and WAIT for it to release the pool, so this stream acquires the cast as the FIRST
	// viewer and applies its own tier cleanly (no shared-cast reconfigure needed).
	self := &takeoverConn{evict: make(chan struct{}), done: make(chan struct{})}
	s.takeoverMu.Lock()
	prev := s.takeoverConns[scope]
	s.takeoverConns[scope] = self
	s.takeoverMu.Unlock()
	if prev != nil {
		close(prev.evict)
		select {
		case <-prev.done: // prior fully released the pool → we acquire as first
		case <-time.After(2 * time.Second):
			// Safety: never hang on a prior wedged mid-write to a half-dead socket (its
			// write deadline can exceed this). We then proceed before it released, so this
			// stream may briefly SHARE the prior's cast at the prior's tier (castAcquire is
			// not-first) until the prior's ctx is cancelled and it releases. Frames keep
			// flowing (never frozen — the refcount can't hit 0 while we hold it); the only
			// cost is our requested tier not applying until a tab-follow re-bind. Rare
			// (needs a wedged reconnect), self-correcting, and non-fatal — accepted.
		}
	}
	// Deregister + signal release on exit. Registered BEFORE `defer stop()` so (LIFO) stop
	// runs first: self.done then closes only AFTER the pool is released, which is the signal
	// a successor waits on above.
	defer func() {
		s.takeoverMu.Lock()
		if s.takeoverConns[scope] == self {
			delete(s.takeoverConns, scope)
			// Prune the idle clock too — only when WE are still the registered conn, so
			// an already-evicted prior stream can't delete the successor's fresh entry.
			s.inputMu.Lock()
			delete(s.lastInput, scope)
			s.inputMu.Unlock()
		}
		s.takeoverMu.Unlock()
		close(self.done)
	}()

	frames := make(chan string, 8)
	// ended closes when the frame source dies (the streamed instance was
	// closed under the takeover) — end this HTTP stream then, so bob's
	// proxy `done` chain fires and the human's overlay gets a clean stream
	// end instead of keepalives over a dead source.
	stop, ended, effTier, err := s.startScreencast(scope, quality, func(f browser.ScreencastFrame) {
		select {
		case frames <- f.DataB64:
		default: // handler lagging — drop (best-effort)
		}
	})
	if err != nil {
		http.Error(w, "screencast unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer stop()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return // streaming genuinely unsupported (shouldn't happen off the plain mux)
	}

	// Report the EFFECTIVE tier (post load-clamp) once, up front, as an in-band control
	// line: a base64 JPEG frame can never begin with '~' (not in the base64 alphabet),
	// so the webui distinguishes "~tier:<t>" from a frame with no proxy changes — the
	// 画质 button then shows what is actually running, not just what was requested.
	if effTier != "" {
		_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err := fmt.Fprintf(w, "data: ~tier:%s\n\n", effTier); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}

	heartbeat := time.NewTicker(takeoverHeartbeatInterval)
	defer heartbeat.Stop()
	s.touchInput(scope) // start the idle clock
	idle := time.NewTicker(30 * time.Second)
	defer idle.Stop()

	sendFrame := func(f string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err := fmt.Fprintf(w, "data: %s\n\n", f); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// "slow" tier: a wall-clock gate forwards at most one frame per interval (the lever
	// everyNth can't express). Frames inside a window collapse to the LATEST and flush on
	// the tick — so a page change shows within `interval` instead of being dropped — while
	// the FIRST frame is sent immediately so the stream isn't blank for a whole interval
	// at connect. interval == 0 (high/med/low) → every frame forwarded as it arrives.
	interval := s.cfg.TierMinIntervalMs(effTier)
	var slowC <-chan time.Time
	if interval > 0 {
		st := time.NewTicker(time.Duration(interval) * time.Millisecond)
		defer st.Stop()
		slowC = st.C
	}
	var pending string
	var havePending, wroteFirst bool

	for {
		select {
		case <-r.Context().Done():
			return
		case <-self.evict:
			// A newer viewer for this scope displaced us. Tell the frontend (so it stops
			// and does NOT auto-reconnect, which would ping-pong against the new viewer),
			// then end. Short deadline so a dead client can't delay the successor's acquire.
			_ = rc.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, _ = io.WriteString(w, "data: ~evicted\n\n")
			_ = rc.Flush()
			return
		case <-ended:
			return // frame source died — end the stream cleanly
		case <-idle.C:
			if s.inputIdle(scope) > takeoverInputIdleTTL {
				slog.Info("browserd takeover: idle (no input) — closing stream", "scope", scope, "ttl", takeoverInputIdleTTL.String())
				return // abandoned takeover → unpins the copy → controlled-close writeback
			}
		case <-heartbeat.C:
			_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if _, err := io.WriteString(w, ":keepalive\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case f := <-frames:
			if interval <= 0 || !wroteFirst {
				wroteFirst = true
				if !sendFrame(f) {
					return
				}
			} else {
				pending, havePending = f, true // latest-wins until the next slow tick
			}
		case <-slowC: // only fires for the "slow" tier (nil channel otherwise)
			if havePending {
				havePending = false
				if !sendFrame(pending) {
					return
				}
			}
		}
	}
}

// takeoverInput — POST /takeover/input?scope=… — dispatches one
// mouse/key/text event into the live browser owned by `scope`, forwarded
// verbatim by bob's webui proxy. The "text" kind (Input.insertText) is what
// the overlay's IME text box rides — composed CJK arrives as one whole
// string, never as keystrokes.
func (s *Server) takeoverInput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.takeoverScope(w, r)
	if !ok {
		return
	}
	s.touchInput(scope) // keep the takeover idle clock alive
	var ev browser.InputEvent
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&ev); err != nil {
		http.Error(w, "bad input event", http.StatusBadRequest)
		return
	}
	if err := s.pool.DispatchInputForScope(scope, ev); err != nil {
		// Surface the real dispatch error to the trusted bob caller (this
		// face is reached only via bob's proxy, never the human directly).
		// Without it the failure is a blind generic 400 and the actual CDP /
		// session cause never escapes browserd.
		slog.Warn("browserd takeover: input dispatch failed", "scope", scope, "kind", ev.Kind, "err", err)
		http.Error(w, "input dispatch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// takeoverTabs — POST /takeover/tabs?scope=… — runs one tab action
// (list / switch / close) on the browser owned by `scope` so the human holding
// control can drive tabs from the overlay, and returns the tab-list payload
// (browser.RunTab's map). A switch updates the scope's active tab, which the
// screencast follow-tab supervisor re-binds the stream to. Reached only via
// bob's webui proxy (same api-key auth as screencast/input).
func (s *Server) takeoverTabs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.takeoverScope(w, r)
	if !ok {
		return
	}
	var req struct {
		Action string `json:"action"`
		Index  int    `json:"index"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad tab request", http.StatusBadRequest)
		return
	}
	payload, err := s.pool.RunTabForScope(scope, req.Action, req.Index)
	if err != nil {
		slog.Warn("browserd takeover: tab op failed", "scope", scope, "action", req.Action, "err", err)
		http.Error(w, "tab op failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Warn("browserd takeover: encode tab payload failed", "scope", scope, "err", err)
	}
}
