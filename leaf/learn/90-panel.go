package learn

import (
	"context"
	"strconv"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// panel is the learn engine's self-description for the webui (docs/webui-trunk.md).
// It surfaces what the optimizer is working over: the registered sources, every
// target's distill watermark, and whether a distiller model is wired. The home
// graph draws learn downstream of the catalogs it reads (tools/skills/agora
// role playbooks) and the model pool it distills through.
func (m *Module) panel() contract.Panel {
	return contract.Panel{
		ID:        "learn",
		Title:     "Learn",
		AdminOnly: true, // distill target keys expose tool/skill/company internals
		Upstreams: []string{"tools", "skills", "agora", "model-pool"},
		State:     func(ctx context.Context) []contract.StateField { return m.eng.snapshot(ctx) },
	}
}

// snapshot renders the engine's live state for the webui panel. It enumerates each
// source's targets fresh (mirrors runCycle) so dynamic targets show up, and pairs
// each with its last-distilled watermark. A copy of lastRun is taken under the lock
// — the sweep worker mutates it, the panel only reads.
func (e *engine) snapshot(ctx context.Context) []contract.StateField {
	e.mu.Lock()
	srcs := append([]contract.LearnSource(nil), e.sources...)
	last := make(map[string]string, len(e.lastRun))
	for k, v := range e.lastRun {
		if !v.IsZero() {
			last[k] = clock.Stamp(v)
		}
	}
	e.mu.Unlock()

	distiller, dstatus := "absent", "warn"
	if e.model() != nil {
		distiller, dstatus = "wired", "ok"
	}

	rows := make([][]contract.Cell, 0)
	for _, src := range srcs {
		for _, l := range src.Targets(ctx) {
			key := l.Key()
			when := last[key]
			if when == "" {
				when = "never"
			}
			rows = append(rows, []contract.Cell{{Text: key}, {Text: when}})
		}
	}
	return []contract.StateField{
		{Kind: "stat", Label: "sources", Value: strconv.Itoa(len(srcs))},
		{Kind: "stat", Label: "targets", Value: strconv.Itoa(len(rows))},
		{Kind: "stat", Label: "distiller", Value: distiller, Status: dstatus},
		{Kind: "table", Label: "targets", Columns: []string{"target", "last distilled"}, Rows: rows},
	}
}
