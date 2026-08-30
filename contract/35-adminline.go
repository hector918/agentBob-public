package contract

import "context"

// AdminLevel classifies an admin-line event.
type AdminLevel string

const (
	AdminInfo  AdminLevel = "info"
	AdminWarn  AdminLevel = "warn"
	AdminError AdminLevel = "error"
)

// AdminLine is the operator-summon funnel: urgent subsystem events (source
// health, a dead/cooling model entry, …) route through Notify to reach the
// admin. Provided by the adminline module; consumers take it as an optional
// collaborator (a nil AdminLine is a no-op for them). The persistence +
// forwarding machinery lives in the adminline module, not here.
type AdminLine interface {
	Notify(ctx context.Context, level AdminLevel, origin, format string, args ...any)
}
