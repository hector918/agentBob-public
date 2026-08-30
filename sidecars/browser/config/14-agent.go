package config

import (
	"time"
)

// DefaultMaxLoopIter is THE single fallback for the per-turn tool-round cap
// when config.yaml's agent.max_loop_iter is unset. config.yaml is the source
// of truth (the seed sets it explicitly); this const is only the "field was
// deleted" safety, and every layer (this package's MaxLoopIterEff + the
// orchestrator's effMaxLoopIter) references it so they can never drift.
//
//: 12 → 40. The prior 12 (Audit G5) starved legitimate multi-step
// work — writing N doc sections then deliver_file needs well over 12 tool
// rounds and was getting guillotined mid-task, with the send-file step never
// reached. (hermes uses 90; 40 is the local-model compromise.)
const DefaultMaxLoopIter = 40

// DefaultLoopBudget is the no-progress tolerance of the progress-budget loop
// (docs/loop-budget.md §3): a turn whose last progress (new tool information /
// production success) is ≥ this many iters behind is considered budget-dry and
// enters the blocked exit (nudge → grace → salvage). Progress refills the
// budget implicitly (staleness resets), so a healthy long task runs up to
// MaxLoopIter regardless. 12 = the hermes-era "cap 12 + refund" experience.
const DefaultLoopBudget = 12

type AgentConfig struct {
	Name string `yaml:"name"` // display name; default "bob"
	// MaxLoopIter caps how many tool rounds one user turn may run. A
	// stuck small model can spin forever; this stops it. 0 →
	// DefaultMaxLoopIter. config.yaml is the source of truth.
	MaxLoopIter int `yaml:"max_loop_iter"`
	// MaxParallelTools caps how many tool_calls in one round dispatch
	// concurrently. The model can emit any number; we honor only N at a
	// time and run the rest sequentially. Prevents subprocess flood
	// (e.g. 50 chromedp / scrapling at once). 0 → default 8.
	// Audit B2.
	MaxParallelTools int `yaml:"max_parallel_tools"`
	// ToolResultBeltCapBytes is the universal hard ceiling on a single
	// tool result's content (in bytes), applied BEFORE persistence and
	// BEFORE the optional auto-compression. Independent of
	// models.yaml `defaults.max_input_tokens` — the belt still applies
	// when compression is off. A 100K-token tool result would poison
	// every downstream step (compression OOMs, chat-call OOMs, history
	// replay slow).
	//
	// 0 → default 262144 (256 KB ≈ 64K tokens). Tune up for large-ctx
	// backends (e.g. Anthropic 200K tokens ≈ ~800 KB markdown). Tune
	// down for tight-VRAM local models. When compression IS on AND
	// (defaults.max_input_tokens/2)*4 is tighter, that wins.
	ToolResultBeltCapBytes int `yaml:"tool_result_belt_cap_bytes"`
	// ToolResultCompressThresholdTokens is the size at which a tool
	// result gets routed through the small model to summarize before
	// being fed back to the main model. Applied in
	// agent-orchestrator/40-dispatch.go's maybeCompressToolResult,
	// AFTER the belt cap (tool_result_belt_cap_bytes) and BEFORE
	// persisting the tool row to the messages table — the model
	// downstream only ever sees the summary, never the raw blob.
	//
	// Why this matters beyond just "save tokens": large raw HTML / XML
	// blobs land in history verbatim and the main model often fails
	// to extract anything useful from them (observed: 10 KB BBC RSS
	// XML left untouched and the model said "RSS 上没什么特别的"
	// while the full feed was in context). A summary of the same
	// content is more actionable — the small model parses the
	// structure and surfaces what matters before the main model has
	// to.
	//
	// 0 → default 2000 (≈ 8 KB chars). Tools that need verbatim
	// output (file reads, structured tool envelopes) set
	// Spec.NoAutoCompress=true to opt out unconditionally.
	ToolResultCompressThresholdTokens int `yaml:"tool_result_compress_threshold_tokens"`
	// SubLoopMaxIter caps the tool rounds of ONE delegated sub-loop
	// (docs/subagent.md). Lower than max_loop_iter on purpose — a
	// sub-task is a single self-contained job, not a whole turn.
	// 0 → DefaultSubLoopMaxIter.
	SubLoopMaxIter int `yaml:"subloop_max_iter"`
	// SubLoopTimeoutSeconds is the wall-clock ceiling for one delegated
	// sub-loop. The sub returns an honest partial product when it fires
	// (fail-open, docs/subagent.md D3). 0 → DefaultSubLoopTimeoutSeconds.
	SubLoopTimeoutSeconds int `yaml:"subloop_timeout_seconds"`
	// LoopBudget is the loop's no-progress tolerance in iters
	// (docs/loop-budget.md): progress (new tool information / production)
	// implicitly refills it; a turn that stays progress-free this long gets
	// the no-progress nudge, then the blocked exit. max_loop_iter remains the
	// absolute ceiling. 0 → DefaultLoopBudget.
	LoopBudget int `yaml:"loop_budget"`
}

