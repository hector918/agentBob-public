package retrieval

import (
	"context"
	"strconv"
	"time"

	"agentbob/contract"
)

// staleOutbox is when a queue head stops reading as "busy" and starts reading as
// "stuck". The feeder ticks every few seconds, so anything the leaf accepts is
// gone within one tick; an hour-old head means nobody is accepting.
const staleOutbox = time.Hour

// panel is the cold-memory feed's self-description for the webui: the outbox
// depth, how old its head is, and whether the drain is currently failing.
//
// The drain state is the point of this panel. The feeder logs loudly on the
// up→down edge and then goes quiet (00-module.go's drainFailing latch), so a leaf
// that stays down produces exactly one WARN per bob restart — on that
// hid a 21-day outage whose only trace was the row count. This puts the same
// state where it is looked at rather than where it is logged.
// st is passed in rather than read off the Module: Start only registers this
// panel on the configured path, so the outbox always exists here — capturing it
// says so, instead of a nil check that can never fire.
func (m *Module) panel(st *outbox) contract.Panel {
	return contract.Panel{
		ID:        "retrieval",
		Title:     "Cold memory",
		AdminOnly: true, // the leaf address + failure text are deployment detail
		State: func(ctx context.Context) []contract.StateField {
			fields := []contract.StateField{
				{Kind: "stat", Label: "leaf", Value: m.cfg.BaseURL},
			}
			s, err := st.stats(ctx)
			if err != nil {
				// "?" not 0 — an unread queue must never render as an empty one.
				return append(fields,
					contract.StateField{Kind: "stat", Label: "outbox", Value: "?"},
					contract.StateField{Kind: "text", Label: "note", Text: err.Error()})
			}
			drainErr := m.drainErr()
			fields = append(fields,
				contract.StateField{
					Kind: "stat", Label: "outbox", Value: strconv.FormatInt(s.Rows, 10),
					Unit: "/ " + strconv.Itoa(outboxCap), Status: outboxStatus(s, drainErr),
				},
				contract.StateField{Kind: "stat", Label: "oldest", Value: oldestText(s)},
				contract.StateField{Kind: "stat", Label: "drain", Value: drainText(drainErr), Status: drainStatus(drainErr)},
			)
			if drainErr != "" {
				// The reason, not just the colour: this is the one field that says WHY
				// the queue stopped moving (a 404 on the leaf's embedder, on the day).
				fields = append(fields, contract.StateField{Kind: "text", Label: "last drain error", Text: drainErr})
			}
			return fields
		},
	}
}

// outboxStatus colours the depth. A non-empty outbox is normal — it fills
// between ticks — so depth alone is never a warning; what matters is a head that
// is not moving, or a drain that is failing outright.
func outboxStatus(s outboxStats, drainErr string) string {
	switch {
	case drainErr != "":
		return "down"
	case s.OldestAge >= staleOutbox:
		return "warn"
	default:
		return ""
	}
}

func oldestText(s outboxStats) string {
	if s.Rows == 0 {
		return "—"
	}
	return s.OldestAge.Round(time.Second).String()
}

func drainText(drainErr string) string {
	if drainErr != "" {
		return "failing"
	}
	return "ok"
}

func drainStatus(drainErr string) string {
	if drainErr != "" {
		return "down"
	}
	return "ok"
}
