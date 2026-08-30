// Package slash is the slash-command registry — the table of "/..." commands and
// the dispatch that runs them. It provides a contract.SlashRegistry: each
// command-owning module registers its commands at Start (the registry holds the
// table, the module holds the logic), and the inbound flow dispatches a "/..."
// event through it after the gate, instead of running a turn.
//
// The registry itself owns only the built-in /help (which reads its own table).
// See docs/slash-rebuild.md.
package slash

import (
	"context"
	"reflect"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/trunk"
)

// Module is the slash registry as a trunk module.
type Module struct {
	reg   *registry
	state atomic.Int32 // trunk.State
}

// New returns a slash module.
func New() *Module { return &Module{reg: newRegistry()} }

func (m *Module) Name() string { return "slash" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.SlashRegistry]()}
}

func (m *Module) Needs() []reflect.Type { return nil }

// Optional is false: command-owning modules Require the registry at Start.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	m.reg.registerBuiltins() // /help — must exist before other modules add commands
	trunk.Provide[contract.SlashRegistry](reg, m.reg)
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

var _ trunk.Module = (*Module)(nil)
