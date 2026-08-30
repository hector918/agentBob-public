package session

import (
	"context"
	"strconv"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// panel is the session arbiter's self-description for the webui: the live
// busy/pending/dropped/tracked counts as stats.
func (m *Module) panel() contract.Panel {
	return contract.Panel{
		ID:        "session",
		Title:     "Sessions",
		AdminOnly: true,                // the running table names live scopes (who is talking to bob) — identity data
		Upstreams: []string{"sources"}, // the arbiter consumes the gateway (stoma) bus
		State: func(ctx context.Context) []contract.StateField {
			s := m.mgr.Snapshot()
			droppedStatus := ""
			if s.Dropped > 0 {
				droppedStatus = "warn"
			}
			running := m.mgr.InFlight()
			fields := []contract.StateField{
				{Kind: "stat", Label: "busy", Value: strconv.Itoa(s.Busy)},
				{Kind: "stat", Label: "pending events", Value: strconv.Itoa(s.PendingEvents)},
				// The DURABLE queue beside the in-memory one above: rows that survive a
				// crash and are replayed on boot. It normally sits near zero and drains
				// within a turn; a number that stays up means turns are not completing.
				{Kind: "stat", Label: "pending WAL", Value: walText(ctx, m.mgr)},
				{Kind: "stat", Label: "dropped", Value: strconv.Itoa(s.Dropped), Status: droppedStatus},
				{Kind: "stat", Label: "tracked sids", Value: strconv.Itoa(s.Tracked)},
				// How long the longest-running sid has been CONTINUOUSLY busy — a busy
				// count alone can sit at 1 forever and look like ordinary work. This is
				// the busy CHAIN's age, not the current turn's: a sid keeps its start
				// stamp across drain turns (an album drains one image per turn, a group
				// one sender per turn), so a healthy 10-image album legitimately reads
				// tens of minutes. What it catches is a sid that is not getting free —
				// one wedged turn, or a drain chain that never empties.
				{Kind: "stat", Label: "busy longest", Value: busyLongestText(running)},
			}
			// Running-turns table: who's working right now + how long, so an operator sees
			// live activity instead of a blank wait (a turn can take minutes).
			if len(running) > 0 {
				now := clock.Now() // same scale StartedAt was stamped on
				rows := make([][]contract.Cell, 0, len(running))
				for _, r := range running {
					rows = append(rows, []contract.Cell{
						{Text: r.Scope},
						{Text: ageText(now, r.StartedAt)},
						// TimeOfDay, not Stamp: this table only ever holds turns running
						// RIGHT NOW, so the date is the same noise on every row while the
						// seconds are what pairs with the elapsed column beside it.
						{Text: clock.TimeOfDay(r.StartedAt)},
					})
				}
				fields = append(fields, contract.StateField{
					Kind:    "table",
					Label:   "running",
					Columns: []string{"scope", "elapsed", "started"},
					Rows:    rows,
				})
			}
			return fields
		},
	}
}

// walText renders the durable pending-inbound depth. A failed read renders "?",
// never 0: a zero here says "the recovery queue is empty", which is the one
// thing we must not claim when we could not look.
func walText(ctx context.Context, mgr *Manager) string {
	n, err := mgr.PendingWALCount(ctx)
	if err != nil {
		return "?"
	}
	return strconv.Itoa(n)
}

// busyLongestText renders how long the longest continuously-busy sid has been busy
// (the same quantity the running table's "elapsed" column shows, taken at its
// maximum). Nothing running renders as a dash rather than a zero, which would
// read as "instant". It scans for the earliest start rather than taking
// InFlight()'s head: that the slice arrives oldest-first is the running TABLE's
// ordering concern, and re-sorting it for display must not silently turn this
// stat into an arbitrary sid's age.
func busyLongestText(running []InFlightRow) string {
	if len(running) == 0 {
		return "—"
	}
	oldest := running[0].StartedAt
	for _, r := range running[1:] {
		if r.StartedAt.Before(oldest) {
			oldest = r.StartedAt
		}
	}
	return ageText(clock.Now(), oldest)
}

// ageText renders how long ago t was, clamped at zero.
//
// The clamp is not paranoia: clock.Now() returns a .UTC() time, and .UTC()
// STRIPS the monotonic reading, so this is plain wall-clock arithmetic. The
// hourly calibration resync can move the offset backwards (bob's host and the
// DB host are two independently drifting clocks), which puts a turn's start
// stamp in the future — and time.Duration prints that as "-2s", which reads as
// a bug in bob rather than a hiccup in the clock. Mirrors the model panel's
// cooldown countdown, which already refuses to render a negative remainder.
func ageText(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
