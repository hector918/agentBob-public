package accounts

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"agentbob/contract"
	"agentbob/leaf/accounts/store"
)

// accountsPageSize is the roster page size (matches the skeleton webui).
const accountsPageSize = 50

var rosterCols = []string{"id", "name", "flow", "status", "note"}

// panel is the accounts module's self-description for the webui. It surfaces the
// roster (admin-only) as a count stat + a server-side-paged table. Usage roll-ups
// are per-account joins too costly to run for every account each tick, so they
// belong to a detail drill, not the roster — the roster stays light.
func (m *Module) panel() contract.Panel {
	return contract.Panel{
		ID:        "accounts",
		Title:     "Accounts",
		AdminOnly: true,
		State: func(ctx context.Context) []contract.StateField {
			accts, total, err := m.mgr.store.AccountRosterPage(ctx, accountsPageSize, 0)
			if err != nil {
				return []contract.StateField{{Kind: "text", Label: "accounts", Text: "load failed: " + err.Error()}}
			}
			return []contract.StateField{
				{Kind: "stat", Label: "accounts", Value: strconv.FormatInt(total, 10)},
				{
					Kind: "table", ID: "roster", Label: "roster",
					Columns: rosterCols, Rows: rosterRows(accts),
					Paged: true, Total: total, PageSize: accountsPageSize,
				},
			}
		},
		Page: func(ctx context.Context, fieldID string, limit, offset int) (contract.TablePage, error) {
			if fieldID != "roster" {
				return contract.TablePage{}, fmt.Errorf("accounts: unknown table %q", fieldID)
			}
			accts, total, err := m.mgr.store.AccountRosterPage(ctx, limit, offset)
			if err != nil {
				return contract.TablePage{}, err
			}
			return contract.TablePage{Rows: rosterRows(accts), Total: total, Limit: limit, Offset: offset}, nil
		},
		// View opens one account's detail layer (handles + usage + manage actions),
		// reached by clicking a roster row's id cell (Cell.Open). viewID is the account id.
		View: func(ctx context.Context, viewID string) ([]contract.StateField, error) {
			return m.accountDetail(ctx, viewID)
		},
	}
}

// rosterRows renders accounts into the roster table's cells.
func rosterRows(accts []store.Account) [][]contract.Cell {
	rows := make([][]contract.Cell, 0, len(accts))
	for _, a := range accts {
		flow := a.Flow
		if flow == "" {
			flow = "(pending)" // bare/onboarding account: NO entitlement yet — not "normal"
		}
		rows = append(rows, []contract.Cell{
			// the id cell is a link into the account's detail layer (handles, usage, and the
			// manage actions: toggle agora, pause/resume).
			{Text: a.ID, Open: &contract.ViewTarget{Panel: "accounts", View: a.ID, Kind: "layer", Title: "Account " + a.ID}},
			{Text: a.DisplayName}, {Text: flow},
			{Text: statusText(a.Status), Status: statusColor(a.Status)},
			{Text: a.Note},
		})
	}
	return rows
}

func statusText(s string) string {
	if s == "" {
		return "active"
	}
	return s
}

func statusColor(s string) string {
	if s == "paused" {
		return "warn"
	}
	return "ok"
}

