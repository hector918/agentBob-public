package trunk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
)

// State is a module's coarse health, observed by the trunk. M0 keeps it
// minimal; the circuit-breaker (docs/architecture-vision.md §6) consumes it
// later.
type State int

const (
	StateNew     State = iota // constructed, not started
	StateReady                // Start succeeded
	StateFailed               // Start failed, or self-detected unhealthy
	StateStopped              // Stop completed
)

// Module is a behavioural subsystem that registers onto the trunk. The trunk
// drives Start/Stop in dependency order; the module owns its own resources and
// FSM. Every method is content-free to the trunk: it only reads the declared
// Provides/Needs to order startup and invokes the lifecycle hooks.
//
// The sequencer keys its internal bookkeeping (provider map, in-degrees,
// dependents) by slice index, so a Module need not be comparable.
type Module interface {
	Name() string

	// Provides lists the capability interface types this module registers into
	// the Registry during Start. Pure declaration — it does not call anything.
	Provides() []reflect.Type

	// Needs lists the capability interface types that must be registered before
	// this module's Start runs. Pure declaration.
	Needs() []reflect.Type

	// Optional reports whether a Start failure is tolerable. true → the trunk
	// logs and continues with a degraded subset (fail-open); false (critical) →
	// the trunk unwinds and aborts startup.
	Optional() bool

	// Start acquires resources, starts goroutines, and registers its Provides.
	// CONTRACT: register Provides LAST — after every fallible step. On error the
	// trunk never calls Stop (only successful Starts are unwound), so anything
	// Start leaked before failing stays leaked; an Optional module that errored
	// AFTER Providing is escalated to a boot abort (see the Start loop) rather
	// than left as a half-initialized capability downstream code would wire to.
	// Runs only after every Needs capability is registered.
	Start(ctx context.Context, reg *Registry) error

	// Health is the trunk's observation point.
	Health() State

	// Stop releases resources. Called in reverse Start order.
	Stop(ctx context.Context) error
}

// Trunk holds the registered modules and the capability registry, and drives
// ordered startup/shutdown.
type Trunk struct {
	reg      *Registry
	hk       *houseKeeper
	mods     []Module
	started  []Module // in Start order; Stop walks this in reverse
	didStart bool     // Start is single-shot
}

// New returns a trunk with an empty registry and no modules. The housekeeper
// (mechanism 4) is published into the registry up front so a module can resolve
// it (TryRequire[Housekeeper]) and register its persistence sweeps at Start.
func New() *Trunk {
	t := &Trunk{reg: NewRegistry(), hk: newHousekeeper()}
	Provide[Housekeeper](t.reg, t.hk)
	return t
}

// Registry exposes the capability registry, for entry-point wiring and tests.
func (t *Trunk) Registry() *Registry { return t.reg }

// Register adds a module to be started. Registration order is irrelevant; the
// trunk topo-sorts by Provides/Needs at Start.
func (t *Trunk) Register(m Module) { t.mods = append(t.mods, m) }

// Start topo-sorts the modules by their declared Provides/Needs and starts them
// in dependency order. A critical module's Start failure unwinds the
// already-started modules and returns the error; an optional module's failure
// is logged and skipped (fail-open). Single-shot: a second call errors.
//
// Before starting a module, the sequencer verifies each of its declared Needs
// is actually registered. A Need can be absent even though topo-sort passed —
// its provider was an OPTIONAL module that failed Start and was skipped. Rather
// than let the module's Start panic on Require, that module is itself treated as
// unstartable: skipped if optional, aborting if critical.
func (t *Trunk) Start(ctx context.Context) error {
	if t.didStart {
		return fmt.Errorf("trunk: Start already called")
	}
	t.didStart = true

	order, err := t.topoOrder()
	if err != nil {
		return err
	}
	for _, m := range order {
		if missing := t.unmetNeeds(m); len(missing) > 0 {
			if m.Optional() {
				slog.Warn("trunk: optional module skipped; a needed capability is unavailable (its provider failed)",
					"module", m.Name(), "missing", typeNames(missing))
				continue
			}
			t.unwind(ctx)
			return fmt.Errorf("trunk: critical module %q cannot start: needed capabilities %v unavailable (a provider failed to start)",
				m.Name(), typeNames(missing))
		}
		if err := m.Start(ctx, t.reg); err != nil {
			if m.Optional() {
				// Degrade-and-continue is only safe when the failed Start left NOTHING
				// behind: a module that already registered a Provide before erroring
				// has leaked a half-initialized impl into the Registry — downstream
				// hard-Requires would wire to it and hk tasks would fire against it,
				// with Stop never called (only successful Starts join t.started).
				// That contract used to be implicit ("Provide last"); enforce it.
				for _, p := range m.Provides() {
					if t.reg.has(p) {
						t.unwind(ctx)
						return fmt.Errorf("trunk: optional module %q Start failed AFTER registering %s — half-initialized capability would leak, aborting: %w",
							m.Name(), p.String(), err)
					}
				}
				slog.Warn("trunk: optional module Start failed; continuing degraded",
					"module", m.Name(), "err", err)
				continue
			}
			t.unwind(ctx)
			return fmt.Errorf("trunk: critical module %q Start failed: %w", m.Name(), err)
		}
		t.started = append(t.started, m)
		// A module MUST register every capability it declares in Provides() — the topo
		// order trusted that declaration to sequence consumers after it. Catch a
		// declared-but-unregistered provide HERE (a clear startup error naming the
		// module) instead of letting a downstream hard Require panic deep in another
		// module's Start, or a TryRequire consumer silently degrade. (The reverse —
		// registered-but-undeclared — is already caught: a hard Need of an undeclared
		// type fails topo with a missing-provider error.)
		for _, p := range m.Provides() {
			if t.reg.has(p) {
				continue
			}
			if m.Optional() {
				slog.Warn("trunk: optional module declared a capability it did not register; consumers degrade",
					"module", m.Name(), "capability", p.String())
				continue
			}
			t.unwind(ctx)
			return fmt.Errorf("trunk: module %q declared Provides %s but did not register it in Start", m.Name(), p.String())
		}
	}
	// Every module has registered its sweeps — start draining the priority queue.
	t.hk.start()
	return nil
}

