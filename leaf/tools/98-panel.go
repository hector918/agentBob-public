package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"agentbob/contract"
)

// insightSID prefixes a tool's INSIGHT.md editor setting id ("insight:<tool>"); the
// per-row Edit button addresses Read/Apply by it.
const insightSID = "insight:"

// panel is the tool catalog's self-description for the webui (docs/webui-trunk.md).
// It surfaces the registered tool set and which tools have accreted learned usage
// notes (leaf/learn writes those), and lets a logged-in admin view/edit each tool's
// learned INSIGHT.md inline (per-row Edit button → markdown model editor → Read/Apply).
// AdminOnly: the learned notes are an operator surface, redacted until login.
func (m *Module) panel() contract.Panel {
	return contract.Panel{
		ID:        "tools",
		Title:     "Tools",
		AdminOnly: true,
		State: func(_ context.Context) []contract.StateField {
			names := m.cat.names()
			learned := 0
			rows := make([][]contract.Cell, 0, len(names))
			for _, n := range names {
				mark := ""
				if m.cat.addendum(n) != "" {
					mark = "✓"
					learned++
				}
				rows = append(rows, []contract.Cell{
					{Text: n, Edit: &contract.EditTarget{Panel: "tools", Setting: insightSID + n, Lang: "markdown"}},
					{Text: mark},
				})
			}
			return []contract.StateField{
				{Kind: "stat", Label: "tools", Value: strconv.Itoa(len(names))},
				{Kind: "stat", Label: "learned", Value: strconv.Itoa(learned)},
				{Kind: "stat", Label: "live channels", Value: strconv.Itoa(m.pool.live())},
				{Kind: "table", Label: "catalog", Columns: []string{"tool", "learned"}, Rows: rows},
			}
		},
		// Per-tool INSIGHT.md editor, opened from a row's Edit button (sid =
		// "insight:<tool>"). Not in Settings — it's per-row, opened contextually; the
		// route addresses Read/Apply by sid directly (mirrors agora's per-company editors).
		// Reuses the learn target's write path (setAddendum + the INSIGHT.md store), so a
		// hand edit and a distilled rewrite share one path; an empty save clears the note
		// (the store deletes an empty file).
		Read: func(_ context.Context, sid string) (string, error) {
			name, ok := strings.CutPrefix(sid, insightSID)
			if !ok {
				return "", fmt.Errorf("tools: bad setting %q", sid)
			}
			if _, exists := m.cat.Lookup(name); !exists {
				return "", fmt.Errorf("tools: unknown tool %q", name)
			}
			return m.cat.addendum(name), nil // the in-package learned-text accessor (Apply reuses toolTarget for the setAddendum+store write path)
		},
		Apply: func(ctx context.Context, sid, body string) contract.WriteResult {
			name, ok := strings.CutPrefix(sid, insightSID)
			if !ok {
				return contract.WriteResult{Phase: "validate", Error: "bad setting"}
			}
			if _, exists := m.cat.Lookup(name); !exists {
				return contract.WriteResult{Phase: "validate", Error: "unknown tool"}
			}
			if err := (toolTarget{cat: m.cat, name: name, store: m.store}).Apply(ctx, body); err != nil {
				return contract.WriteResult{Phase: "write", Error: err.Error()}
			}
			return contract.WriteResult{Success: true, Phase: "write"}
		},
	}
}
