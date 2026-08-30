package core

import (
	"context"
	"sync"
)

// KindTokens is one model-kind's accumulated token counts within a turn.
type KindTokens struct{ In, Out int64 }

// TurnUsage accumulates one turn's real API token cost across all its model
// calls (tool-loop rounds + streaming + salvage), broken down by model KIND
// (llm / ocr / asr / translate / …). The orchestrator creates one per turn,
// stashes it on the turn context (WithTurnUsage), and the model pool's per-call
// success bookkeeping adds each call's usage to it tagged by the served entry's
// kind; at turn end the orchestrator reads ByKind() and records it against the
// triggering sender's handle (docs/accounts.md §13.1). Concurrency-safe — a
// turn may stream while a background call records.
type TurnUsage struct {
	mu     sync.Mutex
	byKind map[string]KindTokens
}

// Add folds one model call's input/output token counts in, under model kind
// `kind` (core.KindLLM / KindOCR / …). nil-safe.
//
// INVARIANT: every kind added here is TOKEN-counted (llm / ocr / translate /
// embedding) — In/Out are token counts, so per-kind sums (and any cross-kind
// aggregation a reader does over ByKind) stay meaningful. Native-unit services
// (ASR seconds, TTS chars) MUST NOT be Add()ed; they need a separate
// accounting seam (a unit dimension or a parallel ledger) — decide that before
// wiring the first non-token kind.
func (u *TurnUsage) Add(kind string, inputTokens, outputTokens int64) {
	if u == nil {
		return
	}
	u.mu.Lock()
	if u.byKind == nil {
		u.byKind = map[string]KindTokens{}
	}
	k := u.byKind[kind]
	k.In += inputTokens
	k.Out += outputTokens
	u.byKind[kind] = k
	u.mu.Unlock()
}

// ByKind returns a copy of the per-kind token breakdown. nil-safe → nil. The
// returned map is a snapshot the caller owns (safe to read after the turn).
func (u *TurnUsage) ByKind() map[string]KindTokens {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]KindTokens, len(u.byKind))
	for kind, k := range u.byKind {
		out[kind] = k
	}
	return out
}

type turnUsageKey struct{}

// WithTurnUsage returns ctx carrying u so downstream model calls can Add to it.
func WithTurnUsage(ctx context.Context, u *TurnUsage) context.Context {
	return context.WithValue(ctx, turnUsageKey{}, u)
}

// TurnUsageFrom returns the turn's accumulator, or nil when none is set (e.g. a
// model call outside a turn — side-LLM, judge). Callers must nil-tolerate.
func TurnUsageFrom(ctx context.Context) *TurnUsage {
	u, _ := ctx.Value(turnUsageKey{}).(*TurnUsage)
	return u
}
