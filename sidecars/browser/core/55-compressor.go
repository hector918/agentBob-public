package core

import "context"

// Compressor turns a span of older conversation messages into a compact summary.
// It is the seam for context compression: the agent decides *when* to compress and
// *how to fold the summary back into the session* (a new generation, summary first);
// the Compressor only has to produce the summary text. The default implementation
// (agent-orchestrator) is a single LLM call with a placeholder prompt — a better
// prompt, a cheaper dedicated model, Hermes-style multi-phase, etc. plug in here
// without touching anything else.
type Compressor interface {
	// Compress summarizes msgs (oldest→newest) into a short text. The caller has
	// already chosen which messages to summarize and which recent ones to keep
	// verbatim. An error means "couldn't summarize" — the caller should proceed
	// with the (over-budget) history rather than lose it.
	Compress(ctx context.Context, msgs []Message) (summary string, err error)
}
