package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/claimtoken"
)

// fakePanel builds a panel whose State returns one stat + one table field.
func fakePanel(id, title string, admin bool) contract.Panel {
	return contract.Panel{
		ID:        id,
		Title:     title,
		AdminOnly: admin,
		State: func(_ context.Context) []contract.StateField {
			return []contract.StateField{
				{Kind: "stat", Label: "count", Value: "3"},
				{Kind: "table", Label: "rows", Columns: []string{"a"},
					Rows: [][]contract.Cell{{{Text: "x", Status: "ok"}}}},
			}
		},
	}
}

// panicPanel builds a panel whose State panics — used to prove one bad panel can't
// take down the poller or skip the panels after it.
func panicPanel(id string) contract.Panel {
	return contract.Panel{
		ID:    id,
		Title: id,
		State: func(_ context.Context) []contract.StateField {
			panic("panel boom")
		},
	}
}

// TestRefreshPanicIsolated: a panel whose State panics degrades to empty fields and
// does NOT stop the poller — every other panel (including ones registered after the
// bad one) is still polled and published to the cache.
func TestRefreshPanicIsolated(t *testing.T) {
	reg := newRegistry()
	reg.RegisterPanel(panicPanel("bad"))
	reg.RegisterPanel(fakePanel("good", "Good", false))

	s := &server{reg: reg, auth: NewAuth(claimtoken.New()), start: time.Now()}
	s.refresh(context.Background()) // must not panic out

	cached := s.cached.Load()
	if cached == nil || len(cached.Panels) != 2 {
		t.Fatalf("expected 2 panels published, got %+v", cached)
	}
	byID := map[string][]contract.StateField{}
	for _, p := range cached.Panels {
		byID[p.ID] = p.State
	}
	if len(byID["bad"]) != 0 {
		t.Errorf("panicking panel should publish empty state, got %v", byID["bad"])
	}
	if len(byID["good"]) == 0 {
		t.Error("a healthy panel after a panicking one must still be polled + published")
	}
}

// TestStateRoundTripAndRedaction proves the generic pipeline end to end:
// a module's self-described panel is collected, polled, and rendered into
// /api/state — and an AdminOnly panel is redacted for the (unauthenticated)
// caller while a normal panel is shown.
func TestStateRoundTripAndRedaction(t *testing.T) {
	reg := newRegistry()
	reg.RegisterPanel(fakePanel("public", "Public", false))
	reg.RegisterPanel(fakePanel("secret", "Secret", true))

	s := &server{reg: reg, auth: NewAuth(claimtoken.New()), start: time.Now()}
	s.refresh(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rr := httptest.NewRecorder()
	s.handleState(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got wireState
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rr.Body.String())
	}
	if len(got.Panels) != 1 {
		t.Fatalf("got %d panels, want 1 (admin panel must be redacted)", len(got.Panels))
	}
	p := got.Panels[0]
	if p.ID != "public" {
		t.Fatalf("visible panel = %q, want \"public\"", p.ID)
	}
	if len(p.State) != 2 || p.State[0].Kind != "stat" || p.State[0].Value != "3" {
		t.Fatalf("stat field did not round-trip: %+v", p.State)
	}
	if p.State[1].Kind != "table" || len(p.State[1].Rows) != 1 || p.State[1].Rows[0][0].Status != "ok" {
		t.Fatalf("table field did not round-trip: %+v", p.State[1])
	}
	if strings.Contains(rr.Body.String(), "Secret") {
		t.Fatalf("admin panel leaked into unauthenticated /api/state: %s", rr.Body.String())
	}
}

// TestIndexRouteExact pins the "GET /{$}" index registration: the dashboard has no
// client-side routing, so only exactly "/" serves the index HTML — an unmatched path
// (notably a typo'd /api/ one) must 404, not 200 with HTML that the frontend would
// then choke on as a JSON parse error.
func TestIndexRouteExact(t *testing.T) {
	s := &server{reg: newRegistry(), auth: NewAuth(claimtoken.New()), start: time.Now()}
	mux := s.routes()

	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}

	if rr := get("/"); rr.Code != http.StatusOK || !strings.Contains(rr.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d (%s), want 200 text/html", rr.Code, rr.Header().Get("Content-Type"))
	}
	if rr := get("/no-such-page"); rr.Code != http.StatusNotFound {
		t.Fatalf("GET /no-such-page = %d, want 404", rr.Code)
	}
	if rr := get("/api/statee"); rr.Code != http.StatusNotFound {
		t.Fatalf("typo'd api path = %d, want 404 (not index HTML)", rr.Code)
	}
}

// TestRegisterPanelDuplicatePanics guards the wiring-bug posture.
func TestRegisterPanelDuplicatePanics(t *testing.T) {
	reg := newRegistry()
	reg.RegisterPanel(fakePanel("dup", "A", false))
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate panel id did not panic")
		}
	}()
	reg.RegisterPanel(fakePanel("dup", "B", false))
}
