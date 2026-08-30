package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentbob/contract"
)

// terminalTool runs a shell command in a space. Each call is an INDEPENDENT
// `bash -c` (localExecChannel.Run) — cwd / env do NOT persist across calls; only
// files written into the space persist (the same space's file channel sees them, so
// write-then-run works). Need state across steps? Chain it in one command (cd … &&
// …, leading exports). A non-zero exit is reported (the command ran) — only a channel
// error (timeout / dead session) is a tool failure. (A real continuous pty session is
// future work; not built.)
type terminalTool struct{}

func (terminalTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name:        "terminal",
		Description: "在空间里跑 shell 命令。每次调用是独立的 bash -c：cwd/env 不跨命令保留，只有写进空间的文件持久。需要跨步骤状态就串进同一条命令（cd … && …、前置 export）。command=要跑的命令；space 留空=默认空间。",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"command":{"type":"string","description":"shell 命令"},` +
			`"space":{"type":"string","description":"空间名，留空=默认空间"}` +
			`},"required":["command"]}`),
	}
}

// Serialize: a shell session mutates global state (cwd, env), so terminal calls
// run one at a time within a round.
func (terminalTool) Serialize() bool { return true }

func (terminalTool) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	if tc.Channels == nil {
		return contract.ErrResult("terminal is not available in this turn")
	}
	var p struct {
		Command, Space string
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return contract.ErrResult("bad arguments: " + err.Error())
	}
	if strings.TrimSpace(p.Command) == "" {
		return contract.ErrResult("empty command")
	}
	ch, err := tc.Channels.OpenExec(ctx, p.Space)
	if err != nil {
		return contract.ErrResult("open shell: " + err.Error())
	}
	res, err := ch.Run(ctx, p.Command)
	if err != nil {
		// timeout / dead session — a real tool failure (the partial output helps).
		return contract.ErrResult("shell: " + err.Error()).WithHint(res.Output)
	}
	out := res.Output
	if res.ExitCode != 0 {
		out += fmt.Sprintf("\n[exit code %d]", res.ExitCode)
	}
	return contract.OKResult(out)
}
