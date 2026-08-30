package model

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/i18n"
)

// poolAdmin is the narrow set of operator actions the /model subcommands need
// beyond the read-only Snapshot. It is satisfied by the concrete *MultiPool (the
// wrappers already exist there) — a type-assert miss means
// "no live pool", so these stay OUT of contract.ModelPool (a slash convenience,
// not a pool capability every consumer must implement).
type poolAdmin interface {
	Pause(name string) error
	Resume(name string) error
	Reload() error
}

// slashModel handles /model (admin-only): with no args it lists the pool's
// entries and live state; `pause <name>` / `resume <name>` / `reload` drive the
// operator wrappers. Read path goes through the contract Snapshot; the write
// subcommands type-assert poolAdmin so a non-admin pool impl degrades to a clear notice
// instead of a panic. Lives in the model module — the pool's own command,
// registered into the shared table (no cross-module reach).
func (m *Module) slashModel(_ context.Context, sc contract.SlashContext) error {
	arg := strings.TrimSpace(sc.Args)
	verb, rest := arg, ""
	if i := strings.IndexAny(arg, " \t"); i >= 0 {
		verb, rest = arg[:i], strings.TrimSpace(arg[i+1:])
	}
	verb = strings.ToLower(verb)
	switch verb {
	case "", "list":
		return sc.Sink.Finish(m.renderPool(sc.Lang))
	case "pause", "resume", "reload":
		msg, err := m.poolAction(sc.Lang, verb, rest)
		if err != nil {
			_ = sc.Sink.Finish(msg)
			return err
		}
		return sc.Sink.Finish(msg)
	default:
		_ = sc.Sink.Finish(i18n.T("slash.model.usage", sc.Lang))
		return errors.New("model: unknown subcommand")
	}
}

// renderPool formats the live snapshot: a header line (in-flight + context
// budget) then one line per entry sorted by priority desc, name asc.
func (m *Module) renderPool(lang string) string {
	snap := m.pool.Snapshot()
	if len(snap.Entries) == 0 {
		return i18n.T("slash.model.empty", lang)
	}
	es := append([]contract.ModelInfo(nil), snap.Entries...)
	sort.Slice(es, func(i, j int) bool {
		if es[i].Priority != es[j].Priority {
			return es[i].Priority > es[j].Priority
		}
		return es[i].Name < es[j].Name
	})
	var b strings.Builder
	now := time.Now()
	fmt.Fprintf(&b, i18n.T("slash.model.header", lang), len(es), snap.InFlight)
	for _, e := range es {
		// stateText, not raw e.State: chat is the ONLY pool surface when the operator
		// is away from the LAN webui, and "cooling" without the remaining cooldown
		// withholds the answer they came for (usable in 3s or in 5min).
		fmt.Fprintf(&b, "\n"+i18n.T("slash.model.entry", lang), e.Name, stateText(e, now), e.Provider, e.Model)
		if e.InFlight > 0 {
			fmt.Fprintf(&b, i18n.T("slash.model.entry_inflight", lang), e.InFlight)
		}
		if len(e.Tags) > 0 {
			fmt.Fprintf(&b, i18n.T("slash.model.entry_tags", lang), strings.Join(e.Tags, ","))
		}
		if e.LastError != "" {
			fmt.Fprintf(&b, "\n"+i18n.T("slash.model.entry_last_error", lang), e.LastError)
		}
	}
	return b.String()
}

// poolAction runs an operator subcommand, returning the user-facing reply and an
// error that reflects the action's outcome (nil = succeeded).
func (m *Module) poolAction(lang, verb, name string) (string, error) {
	pa, ok := m.pool.(poolAdmin)
	if !ok {
		return i18n.T("slash.model.no_pool", lang), errors.New("model: no live pool")
	}
	if verb != "reload" && name == "" {
		return fmt.Sprintf(i18n.T("slash.model.action_usage", lang), verb), fmt.Errorf("model: %s requires a name", verb)
	}
	var err error
	switch verb {
	case "pause":
		err = pa.Pause(name)
	case "resume":
		err = pa.Resume(name)
	case "reload":
		err = pa.Reload()
	}
	if err != nil {
		return fmt.Sprintf(i18n.T("slash.model.action_failed", lang), err.Error()), err
	}
	switch verb {
	case "pause":
		return fmt.Sprintf(i18n.T("slash.model.paused", lang), name), nil
	case "resume":
		return fmt.Sprintf(i18n.T("slash.model.resumed", lang), name), nil
	default:
		return i18n.T("slash.model.reloaded", lang), nil
	}
}
