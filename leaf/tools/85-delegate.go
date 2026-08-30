package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"agentbob/contract"
)

// delegate runs a self-contained sub-task in a CLEAN child context and returns only
// its product — the intermediate steps (file dumps, command output, long reads)
// never enter the current conversation (spec §7). It reads ToolContext.Sub (the
// turn core's depth-1 sub-runner); inside a sub-turn Sub refuses, so a sub can't
// delegate further. Serialize: one blocking sub-loop at a time. NoAutoCompress: the
// product must not be summarized into prose.
type delegate struct{}

// suite is a FIXED named tool set + its operation guide — the model picks a suite by
// NAME, never a free tool list (one less injection surface). delegate itself is in
// no suite (depth 1).
type suite struct {
	tools []string
	guide string
}

var suites = map[string]suite{
	"files": {
		tools: []string{"fs", "terminal"},
		guide: "你可以用 fs 读写/搜索文件（ls/cat/find/grep/mkdir/rm/mv/write）、用 terminal 跑命令来完成这个子任务。" +
			"读到的文件内容只在你这个子上下文里，处理完把结论写进产出即可。",
	},
}

func suiteNames() []string {
	out := make([]string, 0, len(suites))
	for n := range suites {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (delegate) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "delegate",
		Description: "把一个独立子任务委派到干净的子上下文里执行，只收回最终产出（中间过程不进当前对话）。" +
			"适合上下文会变脏的活（读很多文件、跑很多命令）。任务书必须自包含：" +
			"目标、约束、判定标准都要写清楚，子任务没有渠道反过来问你。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "task":  {"type": "string", "description": "自包含的任务书：要做什么、约束、判定标准"},
    "suite": {"type": "string", "enum": ["files"], "description": "子任务可用的工具套件名"},
    "skills": {"type": "array", "items": {"type": "string"}, "description": "可选：要给子任务带上的技能名"}
  },
  "required": ["task", "suite"]
}`),
		NoAutoCompress: true,
	}
}

func (delegate) Serialize() bool { return true }

func (delegate) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var a struct {
		Task   string   `json:"task"`
		Suite  string   `json:"suite"`
		Skills []string `json:"skills"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return contract.ErrResult("参数解析失败：" + err.Error())
	}
	if strings.TrimSpace(a.Task) == "" {
		return contract.ErrResult("缺少任务书 task").WithHint("把目标/约束/判定标准写成自包含的一段")
	}
	if tc.Sub == nil {
		return contract.ErrResult("当前无法委派（子循环不可用，或已在子任务内）")
	}
	s, ok := suites[a.Suite]
	if !ok {
		return contract.ErrResult("未知工具套件：" + a.Suite).WithHint("可用套件：" + strings.Join(suiteNames(), "、"))
	}

	res := tc.Sub.RunSub(ctx, contract.SubSpec{
		Task:   a.Task,
		Suite:  s.tools,
		Guide:  s.guide,
		Skills: a.Skills,
	})
	if res.Err != nil {
		return contract.ErrResult("子任务失败：" + res.Err.Error())
	}
	out := contract.OKResult(res.Product)
	if res.Partial {
		out = out.WithHint("子任务未干净收尾（撞上限/超时），产出可能不完整——可补充任务书后再委派")
	}
	return out
}
