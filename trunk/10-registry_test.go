package trunk_test

import (
	"testing"

	"agentbob/trunk"
)

// test capability interfaces.
type greeter interface{ Greet() string }
type counter interface{ Count() int }

type greeterImpl struct{ s string }

func (g greeterImpl) Greet() string { return g.s }

func TestRegistry_ProvideRequireRoundTrip(t *testing.T) {
	r := trunk.NewRegistry()
	trunk.Provide[greeter](r, greeterImpl{s: "hi"})

	got := trunk.Require[greeter](r)
	if got.Greet() != "hi" {
		t.Fatalf("Greet() = %q, want %q", got.Greet(), "hi")
	}
}

func TestRegistry_TryRequireMissing(t *testing.T) {
	r := trunk.NewRegistry()
	if _, ok := trunk.TryRequire[greeter](r); ok {
		t.Fatal("TryRequire on empty registry returned ok=true")
	}
}

func TestRegistry_RequireMissingPanics(t *testing.T) {
	r := trunk.NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("Require on missing capability did not panic")
		}
	}()
	_ = trunk.Require[greeter](r)
}

func TestRegistry_DoubleProvidePanics(t *testing.T) {
	r := trunk.NewRegistry()
	trunk.Provide[greeter](r, greeterImpl{s: "a"})
	defer func() {
		if recover() == nil {
			t.Fatal("double Provide of same capability did not panic")
		}
	}()
	trunk.Provide[greeter](r, greeterImpl{s: "b"})
}

// concrete (non-interface) type must be rejected.
type notIface struct{}

func TestRegistry_ProvideNonInterfacePanics(t *testing.T) {
	r := trunk.NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("Provide of a non-interface type did not panic")
		}
	}()
	trunk.Provide[notIface](r, notIface{})
}

func TestRegistry_DistinctCapabilitiesCoexist(t *testing.T) {
	r := trunk.NewRegistry()
	trunk.Provide[greeter](r, greeterImpl{s: "hi"})
	trunk.Provide[counter](r, counterImpl{n: 7})

	if trunk.Require[greeter](r).Greet() != "hi" {
		t.Fatal("greeter lost after registering a second capability")
	}
	if trunk.Require[counter](r).Count() != 7 {
		t.Fatal("counter not retrievable")
	}
}

type counterImpl struct{ n int }

func (c counterImpl) Count() int { return c.n }

func TestRegistry_ProvideNilPanics(t *testing.T) {
	r := trunk.NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("Provide of a nil implementation did not panic")
		}
	}()
	var g greeter // nil interface
	trunk.Provide[greeter](r, g)
}
