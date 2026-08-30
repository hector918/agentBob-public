package agora

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// chatPageSize bounds one page of the inbox chat-log table. Sized so a full page of
// single-line rows fills most of the glance frame and the paging footer still shows
// (rows are flattened to one line each — see chatRows). Newest rows first.
const chatPageSize = 24

// chatPanel is the agora chat-log reader's webui self-description, registered by
// THIS flow module (the composition lives in the flow/strategy layer): it resolves
// an inbox to its chat scopes via contract.Agora and pages the merged history via
// contract.ChatHistory. leaf/agora (the org graph) links here with a GraphAction.Open
// on each inbox node; the org graph and the message store stay decoupled — neither
// knows the other, the flow joins them. AdminOnly (conversation content).
//
// Tier "none": this is NOT a top-level subsystem — chat history is a LEAF of the
// company→member→inbox drill-down, reached only via an inbox node's "chat log". So
// the panel is registered (addressable for View/Page) but HIDDEN from the home graph;
// it would otherwise masquerade as a third flow. A View("inbox:<id>") returns the
// first page as a Paged table with field ID "chat:<id>"; later pages via Page.
func (f *flow) chatPanel() contract.Panel {
	return contract.Panel{
		ID:        "agora-chat",
		Title:     "Agora Chat",
		AdminOnly: true,
		Tier:      "none", // hidden sub-panel (see above) — not drawn as a home node
		State:     func(_ context.Context) []contract.StateField { return nil },
		View: func(ctx context.Context, viewID string) ([]contract.StateField, error) {
			inboxID, ok := strings.CutPrefix(viewID, "inbox:")
			if !ok || inboxID == "" {
				return nil, fmt.Errorf("agora-chat: bad view %q", viewID)
			}
			msgs, total, err := f.inboxPage(ctx, inboxID, chatPageSize, 0)
			if err != nil {
				return nil, err
			}
			if total == 0 {
				return []contract.StateField{{Kind: "text", Label: "chat log", Text: "(no conversation yet)"}}, nil
			}
			return []contract.StateField{{
				Kind: "table", ID: "chat:" + inboxID, Label: "chat log",
				Columns:  []string{"time", "who", "message"},
				Rows:     chatRows(msgs),
				Paged:    true,
				Total:    total,
				PageSize: chatPageSize,
				RowCopy:  true, // hover-highlight a row, click to copy it
			}}, nil
		},
		Page: func(ctx context.Context, fieldID string, limit, offset int) (contract.TablePage, error) {
			inboxID, ok := strings.CutPrefix(fieldID, "chat:")
			if !ok || inboxID == "" {
				return contract.TablePage{}, fmt.Errorf("agora-chat: bad table %q", fieldID)
			}
			if limit <= 0 {
				limit = chatPageSize
			}
			msgs, total, err := f.inboxPage(ctx, inboxID, limit, offset)
			if err != nil {
				return contract.TablePage{}, err
			}
			return contract.TablePage{Rows: chatRows(msgs), Total: total, Limit: limit, Offset: offset}, nil
		},
	}
}

// inboxPage composes one page of an inbox's chat log: inbox → its chat scopes (agora)
// → merged paged history (ChatHistory). Either dependency absent → empty (the webui
// shows "no conversation"); a non-agora deploy never reaches here.
func (f *flow) inboxPage(ctx context.Context, inboxID string, limit, offset int) ([]contract.Message, int64, error) {
	ag, chat := f.ag(), f.chat()
	if ag == nil || chat == nil {
		return nil, 0, nil
	}
	scopes := ag.InboxScopes(ctx, inboxID)
	if len(scopes) == 0 {
		return nil, 0, nil
	}
	return chat.MessagesForScopes(ctx, scopes, limit, offset)
}

// chatRows renders messages into the two-column table ("who", "message"). A bare
// tool-call assistant row (no text) shows the tool names; a tool result is labeled
// by the tool it answers.
func chatRows(msgs []contract.Message) [][]contract.Cell {
	rows := make([][]contract.Cell, 0, len(msgs))
	for _, m := range msgs {
		who, body := m.Role, m.Content
		if body == "" && len(m.ToolCalls) > 0 {
			names := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Name)
			}
			body = "🔧 " + strings.Join(names, ", ")
		}
		if m.Role == "tool" && m.ToolName != "" {
			who = "tool:" + m.ToolName
		}
		ts := ""
		if m.TimeUnix > 0 {
			ts = clock.Stamp(time.Unix(int64(m.TimeUnix), 0))
		}
		// One row = one line: collapse newlines so a multi-line message doesn't make
		// a tall row that overflows the glance frame (the webui clips the single line
		// to the column width).
		rows = append(rows, []contract.Cell{{Text: ts}, {Text: who}, {Text: oneLine(body)}})
	}
	return rows
}

// oneLine flattens any whitespace run (incl. newlines) to a single space and trims —
// so a chat row stays one line in the table.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
