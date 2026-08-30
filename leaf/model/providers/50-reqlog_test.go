package providers

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactSystemContent pins that the system prompt is stripped from a
// logged request body — for both the OpenAI messages-array shape and the
// Anthropic top-level system field — while everything else survives and
// the result stays valid JSON.
func TestRedactSystemContent(t *testing.T) {
	t.Run("openai messages array", func(t *testing.T) {
		in := []byte(`{"model":"m","messages":[` +
			`{"role":"system","content":"you are a very long constant prompt"},` +
			`{"role":"user","content":"hi"}]}`)
		out := redactSystemContent(in)

		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("output not valid JSON: %v", err)
		}
		msgs := m["messages"].([]any)
		sys := msgs[0].(map[string]any)
		if s, _ := sys["content"].(string); !strings.HasPrefix(s, "<system omitted:") {
			t.Fatalf("system content not redacted: %q", s)
		}
		// User message and sibling fields untouched.
		usr := msgs[1].(map[string]any)
		if usr["content"] != "hi" {
			t.Fatalf("user content altered: %v", usr["content"])
		}
		if m["model"] != "m" {
			t.Fatalf("model field altered: %v", m["model"])
		}
	})

	t.Run("anthropic top-level system", func(t *testing.T) {
		in := []byte(`{"model":"m","system":"constant prompt","messages":[{"role":"user","content":"hi"}]}`)
		out := redactSystemContent(in)

		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("output not valid JSON: %v", err)
		}
		if s, _ := m["system"].(string); !strings.HasPrefix(s, "<system omitted:") {
			t.Fatalf("system field not redacted: %q", s)
		}
	})

	t.Run("anthropic array-form system (multimodal blocks)", func(t *testing.T) {
		// Anthropic system can be an array of content blocks, not a string —
		// redactionMarker's default branch must collapse it to the marker and
		// keep the body valid JSON.
		in := []byte(`{"model":"m","system":[{"type":"text","text":"block one"},{"type":"text","text":"block two"}],"messages":[{"role":"user","content":"hi"}]}`)
		out := redactSystemContent(in)

		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("output not valid JSON: %v", err)
		}
		s, ok := m["system"].(string)
		if !ok || !strings.HasPrefix(s, "<system omitted:") {
			t.Fatalf("array-form system not collapsed to marker: %v", m["system"])
		}
	})

	t.Run("no system → unchanged", func(t *testing.T) {
		in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		out := redactSystemContent(in)
		if string(out) != string(in) {
			t.Fatalf("body without system was modified:\n in: %s\nout: %s", in, out)
		}
	})

	t.Run("invalid JSON → unchanged", func(t *testing.T) {
		in := []byte(`not json at all`)
		out := redactSystemContent(in)
		if string(out) != string(in) {
			t.Fatalf("invalid-JSON body was modified: %s", out)
		}
	})
}
