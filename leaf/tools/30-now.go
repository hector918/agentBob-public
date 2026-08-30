package tools

import (
	"context"
	"encoding/json"
	"time"

	"agentbob/contract"
)

// now returns the current server time — a trivial, no-resource tool used to
// validate the tool round-trip (model calls it → result fed back → model
// continues to a final reply).
type now struct{}

func (now) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name:        "now",
		Description: "返回当前服务器时间（RFC3339）。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (now) Serialize() bool { return false }

func (now) Run(_ context.Context, _ contract.ToolContext, _ json.RawMessage) contract.ToolResult {
	return contract.OKResult(time.Now().Format(time.RFC3339))
}
