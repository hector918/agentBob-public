package turn

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"agentbob/contract"
)

func TestDedupeCalls(t *testing.T) {
	in := []contract.ToolCall{
		{ID: "a", Name: "x"},
		{ID: "a", Name: "x"}, // duplicate id → dropped
		{ID: "", Name: "y"},  // empty id → backfilled synth id
		{ID: "", Name: "z"},  // empty id → distinct synth id (distinct call, kept)
	}
	out := dedupeCalls(in)
	if len(out) != 3 {
		t.Fatalf("dedupe len = %d, want 3 (%+v)", len(out), out)
	}
	if out[0].ID != "a" {
		t.Errorf("ids[0] = %q, want a", out[0].ID)
	}
	if !strings.HasPrefix(out[1].ID, "synth-") || !strings.HasPrefix(out[2].ID, "synth-") {
		t.Errorf("backfilled ids = %q/%q, want synth- prefix", out[1].ID, out[2].ID)
	}
	if out[1].ID == out[2].ID {
		t.Errorf("backfilled ids collide within a round: %q", out[1].ID)
	}
	// Window-uniqueness: a LATER round's backfilled ids must not repeat an earlier
	// round's (id-keyed orphan repair over the whole replay window depends on it).
	later := dedupeCalls([]contract.ToolCall{{ID: "", Name: "y"}})
	if later[0].ID == out[1].ID || later[0].ID == out[2].ID {
		t.Errorf("synth id repeats across rounds: %q", later[0].ID)
	}
}

// TestDedupeCallsSanitizesArgs locks in the poison-replay guard: a tool_call with
// malformed / empty arguments JSON is neutralized to "{}" (so it can't be replayed
// as an unparseable request that fails every failover candidate), while valid
// arguments pass through untouched.
func TestDedupeCallsSanitizesArgs(t *testing.T) {
	out := dedupeCalls([]contract.ToolCall{
		{ID: "1", Name: "fs", Arguments: `{"cmd": "write", "path": "x.json"`}, // truncated → {}
		{ID: "2", Name: "now", Arguments: ``},                                 // empty → {}
		{ID: "3", Name: "corpus_import", Arguments: `{"file":"a.json"}`},      // valid → kept
	})
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	if out[0].Arguments != "{}" {
		t.Errorf("truncated args = %q, want {}", out[0].Arguments)
	}
	if out[1].Arguments != "{}" {
		t.Errorf("empty args = %q, want {}", out[1].Arguments)
	}
	if out[2].Arguments != `{"file":"a.json"}` {
		t.Errorf("valid args mutated = %q", out[2].Arguments)
	}
}

func TestTruncateBytes(t *testing.T) {
	// "é" is 2 bytes (0xc3 0xa9); a 2-byte cut of "héllo" lands mid-rune → "h".
	if got := truncateBytes("héllo", 2); got != "h" {
		t.Errorf("truncateBytes mid-rune = %q, want h", got)
	}
	if got := truncateBytes("héllo", 3); got != "hé" {
		t.Errorf("truncateBytes on boundary = %q, want hé", got)
	}
	if got := truncateBytes("hi", 10); got != "hi" {
		t.Errorf("under cap = %q, want hi", got)
	}
}

func TestBeltTruncate(t *testing.T) {
	if got := beltTruncate("small"); got != "small" {
		t.Errorf("under cap should be unchanged, got %q", got)
	}
	big := strings.Repeat("a", beltCapBytes+10_000)
	got := beltTruncate(big)
	if len(got) > beltCapBytes {
		t.Errorf("belt result %d bytes > cap %d", len(got), beltCapBytes)
	}
	if !utf8.ValidString(got) {
		t.Error("belt result is not valid utf8")
	}
	if !strings.Contains(got, "已截断") {
		t.Error("belt result missing the truncation marker")
	}
}

// summPool embeds the test pool and overrides Chat to return a canned summary.
type summPool struct {
	*fakePool
	summary string
	chatErr error
}

func (p summPool) Chat(context.Context, contract.ModelRequest, []contract.Message) (contract.ChatResponse, error) {
	if p.chatErr != nil {
		return contract.ChatResponse{}, p.chatErr
	}
	return contract.ChatResponse{Content: p.summary}, nil
}

func TestSummarize(t *testing.T) {
	c := &core{pool: summPool{fakePool: &fakePool{}, summary: "  精简版  "}}
	got := c.summarize(context.Background(), "一大段很长的工具输出")
	if !strings.Contains(got, "精简版") || !strings.Contains(got, "以下为摘要") {
		t.Errorf("summarize = %q, want a prefixed summary", got)
	}

	// Fail-safe: a model error keeps the payload verbatim, never empty.
	c2 := &core{pool: summPool{fakePool: &fakePool{}, chatErr: context.DeadlineExceeded}}
	if got := c2.summarize(context.Background(), "原文"); got != "原文" {
		t.Errorf("summarize fail-safe = %q, want verbatim 原文", got)
	}
}
