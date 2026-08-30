package arrangement

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/i18n"
)

// registerSlash registers /arrangement (admin-only): tick/push cadence, cancel, status.
// UI strings are English; the command + subcommand keywords are the syntax the admin types.
func (m *Module) registerSlash(reg contract.SlashRegistry) {
	reg.Register(contract.SlashCommand{
		Name:        "arrangement",
		Description: "Task arrangements: status / tick <dur> (select cadence) / push <dur> (push interval) / maxconcurrent_for_arrangement <n> / cancel <id>",
		DescKey:     "slash.arrangement.desc",
		AdminOnly:   true,
		Handler: func(ctx context.Context, sc contract.SlashContext) error {
			fields := strings.Fields(sc.Args)
			switch {
			case len(fields) >= 2 && fields[0] == "tick":
				d, err := time.ParseDuration(fields[1])
				if err != nil {
					return sc.Sink.Finish(i18n.T("slash.arrangement.bad_duration_tick", sc.Lang, err.Error()))
				}
				return sc.Sink.Finish(i18n.T("slash.arrangement.tick_set", sc.Lang, m.setTick(d).String()))
			case len(fields) >= 2 && fields[0] == "push":
				d, err := time.ParseDuration(fields[1])
				if err != nil {
					return sc.Sink.Finish(i18n.T("slash.arrangement.bad_duration_push", sc.Lang, err.Error()))
				}
				return sc.Sink.Finish(i18n.T("slash.arrangement.push_set", sc.Lang, m.setPushInterval(d).String()))
			case len(fields) >= 2 && fields[0] == "maxconcurrent_for_arrangement":
				n, err := strconv.Atoi(fields[1])
				if err != nil {
					return sc.Sink.Finish(i18n.T("slash.arrangement.need_integer", sc.Lang, err.Error()))
				}
				return sc.Sink.Finish(i18n.T("slash.arrangement.maxconcurrent_set", sc.Lang, m.setMaxConcurrentArrangements(n)))
			case len(fields) >= 2 && fields[0] == "cancel":
				if err := m.Cancel(ctx, fields[1]); err != nil {
					return sc.Sink.Finish(i18n.T("slash.arrangement.cancel_failed", sc.Lang, err.Error()))
				}
				return sc.Sink.Finish(i18n.T("slash.arrangement.cancelled", sc.Lang, fields[1]))
			default:
				return sc.Sink.Finish(m.renderStatus(ctx, sc.Lang))
			}
		},
	})
}

func (m *Module) renderStatus(ctx context.Context, lang string) string {
	arrs, aerr := m.arrangementRows(ctx)
	items, ierr := m.store.ListLive(ctx)
	var b strings.Builder
	fmt.Fprintf(&b, i18n.T("slash.arrangement.status_header", lang),
		len(arrs), time.Duration(m.tickNanos.Load()).String(), time.Duration(m.pushNanos.Load()).String(), m.pushDepth(), m.maxConcurrentArrangements.Load())
	if aerr != nil || ierr != nil {
		// Don't render healthy-empty on a store failure — say the view may be partial
		// (same precedent as the panel detail's "items failed to load" note).
		b.WriteString(i18n.T("slash.arrangement.status_load_failed", lang))
	}
	for _, a := range arrs {
		fmt.Fprintf(&b, i18n.T("slash.arrangement.status_arr_row", lang), a.ID, a.Status, a.Company, shortOr(a.Author, "—"), strings.Join(a.Stages, "→"))
	}
	if len(items) == 0 {
		b.WriteString(i18n.T("slash.arrangement.status_no_items", lang))
		return b.String()
	}
	fmt.Fprintf(&b, i18n.T("slash.arrangement.status_items_header", lang), len(items))
	now := nowUnix()
	for _, it := range items {
		row := itemRow(it, now)
		extra := ""
		if row.InFlightSeconds > 0 {
			extra = i18n.T("slash.arrangement.status_inflight", lang, row.InFlightSeconds)
		}
		fmt.Fprintf(&b, i18n.T("slash.arrangement.status_item_row", lang), row.ItemID, row.Role, row.Status, extra, row.AgeSeconds)
	}
	return b.String()
}

