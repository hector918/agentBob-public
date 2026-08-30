package browserremote

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agentbob/contract"
)

// TestTakeover exercises the takeover proxy against a fake browserd: control-plane
// live/profiles reads, api-key-authed input (no ticket), and a one-frame screencast
// that ends. The takeover face is authed by the SAME control-plane api key (Bearer),
// never a per-takeover ticket — and the api key never appears in the URL.
func TestTakeover(t *testing.T) {
	const apiKey = "s3cret"
	var inputAuth, inputTicket string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/ticket":
			// The ticket-mint endpoint is RETIRED — the client must never call it.
			t.Errorf("client called retired /internal/ticket endpoint")
			http.NotFound(w, r)
		case "/live":
			_, _ = w.Write([]byte(`{"ok":true,"data":{"live":true}}`))
		case "/profiles":
			_, _ = w.Write([]byte(`{"ok":true,"data":{"profiles":[{"key":"s_abc"},{"key":""}]}}`))
		case "/takeover/input":
			inputAuth = r.Header.Get("Authorization")
			inputTicket = r.URL.Query().Get("ticket")
			w.WriteHeader(http.StatusNoContent)
		case "/takeover/screencast":
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			fmt.Fprint(w, "data: AAAA\n\n")
			w.(http.Flusher).Flush()
			// returning ends the stream → client's done closes
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, apiKey)
	c.takeoverBase = srv.URL // override the derived host:8378 to hit the fake
	ctx := context.Background()

	if live, err := c.LiveForSid(ctx, "s_abc"); err != nil || !live {
		t.Fatalf("LiveForSid = (%v, %v), want (true, nil)", live, err)
	}
	if sids, err := c.LiveSids(ctx); err != nil || len(sids) != 1 || sids[0] != "s_abc" {
		t.Fatalf("LiveSids = (%v, %v), want ([s_abc], nil) — blank keys dropped", sids, err)
	}

	if err := c.Input("s_abc", contract.BrowserInput{Kind: "mouse", MouseType: "move", X: 1, Y: 2}); err != nil {
		t.Fatalf("Input: %v", err)
	}
	if inputAuth != "Bearer "+apiKey {
		t.Fatalf("input Authorization = %q, want %q (api-key authed)", inputAuth, "Bearer "+apiKey)
	}
	if inputTicket != "" {
		t.Fatalf("input request carried a ticket %q — tickets are retired", inputTicket)
	}

	frames := make(chan string, 4)
	stop, done, err := c.Screencast("s_abc", "", func(b64 string) { frames <- b64 })
	if err != nil {
		t.Fatalf("Screencast: %v", err)
	}
	select {
	case f := <-frames:
		if f != "AAAA" {
			t.Fatalf("frame = %q, want AAAA", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame received")
	}
	<-done // stream ended
	stop() // idempotent
}