// unmetNeeds returns the capability types m declared in Needs that are not (yet)
// registered — i.e. whose provider was skipped.
func (t *Trunk) unmetNeeds(m Module) []reflect.Type {
	var missing []reflect.Type
	for _, n := range m.Needs() {
		if !t.reg.has(n) {
			missing = append(missing, n)
		}
	}
	return missing
}

func typeNames(ts []reflect.Type) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.String()
	}
	return out
}

// Stop halts the housekeeper (so no sweep runs against a stopping module's
// store) then stops the started modules in reverse Start order, joining errors.
func (t *Trunk) Stop(ctx context.Context) error {
	t.hk.stop()
	var errs []error
	for i := len(t.started) - 1; i >= 0; i-- {
		m := t.started[i]
		if err := m.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("module %q Stop: %w", m.Name(), err))
		}
	}
	t.started = nil
	return errors.Join(errs...)
}

// unwind stops already-started modules during a startup abort, best-effort.
func (t *Trunk) unwind(ctx context.Context) {
	for i := len(t.started) - 1; i >= 0; i-- {
		if err := t.started[i].Stop(ctx); err != nil {
			slog.Warn("trunk: Stop during startup-abort unwind failed",
				"module", t.started[i].Name(), "err", err)
		}
	}
	t.started = nil
}

// topoOrder returns the modules in dependency order (each module after every
// module that provides one of its Needs). Errors on duplicate provider, missing
// provider, self-dependency, or a cycle. Among modules with no outstanding
// dependency, registration order is preserved for determinism.
func (t *Trunk) topoOrder() ([]Module, error) {
	n := len(t.mods)

	// capability type → index of the module providing it.
	providerOf := make(map[reflect.Type]int, n)
	for i, m := range t.mods {
		for _, c := range m.Provides() {
			if prev, dup := providerOf[c]; dup {
				return nil, fmt.Errorf("trunk: capability %s provided by both %q and %q",
					c, t.mods[prev].Name(), m.Name())
			}
			providerOf[c] = i
		}
	}

	indeg := make([]int, n)
	dependents := make([][]int, n) // provider index → consumer indices
	for i, m := range t.mods {
		seen := make(map[int]bool)
		for _, need := range m.Needs() {
			p, ok := providerOf[need]
			if !ok {
				return nil, fmt.Errorf("trunk: module %q needs capability %s but no module provides it",
					m.Name(), need)
			}
			if p == i {
				return nil, fmt.Errorf("trunk: module %q needs capability %s it provides itself",
					m.Name(), need)
			}
			if seen[p] {
				continue
			}
			seen[p] = true
			dependents[p] = append(dependents[p], i)
			indeg[i]++
		}
	}

	queue := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	order := make([]int, 0, n)
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		order = append(order, i)
		for _, d := range dependents[i] {
			indeg[d]--
			if indeg[d] == 0 {
				queue = append(queue, d)
			}
		}
	}
	if len(order) != n {
		var stuck []string
		for i := 0; i < n; i++ {
			if indeg[i] > 0 {
				stuck = append(stuck, t.mods[i].Name())
			}
		}
		return nil, fmt.Errorf("trunk: dependency cycle among modules %v", stuck)
	}

	out := make([]Module, n)
	for j, i := range order {
		out[j] = t.mods[i]
	}
	return out, nil
}
