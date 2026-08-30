package prompt

import (
	"strings"
	"testing"

	"agentbob/contract"
)

func TestEstTokens(t *testing.T) {
	if got := EstTokens("你好"); got != 2 { // 2 CJK ≈ 2
		t.Errorf("EstTokens(CJK) = %d, want 2", got)
	}
	if got := EstTokens("abcd"); got != 1 { // 4 ascii × 0.25 = 1
		t.Errorf("EstTokens(ascii) = %d, want 1", got)
	}
}

// EstTokensMsg / EstTokensMsgs must include tool_call arguments (often the bulk of a
// large-args assistant row) — the pool's pre-routing and the turn's compaction both
// depend on this, so under-counting args would let a big prompt slip past both.
func TestEstTokensMsgs_IncludesToolArgs(t *testing.T) {
	args := `{"path":"some/file","content":"` + strings.Repeat("x", 400) + `"}`
	msgs := []contract.Message{
		{Role: "assistant", Content: "", ToolCalls: []contract.ToolCall{{Name: "t", Arguments: args}}},
	}
	if got := EstTokensMsgs(msgs); got < 90 {
		t.Errorf("EstTokensMsgs ignored the tool_call arguments: got %d, want ~100", got)
	}
}
