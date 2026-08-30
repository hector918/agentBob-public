package compose

import (
	"fmt"
	"strings"

	"agentbob/contract"
)

// This file holds the SHARED /prompt dump rendering, so every flow's DumpPrompt assembles
// the same skeleton (system prompt + user input + tools + skills) and prepares the user
// text the same way. Each flow only differs in what it COMPOSES (its system prompt + bags
// + any extras) — the rendering and the user-text prep live here so the two flows can't
// silently diverge (the D19/D20 bug class, where one DumpPrompt forgot the turn's
// stripMemberTag / audio handling).

// DumpUserText renders the user-input text for a READ-ONLY /prompt dump. Voice/audio is
// transcribed at INGESTION now (session submit, F165), so a stored turn's transcript is
// already folded into ev.Text — the dump shows it like any other text, with no special
// audio handling. Callers that have a member sub-scope strip its addressing tag BEFORE
// calling this (mirroring RunTurn), so the dump matches the turn.
func DumpUserText(events []contract.MessageEvent) string {
	return ComposeUserText(events)
}

// RenderDump assembles the /prompt text shared by every flow: the system prompt, this
// turn's user input, optional flow-specific extras (e.g. agora's identity block, "" for
// none), then the authorized tools + skills. The flow supplies only WHAT it composed; the
// skeleton is one place so the flows can't drift.
func RenderDump(systemPrompt, userText string, tools contract.ToolSet, skills contract.SkillSet, extras string) string {
	var b strings.Builder
	b.WriteString("# 系统提示\n\n")
	b.WriteString(systemPrompt)
	b.WriteString("\n\n# 本轮用户输入\n\n")
	b.WriteString(userText)
	if strings.TrimSpace(extras) != "" {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimRight(extras, "\n"))
	}
	if tools != nil {
		specs := tools.Specs()
		fmt.Fprintf(&b, "\n\n# 工具（%d）\n", len(specs))
		for _, s := range specs {
			fmt.Fprintf(&b, "- %s：%s\n", s.Name, OneLine(s.Description))
		}
	}
	if skills != nil {
		infos := skills.List()
		fmt.Fprintf(&b, "\n# 技能（%d）\n", len(infos))
		for _, in := range infos {
			fmt.Fprintf(&b, "- %s：%s\n", in.Name, OneLine(in.Description))
		}
	}
	return b.String()
}
