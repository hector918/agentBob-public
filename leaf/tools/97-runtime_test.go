package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"agentbob/contract"
)

// fakePanelReg is a minimal PanelRegistry for the runtime_state tests.
type fakePanelReg struct{ panels []contract.Panel }

func (f *fakePanelReg) RegisterPanel(p contract.Panel) { f.panels = append(f.panels, p) }
func (f *fakePanelReg) Panels() []contract.Panel       { return f.panels }

func runRuntime(t *testing.T, reg contract.PanelRegistry, section string) contract.ToolResult {
	t.Helper()
	args := json.RawMessage(`{}`)
	if section != "" {
		args = json.RawMessage(`{"section":"` + section + `"}`)
	}
	return runtimeStateTool{panels: reg}.Run(context.Background(), contract.ToolContext{}, args)
}

func poolPanel() contract.Panel {
	return contract.Panel{
		ID: "model-pool", Title: "Model Pool",
		State: func(context.Context) []contract.StateField {
			return []contract.StateField{
				{Kind: "stat", Label: "in-flight", Value: "2"},
				{Kind: "table", Label: "backends", Columns: []string{"name", "state"}, Rows: [][]contract.Cell{
					{{Text: "smart"}, {Text: "cooling", Status: "down", Detail: "connect refused"}},
					{{Text: "nv-nano"}, {Text: "live", Status: "ok"}},
				}},
			}
		},
	}
}

func TestRuntimeIndexListsSectionsWithGlance(t *testing.T) {
	reg := &fakePanelReg{panels: []contract.Panel{poolPanel()}}
	res := runRuntime(t, reg, "")
	if !res.OK {
		t.Fatalf("index not OK: %q", res.Error)
	}
	if !strings.Contains(res.Data, "[model-pool]") || !strings.Contains(res.Data, "Model Pool") {
		t.Fatalf("index missing section header: %q", res.Data)
	}
	// glance surfaces the stat field inline
	if !strings.Contains(res.Data, "in-flight=2") {
		t.Fatalf("index glance missing stat: %q", res.Data)
	}
}

func TestRuntimeSectionRendersTableWithStatusAndDetail(t *testing.T) {
	reg := &fakePanelReg{panels: []contract.Panel{poolPanel()}}
	res := runRuntime(t, reg, "model-pool")
	if !res.OK {
		t.Fatalf("section not OK: %q", res.Error)
	}
	for _, want := range []string{"# Model Pool [model-pool]", "backends（2 行）", "smart", "❌down", "ⓘconnect refused"} {
		if !strings.Contains(res.Data, want) {
			t.Fatalf("section render missing %q in:\n%s", want, res.Data)
		}
	}
}

func TestRuntimeUnknownSectionListsValidOnes(t *testing.T) {
	reg := &fakePanelReg{panels: []contract.Panel{poolPanel()}}
	res := runRuntime(t, reg, "nope")
	if res.OK {
		t.Fatalf("unknown section should error")
	}
	if !strings.Contains(res.Hint, "model-pool") {
		t.Fatalf("unknown-section hint should list valid sections: %q", res.Hint)
	}
}

func TestRuntimePanicInStateIsIsolated(t *testing.T) {
	bad := contract.Panel{ID: "boom", Title: "Boom", State: func(context.Context) []contract.StateField {
		panic("kaboom")
	}}
	reg := &fakePanelReg{panels: []contract.Panel{bad, poolPanel()}}
	// index must not panic and must still list the healthy panel
	idx := runRuntime(t, reg, "")
	if !idx.OK || !strings.Contains(idx.Data, "[model-pool]") {
		t.Fatalf("panic in one panel broke the index: ok=%v data=%q", idx.OK, idx.Data)
	}
	// reading the bad section surfaces the panic as text, not a crash
	sec := runRuntime(t, reg, "boom")
	if !sec.OK || !strings.Contains(sec.Data, "panic") {
		t.Fatalf("bad section should render panic note: ok=%v data=%q", sec.OK, sec.Data)
	}
}

func TestRuntimeLargeFieldIsBounded(t *testing.T) {
	// A single oversized text field (CJK) must not blow past the section cap by its
	// full size — it is truncated per-field.
	big := strings.Repeat("错", 50000)
	huge := contract.Panel{ID: "huge", Title: "Huge", State: func(context.Context) []contract.StateField {
		return []contract.StateField{{Kind: "text", Label: "dump", Text: big}}
	}}
	reg := &fakePanelReg{panels: []contract.Panel{huge}}
	res := runRuntime(t, reg, "huge")
	if !res.OK {
		t.Fatalf("section not OK: %q", res.Error)
	}
	if !strings.Contains(res.Data, "已截断") {
		t.Fatalf("large field should be truncated")
	}
	// bounded well under the raw 50000-rune input (cap 4000 + header/markers)
	if n := len([]rune(res.Data)); n > runtimeFieldCap+200 {
		t.Fatalf("section not bounded: %d runes", n)
	}
	// truncation must not split a rune → output stays valid UTF-8
	if !utf8.ValidString(res.Data) {
		t.Fatalf("truncated output is not valid UTF-8")
	}
}

func TestRuntimeNilRegistry(t *testing.T) {
	res := runtimeStateTool{panels: nil}.Run(context.Background(), contract.ToolContext{}, json.RawMessage(`{}`))
	if res.OK {
		t.Fatalf("nil registry should error")
	}
}
