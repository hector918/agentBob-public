package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestModelEntryYAMLParseMinimal pins that a minimal entry (no optional
// blocks) parses cleanly — existing models.yaml files load unchanged.
func TestModelEntryYAMLParseMinimal(t *testing.T) {
	yamlBody := `
entries:
  - name: small
    provider: ollama
    model: qwen2.5:7b
`
	var mc ModelsConfig
	if err := yaml.Unmarshal([]byte(yamlBody), &mc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(mc.Entries) != 1 {
		t.Fatalf("entries=%d, want 1", len(mc.Entries))
	}
	if mc.Entries[0].Name != "small" {
		t.Errorf("name=%q, want small", mc.Entries[0].Name)
	}
}

// TestModelDefaultsYAMLRoundtrip pins the schema (per-model
// output cap): the `defaults:` block parses cleanly into all three
// fields, and the per-entry `max_output_tokens` lands in MaxOutputTokens
// + wins over defaults via EffectiveOutputCap.
func TestModelDefaultsYAMLRoundtrip(t *testing.T) {
	yamlBody := `
defaults:
  min_context_window: 65536
  max_input_tokens: 24000
  max_output_tokens: 8192
entries:
  - name: small
    provider: ollama
    model: qwen2.5:7b
    max_output_tokens: 4096
`
	var mc ModelsConfig
	if err := yaml.Unmarshal([]byte(yamlBody), &mc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mc.Defaults.MinContextWindow != 65536 {
		t.Errorf("Defaults.MinContextWindow=%d, want 65536", mc.Defaults.MinContextWindow)
	}
	if mc.Defaults.MaxInputTokens != 24000 {
		t.Errorf("Defaults.MaxInputTokens=%d, want 24000", mc.Defaults.MaxInputTokens)
	}
	if mc.Defaults.MaxOutputTokens != 8192 {
		t.Errorf("Defaults.MaxOutputTokens=%d, want 8192", mc.Defaults.MaxOutputTokens)
	}
	if got := mc.EffectiveMaxInputTokens(); got != 24000 {
		t.Errorf("EffectiveMaxInputTokens=%d, want 24000", got)
	}
	if len(mc.Entries) != 1 {
		t.Fatalf("entries=%d, want 1", len(mc.Entries))
	}
	if got := mc.Entries[0].MaxOutputTokens; got != 4096 {
		t.Errorf("MaxOutputTokens=%d, want 4096", got)
	}
	// Output cap resolution: per-entry wins over defaults (both paths).
	if got := mc.EffectiveOutputCap(mc.Entries[0]); got != 4096 {
		t.Errorf("EffectiveOutputCap=%d, want 4096 (per-entry)", got)
	}
	// An entry with no per-entry cap falls through to defaults.
	noCap := ModelEntry{Name: "x"}
	if got := mc.EffectiveOutputCap(noCap); got != 8192 {
		t.Errorf("EffectiveOutputCap(no per-entry)=%d, want 8192 (defaults)", got)
	}
}
