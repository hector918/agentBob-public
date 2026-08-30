package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"agentbob/contract"
)

// skillView reads a skill's full instruction body on demand. The prompt lists the
// available skills (name + description); the model calls this to pull the body of
// the one it decides to use. It reads ToolContext.Skills — the turn's ALREADY
// authorized projection (the flow built it via warrant), so an unauthorized or
// unknown name simply isn't there. No gating happens here; the projection is the
// gate (symmetric with how the turn dispatches only authorized tools).
type skillView struct{}

func (skillView) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name:        "skill_view",
		Description: "读取一个技能的完整说明。先从系统提示里的技能清单挑一个，再用它的名字调用，拿到该技能的操作指引后照着做。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"技能名（技能清单里列出的那个）"}},"required":["name"]}`),
		// 技能说明是模型照着做的原文，summarize 会丢规则、甚至塌成无用摘要（反复重调 → 退化墙）。
		NoAutoCompress: true,
	}
}

func (skillView) Serialize() bool { return false }

func (skillView) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return contract.ErrResult("参数解析失败：" + err.Error())
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return contract.ErrResult("缺少技能名").WithHint("传入技能清单里列出的某个 name")
	}
	if tc.Skills == nil {
		return contract.ErrResult("当前没有可用技能")
	}
	body, ok := tc.Skills.Read(name)
	if !ok {
		return contract.ErrResult("找不到或无权使用该技能：" + name).WithHint("只调用技能清单里列出的技能")
	}
	// A skill that ships scripts/templates: materialize them into the session space
	// (skills/<name>/<rel>) so the model can run them via terminal. Best-effort —
	// a missing channel or a write error just omits the note; the body still returns.
	if note := materializeSkillFiles(ctx, tc, name); note != "" {
		body += "\n\n" + note
	}
	return contract.OKResult(body)
}

// materializeSkillFiles writes the skill's auxiliary files into the session space
// under skills/<name>/ and returns a note telling the model where they landed (""
// when there are no files, no channel, or nothing was written).
func materializeSkillFiles(ctx context.Context, tc contract.ToolContext, name string) string {
	if tc.Channels == nil || tc.Skills == nil {
		return ""
	}
	files, err := tc.Skills.Files(name)
	if err != nil || len(files) == 0 {
		return ""
	}
	// Guard the skill name too (not just f.Rel below): it is the one path component
	// interpolated into base. IsLocal rejects "", absolute, and any traversal — so a
	// catalog name can't compose a path escaping skills/. (The channel re-roots anyway;
	// defense-in-depth matching the f.Rel guard.)
	if !filepath.IsLocal(name) {
		return ""
	}
	ch, err := tc.Channels.OpenFile(ctx, "")
	if err != nil {
		return ""
	}
	defer ch.Close()
	base := "skills/" + name
	wrote := 0
	for _, f := range files {
		// Path guard: Rel must stay strictly under skills/<name>/ — IsLocal rejects
		// "", absolute, and ANY interior traversal ("a/../../x"), not just a "../"
		// prefix. (forwardRel already yields clean Rels and the channel re-roots, so
		// this is defense-in-depth that matches its own claim.)
		if !filepath.IsLocal(f.Rel) {
			continue
		}
		if werr := ch.Write(ctx, base+"/"+f.Rel, f.Data); werr == nil {
			wrote++
		}
	}
	if wrote == 0 {
		return ""
	}
	return fmt.Sprintf("〔本技能的脚本/文件已就位于工作空间 `%s/` 下（只读，勿改），用 terminal 运行，例如 `python %s/scripts/<脚本名>`。〕", base, base)
}
