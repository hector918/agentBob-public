package agora

import (
	"strings"
	"testing"

	"agentbob/contract"
)

func TestParseSkillTurn(t *testing.T) {
	history := []contract.Message{
		{Role: "user", Content: "看这张超声"},
		{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "c1", Name: "skill_view", Arguments: `{"name":"ultrasound"}`}}},
		{Role: "tool", ToolCallID: "c1", ToolName: "skill_view", Content: `{"ok":true,"data":"..."}`},
		{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "c2", Name: "ocr", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "c2", ToolName: "ocr", Content: `{"ok":false,"error":"x"}`},
	}
	engaged, actions, windowMiss := parseSkillTurn(history)
	if windowMiss {
		t.Fatal("a history anchored by a user row is not a window miss")
	}
	if len(engaged) != 1 || !engaged["ultrasound"] {
		t.Fatalf("engaged = %v, want {ultrasound}", engaged)
	}
	joined := strings.Join(actions, "; ")
	if !strings.Contains(joined, "skill_view→成功") || !strings.Contains(joined, "ocr→失败") {
		t.Fatalf("actions = %q, want skill_view→成功 + ocr→失败", joined)
	}
}

// A FAILED skill_view is not an engagement; a turn with no skill_view yields nothing.
func TestParseSkillTurn_NoEngagement(t *testing.T) {
	history := []contract.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "c1", Name: "skill_view", Arguments: `{"name":"x"}`}}},
		{Role: "tool", ToolCallID: "c1", Content: `{"ok":false,"error":"unknown"}`}, // failed view
	}
	if engaged, _, _ := parseSkillTurn(history); len(engaged) != 0 {
		t.Fatalf("failed skill_view must not engage, got %v", engaged)
	}
	if engaged, _, _ := parseSkillTurn([]contract.Message{{Role: "user", Content: "hi"}}); len(engaged) != 0 {
		t.Fatal("no skill_view → no engagement")
	}
}

// A history with NO user row (the turn scrolled past the replay cap) is a window
// MISS — the snapshot must say the actions are unknown, not falsely "无工具调用".
func TestParseSkillTurn_WindowMiss(t *testing.T) {
	history := []contract.Message{
		{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "c1", Name: "ocr", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "c1", Content: `{"ok":true}`},
	}
	engaged, actions, windowMiss := parseSkillTurn(history)
	if !windowMiss {
		t.Fatal("no user row → window miss")
	}
	if len(engaged) != 0 || len(actions) != 0 {
		t.Fatalf("miss window collects nothing, got engaged=%v actions=%v", engaged, actions)
	}
	snap := buildSnapshot("q", nil, "bad", true)
	if !strings.Contains(snap, "动作未知") || strings.Contains(snap, "无工具调用") {
		t.Fatalf("window-miss snapshot must say actions unknown, not empty:\n%s", snap)
	}
}

// parseSkillTurn slices from the LAST user row — an earlier turn's skill_view must not
// leak into this turn's attribution.
func TestParseSkillTurn_ThisTurnOnly(t *testing.T) {
	history := []contract.Message{
		{Role: "user", Content: "turn1"},
		{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "a", Name: "skill_view", Arguments: `{"name":"old"}`}}},
		{Role: "tool", ToolCallID: "a", Content: `{"ok":true}`},
		{Role: "assistant", Content: "done1"},
		{Role: "user", Content: "turn2"}, // this turn starts here
		{Role: "assistant", Content: "no skill this turn"},
	}
	if engaged, _, _ := parseSkillTurn(history); len(engaged) != 0 {
		t.Fatalf("an earlier turn's skill must not leak, got %v", engaged)
	}
}

func TestEnvelopeOK(t *testing.T) {
	if !envelopeOK(`{"ok":true,"data":"x"}`) {
		t.Error("ok:true should parse true")
	}
	if envelopeOK(`{"ok":false,"error":"e"}`) {
		t.Error("ok:false should be false")
	}
	if envelopeOK(`not json`) {
		t.Error("garbage should be false")
	}
}

func TestBuildSnapshot(t *testing.T) {
	snap := buildSnapshot("用户问题", []string{"skill_view→成功", "ocr→失败"}, "没能完成", false)
	for _, want := range []string{"【用户请求】", "用户问题", "【本轮动作】", "ocr→失败", "【未达标产出】", "没能完成"} {
		if !strings.Contains(snap, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, snap)
		}
	}
	// capRunes bounds + ellipsizes.
	if got := capRunes("abcdef", 3); got != "abc…" {
		t.Fatalf("capRunes = %q, want abc…", got)
	}
}
