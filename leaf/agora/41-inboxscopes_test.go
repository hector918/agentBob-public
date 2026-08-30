package agora

import (
	"context"
	"testing"
)

// TestInboxScopes pins the reverse lookup the webui chat-log reader rides: an inbox
// resolves to its OWN native scope plus every wired inbox_source scope (deduped),
// and an unknown inbox yields nothing.
func TestInboxScopes(t *testing.T) {
	a := newTestImpl(t)
	ctx := context.Background()

	// ib_bob: own member scope + two wired source scopes (dm:200 + group:200).
	want := map[string]bool{
		MemberOwnedInboxScope("bob", "co_acme"): true,
		"telegram:dm:200":                       true,
		"telegram:group:200":                    true,
	}
	got := a.InboxScopes(ctx, "ib_bob")
	if len(got) != len(want) {
		t.Fatalf("ib_bob scopes = %v, want %d distinct", got, len(want))
	}
	seen := map[string]bool{}
	for _, s := range got {
		if !want[s] {
			t.Fatalf("ib_bob unexpected scope %q (got %v)", s, got)
		}
		if seen[s] {
			t.Fatalf("ib_bob duplicate scope %q", s)
		}
		seen[s] = true
	}

	// unknown inbox → empty.
	if s := a.InboxScopes(ctx, "ib_nope"); len(s) != 0 {
		t.Fatalf("unknown inbox: got %v, want empty", s)
	}
}
