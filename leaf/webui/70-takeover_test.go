package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/claimtoken"
)

type fakeHold struct {
	set       []string
	cleared   []string
	consumed  []string
	noted     map[string]string
	live      bool
	held      bool   // what Held reports (the save gate)
	resumeSid string // what Clear / ConsumeResume report as the worker to wake
}

func (f *fakeHold) Set(key string) { f.set = append(f.set, key) }
func (f *fakeHold) NoteResume(key, sid string) {
	if f.noted == nil {
		f.noted = map[string]string{}
	}
	f.noted[key] = sid
}
func (f *fakeHold) Clear(key string) (bool, string) {
	f.cleared = append(f.cleared, key)
	if !f.live {
		return false, "" // mirrors the real registry: consume only on a live release
	}
	sid := f.resumeSid
	f.resumeSid = ""
	return true, sid
}
func (f *fakeHold) ConsumeResume(key string) string {
	f.consumed = append(f.consumed, key)
	sid := f.resumeSid
	f.resumeSid = ""
	return sid
}
func (f *fakeHold) Held(string) bool { return f.held }

type fakeResume struct {
	sid  string
	note string
}

func (f *fakeResume) ResumeSession(_ context.Context, sid, note string) { f.sid, f.note = sid, note }

// TestHandleBrowserHold: on=true asserts the hold; on=false releases it and, when the
// release was of a LIVE hold, wakes the session.
func TestHandleBrowserHold(t *testing.T) {
	auth := NewAuth(claimtoken.New())
	cookie, ok := auth.SubmitToken(auth.MintScoped("s1"))
	if !ok {
		t.Fatal("submit scoped token")
	}
	hold := &fakeHold{live: true, resumeSid: "s1"}
	resume := &fakeResume{}
	s := &server{
		auth:        auth,
		controlHold: func() contract.BrowserControlHold { return hold },
		resume:      func() contract.SessionResume { return resume },
	}
	mux := s.routes()
	post := func(on string) int {
		req := httptest.NewRequest("POST", "/api/browser/hold?sid=s1", strings.NewReader(`{"on":`+on+`}`))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}

	if code := post("true"); code != http.StatusNoContent {
		t.Fatalf("on=true code=%d", code)
	}
	if len(hold.set) != 1 || hold.set[0] != "s1" {
		t.Fatalf("Set not asserted: %+v", hold.set)
	}

	if code := post("false"); code != http.StatusNoContent {
		t.Fatalf("on=false code=%d", code)
	}
	if len(hold.cleared) != 1 {
		t.Fatalf("Clear not called: %+v", hold.cleared)
	}
	if resume.sid != "s1" || !strings.Contains(resume.note, "交还") {
		t.Fatalf("live release must wake the session: %+v", resume)
	}

	// an UNauthorized sid (cookie covers s1, not s2) is rejected before touching the hold.
	req := httptest.NewRequest("POST", "/api/browser/hold?sid=s2", strings.NewReader(`{"on":true}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("cross-sid hold must be 401, got %d", w.Code)
	}
}

// fakeTakeover backs the /api/browser/save tests (only SaveLogin matters here).
type fakeTakeover struct {
	saved   []string
	saveErr error
}

func (f *fakeTakeover) Screencast(string, string, func(string)) (func(), <-chan struct{}, error) {
	return func() {}, nil, nil
}
func (f *fakeTakeover) Input(string, contract.BrowserInput) error { return nil }
func (f *fakeTakeover) SaveLogin(_ context.Context, scope string) error {
	f.saved = append(f.saved, scope)
	return f.saveErr
}
func (f *fakeTakeover) LiveForSid(context.Context, string) (bool, error) { return true, nil }
func (f *fakeTakeover) LiveSids(context.Context) ([]string, error)       { return nil, nil }

// TestHandleBrowserSave pins the 保存登陆 endpoint: (BP-1) a caller NOT holding control is
// refused before anything is written; a holder saves, then the resume note is consumed
// EXPLICITLY (D17a ConsumeResume — save wakes the worker even if the hold lapsed mid-save)
// and the hold released; a save failure releases nothing (the human retries).
func TestHandleBrowserSave(t *testing.T) {
	auth := NewAuth(claimtoken.New())
	cookie, ok := auth.SubmitToken(auth.MintScoped("s1"))
	if !ok {
		t.Fatal("submit scoped token")
	}
	hold := &fakeHold{resumeSid: "w1"}
	tk := &fakeTakeover{}
	resume := &fakeResume{}
	s := &server{
		auth:        auth,
		takeover:    func() contract.BrowserTakeover { return tk },
		controlHold: func() contract.BrowserControlHold { return hold },
		resume:      func() contract.SessionResume { return resume },
	}
	mux := s.routes()
	post := func() int {
		req := httptest.NewRequest("POST", "/api/browser/save?sid=s1", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}

	// watch-only (no hold) → refused, nothing saved, nothing released, nobody woken.
	if code := post(); code != http.StatusConflict {
		t.Fatalf("save without control must be 409, got %d", code)
	}
	if len(tk.saved) != 0 || len(hold.cleared) != 0 || len(hold.consumed) != 0 || resume.sid != "" {
		t.Fatalf("refused save must touch nothing: %+v %+v", tk.saved, hold.cleared)
	}

	// save failure while holding → 502, hold NOT released (the human keeps control + retries).
	hold.held, hold.live = true, true
	tk.saveErr = context.DeadlineExceeded
	if code := post(); code != http.StatusBadGateway {
		t.Fatalf("failed save must be 502, got %d", code)
	}
	if len(hold.cleared) != 0 || len(hold.consumed) != 0 || resume.sid != "" {
		t.Fatalf("failed save must not release/resume: %+v", hold.cleared)
	}

	// holding control → saved, note consumed via the explicit door, hold released, worker woken —
	// even if the hold lapsed between the gate and the release (live=false here).
	tk.saveErr = nil
	hold.live = false
	if code := post(); code != http.StatusNoContent {
		t.Fatalf("save code=%d", code)
	}
	if len(tk.saved) != 2 || tk.saved[1] != "s1" { // failed attempt above + this one
		t.Fatalf("SaveLogin not called: %+v", tk.saved)
	}
	if len(hold.consumed) != 1 || len(hold.cleared) != 1 {
		t.Fatalf("save must ConsumeResume + Clear exactly once: consumed=%v cleared=%v", hold.consumed, hold.cleared)
	}
	if resume.sid != "w1" || !strings.Contains(resume.note, "交还") {
		t.Fatalf("save must wake the noted worker despite the lapse: %+v", resume)
	}
}
