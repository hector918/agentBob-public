package core

import "context"

// Memory is the agent's cross-session, cross-turn knowledge store. Layered
// markdown files on disk; the layer chain is resolved per-turn from AgentCtx
// (mainly Event.Source / UserID / ChatID / ChatType).
//
// Layer chain (root → leaf):
//
//	constitution            $BOB_HOME/memory/constitution.md
//	platform/<src>          $BOB_HOME/memory/platforms/<src>.md
//	user/<src>/<userID>     $BOB_HOME/memory/users/<src>/<userID>.md
//
// (A per-group layer existed but was removed — never fed
// end-to-end; turnlc only ever wrote platform-level insight files.)
//
//: Recall + Save methods removed (memory_recall + memory_save
// tools deleted). Cross-turn writes happen via `turnlc/30-sweep.go` distilling
// `turn_learnings` into the same layer files; admin-edits via webui YAML
// editor or direct file edit. ReadChain stays as the single read-side
// contract feeding the system prompt.
type Memory interface {
	// ReadChain returns the layered memory concatenated (root-first, each
	// layer headed by "# Memory: <layer>"). Consumed by the compression token
	// budget + the /memory slash display. Missing layer files are silently
	// skipped — they aren't errors.
	ReadChain(ctx context.Context, actx AgentCtx) (string, error)

	// RenderLayer reads ONE memory layer for actx and applies its injection
	// extraction rule, returning the contributed content + its label. ok=false
	// when the layer doesn't apply or the file is missing/empty (a read error
	// is logged + treated as empty — a memory layer must never fail the turn).
	//
	// This is the SINGLE source of truth that BOTH the prompt-injection sources
	// AND ReadChain (budget + /memory) consume, so the three never diverge.
	// Extraction by layer:
	//   - "constitution" / "platform" → whole file (admin-curated)
	//   - "user"                      → only the `## Insights` section
	// Path layout, id sanitisation, and bot-namespacing all live behind this.
	RenderLayer(actx AgentCtx, layer string) (content, label string, ok bool)
}