// panel is the webui presence: cadence stats, the ARRANGEMENTS table (each row's glance
// shows its per-stage content), and the live/parked ITEMS table (longest in-flight first,
// per docs/arrangement.md).
func (m *Module) panel() contract.Panel {
	return contract.Panel{
		ID:        "arrangement",
		Title:     "Arrangements",
		AdminOnly: true,
		State: func(ctx context.Context) []contract.StateField {
			arrs, aerr := m.arrangementRows(ctx)
			byStatus, lerr := m.store.CountByStatus(ctx)
			liveTotal := int64(0)
			for _, n := range byStatus {
				liveTotal += n
			}
			// Parked is what's LEFT, never a list of known park statuses: `park` takes
			// a free-form status (40-impl.go only reserves the engine's own six), and
			// the engine itself mints `rejected_no_upstream`. Enumerating would leave
			// those items inside "Live items" but in none of the three splits — the
			// exact silent gap this readout exists to close. Subtraction is exhaustive
			// by construction: the three always add up to the total.
			queued, inFlight := byStatus["queued"], byStatus["in_flight"]
			parked := liveTotal - queued - inFlight
			fields := []contract.StateField{
				{Kind: "stat", Label: "Arrangements", Value: fmt.Sprintf("%d", len(arrs))},
				{Kind: "stat", Label: "Select every", Value: time.Duration(m.tickNanos.Load()).String()},
				{Kind: "stat", Label: "Push every", Value: time.Duration(m.pushNanos.Load()).String()},
				{Kind: "stat", Label: "Pending push", Value: fmt.Sprintf("%d", m.pushDepth())},
				{Kind: "stat", Label: "Max/company", Value: fmt.Sprintf("%d", m.maxConcurrentArrangements.Load())},
				{Kind: "stat", Label: "Live items", Value: fmt.Sprintf("%d", liveTotal)},
				// The work queue split out of that total: what is waiting to be claimed
				// vs what a worker is holding. "Live items" alone can't tell a healthy
				// backlog from a stalled one — 40 items is fine if they are being
				// claimed, and a problem if every one of them is parked.
				{Kind: "stat", Label: "Queued", Value: fmt.Sprintf("%d", queued)},
				{Kind: "stat", Label: "In flight", Value: fmt.Sprintf("%d", inFlight)},
				// Parked: nobody could take it (unmet), or a worker stopped it on purpose
				// (blocked / any free-form park status). None of these drain on their own —
				// the selector's self-heal or a human has to re-arm them — so any non-zero
				// is worth a colour, and the value names the statuses so "parked on what"
				// doesn't need a second query.
				{Kind: "stat", Label: "Parked", Value: parkedText(parked, byStatus), Status: warnIfPositive(parked)},
			}

			// Arrangements table — the defined pipelines + their per-stage content (glance).
			arrCells := make([][]contract.Cell, 0, len(arrs))
			for _, a := range arrs {
				// The id cell links into the arrangement's inner page (its stages, items, and
				// manage actions). Items + functions live there, not on this roster.
				idCell := contract.Cell{Text: a.ID, Open: &contract.ViewTarget{Panel: "arrangement", View: a.ID, Kind: "layer", Title: "Arrangement " + a.ID}}
				arrCells = append(arrCells, []contract.Cell{
					idCell,
					{Text: a.Company},
					{Text: shortOr(a.Author, "—")},
					{Text: a.Status, Status: arrStatusColor(a.Status)},
					{Text: strings.Join(a.Stages, " → ")},
				})
			}
			fields = append(fields, contract.StateField{
				Kind:    "table",
				Label:   "Arrangements",
				Columns: []string{"Arrangement", "Company", "Author", "Status", "Stages"},
				Rows:    arrCells,
			})
			if aerr != nil || lerr != nil {
				// Same precedent as arrangementDetail: don't render healthy-empty on a
				// store failure — flag the partial load.
				fields = append(fields, contract.StateField{Kind: "text", Label: "note", Text: "failed to load; values may be partial"})
			}
			return fields
		},
		// View opens one arrangement's inner page: its stages, live items, and manage
		// actions (cancel) — reached by clicking a roster row's id cell (Cell.Open). All
		// functions there prefill the dock for Enter-to-confirm. viewID is the arrangement id.
		View: func(ctx context.Context, viewID string) ([]contract.StateField, error) {
			return m.arrangementDetail(ctx, viewID)
		},
	}
}

