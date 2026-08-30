package trunk_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentbob/trunk"
)

// fakeMod is a configurable Module that records start/stop order into a shared
// log. It declares Provides/Needs as raw capability types; a module that another
// module Needs sets provideFn to actually register its capability on Start (the
// sequencer verifies a module's Needs are registered before starting it).
type fakeMod struct {
	name      string
	provides  []reflect.Type
	needs     []reflect.Type
	optional  bool
	startErr  error
	state     trunk.State
	log       *[]string
	provideFn func(*trunk.Registry) // registers real impls for declared Provides
}

func (f *fakeMod) Name() string             { return f.name }
func (f *fakeMod) Provides() []reflect.Type { return f.provides }
func (f *fakeMod) Needs() []reflect.Type    { return f.needs }
func (f *fakeMod) Optional() bool           { return f.optional }
func (f *fakeMod) Health() trunk.State      { return f.state }

func (f *fakeMod) Start(ctx context.Context, reg *trunk.Registry) error {
	if f.startErr != nil {
		f.state = trunk.StateFailed
		return f.startErr
	}
	if f.provideFn != nil {
		f.provideFn(reg)
	}
	*f.log = append(*f.log, "start:"+f.name)
	f.state = trunk.StateReady
	return nil
}

func (f *fakeMod) Stop(ctx context.Context) error {
	*f.log = append(*f.log, "stop:"+f.name)
	f.state = trunk.StateStopped
	return nil
}

var (
	capA = trunk.TypeOf[greeter]()
	capB = trunk.TypeOf[counter]()
)

func TestLifecycle_StartsInDependencyOrder(t *testing.T) {
	var log []string
	// b needs capA (provided by a) → a must start before b. Register b first to
	// prove ordering comes from deps, not registration order.
	b := &fakeMod{name: "b", needs: []reflect.Type{capA}, log: &log}
	a := &fakeMod{name: "a", provides: []reflect.Type{capA}, log: &log,
		provideFn: func(r *trunk.Registry) { trunk.Provide[greeter](r, greeterImpl{s: "x"}) }}

	tk := trunk.New()
	tk.Register(b)
	tk.Register(a)
	if err := tk.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := strings.Join(log, ","); got != "start:a,start:b" {
		t.Fatalf("start order = %q, want a before b", got)
	}

	log = nil
	if err := tk.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := strings.Join(log, ","); got != "stop:b,stop:a" {
		t.Fatalf("stop order = %q, want reverse (b before a)", got)
	}
}

func TestLifecycle_CycleDetected(t *testing.T) {
	var log []string
	a := &fakeMod{name: "a", provides: []reflect.Type{capA}, needs: []reflect.Type{capB}, log: &log}
	b := &fakeMod{name: "b", provides: []reflect.Type{capB}, needs: []reflect.Type{capA}, log: &log}

	tk := trunk.New()
	tk.Register(a)
	tk.Register(b)
	err := tk.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
	if len(log) != 0 {
		t.Fatalf("no module should have started on a cycle, got %v", log)
	}
}

func TestLifecycle_MissingProvider(t *testing.T) {
	var log []string
	b := &fakeMod{name: "b", needs: []reflect.Type{capA}, log: &log}
	tk := trunk.New()
	tk.Register(b)
	err := tk.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no module provides") {
		t.Fatalf("expected missing-provider error, got %v", err)
	}
}

func TestLifecycle_DuplicateProvider(t *testing.T) {
	var log []string
	a1 := &fakeMod{name: "a1", provides: []reflect.Type{capA}, log: &log}
	a2 := &fakeMod{name: "a2", provides: []reflect.Type{capA}, log: &log}
	tk := trunk.New()
	tk.Register(a1)
	tk.Register(a2)
	err := tk.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "provided by both") {
		t.Fatalf("expected duplicate-provider error, got %v", err)
	}
}

func TestLifecycle_SelfDependency(t *testing.T) {
	var log []string
	a := &fakeMod{name: "a", provides: []reflect.Type{capA}, needs: []reflect.Type{capA}, log: &log}
	tk := trunk.New()
	tk.Register(a)
	err := tk.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "provides itself") {
		t.Fatalf("expected self-dependency error, got %v", err)
	}
}

func TestLifecycle_OptionalFailureContinues(t *testing.T) {
	var log []string
	// opt is optional and fails; b does NOT depend on it, so startup proceeds.
	opt := &fakeMod{name: "opt", optional: true, startErr: errors.New("boom"), log: &log}
	b := &fakeMod{name: "b", log: &log}
	tk := trunk.New()
	tk.Register(opt)
	tk.Register(b)
	if err := tk.Start(context.Background()); err != nil {
		t.Fatalf("optional failure should not abort Start, got %v", err)
	}
	if strings.Join(log, ",") != "start:b" {
		t.Fatalf("log = %v, want only b started", log)
	}
}

func TestLifecycle_CriticalFailureUnwinds(t *testing.T) {
	var log []string
	// a starts ok; bad (critical) needs capA so starts after a, then fails →
	// trunk must unwind a.
	a := &fakeMod{name: "a", provides: []reflect.Type{capA}, log: &log,
		provideFn: func(r *trunk.Registry) { trunk.Provide[greeter](r, greeterImpl{s: "x"}) }}
	bad := &fakeMod{name: "bad", needs: []reflect.Type{capA}, startErr: errors.New("boom"), log: &log}
	tk := trunk.New()
	tk.Register(a)
	tk.Register(bad)
	err := tk.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "critical module \"bad\"") {
		t.Fatalf("expected critical-failure error, got %v", err)
	}
	if strings.Join(log, ",") != "start:a,stop:a" {
		t.Fatalf("log = %v, want a started then unwound (stop:a)", log)
	}
}

func TestLifecycle_OptionalProviderFailureAbortsConsumerCleanly(t *testing.T) {
	var log []string
	// opt is optional, declares it provides capA, but fails Start → capA never
	// registered. cons (critical) Needs capA. The sequencer must abort cons with
	// a clean error, NOT let cons's Start panic on Require.
	opt := &fakeMod{name: "opt", optional: true, provides: []reflect.Type{capA},
		startErr: errors.New("boom"), log: &log}
	cons := &fakeMod{name: "cons", needs: []reflect.Type{capA}, log: &log}
	tk := trunk.New()
	tk.Register(opt)
	tk.Register(cons)

	err := tk.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `critical module "cons" cannot start`) {
		t.Fatalf("expected clean abort for cons, got %v", err)
	}
	if strings.Contains(strings.Join(log, ","), "start:cons") {
		t.Fatalf("cons must not have started; log = %v", log)
	}
}

func TestStart_SecondCallErrors(t *testing.T) {
	tk := trunk.New()
	if err := tk.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := tk.Start(context.Background()); err == nil {
		t.Fatal("second Start should error")
	}
}
