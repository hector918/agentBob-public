package feishu

import "testing"

// shouldDropAsGroupNoise is the group-noise gate: drop a group/topic message ONLY when
// it neither @-mentions the bot NOR replies to it; a p2p (DM) message is never dropped.
// This is the invariant the M6 reply-parent fold relies on — that an @-mention or a p2p
// chat makes the gate IGNORE replyToBot — so it's worth pinning directly (the routing
// path has no other coverage).
func TestShouldDropAsGroupNoise(t *testing.T) {
	cases := []struct {
		name       string
		chatType   string
		mentions   bool
		replyToBot bool
		wantDrop   bool
	}{
		{"group, no @, no reply → drop", "group", false, false, true},
		{"group, @'d → keep (mention dominates)", "group", true, false, false},
		{"group, @'d + reply → keep", "group", true, true, false},
		{"group, reply-to-bob → keep", "group", false, true, false},
		{"topic, no @, no reply → drop", "topic", false, false, true},
		{"p2p, no @, no reply → keep (DM never gated)", "p2p", false, false, false},
		{"p2p, reply → keep", "p2p", false, true, false},
	}
	for _, c := range cases {
		if got := shouldDropAsGroupNoise(c.chatType, c.mentions, c.replyToBot); got != c.wantDrop {
			t.Errorf("%s: drop=%v want %v", c.name, got, c.wantDrop)
		}
	}
}
