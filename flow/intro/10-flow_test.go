package intro

import "testing"

// TestIntroReplyKey pins F151: a shared group gets the NEUTRAL nudge (never the
// status-specific pending/paused text), while a DM keeps the specific message.
func TestIntroReplyKey(t *testing.T) {
	if got := introReplyKey("intro.paused", true); got != "intro.group_dm_nudge" {
		t.Errorf("paused in a group must not disclose status, got %q", got)
	}
	if got := introReplyKey("intro.pending", true); got != "intro.group_dm_nudge" {
		t.Errorf("pending in a group must be the neutral nudge, got %q", got)
	}
	if got := introReplyKey("intro.paused", false); got != "intro.paused" {
		t.Errorf("DM must keep the specific message, got %q", got)
	}
}

// TestIntroDedupKey pins F151: the dedup key includes the CHAT, so a group greet and a DM
// ask from the same sender dedup SEPARATELY (a group notice can't silence the DM).
func TestIntroDedupKey(t *testing.T) {
	group := introDedupKey("tg", "g1", "u1", "intro.group_dm_nudge")
	dm := introDedupKey("tg", "u1", "u1", "intro.pending")
	if group == dm {
		t.Fatal("a group greet must not share a dedup key with the DM ask (F151)")
	}
	// Same chat + sender + reply → same key (still one send per notice per chat).
	if introDedupKey("tg", "g1", "u1", "intro.group_dm_nudge") != group {
		t.Fatal("dedup key must be stable for the same (source, chat, sender, reply)")
	}
}
