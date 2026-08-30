// Package turn is the turn core — the turn-execution mechanism. It provides
// contract.Turn: given a flow-composed spec (system prompt, user text, model
// selection, sink), it drives one turn through its lifecycle — read history,
// assemble the messages, call the model, stream the reply, persist the exchange.
// The turn core reads + appends to the conversation history via contract.MessageStore
// (session owns the bob_messages table).
//
// It runs the full bounded loop (spec §4): round iteration (model → tool dispatch →
// repeat) up to the selected driver's round cap (docs/turn-driver-split.md), plus the §6 Ring-1 hardening — 落库纪律, 结果管道, nudge 体系,
// 进展谓词, 安全守卫, 退化检测, salvage 阶梯, bloat 救捞, 上下文压缩, 路由标签, 孤儿修复 —
// and depth-1 sub-loop delegation (§7). A flow composes the spec and drives the core;
// the core knows nothing about which flow drove it.
package turn

import (
	"context"
	"reflect"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/trunk"
)

// Module is the turn core as a trunk module.
type Module struct {
	core  *core
	state atomic.Int32 // trunk.State
}

// New returns a turn module.
func New() *Module { return &Module{} }

func (m *Module) Name() string { return "turn" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.Turn]()}
}

// Needs the conversation-history store (now owned + provided by session) and the
// model pool. turn no longer touches contract.DB directly — its only DB use was
// the message store, which has moved to session. MessageStore is a HARD Need:
// without history there is no meaningful turn ("没有 session 就没有 turn"). The
// topo-sort stays acyclic — session's reverse use of turn is the soft, lazily
// resolved TurnHandler edge (a TryRequire, invisible to the sort).
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.MessageStore](),
		trunk.TypeOf[contract.ModelPool](),
	}
}

// Optional is false: without the turn core a flow can compose a turn but never
// run it.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	// History store + migration belong to session now; turn only consumes the
	// store through the contract interface (resolved here; session started first
	// because turn hard-Needs contract.MessageStore).
	m.core = &core{
		store: trunk.Require[contract.MessageStore](reg),
		pool:  trunk.Require[contract.ModelPool](reg),
	}
	trunk.Provide[contract.Turn](reg, m.core)
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

var _ trunk.Module = (*Module)(nil)
