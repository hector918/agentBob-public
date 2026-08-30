package modelcfg

import "testing"

// base returns a minimal valid single-entry config; tests mutate the kind.
func base(kind string) *ModelsConfig {
	return &ModelsConfig{Entries: []ModelEntry{
		{Name: "m1", Provider: "openai", Model: "gpt", Kind: kind},
	}}
}

func TestValidate_KindClosedSet(t *testing.T) {
	// Accepted: empty (→ llm default) and every known kind, case-insensitively.
	for _, k := range []string{"", "llm", "asr", "translate", "ocr", "ASR", "  Llm  ", "OCR"} {
		if err := base(k).Validate(); err != nil {
			t.Errorf("kind %q: want accepted, got %v", k, err)
		}
	}
	// Rejected: a typo is not a routing class — reject the whole config so it
	// degrades to the NullPool floor instead of loading a never-routable entry.
	for _, k := range []string{"lmm", "chat", "vision", "asr2"} {
		if err := base(k).Validate(); err == nil {
			t.Errorf("kind %q: want rejected, got nil", k)
		}
	}
}
