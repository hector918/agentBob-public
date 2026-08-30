package prompt

import (
	"strings"

	"agentbob/contract"
)

// factory is the contract.PromptFactory: it hands out fresh builders.
type factory struct{}

func (factory) New() contract.Prompt { return NewBuilder() }

// NewBuilder returns a fresh empty prompt builder. The factory hands these out
// per turn; exported for callers that hold a builder directly (tests, a flow
// that doesn't go through the trunk-provided factory).
func NewBuilder() contract.Prompt { return &builder{} }

// builder is the dumb layered prompt. Layers are held in insertion order; the
// first SetLayer for a name fixes its slot, later ones update it in place.
type builder struct {
	order  []string          // layer names, in first-set order
	layers map[string]string // name → content
}

func (b *builder) SetLayer(name, content string) {
	if b.layers == nil {
		b.layers = map[string]string{}
	}
	if _, ok := b.layers[name]; !ok {
		b.order = append(b.order, name)
	}
	b.layers[name] = content
}

// Layer returns the named layer's current content ("" if unset). Not part of
// contract.Prompt — a concrete accessor tests reach via a type assertion.
func (b *builder) Layer(name string) string { return b.layers[name] }

// layerSep separates assembled layers in the system prompt.
const layerSep = "\n\n"

func (b *builder) Build() string {
	parts := make([]string, 0, len(b.order))
	for _, name := range b.order {
		if c := strings.TrimSpace(b.layers[name]); c != "" {
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, layerSep)
}

var (
	_ contract.PromptFactory = factory{}
	_ contract.Prompt        = (*builder)(nil)
)
