// Tests for buildEntry's context-window resolution: the yaml declaration is
// authoritative when set (声明即真相); priority is NEVER rewritten by the pool
// (the old small-context demote is gone, and the picker is fully window-blind
// — the turn sizes to the static winner's declared window, WindowFor).
//
// White-box (package model) — buildEntry is unexported. allowProbe=false
// keeps the tests deterministic (no network probe); the yaml ContextWindow
// then drives the resolution.
package model

import "testing"

func ctxEntry(name string, priority, contextWindow int) Entry {
	return Entry{
		Name:          name,
		Provider:      "openrouter", // has a preset → base_url resolves, no probe path
		Model:         name + "-model",
		Priority:      priority,
		ContextWindow: contextWindow,
		Tags:          []string{"smart"},
	}
}

func TestBuildEntryKeepsPriorityForSmallContext(t *testing.T) {
	// A 32K-window entry keeps its declared priority — no demote, no window
	// routing: an oversized prompt 400s and the turn compacts to this window.
	row, err := buildEntry(0, ctxEntry("small", 3, 32000), false)
	if err != nil {
		t.Fatalf("buildEntry: %v", err)
	}
	if row.info.Priority != 3 {
		t.Fatalf("priority must be the operator's declaration: want 3, got %d", row.info.Priority)
	}
	if row.info.ContextWindow != 32000 || row.info.ContextSource != "yaml" {
		t.Fatalf("declared window must win: got window=%d source=%q", row.info.ContextWindow, row.info.ContextSource)
	}
}

func TestBuildEntryKeepsBigContext(t *testing.T) {
	row, err := buildEntry(0, ctxEntry("big", 3, 200000), false)
	if err != nil {
		t.Fatalf("buildEntry: %v", err)
	}
	if row.info.Priority != 3 {
		t.Fatalf("a 200K-context entry must keep its priority 3, got %d", row.info.Priority)
	}
}

func TestBuildEntryNegativePrioritySurvives(t *testing.T) {
	// An operator-authored backup priority is preserved verbatim.
	row, err := buildEntry(0, ctxEntry("backup", -10, 32000), false)
	if err != nil {
		t.Fatalf("buildEntry: %v", err)
	}
	if row.info.Priority != -10 {
		t.Fatalf("operator priority must survive: want -10, got %d", row.info.Priority)
	}
}

func TestBuildEntryDisabledUntouched(t *testing.T) {
	// disabled (< -10000) → the disabled marker is preserved.
	row, err := buildEntry(0, ctxEntry("off", -10001, 32000), false)
	if err != nil {
		t.Fatalf("buildEntry: %v", err)
	}
	if row.info.Priority != -10001 {
		t.Fatalf("a disabled entry must stay disabled, got priority %d", row.info.Priority)
	}
}

func TestBuildEntryUnknownContextDefaults128K(t *testing.T) {
	// no probe, no yaml context, no preset default → the 128K fallback.
	row, err := buildEntry(0, ctxEntry("unknown", 3, 0), false)
	if err != nil {
		t.Fatalf("buildEntry: %v", err)
	}
	if row.info.ContextWindow != fallbackContextWindow {
		t.Fatalf("an unknown context window must default to %d, got %d",
			fallbackContextWindow, row.info.ContextWindow)
	}
	if row.info.Priority != 3 {
		t.Fatalf("priority must be untouched, got %d", row.info.Priority)
	}
}

func TestBuildEntryYamlContextWins(t *testing.T) {
	row, err := buildEntry(0, ctxEntry("yaml-set", 3, 64000), false)
	if err != nil {
		t.Fatalf("buildEntry: %v", err)
	}
	if row.info.ContextWindow != 64000 || row.info.ContextSource != "yaml" {
		t.Fatalf("the yaml context window is authoritative: got window=%d source=%q",
			row.info.ContextWindow, row.info.ContextSource)
	}
}
