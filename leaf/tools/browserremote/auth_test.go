package browserremote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPost_BearerHeader pins the A1 control-plane auth the trunk port had dropped:
// a configured BROWSERD_API_KEY must ride every request as `Authorization: Bearer
// <key>` (browserd 401s without it), and an empty key must send no header.
func TestPost_BearerHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	if err := New(srv.URL, "s3cr3t").post(context.Background(), "/x", map[string]any{}, nil); err != nil {
		t.Fatalf("post with key: %v", err)
	}
	if got != "Bearer s3cr3t" {
		t.Fatalf("Authorization header = %q, want %q", got, "Bearer s3cr3t")
	}

	got = "sentinel"
	if err := New(srv.URL, "").post(context.Background(), "/x", map[string]any{}, nil); err != nil {
		t.Fatalf("post without key: %v", err)
	}
	if got != "" {
		t.Fatalf("no-key client must send no Authorization header, got %q", got)
	}
}
