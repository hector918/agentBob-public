package tools

import (
	"context"
	"encoding/json"
	"strings"

	"agentbob/contract"
)

// The arrangement tools (docs/arrangement.md §tools): thin shells over
// contract.Arrangements (leaf/arrangement), reached lazily (this repo has no
// ToolRegistry). All keyed by tc.Scope. Absent arrangement module → the tools report
// the feature is off.

// ---- define (privileged: can author for any company; the grant IS the trust boundary) ----

type arrangementDefine struct{ a func() contract.Arrangements }

func (arrangementDefine) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "arrangement_define",
		Description: "新建一条任务编排(orchestration)。company=目标公司名;spec=一份 JSON:" +
			`{"buckets":[{"role":"岗位名","content":"这个岗位要做什么/验收要求"}, ...]},桶按数组顺序串成流程。` +
			"创建即自动开跑(自动投一件入口任务,从第一个环节开始流转),运行后只能取消、不能改。",
		SideEffect: true,
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "company": {"type": "string", "description": "目标公司名"},
    "spec":    {"type": "string", "description": "编排 JSON：{\"buckets\":[{\"role\":..,\"content\":..}]}"}
  },
  "required": ["company", "spec"]
}`),
	}
}

func (arrangementDefine) Serialize() bool { return false }

func (t arrangementDefine) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	a := resolveArrangements(t.a)
	if a == nil {
		return contract.ErrResult("arrangement_define: 任务编排未启用")
	}
	var in struct {
		Company string `json:"company"`
		Spec    string `json:"spec"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return contract.ErrResult("arrangement_define: 参数解析失败：" + err.Error())
	}
	id, err := a.Define(ctx, contract.ArrangeDefine{CallerScope: tc.Scope, Company: strings.TrimSpace(in.Company), Spec: in.Spec})
	if err != nil {
		return contract.ErrResult("arrangement_define: " + err.Error())
	}
	return contract.OKResult("已创建并启动编排 id=" + id)
}

// ---- inject ----

type arrangementInject struct{ a func() contract.Arrangements }

func (arrangementInject) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "arrangement_inject",
		Description: "把一个新任务投放进某条编排。arrangement_id=编排 id;payload=任务内容;" +
			"stage 可选(默认从第一个环节开始;填某个环节名则直接落到那个环节——用于把一批拆成多条小任务投到下游环节);priority 可选(越大越先)。",
		SideEffect: true,
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "arrangement_id": {"type": "string", "description": "编排 id"},
    "payload":        {"type": "string", "description": "任务内容(UTF-8 纯文本)"},
    "stage":          {"type": "string", "description": "可选:直接投到的环节名(默认入口);扇出时用,把子任务投到下游环节"},
    "priority":       {"type": "integer", "description": "可选:优先级,越大越先(默认 0)"}
  },
  "required": ["arrangement_id", "payload"]
}`),
	}
}

func (arrangementInject) Serialize() bool { return false }

func (t arrangementInject) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	a := resolveArrangements(t.a)
	if a == nil {
		return contract.ErrResult("arrangement_inject: 任务编排未启用")
	}
	var in struct {
		ArrangementID string `json:"arrangement_id"`
		Payload       string `json:"payload"`
		Stage         string `json:"stage"`
		Priority      int    `json:"priority"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return contract.ErrResult("arrangement_inject: 参数解析失败：" + err.Error())
	}
	id, err := a.Inject(ctx, contract.ArrangeInject{CallerScope: tc.Scope, ArrangementID: strings.TrimSpace(in.ArrangementID), Stage: strings.TrimSpace(in.Stage), Payload: in.Payload, Priority: in.Priority})
	if err != nil {
		return contract.ErrResult("arrangement_inject: " + err.Error())
	}
	return contract.OKResult("已投放,任务 id=" + id)
}

// ---- pull ----

type arrangementPull struct{ a func() contract.Arrangements }