// DefaultSubLoopMaxIter / DefaultSubLoopTimeoutSeconds — sub-loop caps
// when config.yaml leaves them unset. ~20 rounds / ~10 minutes covers a
// browser task (navigate → fill → verify) with headroom; both exits
// return an honest partial product rather than an error.
const (
	DefaultSubLoopMaxIter        = 20
	DefaultSubLoopTimeoutSeconds = 600
)

func (a AgentConfig) MaxLoopIterEff() int {
	if a.MaxLoopIter <= 0 {
		return DefaultMaxLoopIter
	}
	return a.MaxLoopIter
}

func (a AgentConfig) LoopBudgetEff() int {
	if a.LoopBudget <= 0 {
		return DefaultLoopBudget
	}
	return a.LoopBudget
}

func (a AgentConfig) MaxParallelToolsEff() int {
	if a.MaxParallelTools <= 0 {
		return 8
	}
	return a.MaxParallelTools
}

func (a AgentConfig) ToolResultBeltCapBytesEff() int {
	if a.ToolResultBeltCapBytes <= 0 {
		return 256 * 1024
	}
	return a.ToolResultBeltCapBytes
}

func (a AgentConfig) ToolResultCompressThresholdTokensEff() int {
	if a.ToolResultCompressThresholdTokens <= 0 {
		return 2000
	}
	return a.ToolResultCompressThresholdTokens
}

func (a AgentConfig) SubLoopMaxIterEff() int {
	if a.SubLoopMaxIter <= 0 {
		return DefaultSubLoopMaxIter
	}
	return a.SubLoopMaxIter
}

func (a AgentConfig) SubLoopTimeoutEff() time.Duration {
	if a.SubLoopTimeoutSeconds <= 0 {
		return DefaultSubLoopTimeoutSeconds * time.Second
	}
	return time.Duration(a.SubLoopTimeoutSeconds) * time.Second
}

