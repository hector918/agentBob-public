package browserremote

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentbob/contract"
)

// TestRun_BackendUnconfigured: with no BROWSERD_URL the tool is still registered
// (visible) but a Run reports the backend unavailable instead of attempting a
// doomed connection — and instead of the tool vanishing from the catalog.
func TestRun_BackendUnconfigured(t *testing.T) {
	tool := NewTool(New("", ""), nil, nil, nil) // empty base = BROWSERD_URL unset; nil holds = never held; nil vision
	res := tool.Run(context.Background(), contract.ToolContext{Sid: "s"},
		json.RawMessage(`{"action":"browser_navigate","url":"https://example.com"}`))
	if res.OK {
		t.Fatalf("unconfigured backend must not succeed, got %+v", res)
	}
	if !strings.Contains(res.Error, "未接入") {
		t.Fatalf("expected a backend-unavailable error, got %q", res.Error)
	}
}

// TestConfigured reflects the BROWSERD_URL presence gate.
func TestConfigured(t *testing.T) {
	if New("", "").Configured() {
		t.Error("empty base must be unconfigured")
	}
	if !New("http://browserd.invalid:8377", "").Configured() {
		t.Error("a set base must be configured")
	}
}
