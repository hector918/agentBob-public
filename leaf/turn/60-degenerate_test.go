package turn

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"agentbob/contract"
)

func TestIsDegenerate(t *testing.T) {
	if isDegenerate("short reply") {
		t.Error("a short reply is never degenerate")
	}
	if !isDegenerate(strings.Repeat("啊", 2000)) {
		t.Error("2000× the same rune is a wall (distinct=1)")
	}
	// 2600 runes, 26 distinct, no long run → healthy.
	if isDegenerate(strings.Repeat("abcdefghijklmnopqrstuvwxyz", 100)) {
		t.Error("diverse long text is not degenerate")
	}
	// ≥15 distinct but a 700-run trips the run branch.
	runWall := strings.Repeat("abcdefghijklmnopqrst", 50) + strings.Repeat("z", 700)
	if !isDegenerate(runWall) {
		t.Error("a >600 single-rune run is a wall even with many distinct chars")
	}
}

func TestCleanseDegenerate(t *testing.T) {
	wall := strings.Repeat("啊", 2000)
	in := []contract.Message{
		{Role: "user", Content: wall},      // user rows are left alone
		{Role: "assistant", Content: wall}, // degenerate assistant → marker
		{Role: "assistant", Content: "正常回复"},
	}
	out := cleanseDegenerate(in)
	if out[0].Content != wall {
		t.Error("user row must not be cleansed")
	}
	if out[1].Content != degenDiscardedMarker {
		t.Errorf("degenerate assistant not cleansed: %q", out[1].Content[:20])
	}
	if out[2].Content != "正常回复" {
		t.Error("healthy assistant row altered")
	}
}

// degenPool streams a repetition wall (tripping the degenerate watcher) and returns
// a clean reply on the salvage Chat call.
type degenPool struct {
	*fakePool
	salvageReply string
}

func (degenPool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	wall := strings.Repeat("啊", 2000)
	accum := &contract.StreamAccumulator{Text: wall}
	if err := w(contract.StreamEvent{Text: wall}, accum); err != nil {
		// The real pool wraps a watcher trip in its non-retryable sentinel; here we
		// just propagate so runRound's errors.Is(errDegenerate) fires.
		return contract.ChatResponse{}, fmt.Errorf("pool wrap: %w", err)
	}
	return contract.ChatResponse{Content: wall}, nil
}
func (p degenPool) Chat(context.Context, contract.ModelRequest, []contract.Message) (contract.ChatResponse, error) {
	return contract.ChatResponse{Content: p.salvageReply}, nil
}

// TestRun_Degenerate (dev-pg): a streamed repetition wall is discarded (never
// finished/persisted, I11) and the turn routes to salvage.
func TestRun_Degenerate(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := uscope("turndegen")
	c := &core{store: st, pool: degenPool{fakePool: &fakePool{}, salvageReply: "已尽力给你一个结果"}}
	sink := &recSink{}

	res := c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	if strings.Contains(res.Reply, "啊啊啊") {
		t.Fatalf("degenerate wall leaked into the reply: %q", res.Reply[:30])
	}
	if res.Reply != "已尽力给你一个结果" {
		t.Fatalf("salvage reply = %q, want the clean salvage line", res.Reply)
	}
	// The wall must never have been persisted as an assistant row.
	msgs, _ := st.GetMessages(ctx, sid, 0)
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.Content, "啊啊啊") {
			t.Fatal("degenerate wall was persisted")
		}
	}
}
