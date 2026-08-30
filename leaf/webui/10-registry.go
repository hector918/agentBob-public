package webui

import (
	"fmt"
	"sort"
	"sync"

	"agentbob/contract"
)

// registry is the contract.PanelRegistry: an id→panel table. Panel-owning
// modules RegisterPanel at Start (push); the http layer reads Panels() live on
// each refresh tick. Mirrors leaf/slash's registry.
type registry struct {
	mu     sync.RWMutex
	panels map[string]contract.Panel
}

func newRegistry() *registry { return &registry{panels: map[string]contract.Panel{}} }

// RegisterPanel adds a panel. Panics on a duplicate id or a nil State closure —
// wiring bugs that must surface at startup, same posture as trunk.Provide.
func (r *registry) RegisterPanel(p contract.Panel) {
	if p.ID == "" || p.State == nil {
		panic(fmt.Sprintf("webui: invalid panel %q (empty id or nil State)", p.ID))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.panels[p.ID]; dup {
		panic(fmt.Sprintf("webui: duplicate panel %q", p.ID))
	}
	r.panels[p.ID] = p
}

// Panels returns every registered panel sorted by id.
func (r *registry) Panels() []contract.Panel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]contract.Panel, 0, len(r.panels))
	for _, p := range r.panels {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// panelByID returns the registered panel and whether it exists (handlers route
// setting/page requests by id).
func (r *registry) panelByID(id string) (contract.Panel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.panels[id]
	return p, ok
}

var _ contract.PanelRegistry = (*registry)(nil)
