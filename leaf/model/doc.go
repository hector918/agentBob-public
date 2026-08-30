// Package model is the agent's model-pool layer.
//
// It routes model requests (Request{Requires, Prefer, Tools, PinnedEntry}) to
// LLM endpoints, handles fallback on errors, tracks per-entry liveness
// (passive circuit breaker), records usage to the model_usage table, and
// manages runtime pause/resume.
//
// Two implementations of contract.ModelPool live here:
//   - the empty pool state — no model configured; every chat call errors while
//     the mtime watch waits for a fixed models.yaml (hot-loads, no restart)
//   - MultiPool — full models.yaml; tag-match + fallback + liveness
//
// The agent only depends on the contract.ModelPool interface, so "pool module
// rolled back to single model" works without touching agent code.
//
// Submodule providers/ holds the contract.Chatter implementations (OpenAI-compat,
// Anthropic, …) and the provider preset table.
package model
