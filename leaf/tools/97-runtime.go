package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentbob/contract"
)

// runtime_state is an admin-only, READ-ONLY self-inspection tool: it projects the
// live in-process runtime state the webui panels already expose
// (contract.PanelRegistry) as text the MODEL can read, so agentbob can debug ITSELF
// inside the admin conversation ("which model backend is down", "how deep is the
// queue", "why did that source error"). It is NOT a new data source — it renders the
// SAME Panel.State() closures the webui polls each tick, one panel = one section.
//
// Admin-only is enforced the standard, code-free way (docs/tools-warrant-credentials.md,
// project-tier-admin-removed): the capability "tool:use:runtime_state" reconciles into
// permissions.yaml default-OFF for every principal (leaf/warrant reconcile); the admin
// flips it ON for the "admin" principal only. The normal flow mints "admin"/"user" as
// the warrant principal (flowIdentity → ev.IsAdmin), and toolBag = warrant.Authorize
// over that principal's grants — so the tool lands in the bag ONLY on admin turns. No
// code gate, no ToolContext.IsAdmin bit: the grant matrix IS the gate.
//
// Read-only + credential-free by construction: StateField is the webui DISPLAY
// projection — a DSN / api key / cookie never enters one — so reusing panels satisfies
// "runtime state must not leak credentials" for free. The tool only ever READS panels;
// it never touches their Apply/action faces.
type runtimeStateTool struct{ panels contract.PanelRegistry }

// runtimeSectionCap bounds one section's rendered text; runtimeTableRows caps rows
// per table field; runtimeFieldCap / runtimeCellCap cap a SINGLE field's text and a
// single cell so one oversized panel value (a long error dump, a yaml body) cannot
// blow past runtimeSectionCap by its full size — the between-field cap only stops
// accumulation, not one giant field. A runtime dump is a diagnostic glance, not a
// full export.
const (
	runtimeSectionCap = 12000
	runtimeTableRows  = 40
	runtimeFieldCap   = 4000
	runtimeCellCap    = 500
)

func (runtimeStateTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "runtime_state",
		Description: "只读查看 agentbob 当前进程的运行态，用于自我诊断（模型池后端健康、会话、来源、账号、队列等）。" +
			"历史不会自动进入上下文——不传 section 时列出所有 section 及一行摘要，再传 section 读该 section 的完整状态。" +
			"example: {\"section\":\"model-pool\"}",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "section": {"type": "string", "description": "要读的 section 名（= 运行态面板 ID，如 model-pool）；省略则先列出所有 section"}
  }
}`),
		NoAutoCompress: true, // 运行态是诊断依据，不可被摘要转述
	}
}

// Serialize false: read-only snapshot, safe alongside other calls.
func (runtimeStateTool) Serialize() bool { return false }

func (t runtimeStateTool) Run(ctx context.Context, _ contract.ToolContext, args json.RawMessage) contract.ToolResult {
	if t.panels == nil {
		return contract.ErrResult("运行态自省不可用（无面板注册表）")
	}
	var a struct {
		Section string `json:"section"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return contract.ErrResult("参数解析失败：" + err.Error())
		}
	}
	panels := t.panels.Panels() // sorted by ID
	if len(panels) == 0 {
		return contract.ErrResult("当前没有任何运行态 section")
	}
	sec := strings.TrimSpace(a.Section)
	if sec == "" {
		return contract.OKResult(runtimeIndex(ctx, panels))
	}
	for _, p := range panels {
		if p.ID == sec {
			return contract.OKResult(renderSection(ctx, p))
		}
	}
	ids := make([]string, 0, len(panels))
	for _, p := range panels {
		ids = append(ids, p.ID)
	}
	return contract.ErrResult("未知 section：" + sec).WithHint("可用 section：" + strings.Join(ids, ", "))
}

// runtimeIndex is the section manifest: one line per panel with a compact glance of
// its stat/status fields, so a single no-arg call is an at-a-glance health board.
func runtimeIndex(ctx context.Context, panels []contract.Panel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 运行态 section 清单（共 %d 个）\n", len(panels))
	for _, p := range panels {
		fields := safeState(ctx, p)
		fmt.Fprintf(&b, "[%s] %s", p.ID, p.Title)
		if s := glance(fields); s != "" {
			b.WriteString(" — " + s)
		}
		b.WriteString("\n")
	}
	b.WriteString("用 action：{\"section\":\"<上面某个 ID>\"} 读某个 section 的完整状态。")
	return b.String()
}

// glance renders a one-line summary from a panel's stat/status fields (the quick
// numbers). Skips text/table/graph — those need the full section read.
func glance(fields []contract.StateField) string {
	var parts []string
	for _, f := range fields {
		if f.Kind != "stat" && f.Kind != "status" {
			continue
		}
		v := strings.TrimSpace(f.Value + " " + f.Unit)
		if v == "" {
			v = f.Status
		}
		if v == "" {
			continue
		}
		parts = append(parts, f.Label+"="+v+statusMark(f.Status))
		if len(parts) >= 6 {
			parts = append(parts, "…")
			break
		}
	}
	return strings.Join(parts, "  ")
}

