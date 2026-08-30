package skills

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"agentbob/contract"
)

// insightSID prefixes a skill's INSIGHT.md editor setting id ("insight:<skill>"); the
// per-row Edit button addresses Read/Apply by it.
const insightSID = "insight:"

// panel is the skill catalog's self-description for the webui (docs/webui-trunk.md).
// It surfaces the loaded skills — origin (built-in vs admin-installed), which carry
// a rubric gate, and which carry learned notes — and lets a logged-in admin view/edit
// each skill's learned INSIGHT.md inline (per-row Edit → markdown editor → Read/Apply).
// AdminOnly; symmetric with leaf/tools' panel.
func (m *Module) panel() contract.Panel {
	return contract.Panel{
		ID:        "skills",
		Title:     "Skills",
		AdminOnly: true,
		State: func(_ context.Context) []contract.StateField {
			list := m.cat.List()
			rows := make([][]contract.Cell, 0, len(list))
			for _, s := range list {
				mark := ""
				if m.cat.addendum(s.Origin, s.Name) != "" {
					mark = "✓"
				}
				rubric := ""
				if sk, ok := m.cat.Get(s.Name); ok && sk.Rubric != "" {
					rubric = "✓"
				}
				rows = append(rows, []contract.Cell{
					{Text: s.Name, Edit: &contract.EditTarget{Panel: "skills", Setting: insightSID + s.Name, Lang: "markdown"}},
					{Text: string(s.Origin)},
					{Text: rubric},
					{Text: mark},
				})
			}
			return []contract.StateField{
				{Kind: "stat", Label: "skills", Value: strconv.Itoa(len(list))},
				{Kind: "table", Label: "catalog", Columns: []string{"skill", "origin", "rubric", "learned"}, Rows: rows},
			}
		},
		// Per-skill INSIGHT.md editor, opened from a row's Edit button (sid =
		// "insight:<skill>"). Not in Settings — per-row, opened contextually; the route
		// addresses Read/Apply by sid directly. Reuses the learn target's write path
		// (setAddendum + the INSIGHT.md store, which selects the origin subdir), so a hand
		// edit and a distilled rewrite share one path; an empty save clears the note.
		Read: func(_ context.Context, sid string) (string, error) {
			name, ok := strings.CutPrefix(sid, insightSID)
			if !ok {
				return "", fmt.Errorf("skills: bad setting %q", sid)
			}
			sk, exists := m.cat.Get(name)
			if !exists {
				return "", fmt.Errorf("skills: unknown skill %q", name)
			}
			return m.cat.addendum(sk.Origin, name), nil // the in-package learned-text accessor (Apply reuses skillTarget for the setAddendum+store write path)
		},
		Apply: func(ctx context.Context, sid, body string) contract.WriteResult {
			name, ok := strings.CutPrefix(sid, insightSID)
			if !ok {
				return contract.WriteResult{Phase: "validate", Error: "bad setting"}
			}
			sk, exists := m.cat.Get(name)
			if !exists {
				return contract.WriteResult{Phase: "validate", Error: "unknown skill"}
			}
			if err := (skillTarget{cat: m.cat, name: name, origin: sk.Origin, store: m.store}).Apply(ctx, body); err != nil {
				return contract.WriteResult{Phase: "write", Error: err.Error()}
			}
			return contract.WriteResult{Success: true, Phase: "write"}
		},
	}
}
