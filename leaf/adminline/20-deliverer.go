package adminline

import (
	"context"
	"fmt"
	"strings"

	"agentbob/contract"
	"agentbob/leaf/adminline/store"
)

// Deliverer forwards an admin alert to the attached target. nil = unattached
// (alerts are persisted but never forwarded).
type Deliverer interface {
	Deliver(ctx context.Context, a store.AdminAlert) error
}

// renderAlert formats an alert as readable text.
func renderAlert(a store.AdminAlert) string {
	return fmt.Sprintf("[%s] %s: %s", strings.ToUpper(string(a.Level)), a.Origin, a.Text)
}

// sourceDeliverer forwards an alert as a plain outbound message to a configured
// chat — a human reads it, no agent turn. Written against contract.Source.Send so
// adminline never imports a source package.
type sourceDeliverer struct {
	src    contract.Source
	target contract.Target
}

func (d *sourceDeliverer) Deliver(ctx context.Context, a store.AdminAlert) error {
	return d.src.Send(ctx, d.target, renderAlert(a))
}

// NewDeliverer picks a Deliverer from the outlet spec, resolving a source by name
// through byName (the bus). Forms:
//
//   - ""                         → (nil, nil)  unattached (persist only)
//   - "<source>:<chat_id>"       → forward via that source's Send (DM yourself)
//
// A bad outlet (unparseable, or naming a source that isn't enabled) returns an error;
// the caller logs + degrades to unattached (a bad outlet must never block boot).
func NewDeliverer(outlet string, byName func(string) contract.Source) (Deliverer, error) {
	outlet = strings.TrimSpace(outlet)
	if outlet == "" {
		return nil, nil
	}
	name, chatID, ok := strings.Cut(outlet, ":")
	name, chatID = strings.TrimSpace(name), strings.TrimSpace(chatID)
	if !ok || name == "" || chatID == "" {
		return nil, fmt.Errorf("admin-line: outlet %q is not \"<source>:<chat_id>\"", outlet)
	}
	src := byName(name)
	if src == nil {
		return nil, fmt.Errorf("admin-line: outlet %q names source %q, which is not enabled", outlet, name)
	}
	return &sourceDeliverer{src: src, target: contract.Target{Source: name, ChatID: chatID}}, nil
}
