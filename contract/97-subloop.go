package contract

import "context"

// This file is the delegation seam (spec §7): a tool can run ONE blocking,
// depth-1, serial sub-turn on a clean child context and get back only its product.
// Delegation is not a concurrency primitive — it's a long tool call whose payoff is
// context cleanliness (the parent history holds only the task brief + the product;
// DOM / image bytes / intermediate steps never enter the parent context).

// SubSpec is a delegated sub-turn request the delegate tool hands the SubRunner.
type SubSpec struct {
	// Task is the SELF-CONTAINED brief — it becomes the sub-turn's first user
	// message. The sub has no channel to ask the user back, so the parent must write
	// everything the sub needs (goal, constraints, acceptance) into it.
	Task string
	// Suite is the FIXED tool-name set the sub may use — narrowed from the parent's
	// authorized bag (the model picks a suite NAME, never a free tool list: one less
	// injection surface). A name outside it is refused at the dispatch layer (I17).
	Suite []string
	// Guide is the suite's operation guide — part of the sub's slim system prompt.
	Guide string
	// Skills are parent-NAMED skills whose bodies are inlined into the slim prompt
	// (the sub gets no skill-discovery tool).
	Skills []string
	// ModelRequires are HARD model tags for the sub's rounds (ModelRequest.Requires).
	// The advisor tool pins its consultation to the `advisor` tag (the pool's
	// tag-fallback ruleset routes it to `smart` when no dedicated advisor model is
	// deployed). Empty → the plain KindLLM pick, same as before.
	ModelRequires []string
	// ShareHistory hands the sub a READ-ONLY view of the PARENT's conversation
	// (ToolContext.History inside the sub). The advisor sets it — its whole point
	// is auditing the parent's actual trajectory instead of trusting the asker's
	// retelling. The delegate worker deliberately does NOT: a clean context is its
	// reason to exist.
	ShareHistory bool
}

// SubResult is a sub-turn's outcome. Product is the sub's final reply (the thing the
// parent tool returns). Partial is true when the sub ended WITHOUT a clean final
// (iter cap / wall-clock / a guard) — an honest partial product (I18), never passed
// off as clean. Err is set only when the product has no reader (parent cancelled) or
// a fatal setup failure; a sub panic becomes an Err product, never crashes the
// parent turn (I18).
type SubResult struct {
	Product string
	Partial bool
	Err     error
}

// SubRunner runs one sub-turn (spec §7). Provided to a tool via ToolContext.Sub by
// the turn core; INSIDE a sub-turn it refuses (depth 1 is a hard rule).
type SubRunner interface {
	RunSub(ctx context.Context, spec SubSpec) SubResult
}
