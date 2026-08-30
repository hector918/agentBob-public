// Package prompt is the dumb, layered system-prompt builder. It provides a
// contract.PromptFactory: a flow calls New() once per turn to get a fresh
// builder, fills its layers from context (identity, tools, memory, …), and
// Build()s the system prompt. The builder knows nothing about what any layer
// means — all coupling to context lives in the flow. Because a builder carries
// no conversation state, this module needs no store and no collaborators.
package prompt

import (
	"context"
	"reflect"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/trunk"
)

// Module provides the prompt factory.
type Module struct {
	state atomic.Int32 // trunk.State
}

// New returns a prompt module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "prompt" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.PromptFactory]()}
}

func (m *Module) Needs() []reflect.Type { return nil }

// Optional is false: a flow can't compose a system prompt without it.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	trunk.Provide[contract.PromptFactory](reg, factory{})
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

var _ trunk.Module = (*Module)(nil)
