package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/claimtoken"
)

// settingPanel is a panel with one code setting backed by an in-memory body.
func settingPanel(body *string) contract.Panel {
	return contract.Panel{
		ID:        "cfg",
		Title:     "Config",
		AdminOnly: true,
		Settings:  []contract.SettingSpec{{ID: "main.yaml", Kind: "code", Label: "main", Lang: "yaml"}},
		State: func(_ context.Context) []contract.StateField {
			return []contract.StateField{{Kind: "stat", Label: "ok", Value: "1"}}
		},
		Read: func(_ context.Context, sid string) (string, error) { return *body, nil },
		Apply: func(_ context.Context, sid, b string) contract.WriteResult {
			*body = b
			return contract.WriteResult{Success: true}
		},
	}
}

// TestAuthAndSettingFlow proves the full admin path: mint → submit → cookie →
// admin-only panel visible + setting read/write — and that the same endpoints
// reject a caller with no session.
func TestAuthAndSettingFlow(t *testing.T) {
	body := "name: old"
	reg := newRegistry()
	reg.RegisterPanel(settingPanel(&body))
	s := &server{reg: reg, auth: NewAuth(claimtoken.New()), start: time.Now()}
	s.refresh(context.Background())
	mux := s.routes()

	// Unauthenticated: admin-only panel is redacted, setting read is 401.
	if got := doState(t, mux, ""); strings.Contains(got, "Config") {
		t.Fatalf("admin panel leaked to anonymous caller: %s", got)
	}
	if code := doGet(mux, "/api/panel/cfg/setting/main.yaml", ""); code != http.StatusUnauthorized {
		t.Fatalf("anon setting read = %d, want 401", code)
	}

	// Mint a token and submit it to get a session cookie.
	tok := s.auth.Mint()
	cookie := submit(t, mux, tok)
	if cookie == "" {
		t.Fatal("no session cookie after token submit")
	}

	// Authenticated: panel visible, setting read returns the body.
	if got := doState(t, mux, cookie); !strings.Contains(got, "Config") {
		t.Fatalf("admin panel not visible to authed caller: %s", got)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/panel/cfg/setting/main.yaml", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "name: old" {
		t.Fatalf("setting read = %d %q, want 200 \"name: old\"", rr.Code, rr.Body.String())
	}

	// Write a new value.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/panel/cfg/setting/main.yaml", strings.NewReader("name: new"))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "true") {
		t.Fatalf("setting write = %d %q, want success", rr.Code, rr.Body.String())
	}
	if body != "name: new" {
		t.Fatalf("Apply did not persist: body=%q", body)
	}

	// Single-use: re-submitting the consumed token fails.
	if _, ok := s.auth.SubmitToken(tok); ok {
		t.Fatal("token was reusable — must be single-use")
	}
}

func doState(t *testing.T, mux *http.ServeMux, cookie string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	mux.ServeHTTP(rr, req)
	return rr.Body.String()
}

func doGet(mux *http.ServeMux, path, cookie string) int {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	mux.ServeHTTP(rr, req)
	return rr.Code
}

func submit(t *testing.T, mux *http.ServeMux, tok string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/token", strings.NewReader(tok))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("token submit = %d, want 200", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	return ""
}
