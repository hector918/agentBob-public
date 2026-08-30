package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
)

func TestCatalog(t *testing.T) {
	c := newCatalog(nil, clarify{}, now{})
	if len(c.Specs()) != 2 {
		t.Fatalf("Specs len = %d, want 2", len(c.Specs()))
	}
	if _, ok := c.Lookup("clarify"); !ok {
		t.Error("clarify not found in catalog")
	}
	if _, ok := c.Lookup("now"); !ok {
		t.Error("now not found in catalog")
	}
	if _, ok := c.Lookup("ghost"); ok {
		t.Error("ghost should not be found")
	}

	// A learned addendum is appended to the tool's advertised description.
	c.setAddendum("now", "调用前先确认时区。")
	for _, s := range c.Specs() {
		if s.Name == "now" && !strings.Contains(s.Description, "调用前先确认时区。") {
			t.Errorf("learned addendum not appended to now spec: %q", s.Description)
		}
	}
}

// TestObserverRecordsFailure asserts the observer persists a SHAPE-ONLY record for a
// FAILED call and NOTHING for a success — the failure store the learn source reads via
// the tool target's Traces.
func TestObserverRecordsFailure(t *testing.T) {
	fail := &toolFailureStore{base: t.TempDir()}
	c := newCatalog(fail, now{}, clarify{})

	// A successful call (now) carries no corrective signal → not recorded.
	nowTool, _ := c.Lookup("now")
	nowTool.Run(context.Background(), contract.ToolContext{}, json.RawMessage(`{}`))
	if got := fail.since("now", time.Time{}); len(got) != 0 {
		t.Fatalf("successful call must not be recorded, got %d records", len(got))
	}

	// A failed call (empty clarify errors) → one shape-only record.
	clarifyTool, _ := c.Lookup("clarify")
	clarifyTool.Run(context.Background(), contract.ToolContext{}, json.RawMessage(`{}`))
	got := fail.since("clarify", time.Time{})
	if len(got) != 1 || got[0].OK {
		t.Fatalf("failure store = %+v, want one failed rollout for clarify", got)
	}
	if got[0].Args == "" {
		t.Errorf("recorded failure should carry the arg shape, got empty Args")
	}
}

func TestClarifyExits(t *testing.T) {
	r := clarify{}.Run(context.Background(), contract.ToolContext{}, json.RawMessage(`{"question":"哪个文件?"}`))
	if r.ExitRequest == nil || r.ExitRequest.Reply != "哪个文件?" {
		t.Fatalf("clarify result = %+v, want ExitRequest{哪个文件?}", r)
	}
}

// TestStaySilentExits: stay_silent exits the turn with an EMPTY reply — the turn then
// delivers nothing (the silent path in leaf/turn).
func TestStaySilentExits(t *testing.T) {
	r := staySilent{}.Run(context.Background(), contract.ToolContext{}, json.RawMessage(`{}`))
	if !r.OK || r.ExitRequest == nil || r.ExitRequest.Reply != "" {
		t.Fatalf("stay_silent result = %+v, want OK with an empty-reply ExitRequest", r)
	}
}

func TestClarifyRejectsEmpty(t *testing.T) {
	r := clarify{}.Run(context.Background(), contract.ToolContext{}, json.RawMessage(`{}`))
	if r.OK || r.ExitRequest != nil {
		t.Fatalf("empty clarify should error, got %+v", r)
	}
}

func TestNow(t *testing.T) {
	r := now{}.Run(context.Background(), contract.ToolContext{}, json.RawMessage(`{}`))
	if !r.OK || r.Data == "" {
		t.Fatalf("now result = %+v, want OK with a time", r)
	}
}
