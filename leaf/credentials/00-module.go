package credentials

import (
	"context"
	"fmt"
	"reflect"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/trunk"
)

// Module is the credential vault as a trunk module: it provides contract.Broker
// over $BOB_HOME/credentials/*.env. warrant Requires it (optionally) to build
// remote-space channels; absent → remote spaces are unavailable (local still work).
type Module struct {
	home  string
	kinds map[string]KindFactory
	state atomic.Int32 // trunk.State
}

// KindFactory builds a configured client from a credential's parsed fields. ssh
// is built in; other kinds (e.g. wordpress) are registered by the wiring layer
// via RegisterKind so the kind's domain code (and its type) stays in its own
// package — credentials never imports it, the secret stays inside the built
// client. Returns `any`; warrant's caller type-asserts.
type KindFactory func(data map[string]string) (any, error)

// New returns a credentials module reading $home/credentials/. ssh is registered
// built in; call RegisterKind (before the module Starts) to add more kinds.
func New(home string) *Module {
	return &Module{home: home, kinds: map[string]KindFactory{
		"ssh": func(d map[string]string) (any, error) { return buildSSH(d) },
	}}
}

// RegisterKind adds a kind factory (last-writer-wins). Called from the wiring
// layer at startup, before Start captures the table into the broker. A kind's
// factory lives in the consuming tool's package (e.g. wordpress.BuildClient) so
// credentials stays free of provider types.
func (m *Module) RegisterKind(kind string, f KindFactory) {
	m.kinds[kind] = f
}

func (m *Module) Name() string { return "credentials" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.Broker]()}
}

func (m *Module) Needs() []reflect.Type { return nil }

// Optional is true: without the vault, remote spaces can't be built — local
// spaces still work, so the pipeline runs.
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	// Freeze the broker's view: copy the kind table so a post-Start RegisterKind on the
	// Module can't mutate the live map the broker dispatches on.
	kinds := make(map[string]KindFactory, len(m.kinds))
	for k, f := range m.kinds {
		kinds[k] = f
	}
	trunk.Provide[contract.Broker](reg, &broker{home: m.home, kinds: kinds})
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// broker implements contract.Broker by dispatching on the credential's kind
// through the registered factory table (captured from the Module at Start).
type broker struct {
	home  string
	kinds map[string]KindFactory
}

func (b *broker) Build(_ context.Context, name string) (any, error) {
	kind, data, err := load(b.home, name)
	if err != nil {
		return nil, err
	}
	f, ok := b.kinds[kind]
	if !ok {
		return nil, fmt.Errorf("credentials: unsupported kind %q for %q", kind, name)
	}
	return f(data)
}

// NamesByKind lists the vault credential names whose kind= equals kind. Reads
// only the kind field of each file; a malformed/badly-named file is skipped.
func (b *broker) NamesByKind(_ context.Context, kind string) ([]string, error) {
	return namesByKind(b.home, kind)
}

var (
	_ trunk.Module    = (*Module)(nil)
	_ contract.Broker = (*broker)(nil)
)