// TurnLifecycleConfig configures the turn-lifecycle module's safety
// guards and sweep-time learning behavior. All fields have sensible
// defaults (see *Eff() getters); admins override only when needed.
type TurnLifecycleConfig struct {
	// Same-tool-args loop detection. After this many calls to the same
	// tool with the same arg JSON in one turn, the module sets the turn
	// exit signal to Flow31ExitStuckLoop and pushes a nudge. 0 → use default 3.
	SameToolArgsMax int `yaml:"same_tool_args_max"`
	// Same-domain-cross-mode failure threshold for fetch-family tools.
	// Catches "alternating mode / URL keeps the per-(url,mode) bucket
	// from filling up" — counts by (tool-family, host) instead.
	// 0 → default 4.
	SameDomainFailMax int `yaml:"same_domain_fail_max"`
	// Same-tool repeated-FAILURE threshold. After this many CONSECUTIVE
	// failed calls to the same tool in one turn — regardless of args, and
	// reset by any success of that tool — the module sets the turn exit to
	// Flow31ExitStuckLoop. Catches a model spamming one tool with varied
	// args that all fail (which the args-exact SameToolArgsMax misses).
	// Higher than SameToolArgsMax so a flaky-then-recovering tool has room.
	// 0 → default 6.
	SameToolFailMax int `yaml:"same_tool_fail_max"`
	// SmartEscalationFailures — after this many failed tool calls in
	// one turn, runToolRound bumps Prefer from ["toolcall"] (which
	// usually routes to a small/cheap model) to ["smart"] so the
	// larger model gets a chance to recover. 0 → default 4.
	SmartEscalationFailures int `yaml:"smart_escalation_failures"`
	// LoopDetection toggles the safety guards entirely. Default true.
	// Set to a *bool false to disable.
	LoopDetection *bool `yaml:"loop_detection"`
	// MinAgeDaysForLearning — sweep only analyses sessions ended at
	// least this many days ago (settling delay so the user can come
	// back to a session within this window). Default 3.
	MinAgeDaysForLearning int `yaml:"min_age_days_for_learning"`
	// LearningBatchTokenCap — when sweep batches multiple sessions into
	// one LLM call, split when the prompt would exceed this token
	// count. Default 8000.
	LearningBatchTokenCap int `yaml:"learning_batch_token_cap"`
	// LearningSweepLimit — at most this many sessions analysed per
	// sweep run, oldest first. Default 50 (prevents one sweep from
	// dragging on for hours after a long downtime).
	LearningSweepLimit int `yaml:"learning_sweep_limit"`
	// ToolInsightsMaxPerTool — per-tool LRU cap, counted in ENTRIES (not
	// bytes) within each "## <tool>" section of the single
	// $BOB_HOME/tools/insights.md file. autoinsight entries past this
	// count get evicted oldest-first; admin entries are never counted nor
	// evicted. Default 20. (Replaced the old byte-based
	// ToolInsightsMaxBytes in the single-file redesign —
	// docs/learning-redesign.md.)
	ToolInsightsMaxPerTool int `yaml:"tool_insights_max_per_tool"`
	// InsightCompressThreshold — the compaction pass (CompressInsights)
	// only runs on a tool section once its autoinsight entry count reaches
	// this many, so a near-empty section never burns an LLM call. Default
	// 12. See docs/learning-redesign.md §6.
	InsightCompressThreshold int `yaml:"insight_compress_threshold"`
}

func (c TurnLifecycleConfig) SameToolArgsMaxEff() int {
	if c.SameToolArgsMax <= 0 {
		return 3
	}
	return c.SameToolArgsMax
}

func (c TurnLifecycleConfig) SameDomainFailMaxEff() int {
	if c.SameDomainFailMax <= 0 {
		return 4
	}
	return c.SameDomainFailMax
}

func (c TurnLifecycleConfig) SameToolFailMaxEff() int {
	if c.SameToolFailMax <= 0 {
		return 6
	}
	return c.SameToolFailMax
}

func (c TurnLifecycleConfig) SmartEscalationFailuresEff() int {
	if c.SmartEscalationFailures <= 0 {
		return 4
	}
	return c.SmartEscalationFailures
}

func (c TurnLifecycleConfig) LoopDetectionEff() bool {
	if c.LoopDetection == nil {
		return true
	}
	return *c.LoopDetection
}

func (c TurnLifecycleConfig) MinAgeForLearning() time.Duration {
	d := c.MinAgeDaysForLearning
	if d <= 0 {
		d = 3
	}
	return time.Duration(d) * 24 * time.Hour
}

func (c TurnLifecycleConfig) LearningBatchTokenCapEff() int {
	if c.LearningBatchTokenCap <= 0 {
		return 8000
	}
	return c.LearningBatchTokenCap
}

func (c TurnLifecycleConfig) LearningSweepLimitEff() int {
	if c.LearningSweepLimit <= 0 {
		return 50
	}
	return c.LearningSweepLimit
}

func (c TurnLifecycleConfig) ToolInsightsMaxPerToolEff() int {
	if c.ToolInsightsMaxPerTool <= 0 {
		return 20
	}
	return c.ToolInsightsMaxPerTool
}

func (c TurnLifecycleConfig) InsightCompressThresholdEff() int {
	if c.InsightCompressThreshold <= 0 {
		return 12
	}
	return c.InsightCompressThreshold
}
