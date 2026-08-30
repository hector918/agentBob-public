package tools

import (
	"context"
	"encoding/json"
	"strings"

	"agentbob/contract"
)

// clarify asks the user a question and ENDS the turn (exit door ②) — the model
// reaches for it when it can't proceed without user input. The reply is the
// question; the turn finishes with it and waits for the user's next message.
type clarify struct{}

func (clarify) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name:        "clarify",
		Description: "需要用户澄清才能继续时，用它向用户提问并结束本轮，等待用户回复。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"要问用户的问题"}},"required":["question"]}`),
		EndsTurn:    true, // process exit (asks the user) — excluded from channel-less sub-turns
	}
}

func (clarify) Serialize() bool { return false }

func (clarify) Run(_ context.Context, _ contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var p struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(args, &p); err != nil || strings.TrimSpace(p.Question) == "" {
		return contract.ErrResult("clarify needs a non-empty question").WithHint(`{"question":"…"}`)
	}
	return contract.ToolResult{OK: true, Data: "asked", ExitRequest: &contract.ToolExit{Reply: strings.TrimSpace(p.Question)}}
}