// arrangementDetail builds one arrangement's inner page: its definition (stages), live/
// parked items, and the manage actions (cancel). Every action is a dock prefill (confirm
// by Enter), matching accounts/gate.
func (m *Module) arrangementDetail(ctx context.Context, id string) ([]contract.StateField, error) {
	d, err := m.store.GetDef(ctx, id)
	if err != nil {
		return nil, err
	}
	out := []contract.StateField{
		{Kind: "stat", Label: "Company", Value: d.Company},
		{Kind: "stat", Label: "Author", Value: shortOr(d.Author, "—")},
		{Kind: "status", Label: "Status", Value: d.Status, Status: arrStatusColor(d.Status)},
	}

	// Stages — the spec buckets (role + content), the arrangement's definition.
	if sp, perr := parseSpec(d.Spec); perr == nil {
		stageRows := make([][]contract.Cell, 0, len(sp.Buckets))
		for i, b := range sp.Buckets {
			stageRows = append(stageRows, []contract.Cell{
				{Text: fmt.Sprintf("%d", i+1)}, {Text: b.Role}, {Text: b.Content},
			})
		}
		out = append(out, contract.StateField{Kind: "table", Label: "Stages", Columns: []string{"#", "Role", "Content"}, Rows: stageRows})
	} else {
		// Don't silently drop the section on a corrupt spec — say so (symmetric with the
		// items-load note below).
		out = append(out, contract.StateField{Kind: "text", Label: "Stages", Text: "spec failed to parse: " + perr.Error()})
	}

	// Items — this arrangement's live/parked work.
	items, ierr := m.store.ListLiveForArrangement(ctx, id)
	now := nowUnix()
	itemRows := make([][]contract.Cell, 0, len(items))
	for _, it := range items {
		row := itemRow(it, now)
		inflight := "—"
		if row.InFlightSeconds > 0 {
			inflight = fmt.Sprintf("%ds", row.InFlightSeconds)
		}
		itemRows = append(itemRows, []contract.Cell{
			{Text: row.Role},
			{Text: row.Status, Status: statusColor(row.Status)},
			{Text: shortOr(row.ClaimedBy, "—")},
			{Text: inflight},
			{Text: fmt.Sprintf("%ds", row.AgeSeconds)},
		})
	}
	out = append(out, contract.StateField{Kind: "table", Label: "Items", Columns: []string{"Stage", "Status", "Holder", "In-flight", "Age"}, Rows: itemRows})

	// Manage — a started arrangement can be cancelled (confirm-by-prefill).
	if d.Status == "started" {
		out = append(out, contract.StateField{
			Kind: "table", Label: "Manage", Columns: []string{"action"},
			Rows: [][]contract.Cell{{{Text: "cancel arrangement", Action: "/arrangement cancel " + id, ActionLabel: "Cancel"}}},
		})
	}
	if ierr != nil {
		out = append(out, contract.StateField{Kind: "text", Label: "note", Text: "items failed to load; values may be partial"})
	}
	return out, nil
}

// statusColor maps an item status to a webui cell color. Only non-terminal items reach
// here (ListLive filters out done/cancelled), so those need no case.
func statusColor(status string) string {
	switch status {
	case "in_flight":
		return "ok"
	case "queued":
		return ""
	case "unmet", "blocked", "rejected_no_upstream":
		return "down" // needs a human's attention
	default:
		return "warn" // any other descriptive park status
	}
}

// arrStatusColor colors an arrangement's status cell.
func arrStatusColor(status string) string {
	switch status {
	case "started":
		return "ok"
	case "cancelled":
		return "down"
	default: // any other/unknown status
		return "warn"
	}
}

// parkedText renders the parked total plus which statuses hold it — everything
// non-terminal that is neither queued nor in flight, whatever the worker named
// it. Sorted so the panel doesn't reshuffle between polls.
func parkedText(parked int64, byStatus map[string]int64) string {
	if parked <= 0 {
		return "0"
	}
	statuses := make([]string, 0, len(byStatus))
	for status := range byStatus {
		if status == "queued" || status == "in_flight" {
			continue
		}
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	parts := make([]string, 0, len(statuses))
	for _, status := range statuses {
		parts = append(parts, fmt.Sprintf("%s %d", status, byStatus[status]))
	}
	return fmt.Sprintf("%d (%s)", parked, strings.Join(parts, ", "))
}

// warnIfPositive colours a count that should normally be zero.
func warnIfPositive(n int64) string {
	if n > 0 {
		return "warn"
	}
	return ""
}

func shortOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
