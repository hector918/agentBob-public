package contract

import "context"

// SlashHandler runs one slash command. It renders its reply through sc.Sink and
// returns an error only for logging (the user already got a reply, or none was
// warranted). It must be safe to run out-of-band — concurrent with a turn — so a
// handler that touches session/turn state goes through that subsystem's locked
// methods, never its raw in-memory state.
type SlashHandler func(ctx context.Context, sc SlashContext) error

// SlashCommand is one registered command: its name (no leading "/"), a one-line
// description for /help, whether it requires an admin sender (the registry
// enforces it), and the handler. The owning module registers its commands into
// the SlashRegistry at Start — the registry holds the table, the module holds
// the logic.
type SlashCommand struct {
	Name        string // "new", "help" — lowercase, no leading slash
	Description string // one-line fallback shown by /help when DescKey is empty
	DescKey     string // i18n key for the description; /help renders i18n.T(DescKey, lang)
	AdminOnly   bool   // require an admin sender; the registry rejects non-admins
	Handler     SlashHandler
}

// SlashContext is the context one command runs in. MINIMAL today: the raw event,
// the parsed argument text, a sink to reply through, and the sender's admin/lang
// flags. It deliberately does NOT pre-resolve scope/sid — commands need
// heterogeneous session context, so a handler resolves what it needs from its own
// module (the single scope authority), and a peek doesn't fabricate a session.
type SlashContext struct {
	Event   MessageEvent
	Args    string // everything after "/name " (set by Dispatch)
	Sink    Sink
	IsAdmin bool
	Lang    string
}

// SlashRegistry holds the command table and dispatches. Provided by the slash
// module; command-owning modules Require it and Register at Start; the inbound
// flow Requires it to Dispatch a "/..." event (after the gate, instead of a turn).
type SlashRegistry interface {
	// Register adds a command. Panics on a duplicate name or a nil handler — both
	// are wiring bugs that must surface at startup.
	Register(cmd SlashCommand)
	// Dispatch parses sc.Event's text as "/name args", runs the matching command
	// (rejecting a non-admin sender on an AdminOnly command), or renders a
	// "no such command" reply when the name is unknown. Synchronous: a handler
	// must be quick (no model call). Returns the handler's error (nil = the
	// command succeeded) so a caller can signal success/failure — the reply text
	// is still rendered through sc.Sink either way. Unknown-command and
	// admin-rejected also return non-nil.
	Dispatch(ctx context.Context, sc SlashContext) error
	// List returns every registered command, sorted by name (for /help).
	List() []SlashCommand
}
