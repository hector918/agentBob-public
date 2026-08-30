package modelgate

import (
	"context"
	"strconv"

	"agentbob/contract"
)

// panel is modelgate's self-description for the webui: whether the API is serving,
// where, and how many LLM entries the pool can hand out (the lane nearly every key
// is admitted to — a key may hold several, and each request names the one it wants).
// Admin-only — the bind address is operational detail. It holds no key data (the
// APIKeys contract is verify-only; key management + listing lives on the accounts
// panel / slash).
func (m *Module) panel() contract.Panel {
	return contract.Panel{
		ID:        "modelgate",
		Title:     "Model API",
		AdminOnly: true,
		Upstreams: []string{"model-pool"},
		State: func(_ context.Context) []contract.StateField {
			serving := m.srv != nil
			addr := m.addr
			if addr == "" {
				addr = "off"
			}
			llm := 0
			if m.pool != nil {
				for _, e := range m.pool.Snapshot().Entries {
					if e.Kind == contract.KindLLM && e.State != "disabled" {
						llm++
					}
				}
			}
			status := "down"
			if serving {
				status = "ok"
			}
			return []contract.StateField{
				{Kind: "status", Label: "serving", Value: boolText(serving), Status: status},
				{Kind: "stat", Label: "address", Value: addr},
				{Kind: "stat", Label: "llm entries", Value: strconv.Itoa(llm)},
			}
		},
	}
}

func boolText(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
