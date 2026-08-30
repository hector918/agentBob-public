package webui

// Takeover HTTP endpoints (docs/wip-takeover-port.md P0). They relay browserd's
// takeover face (via contract.BrowserTakeover, keyed by SID) to the human's browser:
// an SSE screencast + an input POST, plus an admin live-session list. Registered only
// when a BrowserTakeover backend is present. bob is the auth authority — every
// per-sid endpoint runs Authorize(cookie, sid) first (admin coverage "*" sees all;
// a scoped capability session sees only its own sid).

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"agentbob/contract"
)

// handleScreencast streams sid's live browser as Server-Sent Events (base64 JPEG
// frames). Frames are buffered with a tiny channel; if the human's link can't keep
// up a frame is dropped (the next one supersedes it). A 15s heartbeat keeps the
// connection from idling out; the stream ends on client disconnect or upstream end.
func (s *server) handleScreencast(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	if !s.auth.Authorize(cookieOf(r), sid) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tk := s.liveTakeover()
	if tk == nil {
		http.Error(w, "takeover unavailable", http.StatusServiceUnavailable)
		return
	}
	// q is the requested bandwidth tier ("high"|"med"|"low"; "" → med); a REQUEST
	// the backend clamps down under load. Passed straight through to browserd.
	q := r.URL.Query().Get("q")
	frames := make(chan string, 8)
	stop, done, err := tk.Screencast(sid, q, func(b64 string) {
		select {
		case frames <- b64:
		default: // drop — latest frame wins
		}
	})
	if err != nil {
		http.Error(w, "screencast: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer stop()
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ctx := r.Context()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done(): // human closed the EventSource (recede / nav away)
			return
		case <-done: // upstream ended (browserd restart / stop / idle reclaim)
			return
		case b64 := <-frames:
			if _, err := w.Write([]byte("data: " + b64 + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleBrowserInput forwards one human input event (mouse / dock-typed text) to
// sid's live browser. Body capped — input events are tiny.
func (s *server) handleBrowserInput(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	if !s.auth.Authorize(cookieOf(r), sid) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tk := s.liveTakeover()
	if tk == nil {
		http.Error(w, "takeover unavailable", http.StatusServiceUnavailable)
		return
	}
	var ev contract.BrowserInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&ev); err != nil {
		http.Error(w, "bad input", http.StatusBadRequest)
		return
	}
	if err := tk.Input(sid, ev); err != nil {
		http.Error(w, "input failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// takeoverResumeNote is the wake message handed to a session when the human EXPLICITLY
// hands the browser back (control → watch). The browser-specific wording lives here in
// the takeover surface; session.ResumeSession only delivers + wakes (stays generic).
const takeoverResumeNote = "人已完成手动操作并把浏览器交还给你了，请继续之前的浏览器任务。"

// handleBrowserHold drives the human control-hold from the takeover control toggle.
// on=true asserts / heartbeat-renews the hold (bob's browser yields for this sid);
// on=false releases it, and an explicit release of a LIVE hold wakes the session to
// continue (session applies its flow's resume policy — auto only). Gated per-sid like
// the other takeover endpoints.
func (s *server) handleBrowserHold(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	if !s.auth.Authorize(cookieOf(r), sid) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	hold := s.controlHoldReg()
	if hold == nil {
		http.Error(w, "takeover unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.On {
		hold.Set(sid)
	} else if wasLive, resumeSid := hold.Clear(sid); wasLive && resumeSid != "" {
		// wasLive → an explicit hand-back (not a lapse); resumeSid → the worker session that
		// handed off (sid here is the copy's SCOPE, not a session id). Wake that worker.
		if resume := s.resumeSession(); resume != nil {
			resume.ResumeSession(r.Context(), resumeSid, takeoverResumeNote)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// resumeTaker is the explicit-consume door on the control-hold registry (the concrete
// leaf/tools registry implements it beside the contract methods): unconditionally take
// the resume note for key, regardless of hold liveness. 保存登陆 needs it because Clear
// consumes only on a LIVE release — save must wake the worker even after a lapse.
type resumeTaker interface {
	ConsumeResume(key string) (sid string)
}

// handleBrowserSave drives the 保存登陆 button: capture the worker copy's live login into its
// member MASTER sidecar (cookies incl session cookies → future copies seed it), THEN release
// control + wake the worker — which resumes on the SAME live, logged-in copy (no close, no
// re-seed). Save BEFORE release so the master is written before the worker carries on.
func (s *server) handleBrowserSave(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("sid")
	if !s.auth.Authorize(cookieOf(r), scope) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tk := s.liveTakeover()
	if tk == nil {
		http.Error(w, "takeover unavailable", http.StatusServiceUnavailable)
		return
	}
	// 0. gate on the caller currently HOLDING control (BP-1): a watch-only viewer's 💾 must
	// not write the (login-less) copy back over the master. The frontend surfaces the
	// refusal (⚠️ + keeps the takeover open); entering control mode re-asserts the hold,
	// then the retry passes.
	hold := s.controlHoldReg()
	if hold == nil || !hold.Held(scope) {
		http.Error(w, "not in control — enter control mode before saving", http.StatusConflict)
		return
	}
	// 1. save: browserd captures the live login (no close — the copy stays running for the
	// worker). On failure, DON'T release: surface it so the human can retry; the frontend
	// keeps the takeover open.
	if err := tk.SaveLogin(r.Context(), scope); err != nil {
		slog.Warn("webui: save-login writeback failed", "scope", scope, "err", err)
		http.Error(w, "save failed", http.StatusBadGateway)
		return
	}
	// 2. release control + wake the worker, which resumes on the SAME live, logged-in copy.
	// Consume the resume note EXPLICITLY (ConsumeResume), not via Clear's live-release
	// consume: clicking 保存登陆 IS an explicit hand-back, so it must wake the worker even
	// if the hold heartbeat lapsed between the gate above and here — otherwise the worker
	// is stranded idle after the human already handled the login. (handleBrowserHold's
	// off-toggle keeps the wasLive gate: there a lapse must NOT be mistaken for a
	// hand-back, and Clear leaves the note for the retried hand-back.)
	var resumeSid string
	if taker, ok := hold.(resumeTaker); ok {
		resumeSid = taker.ConsumeResume(scope)
	}
	if _, sid := hold.Clear(scope); resumeSid == "" {
		resumeSid = sid // hold impl without the explicit door: live-release consume
	}
	if resumeSid != "" {
		if resume := s.resumeSession(); resume != nil {
			resume.ResumeSession(r.Context(), resumeSid, takeoverResumeNote)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBrowserLive lists the sids with a live browser — the admin takeover picker.
// Admin-only (a scoped session already knows its one sid).
func (s *server) handleBrowserLive(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tk := s.liveTakeover()
	if tk == nil {
		http.Error(w, "takeover unavailable", http.StatusServiceUnavailable)
		return
	}
	sids, err := tk.LiveSids(r.Context())
	if err != nil {
		http.Error(w, "live query failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, sids)
}