// renderSection renders one panel's full State() as text, bounded by runtimeSectionCap.
func renderSection(ctx context.Context, p contract.Panel) string {
	fields := safeState(ctx, p)
	var b strings.Builder
	fmt.Fprintf(&b, "# %s [%s]\n", p.Title, p.ID)
	if len(fields) == 0 {
		b.WriteString("（此 section 当前无运行态字段）")
		return b.String()
	}
	for _, f := range fields {
		if b.Len() > runtimeSectionCap {
			b.WriteString("\n[...本 section 输出达到上限，其余字段略]")
			break
		}
		renderField(&b, f)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderField appends one StateField to b, dispatching on Kind.
func renderField(b *strings.Builder, f contract.StateField) {
	switch f.Kind {
	case "stat":
		v := strings.TrimSpace(f.Value + " " + f.Unit)
		fmt.Fprintf(b, "- %s: %s%s\n", f.Label, v, statusMark(f.Status))
	case "status":
		v := f.Value
		if strings.TrimSpace(v) == "" {
			v = f.Status
		}
		fmt.Fprintf(b, "- %s: %s%s\n", f.Label, v, statusMark(f.Status))
	case "text":
		fmt.Fprintf(b, "## %s\n%s\n", f.Label, capRunes(strings.TrimSpace(f.Text), runtimeFieldCap))
	case "table":
		renderTable(b, f)
	case "graph":
		renderGraph(b, f)
	default:
		if f.Label != "" {
			fmt.Fprintf(b, "- %s\n", f.Label)
		}
	}
}

func renderTable(b *strings.Builder, f contract.StateField) {
	fmt.Fprintf(b, "## %s", f.Label)
	if f.Paged && f.Total > int64(len(f.Rows)) {
		fmt.Fprintf(b, "（共 %d 行，显示前 %d）", f.Total, len(f.Rows))
	} else {
		fmt.Fprintf(b, "（%d 行）", len(f.Rows))
	}
	b.WriteString("\n")
	if len(f.Columns) > 0 {
		b.WriteString("| " + strings.Join(f.Columns, " | ") + " |\n")
	}
	for i, row := range f.Rows {
		if i >= runtimeTableRows {
			fmt.Fprintf(b, "| …其余 %d 行略 |\n", len(f.Rows)-runtimeTableRows)
			break
		}
		cells := make([]string, 0, len(row))
		for _, c := range row {
			cell := capRunes(strings.ReplaceAll(strings.TrimSpace(c.Text), "\n", " "), runtimeCellCap)
			cell += statusMark(c.Status)
			if d := strings.ReplaceAll(strings.TrimSpace(c.Detail), "\n", " "); d != "" {
				cell += " ⓘ" + capRunes(d, runtimeCellCap)
			}
			cells = append(cells, cell)
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
}

func renderGraph(b *strings.Builder, f contract.StateField) {
	fmt.Fprintf(b, "## %s（图：%d 节点）\n", f.Label, len(f.Nodes))
	for i, n := range f.Nodes {
		if i >= runtimeTableRows {
			fmt.Fprintf(b, "- …其余 %d 节点略\n", len(f.Nodes)-runtimeTableRows)
			break
		}
		line := n.Title
		if n.Band != "" {
			line = n.Band + "/" + line
		}
		if n.Sub != "" {
			line += " (" + n.Sub + ")"
		}
		fmt.Fprintf(b, "- %s%s\n", line, statusMark(n.Status))
	}
}

// statusMark renders a status token as a compact suffix; "" / "ok" → nothing shown
// in a way that clutters (ok is the common case).
func statusMark(status string) string {
	switch status {
	case "", "ok":
		return ""
	case "warn":
		return " ⚠️warn"
	case "down":
		return " ❌down"
	default:
		return " [" + status + "]"
	}
}

// capRunes clips s to at most n runes (rune-safe: a runtime value may be CJK and a
// byte cut would split a rune → invalid UTF-8 some model endpoints hard-4xx on),
// appending an elision marker when it truncates.
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…[已截断]"
}

// safeState calls a panel's State() with panic isolation — a single misbehaving
// panel must not crash the whole introspection (same posture as the webui's
// pollPanel). A nil State closure (contract forbids it, but be defensive) yields no
// fields.
func safeState(ctx context.Context, p contract.Panel) (fields []contract.StateField) {
	if p.State == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			fields = []contract.StateField{{Kind: "text", Label: "panel error", Text: fmt.Sprintf("此 section 取数 panic：%v", r)}}
		}
	}()
	return p.State(ctx)
}

var _ contract.Tool = runtimeStateTool{}
