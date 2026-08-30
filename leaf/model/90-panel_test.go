package model

import (
	"testing"
	"time"

	"agentbob/contract"
)

func TestGroupDigits(t *testing.T) {
	cases := map[int64]string{
		0:          "0",
		7:          "7",
		999:        "999",
		1000:       "1,000",
		12345:      "12,345",
		1234567:    "1,234,567",
		1000000000: "1,000,000,000",
		-45678:     "-45,678",
	}
	for in, want := range cases {
		if got := groupDigits(in); got != want {
			t.Errorf("groupDigits(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestStateText_CoolingShowsRemaining(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cooling := contract.ModelInfo{State: "cooling", DeadUntilUnix: now.Unix() + 42}
	if got := stateText(cooling, now); got != "cooling 42s" {
		t.Errorf("stateText(cooling) = %q, want %q", got, "cooling 42s")
	}
	// A live entry carries no cooldown → the state stands alone.
	if got := stateText(contract.ModelInfo{State: "live"}, now); got != "live" {
		t.Errorf("stateText(live) = %q, want %q", got, "live")
	}
	// An elapsed cooldown must not render a negative duration while the pool
	// waits to flip the state on its next touch.
	stale := contract.ModelInfo{State: "cooling", DeadUntilUnix: now.Unix() - 5}
	if got := stateText(stale, now); got != "cooling" {
		t.Errorf("stateText(elapsed) = %q, want %q", got, "cooling")
	}
	// A paused / disabled entry keeps its deadUntil (Pause doesn't clear it; a
	// reload copies it across) but does NOT lift when it elapses — a countdown
	// there would promise an auto-resume that never comes.
	for _, state := range []string{"paused", "disabled"} {
		e := contract.ModelInfo{State: state, DeadUntilUnix: now.Unix() + 300}
		if got := stateText(e, now); got != state {
			t.Errorf("stateText(%s with cooldown) = %q, want %q", state, got, state)
		}
	}
}

// TotalCalls books successes only, so the rate's denominator is calls+errors.
func TestErrText_RateAgainstAttempts(t *testing.T) {
	cases := []struct {
		errs, calls int64
		want        string
	}{
		{0, 80000, "0"}, // clean entry stays a bare zero
		{300, 80000, "300 (0.4%)"},
		{300, 300, "300 (50.0%)"}, // 600 attempts, half of them failed
		{90, 10, "90 (90.0%)"},    // a flapping backend, not "900%"
		{5, 0, "5 (100.0%)"},      // never succeeded — the worst entry, now legible
		{3, 80000, "3 (<0.1%)"},   // real errors never render as a flat "0.0%"
	}
	for _, c := range cases {
		got := errText(contract.ModelInfo{TotalErrors: c.errs, TotalCalls: c.calls})
		if got != c.want {
			t.Errorf("errText(%d/%d) = %q, want %q", c.errs, c.calls, got, c.want)
		}
	}
}

func TestEntryLimits_CarriesProvenance(t *testing.T) {
	e := contract.ModelInfo{
		ContextWindow: 128000, ContextSource: "yaml",
		Concurrency: 4, ConcurrencySource: "probe:vllm",
		Priority: 20,
	}
	want := "context 128,000 (yaml) · concurrency 4 (probe:vllm) · priority 20"
	if got := entryLimits(e); got != want {
		t.Errorf("entryLimits =\n%q\nwant\n%q", got, want)
	}
	// No concurrency cap is a fact worth stating, not an omission — and so is the
	// default priority, which is a rank the picker compares, not missing data.
	wantZero := "concurrency unlimited · priority 0"
	if got := entryLimits(contract.ModelInfo{}); got != wantZero {
		t.Errorf("entryLimits(zero) = %q, want %q", got, wantZero)
	}
}
