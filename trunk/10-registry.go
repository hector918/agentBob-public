package trunk

import (
	"fmt"
	"reflect"
	"sync"
)

// Registry maps a capability interface type to its single registered
// implementation. It is the trunk's discovery mechanism: a provider registers
// its implementation once (Provide), a consumer looks it up once (Require) and
// then holds the reference and calls the implementation directly. The registry
// is deliberately NOT on the per-call hot path — it only does discovery.
//
// One interface type → one implementation. When several implementations of the
// same interface coexist (chat sources, tools), that is a plugin set owned by a
// parent module, not a trunk capability (docs/architecture-vision.md §3).
type Registry struct {
	mu   sync.RWMutex
	caps map[reflect.Type]any
}

// NewRegistry returns an empty capability registry.
func NewRegistry() *Registry {
	return &Registry{caps: make(map[reflect.Type]any)}
}

// TypeOf returns the reflect.Type of interface T. Modules use it to declare
// Provides/Needs, e.g. trunk.TypeOf[core.PromptAssembler]().
func TypeOf[T any]() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

// Provide registers impl as THE implementation of capability interface T.
// Panics if T is not an interface type, or if T already has an implementation —
// both are wiring bugs that must surface at startup.
func Provide[T any](r *Registry, impl T) {
	k := TypeOf[T]()
	if k.Kind() != reflect.Interface {
		panic(fmt.Sprintf("trunk: Provide[%s]: T must be an interface type", k))
	}
	if isNil(impl) {
		panic(fmt.Sprintf("trunk: Provide[%s]: nil implementation", k))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.caps[k]; dup {
		panic(fmt.Sprintf("trunk: Provide[%s]: capability already registered", k))
	}
	r.caps[k] = impl
}

// isNil reports whether impl is a nil interface or a typed-nil of a nilable
// kind — a forgot-to-construct wiring bug that should fail at Provide, not on
// the first method call after a later Require.
func isNil(impl any) bool {
	rv := reflect.ValueOf(impl)
	if !rv.IsValid() {
		return true
	}
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return rv.IsNil()
	default:
		return false
	}
}

// has reports whether capability type t is registered. Used by the lifecycle
// sequencer to verify a module's Needs before starting it.
func (r *Registry) has(t reflect.Type) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.caps[t]
	return ok
}

// Require returns the implementation of capability interface T. Panics if none
// is registered — an unmet hard dependency is a wiring bug, not a silent nil.
func Require[T any](r *Registry) T {
	v, ok := TryRequire[T](r)
	if !ok {
		panic(fmt.Sprintf("trunk: Require[%s]: no implementation registered", TypeOf[T]()))
	}
	return v
}

// TryRequire returns the implementation of T and whether it is present. Use for
// genuinely optional dependencies; prefer Require for hard ones.
func TryRequire[T any](r *Registry) (T, bool) {
	k := TypeOf[T]()
	r.mu.RLock()
	v, ok := r.caps[k]
	r.mu.RUnlock()
	if !ok {
		var zero T
		return zero, false
	}
	return v.(T), true
}
