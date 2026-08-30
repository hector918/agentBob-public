package turn

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/i18n"
)

func TestActionBrief(t *testing.T) {
	// Distinct tool names, first-seen order; the stock floor when nothing ran.
	st := &turnState{toolsRun: []string{"web", "web", "ocr"}}
	got := actionBrief(st, "zh")
	if !strings.Contains(got, "web") || !strings.Contains(got, "ocr") {
		t.Errorf("actionBrief = %q, want it to name web + ocr", got)
	}
	if strings.Count(got, "web") != 1 {
		t.Errorf("actionBrief should list web once, got %q", got)
	}
	// The stock floor is now i18n-keyed, rendered in the passed lang.
	if actionBrief(&turnState{}, "zh") != i18n.T(salvageStockKey, "zh") {
		t.Error("empty trail should fall to the stock floor")
	}
}

func TestSalvageNudgeAndClosing(t *testing.T) {
	if !strings.Contains(salvageNudge(exitIterCap), "卡在") {
		t.Error("iter-cap nudge should steer toward an honest where-stuck answer")
	}
	if !strings.Contains(salvageNudge(exitIterCap), "禁止再调用") {
		t.Error("every nudge carries the no-more-tools core")
	}
	if honestClosing(exitIterCap) == "" {
		t.Error("iter-cap should carry a process-level closing")
	}
	if honestClosing(exitCancelled) != "" {
		t.Error("non-iter-cap states have no closing yet")
	}
}

// salvagePool forces the loop to the cap (every streamed round is empty) but
// returns a real reply on the no-tool salvage Chat call (tier 1).
type salvagePool struct {
	*fakePool
	chatReply string
}

func (salvagePool) ChatStreamWatch(context.Context, contract.ModelRequest, []contract.Message, contract.StreamWatcher) (contract.ChatResponse, error) {
	return contract.ChatResponse{}, nil // empty → Continue → iter cap
}
func (p salvagePool) Chat(context.Context, contract.ModelRequest, []contract.Message) (contract.ChatResponse, error) {
	return contract.ChatResponse{Content: p.chatReply}, nil
}

// TestRun_SalvageModelTier (dev-pg): when the turn caps out, salvage's tier-1
// no-tool model call answers, and the honest closing is appended.
func TestRun_SalvageModelTier(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	old := loopCap
	loopCap = 2
	defer func() { loopCap = old }()

	ctx := context.Background()
	sid := uscope("turnsalvagemodel")
	c := &core{store: st, pool: salvagePool{fakePool: &fakePool{}, chatReply: "我查到了 A，但卡在 B"}}
	sink := &recSink{}

	res := c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	if !strings.Contains(res.Reply, "我查到了 A") {
		t.Fatalf("salvage tier-1 reply not used: %q", res.Reply)
	}
	if !strings.Contains(res.Reply, "继续") {
		t.Errorf("honest closing not appended to a real salvage reply: %q", res.Reply)
	}
	if sink.finished != res.Reply {
		t.Errorf("finished %q != reply %q", sink.finished, res.Reply)
	}
}
