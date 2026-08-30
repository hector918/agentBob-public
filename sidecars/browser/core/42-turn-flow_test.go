package core

import "testing"

// TestTurnFlowState_String pins the string form for every defined
// constant. The strings are used in observer meta + degenerate dump
// records; any drift would silently break log queries.
func TestTurnFlowState_String(t *testing.T) {
	cases := []struct {
		s    TurnFlowState
		want string
	}{
		{FlowUnknown, "unknown"},
		{Flow10StreamOK, "10_stream_ok"},
		{Flow11StreamDegenerate, "11_stream_degenerate"},
		{Flow12StreamCancelled, "12_stream_cancelled"},
		{Flow13StreamBrokeAfterContent, "13_stream_broke_after_content"},
		{Flow20DegenSalvageStarted, "20_degen_salvage_started"},
		{Flow21DegenSalvaged, "21_degen_salvaged"},
		{Flow22DegenSynthetic, "22_degen_synthetic"},
		{Flow23DegenStock, "23_degen_stock"},
		{Flow24DegenNoticeFail, "24_degen_notice_fail"},
		{Flow30ExitCtxCancelled, "30_exit_ctx_cancelled"},
		{Flow31ExitStuckLoop, "31_exit_stuck_loop"},
		{Flow32ExitURLExhausted, "32_exit_url_exhausted"},
		{Flow33ExitDomainStuck, "33_exit_domain_stuck"},
		{Flow34ExitToolRequested, "34_exit_tool_requested"},
		{Flow35ExitIterCap, "35_exit_iter_cap"},
		{Flow36ExitDegenerateSalvage, "36_exit_degenerate_salvage"},
		{TurnFlowState(99), "unknown_99"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("TurnFlowState(%d).String() = %q, want %q", int(tc.s), got, tc.want)
		}
	}
}

// TestTurnFlowState_EndReason pins the mapping to the legacy EndReason
// strings. Observer / store rows still see those strings; this is the
// shim that keeps the contract.
func TestTurnFlowState_EndReason(t *testing.T) {
	cases := []struct {
		s    TurnFlowState
		want string
	}{
		{Flow11StreamDegenerate, EndReasonDegenerate},
		{Flow20DegenSalvageStarted, EndReasonDegenerate},
		{Flow21DegenSalvaged, EndReasonDegenerate},
		{Flow22DegenSynthetic, EndReasonDegenerate},
		{Flow23DegenStock, EndReasonDegenerate},
		{Flow24DegenNoticeFail, EndReasonDegenerate},
		{Flow30ExitCtxCancelled, EndReasonCtxCancelled},
		{Flow31ExitStuckLoop, EndReasonStuckLoop},
		{Flow32ExitURLExhausted, EndReasonURLExhausted},
		{Flow33ExitDomainStuck, EndReasonDomainStuck},
		{Flow34ExitToolRequested, EndReasonToolRequested},
		{Flow35ExitIterCap, EndReasonIterCap},
		{Flow36ExitDegenerateSalvage, EndReasonDegenerate},
		{FlowUnknown, ""},
		{Flow10StreamOK, ""}, // not a terminal failure state
	}
	for _, tc := range cases {
		if got := tc.s.EndReason(); got != tc.want {
			t.Errorf("TurnFlowState(%s).EndReason() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// TestTurnExit_ZeroValue asserts the zero value is FlowUnknown so callers
// can rely on `exit != nil && exit.State != FlowUnknown` as a validity
// check. Empty Detail is fine (most exits don't carry one).
func TestTurnExit_ZeroValue(t *testing.T) {
	var e TurnExit
	if e.State != FlowUnknown {
		t.Errorf("zero TurnExit.State = %v, want FlowUnknown", e.State)
	}
	if e.Detail != "" {
		t.Errorf("zero TurnExit.Detail = %q, want empty", e.Detail)
	}
}
