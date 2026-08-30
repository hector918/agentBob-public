package gate

import (
	"testing"

	"agentbob/contract"
)

// TestAllowAction pins the rejected-row "allow" prefill scope (the discord over-grant
// fix): a GROUP sender is admitted to THAT chat only (per-chat allowlist_add), never
// source-wide; a DM (or a row missing a chat id) is admitted source-level.
func TestAllowAction(t *testing.T) {
	cases := []struct {
		name   string
		ct     contract.ChatType
		chatID string
		want   string
	}{
		{"group → per-chat", contract.ChatGroup, "chan1", "/gate allow discord chan1 u9"},
		{"dm → source-level", contract.ChatDM, "dm1", "/gate allow discord u9"},
		{"group without chat id → source-level (no chat to scope to)", contract.ChatGroup, "", "/gate allow discord u9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := allowAction("discord", c.chatID, c.ct, "u9"); got != c.want {
				t.Fatalf("allowAction(%v, %q) = %q, want %q", c.ct, c.chatID, got, c.want)
			}
		})
	}
}
