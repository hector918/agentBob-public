package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentbob/contract"
)

type fakeExec struct{ result contract.ExecResult }

func (*fakeExec) Alive() bool  { return true }
func (*fakeExec) Close() error { return nil }
func (e *fakeExec) Run(context.Context, string) (contract.ExecResult, error) {
	return e.result, nil
}

type execOpener struct{ ch contract.ExecChannel }

func (execOpener) OpenFile(context.Context, string) (contract.FileChannel, error) {
	return nil, nil
}
func (o execOpener) OpenExec(context.Context, string) (contract.ExecChannel, error) {
	return o.ch, nil
}

func TestTerminalTool(t *testing.T) {
	tc := contract.ToolContext{Channels: execOpener{ch: &fakeExec{result: contract.ExecResult{Output: "hi", ExitCode: 0}}}}
	ctx := context.Background()

	r := (terminalTool{}).Run(ctx, tc, json.RawMessage(`{"command":"echo hi"}`))
	if !r.OK || r.Data != "hi" {
		t.Fatalf("terminal = %+v, want OK hi", r)
	}

	// non-zero exit is reported in the data, not a tool error.
	tc2 := contract.ToolContext{Channels: execOpener{ch: &fakeExec{result: contract.ExecResult{Output: "nope", ExitCode: 2}}}}
	r = (terminalTool{}).Run(ctx, tc2, json.RawMessage(`{"command":"x"}`))
	if !r.OK || !strings.Contains(r.Data, "exit code 2") {
		t.Errorf("non-zero exit = %+v, want OK with exit note", r)
	}

	// empty command + nil channels error.
	if r := (terminalTool{}).Run(ctx, tc, json.RawMessage(`{"command":"  "}`)); r.OK {
		t.Error("empty command should error")
	}
	if r := (terminalTool{}).Run(ctx, contract.ToolContext{}, json.RawMessage(`{"command":"x"}`)); r.OK {
		t.Error("nil channels should error")
	}
}
