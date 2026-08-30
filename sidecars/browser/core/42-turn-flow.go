package core

import "fmt"

// TurnFlowState is the unified state machine of one agent turn. Numbered
// segments — read the first digit to know what phase:
//
//	1X: stream (main model reply) outcome
//	2X: degenerate handler outcome  (only after a 1X degenerate)
//	3X: turn exit reason            (loop-cap / safety guard / tool-requested)
//
// A turn traces through multiple states recorded in TurnState.FlowTrail
// (the single source of truth). The "primary reason" projection used by
// the degenerate dump comes from TurnState.TrailDiagnostic() — first 3X
// exit signal, else last entry.
type TurnFlowState int

const (
	FlowUnknown TurnFlowState = 0 // never emitted; sentinel for unset

	// 1X — main model stream outcome (always present in a real turn)
	Flow10StreamOK                TurnFlowState = 10
	Flow11StreamDegenerate        TurnFlowState = 11
	Flow12StreamCancelled         TurnFlowState = 12
	Flow13StreamBrokeAfterContent TurnFlowState = 13

	// 2X — degenerate handler outcome (only if 1X=11)
	Flow20DegenSalvageStarted TurnFlowState = 20 // entered handleDegenerate; pre-decision
	Flow21DegenSalvaged       TurnFlowState = 21 // a salvage model (small OR expert) returned non-degenerate text; see meta.salvage_model
	Flow22DegenSynthetic      TurnFlowState = 22 // salvage failed; turnlc actions-summary delivered
	Flow23DegenStock          TurnFlowState = 23 // all above failed; stock template delivered
	Flow24DegenNoticeFail     TurnFlowState = 24 // even the notice didn't reach sink

	// 3X — turn exit signals (set by safety guard / tool / iter cap / ctx cancel)
	Flow30ExitCtxCancelled  TurnFlowState = 30
	Flow31ExitStuckLoop     TurnFlowState = 31
	Flow32ExitURLExhausted  TurnFlowState = 32
	Flow33ExitDomainStuck   TurnFlowState = 33
	Flow34ExitToolRequested TurnFlowState = 34 // a tool set ExitRequest in its ToolResult
	Flow35ExitIterCap       TurnFlowState = 35 // loopCap reached without an explicit reason
	// Flow36 is the synthetic exit handleDegenerate stamps before calling
	// bestEffortFallback — type-safe replacement for the old
	// Flow11+Detail=="degenerate" hack. Routes the salvage nudge + suppresses
	// the duplicate observer fan-out (handleDegenerate owns the emit).
	Flow36ExitDegenerateSalvage TurnFlowState = 36
)

// String returns the human-readable name used in logs + emit meta.
// Format: "11_stream_degenerate" — flat ordinal + segment + state.
func (s TurnFlowState) String() string {
	switch s {
	case FlowUnknown:
		return "unknown"
	case Flow10StreamOK:
		return "10_stream_ok"
	case Flow11StreamDegenerate:
		return "11_stream_degenerate"
	case Flow12StreamCancelled:
		return "12_stream_cancelled"
	case Flow13StreamBrokeAfterContent:
		return "13_stream_broke_after_content"
	case Flow20DegenSalvageStarted:
		return "20_degen_salvage_started"
	case Flow21DegenSalvaged:
		return "21_degen_salvaged"
	case Flow22DegenSynthetic:
		return "22_degen_synthetic"
	case Flow23DegenStock:
		return "23_degen_stock"
	case Flow24DegenNoticeFail:
		return "24_degen_notice_fail"
	case Flow30ExitCtxCancelled:
		return "30_exit_ctx_cancelled"
	case Flow31ExitStuckLoop:
		return "31_exit_stuck_loop"
	case Flow32ExitURLExhausted:
		return "32_exit_url_exhausted"
	case Flow33ExitDomainStuck:
		return "33_exit_domain_stuck"
	case Flow34ExitToolRequested:
		return "34_exit_tool_requested"
	case Flow35ExitIterCap:
		return "35_exit_iter_cap"
	case Flow36ExitDegenerateSalvage:
		return "36_exit_degenerate_salvage"
	}
	return fmt.Sprintf("unknown_%d", int(s))
}

// EndReason returns the legacy core.EndReason string this flow state
// maps to (for store persistence + observer compatibility). Stream/Degen
// states all map to EndReasonDegenerate; exit states map per signal.
func (s TurnFlowState) EndReason() string {
	switch s {
	case Flow11StreamDegenerate, Flow20DegenSalvageStarted, Flow21DegenSalvaged,
		Flow22DegenSynthetic, Flow23DegenStock, Flow24DegenNoticeFail,
		Flow36ExitDegenerateSalvage:
		return EndReasonDegenerate
	case Flow30ExitCtxCancelled:
		return EndReasonCtxCancelled
	case Flow31ExitStuckLoop:
		return EndReasonStuckLoop
	case Flow32ExitURLExhausted:
		return EndReasonURLExhausted
	case Flow33ExitDomainStuck:
		return EndReasonDomainStuck
	case Flow34ExitToolRequested:
		return EndReasonToolRequested
	case Flow35ExitIterCap:
		return EndReasonIterCap
	}
	return ""
}

// TurnExit carries the exit reason + a free-form detail string
// (tool name, URL, hostname, user-facing reply for ToolRequested, etc.).
// Used in ToolResult.ExitRequest and tCtx.SetExit.
type TurnExit struct {
	State  TurnFlowState // must be a 3X value
	Detail string
}