func (arrangementPull) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "arrangement_pull",
		Description: "从某条编排的某个环节领取一个待处理任务(领到即归你)。arrangement_id + role。" +
			"返回含 item_id、content(本环节的要求/指令)、payload;处理完用 arrangement_submit 交回。空则无需动作。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "arrangement_id": {"type": "string", "description": "编排 id"},
    "role":           {"type": "string", "description": "你的环节(岗位)名"}
  },
  "required": ["arrangement_id", "role"]
}`),
	}
}

func (arrangementPull) Serialize() bool { return false }

func (t arrangementPull) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	a := resolveArrangements(t.a)
	if a == nil {
		return contract.ErrResult("arrangement_pull: 任务编排未启用")
	}
	var in struct {
		ArrangementID string `json:"arrangement_id"`
		Role          string `json:"role"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return contract.ErrResult("arrangement_pull: 参数解析失败：" + err.Error())
	}
	item, ok, err := a.Pull(ctx, contract.ArrangePull{CallerScope: tc.Scope, ArrangementID: strings.TrimSpace(in.ArrangementID), Role: strings.TrimSpace(in.Role)})
	if err != nil {
		return contract.ErrResult("arrangement_pull: " + err.Error())
	}
	if !ok {
		return contract.OKResult(`{"empty":true}`).WithHint("队列已空(可能被同事取走),无需处理,可调用 stay_silent 结束")
	}
	out, _ := json.Marshal(struct {
		ItemID  string `json:"item_id"`
		Content string `json:"content"`
		Payload string `json:"payload"`
	}{item.ItemID, item.Content, item.Payload})
	return contract.OKResult(string(out))
}

// ---- submit ----

type arrangementSubmit struct{ a func() contract.Arrangements }

func (arrangementSubmit) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "arrangement_submit",
		Description: "把处理完的产出交回编排。item_id=领取时拿到的 id;output=你的产出。" +
			"result:forward=通过/往下一环节(默认);back=打回上一环节(写 reason 理由);park=做不动则停泊(写 status 如 blocked + reason)。",
		SideEffect: true,
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "item_id": {"type": "string", "description": "领取时拿到的任务 id"},
    "output":  {"type": "string", "description": "工作产出(UTF-8 纯文本)"},
    "result":  {"type": "string", "description": "forward 往下 / back 打回 / park 停泊;默认 forward"},
    "status":  {"type": "string", "description": "result=park 时的停泊状态(如 blocked),默认 blocked"},
    "reason":  {"type": "string", "description": "back/park 时的理由(随件带给后续/重做者)"}
  },
  "required": ["item_id"]
}`),
	}
}

func (arrangementSubmit) Serialize() bool { return false }

func (t arrangementSubmit) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	a := resolveArrangements(t.a)
	if a == nil {
		return contract.ErrResult("arrangement_submit: 任务编排未启用")
	}
	var in struct {
		ItemID string `json:"item_id"`
		Output string `json:"output"`
		Result string `json:"result"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return contract.ErrResult("arrangement_submit: 参数解析失败：" + err.Error())
	}
	if strings.TrimSpace(in.ItemID) == "" {
		return contract.ErrResult("arrangement_submit: item_id 不能为空")
	}
	// Validate the routing verb up front (impl's default-forward only covers ""): an
	// unrecognized result would otherwise be silently treated as forward.
	result := strings.TrimSpace(in.Result)
	switch result {
	case "", "forward", "back", "park":
	default:
		return contract.ErrResult("arrangement_submit: result 只能是 forward(往下)/back(打回)/park(停泊),或留空(默认往下)")
	}
	err := a.Submit(ctx, contract.ArrangeSubmit{
		CallerScope: tc.Scope, ItemID: strings.TrimSpace(in.ItemID), Output: in.Output,
		Result: result, Status: strings.TrimSpace(in.Status), Reason: in.Reason,
	})
	if err != nil {
		return contract.ErrResult("arrangement_submit: " + err.Error())
	}
	return contract.OKResult("已交回")
}

func resolveArrangements(thunk func() contract.Arrangements) contract.Arrangements {
	if thunk == nil {
		return nil
	}
	return thunk()
}
