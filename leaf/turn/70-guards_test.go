package turn

import (
	"testing"

	"agentbob/contract"
)

func okCall(name, args string) (contract.ToolCall, contract.ToolResult) {
	return contract.ToolCall{Name: name, Arguments: args}, contract.OKResult("data:" + args)
}

func TestGuard_SameCallRepeat(t *testing.T) {
	c := &core{}
	st := newTurnState()
	spec := contract.TurnSpec{}
	call, res := okCall("echo", "{}")
	// sameArgsMax=3 → the 4th identical successful call trips.
	for i := 0; i < 4; i++ {
		c.recordToolOutcome(st, i, spec, call, res)
	}
	if st.exit == nil || st.exit.state != exitStuckLoop {
		t.Fatalf("4 identical successful calls should trip STUCK_LOOP, exit=%+v", st.exit)
	}
}

// TestGuard_FailedSinceProgressResetOnProgress (§6.10): the smart-escalation
// counter measures "failures since the last progress", not lifetime failures —
// a turn that recovers (new tool info arrives) sheds the smart bias instead of
// wearing it for the rest of a long (looping) turn.
func TestGuard_FailedSinceProgressResetOnProgress(t *testing.T) {
	c := &core{}
	st := newTurnState()
	spec := contract.TurnSpec{}

	fail := contract.ErrResult("boom")
	for i := 0; i < int(smartEscalation); i++ {
		c.recordToolOutcome(st, i, spec, contract.ToolCall{Name: "web", Arguments: "{}"}, fail)
	}
	if got := routeToolRound(contract.ModelRequest{}, st); len(got.Prefer) != 2 || got.Prefer[1] != "smart" {
		t.Fatalf("after %d failures the tool round should prefer smart, got %v", smartEscalation, got.Prefer)
	}

	// Progress (a success with a NEW result hash) clears the struggle counter…
	call, res := okCall("web", `{"q":"ok"}`)
	c.recordToolOutcome(st, 5, spec, call, res)
	if st.failedSinceProgress != 0 {
		t.Fatalf("progress must reset failedSinceProgress, got %d", st.failedSinceProgress)
	}
	if got := routeToolRound(contract.ModelRequest{}, st); len(got.Prefer) != 1 {
		t.Fatalf("a recovered turn must shed the smart bias, got %v", got.Prefer)
	}

	// …and a repeat of the SAME successful result (no new info) does NOT reset:
	// three more failures then the identical success again — the counter keeps
	// counting because nothing advanced.
	for i := 6; i < 9; i++ {
		c.recordToolOutcome(st, i, spec, contract.ToolCall{Name: "web", Arguments: "{}"}, fail)
	}
	c.recordToolOutcome(st, 9, spec, call, res) // same hash as before → not progress
	if st.failedSinceProgress != 3 {
		t.Fatalf("a no-new-info success must not reset the counter, got %d, want 3", st.failedSinceProgress)
	}
}

func TestGuard_DifferentCallResets(t *testing.T) {
	c := &core{}
	st := newTurnState()
	spec := contract.TurnSpec{}
	for i := 0; i < 3; i++ {
		call, res := okCall("echo", "{}")
		c.recordToolOutcome(st, i, spec, call, res)
	}
	// A different call resets the run — no trip even after more identical ones.
	other, ores := okCall("echo", `{"x":1}`)
	c.recordToolOutcome(st, 3, spec, other, ores)
	for i := 4; i < 6; i++ {
		call, res := okCall("echo", "{}")
		c.recordToolOutcome(st, i, spec, call, res)
	}
	if st.exit != nil {
		t.Fatalf("a different call should have reset the repeat run, exit=%+v", st.exit)
	}
}

func TestGuard_ConsecutiveFailures(t *testing.T) {
	c := &core{}
	st := newTurnState()
	spec := contract.TurnSpec{}
	call := contract.ToolCall{Name: "flaky", Arguments: "{}"}
	fail := contract.ErrResult("boom")
	// sameFailMax=6 → the 7th consecutive failure trips.
	for i := 0; i < 7; i++ {
		c.recordToolOutcome(st, i, spec, call, fail)
	}
	if st.exit == nil || st.exit.state != exitStuckLoop {
		t.Fatalf("7 consecutive failures should trip STUCK_LOOP, exit=%+v", st.exit)
	}
}

func TestGuard_SuccessClearsFailStreak(t *testing.T) {
	c := &core{}
	st := newTurnState()
	spec := contract.TurnSpec{}
	call := contract.ToolCall{Name: "flaky", Arguments: "{}"}
	for i := 0; i < 5; i++ {
		c.recordToolOutcome(st, i, spec, call, contract.ErrResult("boom"))
	}
	c.recordToolOutcome(st, 5, spec, call, contract.OKResult("ok")) // success clears
	for i := 6; i < 11; i++ {
		c.recordToolOutcome(st, i, spec, call, contract.ErrResult("boom"))
	}
	if st.exit != nil {
		t.Fatalf("a success should have cleared the fail streak, exit=%+v", st.exit)
	}
}

func TestProgressPredicate(t *testing.T) {
	c := &core{}
	st := newTurnState()
	spec := contract.TurnSpec{}
	web := contract.ToolCall{Name: "web", Arguments: "{}"}
	// New info advances the progress iter; an identical result does not.
	c.recordToolOutcome(st, 0, spec, web, contract.OKResult("page-1"))
	if st.lastProgressIter != 0 {
		t.Fatalf("first new result should mark progress at iter 0, got %d", st.lastProgressIter)
	}
	c.recordToolOutcome(st, 1, spec, web, contract.OKResult("page-1")) // same hash
	if st.lastProgressIter != 0 {
		t.Errorf("an identical result must NOT mark progress, got %d", st.lastProgressIter)
	}
	c.recordToolOutcome(st, 2, spec, web, contract.OKResult("page-2")) // new hash
	if st.lastProgressIter != 2 {
		t.Errorf("a new result should mark progress at iter 2, got %d", st.lastProgressIter)
	}
	if st.lastProductiveIter != -1 {
		t.Errorf("no Delivers tool → production axis stays -1, got %d", st.lastProductiveIter)
	}
}
