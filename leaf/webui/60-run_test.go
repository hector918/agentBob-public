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

// stubSlash echoes the dispatched command line through the sink, so the test can
// assert the synthetic context reached a handler.
type stubSlash struct{}

func (stubSlash) Register(contract.SlashCommand) {}
func (stubSlash) List() []contract.SlashCommand  { return nil }
func (stubSlash) Dispatch(_ context.Context, sc contract.SlashContext) error {
	return sc.Sink.Finish("ran: " + sc.Event.Text)
}

func TestRunAllowlistAndAuth(t *testing.T) {
	s := &server{reg: newRegistry(), auth: NewAuth(claimtoken.New()), slash: stubSlash{}, start: time.Now()}
	mux := s.routes()

	// No session → 401.
	if code := doPost(mux, "/api/run", "/model x", ""); code != http.StatusUnauthorized {
		t.Fatalf("anon /api/run = %d, want 401", code)
	}

	cookie := submit(t, mux, s.auth.Mint())

	// Allowlisted command reaches the (stub) handler.
	body, code := doPostBody(mux, "/api/run", "/model status", cookie)
	if code != http.StatusOK || !strings.Contains(body, "ran: /model status") {
		t.Fatalf("allowlisted run = %d %q, want handler output", code, body)
	}

	// Non-allowlisted command is refused before dispatch (no "ran:").
	body, code = doPostBody(mux, "/api/run", "/new", cookie)
	if code != http.StatusOK || strings.Contains(body, "ran:") {
		t.Fatalf("non-allowlisted run leaked to handler: %d %q", code, body)
	}
}

func doPost(mux *http.ServeMux, path, body, cookie string) int {
	_, code := doPostBody(mux, path, body, cookie)
	return code
}

func doPostBody(mux *http.ServeMux, path, body, cookie string) (string, int) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	mux.ServeHTTP(rr, req)
	return rr.Body.String(), rr.Code
}
