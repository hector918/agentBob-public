package slash

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"agentbob/contract"
	"agentbob/i18n"
)

// registry is the contract.SlashRegistry: a name→command table + dispatch.
type registry struct {
	mu   sync.RWMutex
	cmds map[string]contract.SlashCommand
}

func newRegistry() *registry { return &registry{cmds: map[string]contract.SlashCommand{}} }

// Register adds a command. Panics on a duplicate name, nil handler, or a name
// Dispatch can never match (whitespace/'@'/'/' — parseCommand splits or strips
// them) — wiring bugs that must surface at startup, same posture as trunk.Provide.
func (r *registry) Register(cmd contract.SlashCommand) {
	if cmd.Name == "" || cmd.Handler == nil || strings.ContainsAny(cmd.Name, " \t\n@/") {
		panic(fmt.Sprintf("slash: invalid command %q (empty/unmatchable name or nil handler)", cmd.Name))
	}
	// Store under the lowercased name so it matches Dispatch's lookup
	// (parseCommand lowercases the inbound command) — an upper-case
	// registration would otherwise be unreachable.
	name := strings.ToLower(cmd.Name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.cmds[name]; dup {
		panic(fmt.Sprintf("slash: duplicate command %q", cmd.Name))
	}
	r.cmds[name] = cmd
}

// Dispatch parses the command name, enforces the admin gate, and runs the
// handler (synchronously — handlers must be quick). An unknown name renders the
// unknown notice.
func (r *registry) Dispatch(ctx context.Context, sc contract.SlashContext) error {
	name, args := parseCommand(sc.Event.Text)
	sc.Args = args
	sc.Sink = lineBreakSink(sc.Sink) // slash replies are line-structured UI text — see lineBreakSink
	r.mu.RLock()
	cmd, ok := r.cmds[name]
	r.mu.RUnlock()
	if !ok {
		finish(sc.Sink, i18n.T("slash.dispatch.unknown", sc.Lang), name)
		return fmt.Errorf("unknown command /%s", name)
	}
	if cmd.AdminOnly && !sc.IsAdmin {
		finish(sc.Sink, i18n.T("slash.dispatch.admin_only", sc.Lang), name)
		return fmt.Errorf("command /%s is admin-only", name)
	}
	err := cmd.Handler(ctx, sc)
	if err != nil {
		slog.Warn("slash: command failed", "cmd", name, "err", err)
	}
	return err
}

// List returns the registered commands sorted by name.
func (r *registry) List() []contract.SlashCommand {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]contract.SlashCommand, 0, len(r.cmds))
	for _, c := range r.cmds {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// registerBuiltins adds the commands the registry owns itself (/help).
func (r *registry) registerBuiltins() {
	r.Register(contract.SlashCommand{
		Name:    "help",
		DescKey: "slash.help.desc",
		Handler: r.slashHelp,
	})
}

// slashHelp renders the command list visible to the sender.
func (r *registry) slashHelp(_ context.Context, sc contract.SlashContext) error {
	var b strings.Builder
	b.WriteString(i18n.T("slash.help.header", sc.Lang) + "\n") // blank line under the header
	for _, c := range r.List() {
		if c.AdminOnly && !sc.IsAdmin {
			continue // hide admin-only commands from non-admins
		}
		desc := c.Description
		if c.DescKey != "" {
			desc = i18n.T(c.DescKey, sc.Lang)
		}
		b.WriteString("\n/" + c.Name + " — " + desc)
	}
	// In any non-DM context (group/channel/topic) a slash command must reply to a
	// bot message to address the right session — surface that as a footer.
	if sc.Event.ChatType != contract.ChatDM {
		b.WriteString(i18n.T("slash.help.group_caveat", sc.Lang))
	}
	return sc.Sink.Finish(b.String())
}

// parseCommand splits "/name args..." into a lowercased name and the remaining
// argument text. A bare "/name" yields empty args.
func parseCommand(text string) (name, args string) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "/")
	if i := strings.IndexAny(text, " \t\n"); i >= 0 {
		name, args = text[:i], strings.TrimSpace(text[i+1:])
	} else {
		name = text
	}
	// Strip a telegram "/cmd@botname" suffix so the registry sees the bare command name.
	// (In a multi-bot group the source already drops commands for OTHER bots; this also
	// handles a DM "/cmd@self" and is a harmless no-op when there's no "@".)
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	return strings.ToLower(name), args
}

// lineBreakSink wraps the dispatch sink so a slash reply's line boundaries are
// written the way THIS channel writes them.
//
// A slash reply is LINE-structured UI text — a command list, a session dump, a
// receipt — assembled by the handlers with bare "\n". That does not say "line
// boundary" on every channel: a markdown-rendering channel reads a single
// newline as a soft break and collapses the whole reply into one running
// paragraph. The channel declares its own separator (contract.LineBreakSink,
// backed by streamsink's WireCaps.LineBreak); the classification — "this text
// is bob's own UI, not a model-written table" — is made HERE, because that is
// what a wire, handed pre-joined text, cannot tell.
//
// Two sinks are returned unwrapped: one whose separator is already "\n" (every
// plain-text channel — nothing to rewrite), and a contract.BareProductSink,
// whose Finish full is consumed verbatim as a product (an agora bridge return
// event, a coalesced mail segment, a delegation product) rather than rendered
// to a reader — rewriting its line endings would edit a payload, not fix a
// rendering.
func lineBreakSink(s contract.Sink) contract.Sink {
	if _, bare := s.(contract.BareProductSink); bare {
		return s
	}
	lb, ok := s.(contract.LineBreakSink)
	if !ok {
		return s
	}
	sep := lb.LineBreak()
	if sep == "\n" {
		return s
	}
	return breakingSink{Sink: s, sep: sep}
}

// breakingSink rewrites line boundaries on the content channel. TraceDelta is
// NOT rewritten — trace is the streaming core's own line-structured channel and
// it applies the same separator itself, so touching it here would double it.
// SendFile/LastSent pass through via the embedded Sink.
type breakingSink struct {
	contract.Sink
	sep string
}

// hardBreak rewrites every line boundary as sep. It is a plain per-newline
// substitution and therefore distributes over concatenation — a handler that
// emits ContentDelta chunks and then Finish still produces a `full` with the
// streamed bytes as its exact prefix, which the streaming sink relies on. A
// "\n\n" paragraph break survives a "  \n" separator too: the middle line is
// then whitespace-only, which is still a blank line to a markdown parser.
func (b breakingSink) hardBreak(text string) string {
	return strings.ReplaceAll(text, "\n", b.sep)
}

func (b breakingSink) ContentDelta(text string) { b.Sink.ContentDelta(b.hardBreak(text)) }
func (b breakingSink) Finish(full string) error { return b.Sink.Finish(b.hardBreak(full)) }

// finish delivers a one-off notice, logging a delivery failure.
func finish(sink contract.Sink, text, cmd string) {
	if err := sink.Finish(text); err != nil {
		slog.Warn("slash: notice delivery failed", "cmd", cmd, "err", err)
	}
}

var _ contract.SlashRegistry = (*registry)(nil)
