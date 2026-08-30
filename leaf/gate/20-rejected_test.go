package gate

import (
	"testing"
	"time"

	"agentbob/contract"
)

// TestRejected_ReplyTTL pins the bounce cooldown: first contact → shouldReply;
// repeats inside bounceReplyTTL stay silent (no amplification); a message past the
// window re-arms. The admit TOKEN now lives in the claimtoken facility (the screen
// mints it and SetTokens it onto the entry for the webui feed); ResolveToken is gone
// (Verify lives in the facility). This covers the reply cadence + SetToken.
func TestRejected_ReplyTTL(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	r := NewRejectedSenders(10)
	r.now = func() time.Time { return now }

	if !r.Record("discord", "g1", "u1", "U1", contract.ChatGroup) {
		t.Fatal("first contact: shouldReply=false, want true")
	}
	now = now.Add(30 * time.Second) // inside the 2-min window
	if r.Record("discord", "g1", "u1", "U1", contract.ChatGroup) {
		t.Fatal("repeat within TTL must be silent")
	}
	now = now.Add(bounceReplyTTL) // past the cooldown
	if !r.Record("discord", "g1", "u1", "U1", contract.ChatGroup) {
		t.Fatal("message after cooldown must re-arm the reply")
	}

	// SetToken stamps the feed entry's admit token (minted by the screen via the
	// facility) and returns the token it replaces (D7: caller retires the old one).
	if prev, found := r.SetToken("discord", "g1", "u1", "tok-xyz"); !found || prev != "" {
		t.Fatalf("SetToken first stamp = (%q,%v), want (\"\",true)", prev, found)
	}
	if got := r.List(); len(got) != 1 || got[0].Token != "tok-xyz" {
		t.Fatalf("feed entry token = %+v, want Token=tok-xyz", got)
	}
	if prev, found := r.SetToken("discord", "g1", "u1", "tok-new"); !found || prev != "tok-xyz" {
		t.Fatalf("SetToken re-stamp = (%q,%v), want (\"tok-xyz\",true)", prev, found)
	}
	if prev, found := r.SetToken("discord", "g1", "nobody", "x"); found || prev != "" {
		t.Fatalf("SetToken on an unknown entry = (%q,%v), want (\"\",false)", prev, found)
	}
}
