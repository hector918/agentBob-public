package normal

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/flow/compose"
	"agentbob/heartwood/prompt"
	"agentbob/trunk"
)

// dumpFactory hands out real prompt builders so DumpPrompt renders actual layers.
type dumpFactory struct{}

func (dumpFactory) New() contract.Prompt { return prompt.NewBuilder() }

// dumpToolCat is a minimal contract.ToolCatalog for the dump test.
type dumpToolCat struct{ specs []contract.ToolSpec }

func (c dumpToolCat) Specs() []contract.ToolSpec        { return c.specs }
func (dumpToolCat) Lookup(string) (contract.Tool, bool) { return nil, false }

// allowAllWarrant is a passthrough contract.Warrant for the dump test: it authorizes
// the whole catalog. The bags now fail CLOSED without a warrant, so the test wires
// one to exercise the real authorized path (warrant present → catalog shows through).
type allowAllWarrant struct{}

func (allowAllWarrant) Grants(context.Context, string) contract.GrantSet { return nil }
func (allowAllWarrant) Check(context.Context, string, string, string) (bool, string) {
	return true, ""
}
func (allowAllWarrant) Authorize(_ context.Context, _ contract.GrantSet, specs []contract.ToolSpec) contract.ToolSet {
	return dumpToolCat{specs: specs}
}
func (allowAllWarrant) AuthorizeSkills(_ context.Context, _ contract.GrantSet, infos []contract.SkillInfo) contract.SkillSet {
	return fakeSkillSet{infos: infos}
}
func (allowAllWarrant) File(context.Context, string, string) (contract.FileChannel, error) {
	return nil, nil
}
func (allowAllWarrant) Exec(context.Context, string, string) (contract.ExecChannel, error) {
	return nil, nil
}
func (allowAllWarrant) Cut(string) {}
func (allowAllWarrant) ResolveCredential(context.Context, contract.GrantSet, string) (any, error) {
	return nil, nil
}

// TestDumpPrompt: DumpPrompt renders the assembled system prompt + this turn's user
// input + the authorized tools/skills, without running the turn. A passthrough
// warrant is wired so the bags (fail-closed without one) project the full catalog.
func TestDumpPrompt(t *testing.T) {
	reg := trunk.NewRegistry()
	trunk.Provide[contract.ToolCatalog](reg, dumpToolCat{specs: []contract.ToolSpec{
		{Name: "ocr", Description: "对图片做 OCR"},
	}})
	trunk.Provide[contract.SkillCatalog](reg, fakeSkillSet{infos: []contract.SkillInfo{
		{Name: "arxiv", Description: "搜 arXiv 论文"},
	}})
	trunk.Provide[contract.Warrant](reg, allowAllWarrant{})
	f := &flow{cmp: &compose.Composer{Prompts: dumpFactory{}}, reg: reg}

	work := contract.TurnWork{Events: []contract.MessageEvent{{Text: "你好"}}}
	dump := f.DumpPrompt(context.Background(), work)

	for _, want := range []string{
		"# 系统提示", "你好",
		"# 工具（1）", "ocr",
		"# 技能（1）", "arxiv",
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump missing %q\n---\n%s", want, dump)
		}
	}
}

// TestDumpPromptNoCatalog: with no catalog wired the bags are nil — the dump still
// renders (system prompt + user input), just without tool/skill sections.
func TestDumpPromptNoCatalog(t *testing.T) {
	f := &flow{cmp: &compose.Composer{Prompts: dumpFactory{}}, reg: trunk.NewRegistry()}
	dump := f.DumpPrompt(context.Background(), contract.TurnWork{
		Events: []contract.MessageEvent{{Text: "hi"}},
	})
	if !strings.Contains(dump, "# 系统提示") || !strings.Contains(dump, "hi") {
		t.Errorf("dump missing core sections:\n%s", dump)
	}
}

// TestDumpPrompt_LoopingDiscipline: a LOOPING-stamped session's prompt carries the
// work-discipline layer; a regular one never does (docs/turn-driver-split.md §4).
func TestDumpPrompt_LoopingDiscipline(t *testing.T) {
	f := &flow{cmp: &compose.Composer{Prompts: dumpFactory{}}, reg: trunk.NewRegistry()}

	looping := f.DumpPrompt(context.Background(), contract.TurnWork{
		TurnMode: contract.TurnModeLooping,
		Events:   []contract.MessageEvent{{Text: "干活"}},
	})
	if !strings.Contains(looping, "长任务工作模式") {
		t.Errorf("looping dump missing the work-discipline layer:\n%s", looping)
	}

	regular := f.DumpPrompt(context.Background(), contract.TurnWork{
		Events: []contract.MessageEvent{{Text: "干活"}},
	})
	if strings.Contains(regular, "长任务工作模式") {
		t.Error("regular dump must not carry the work-discipline layer")
	}
}
