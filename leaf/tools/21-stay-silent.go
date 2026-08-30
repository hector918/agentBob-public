package tools

import (
	"context"
	"encoding/json"

	"agentbob/contract"
)

// staySilent ends the turn with NO reply — the silent sibling of clarify (both exit
// the turn via ExitRequest; clarify carries a question, this carries nothing). The
// model reaches for it when the topic is concluded and any reply would just be
// courtesy / acknowledgment / meaningless filler. The empty ExitRequest reply makes
// the turn deliver nothing (20-round.go: an exit with no reply is silent, no salvage);
// for an agent worker the return sink is never finished, so it emits nothing and the
// dispatch chain ends here.
type staySilent struct{}

func (staySilent) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name:        "stay_silent",
		Description: "判断本轮无需回复时调用它，直接结束、不发任何内容。适用于：话题已经聊完；对方只是道谢、确认收到或寒暄；没有新的信息或新的请求需要你回应。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		EndsTurn:    true, // process exit (empty product) — excluded from sub-turns, which must return a product
	}
}

func (staySilent) Serialize() bool { return false }

func (staySilent) Run(_ context.Context, _ contract.ToolContext, _ json.RawMessage) contract.ToolResult {
	// Empty ExitRequest reply → the turn ends silently (no Finish, no salvage).
	return contract.ToolResult{OK: true, Data: "silent", ExitRequest: &contract.ToolExit{}}
}
