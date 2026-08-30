package prompt

import (
	"testing"

	"agentbob/contract"
)

func TestBuilder_InsertionOrderAndUpdateInPlace(t *testing.T) {
	p := factory{}.New().(*builder) // concrete: Layer is a test accessor, not contract.Prompt
	p.SetLayer("identity", "I")
	p.SetLayer("tools", "T")
	p.SetLayer("memory", "M")
	// Re-setting an existing layer updates content but keeps its position.
	p.SetLayer("identity", "I2")

	got := p.Build()
	want := "I2" + layerSep + "T" + layerSep + "M"
	if got != want {
		t.Fatalf("Build() = %q, want %q", got, want)
	}
	if p.Layer("identity") != "I2" {
		t.Fatalf("Layer(identity) = %q, want I2", p.Layer("identity"))
	}
	if p.Layer("absent") != "" {
		t.Fatalf("Layer(absent) = %q, want empty", p.Layer("absent"))
	}
}

func TestBuilder_EmptyLayersDroppedFromBuild(t *testing.T) {
	p := factory{}.New().(*builder)
	p.SetLayer("a", "A")
	p.SetLayer("blank", "   ") // whitespace-only → dropped from Build
	p.SetLayer("b", "B")

	if got := p.Build(); got != "A"+layerSep+"B" {
		t.Fatalf("Build() = %q, want the two non-empty layers joined", got)
	}
	// But the slot is still tracked (Layer returns it; a later SetLayer keeps its position).
	if p.Layer("blank") != "   " {
		t.Fatalf("Layer(blank) = %q, want the whitespace content preserved", p.Layer("blank"))
	}
}

func TestBuilder_FreshPerNew(t *testing.T) {
	f := factory{}
	a := f.New()
	a.SetLayer("x", "1")
	b := f.New() // a separate turn's builder must not see a's layers
	if b.Build() != "" {
		t.Fatalf("a fresh builder is not empty: %q", b.Build())
	}
}

var _ contract.PromptFactory = factory{}
