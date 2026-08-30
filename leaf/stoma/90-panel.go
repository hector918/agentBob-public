package stoma

import (
	"context"
	"strconv"

	"agentbob/contract"
)

// panel is the source bus's self-description for the webui. It surfaces the
// same read-only health view as Snapshot(), translated into the generic field
// vocabulary: a count stat plus a per-source table whose "status" column is
// color-coded.
func (m *Module) panel() contract.Panel {
	return contract.Panel{
		ID:        "sources",
		Title:     "Sources",
		AdminOnly: true, // LastError may carry connection/config detail — operator data
		State: func(_ context.Context) []contract.StateField {
			snap := m.Snapshot()
			rows := make([][]contract.Cell, 0, len(snap))
			for _, s := range snap {
				rows = append(rows, []contract.Cell{
					{Text: s.Name},
					{Text: s.Status, Status: sourceStatus(s.Status)},
					{Text: strconv.Itoa(s.FailStreak)},
					{Text: s.LastError},
				})
			}
			return []contract.StateField{
				{Kind: "stat", Label: "sources", Value: strconv.Itoa(len(snap))},
				{Kind: "table", Label: "health", Columns: []string{"source", "status", "fails", "last error"}, Rows: rows},
			}
		},
	}
}

// sourceStatus maps a SourceStatus.Status string to a webui status color.
func sourceStatus(status string) string {
	switch status {
	case "ready":
		return "ok"
	case "degraded":
		return "warn"
	case "dead":
		return "down"
	default:
		return ""
	}
}