// accountDetail builds one account's detail layer: identity + handles + usage, plus the
// manage-action buttons (toggle agora entitlement, pause/resume). All labels in English.
func (m *Module) accountDetail(ctx context.Context, id string) ([]contract.StateField, error) {
	a, err := m.mgr.store.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	out := []contract.StateField{
		{Kind: "stat", Label: "name", Value: a.DisplayName},
		{Kind: "stat", Label: "flows", Value: strings.Join(contract.EntitledFlows(a.Flow), ", ")},
		{Kind: "status", Label: "status", Value: statusText(a.Status), Status: statusColor(a.Status)},
	}

	// Handles.
	hs, herr := m.mgr.store.HandlesForAccount(ctx, id)
	handleRows := make([][]contract.Cell, 0, len(hs))
	for _, h := range hs {
		handleRows = append(handleRows, []contract.Cell{{Text: h.Source}, {Text: h.PlatformUID}, {Text: h.BoundSource}})
	}
	out = append(out, contract.StateField{
		Kind: "table", Label: "handles",
		Columns: []string{"source", "handle", "bound via"}, Rows: handleRows,
	})

	// Usage (per-kind token in/out).
	u, uerr := m.mgr.store.AccountUsageBreakdown(ctx, id)
	usageRows := make([][]contract.Cell, 0, len(u.TokensByKind))
	for _, kind := range slices.Sorted(maps.Keys(u.TokensByKind)) {
		kt := u.TokensByKind[kind]
		usageRows = append(usageRows, []contract.Cell{
			{Text: kind}, {Text: strconv.FormatInt(kt.In, 10)}, {Text: strconv.FormatInt(kt.Out, 10)},
		})
	}
	out = append(out, contract.StateField{
		Kind: "table", Label: "usage (tokens)",
		Columns: []string{"kind", "in", "out"}, Rows: usageRows,
	})

	// Manage actions: each row is a labeled button prefilling a slash command. A pending
	// (flow-less) account leads with the one-click Approve (grant access + allowlist).
	// A PENDING (flow-less) account gets ONLY the Approve button: the agora
	// toggle would set flow="agora" (no normal, no allowlist), un-pend the
	// account and dead-end the one-click approval (Approve early-returns on any
	// existing entitlement) — a mis-click trap. Toggles appear after approval.
	manageRows := make([][]contract.Cell, 0, 3)
	if strings.TrimSpace(a.Flow) == "" {
		manageRows = append(manageRows, []contract.Cell{approveCell(a)})
		// F42: a PENDING account can ALSO be paused. Approve grants FLOW but never
		// un-pauses (status ⊥ flow), so a paused-pending account needs a Resume button
		// too — otherwise the admin's one-click Approve looks inert and there is no
		// surfaced way to lift the block.
		if a.Status == "paused" {
			manageRows = append(manageRows, []contract.Cell{statusToggleCell(a)})
		}
	} else {
		manageRows = append(manageRows, []contract.Cell{agoraToggleCell(a)}, []contract.Cell{statusToggleCell(a)})
	}
	out = append(out, contract.StateField{
		Kind: "table", Label: "manage",
		Columns: []string{"action"},
		Rows:    manageRows,
	})

	if herr != nil || uerr != nil { // a store hiccup must not read as "no handles / no usage"
		out = append(out, contract.StateField{Kind: "text", Label: "note", Text: "some detail failed to load; values may be partial"})
	}
	return out, nil
}

// approveCell builds the one-click Approve button for a pending (flow-less) account:
// /accounts approve grants normal (+ agora when its onboard scope is an agora scope) and
// allowlists it. Shown only while the account has no flow.
func approveCell(a store.Account) contract.Cell {
	return contract.Cell{Text: "pending approval", Action: "/accounts approve " + a.ID, ActionLabel: "Approve"}
}

// agoraToggleCell builds the enable/disable-agora button, preserving the account's other
// entitlements (toggle only the agora element).
func agoraToggleCell(a store.Account) contract.Cell {
	entitled := contract.EntitledFlows(a.Flow)
	if slices.Contains(entitled, "agora") {
		kept := make([]string, 0, len(entitled))
		for _, f := range entitled {
			if f != "agora" {
				kept = append(kept, f)
			}
		}
		// No floor anymore: an agora-ONLY account leaves kept empty. cmdFlow rejects an
		// empty flows arg (its <2-args guard), so send the explicit "none" clear sentinel
		// (cmdFlow maps it back to "") — otherwise "Disable agora" would silently no-op.
		flows := strings.Join(kept, ",")
		if flows == "" {
			flows = "none"
		}
		return contract.Cell{Text: "agora entitlement", Action: "/accounts flow " + a.ID + " " + flows, ActionLabel: "Disable agora"}
	}
	next := append(append([]string{}, entitled...), "agora")
	return contract.Cell{Text: "agora entitlement", Action: "/accounts flow " + a.ID + " " + strings.Join(next, ","), ActionLabel: "Enable agora"}
}

// statusToggleCell builds the pause/resume button.
func statusToggleCell(a store.Account) contract.Cell {
	if a.Status == "paused" {
		return contract.Cell{Text: "account status", Action: "/accounts resume " + a.ID, ActionLabel: "Resume"}
	}
	return contract.Cell{Text: "account status", Action: "/accounts pause " + a.ID, ActionLabel: "Pause"}
}
