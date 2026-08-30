// Package core: orchestrator.go declares the SideRunner abstraction
// that the agent-orchestrator implements. Sub-packages of
// agent-orchestrator (coretools, turnlc, tools/*) currently can't
// import the orchestrator directly (Go circular-import rule); they
// take a core.SideRunner via dependency injection instead.
//
// See docs/agent-orchestrator-design.md (TODO post-Phase 1) and
// /tmp/agent-orchestrator-refactor-plan.md for the migration plan.
//
// Phase 0: interface declared; no impl wired yet.

package core

import "context"

// SideKind classifies a side-LLM call. Each kind has a fixed prompt
// template owned by the prompt module's BuildSide. The agent-orchestrator
// Service.RunSide dispatches by kind.
//
// Adding a new SideKind:
//  1. Add the constant here (numerical order preserves wire stability)
//  2. Implement the template in prompt.BuildSide's switch
//  3. Caller picks the SideKind + supplies SidePayload + ModelRequest
type SideKind int

const (
	// Chat-style side prompts (use SidePayload.Messages / .Content):
	SideKindCompress        SideKind = iota // history compression
	SideKindToolCompress                    // oversized tool-result compression
	SideKindJudgeTodo                       // todo-completion judge
	SideKindLearningAnalyse                 // turnlc per-turn distill
	SideKindActionsSummary                  // turnlc "what did the agent do" recap
	SideKindInsightCompress                 // turnlc tool-insight compaction (file→file)
)

// String returns the canonical label used in slog audit lines.
func (k SideKind) String() string {
	switch k {
	case SideKindCompress:
		return "compress"
	case SideKindToolCompress:
		return "tool-compress"
	case SideKindJudgeTodo:
		return "judge-todo"
	case SideKindLearningAnalyse:
		return "learning-analyse"
	case SideKindActionsSummary:
		return "actions-summary"
	case SideKindInsightCompress:
		return "insight-compress"
	}
	return "unknown"
}

// SidePayload is the per-call input to RunSide. Each SideKind reads a
// specific subset of fields; others may be left zero. Kept as one struct
// for now (Q6); revisit at end of Phase 2 if per-kind concrete
// types become preferable.
type SidePayload struct {
	// Compress / LearningAnalyse / ActionsSummary read Messages.
	Messages []Message

	// ToolCompress reads Content (the oversized tool result, as one string).
	// Other kinds may also use this as freeform context. JudgeTodo
	// also passes its fully-rendered user body via Content.
	Content string
}

// SideRunner is the abstraction sub-packages depend on to make
// side-LLM calls. The concrete implementation lives in package
// agent-orchestrator (agentorch.Service) and is injected at boot by
// pipeline-startup.
//
// Why an interface in core: sub-packages of agent-orchestrator
// (coretools, turnlc, tools/*) cannot import the orchestrator
// package (would be a circular import — the orchestrator already
// imports them transitively via tool registries / prompt sources).
// SideRunner is the narrow contract they consume; the wide impl
// is constructed exactly once at startup.
type SideRunner interface {
	// RunSide composes a side-LLM prompt via prompt.BuildSide(kind, payload),
	// then dispatches via the model pool with the supplied ModelRequest.
	// Returns the raw ChatResponse so the caller can parse domain-specific
	// verdicts (e.g. todo-judge JSON) without the orchestrator caring about
	// each kind's response shape.
	RunSide(ctx context.Context, kind SideKind, payload SidePayload, req ModelRequest) (ChatResponse, error)
}
