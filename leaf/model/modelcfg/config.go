// Package modelcfg is the model module's own config: it parses
// $BOB_HOME/models.yaml into a validated ModelsConfig. The model module owns
// this file (per-module config); the pool builder bridges ModelsConfig entries
// into model.Entry values.
//
// PORT NOTE (trunk-rebuild): the LOAD path is ported; the Save/Update (webui
// editor, advisory file-locking) path is deferred.
package modelcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ModelsConfig is the parsed models.yaml.
type ModelsConfig struct {
	Entries  []ModelEntry  `yaml:"entries"`
	Defaults ModelDefaults `yaml:"defaults,omitempty"`
}

type ModelDefaults struct {
	MaxOutputTokens int            `yaml:"max_output_tokens,omitempty"`
	Fallback        []FallbackRule `yaml:"fallback,omitempty"`
}

// FallbackRule is the yaml shape of a tag-fallback rule (bridged into the pool's
// own FallbackRule at build time).
type FallbackRule struct {
	Tags []string `yaml:"tags"`
	To   []string `yaml:"to"`
}

type ModelEntry struct {
	Name            string         `yaml:"name"`
	Provider        string         `yaml:"provider"`
	Model           string         `yaml:"model"`
	BaseURL         string         `yaml:"base_url,omitempty"`
	APIKeyEnv       string         `yaml:"api_key_env,omitempty"`
	Kind            string         `yaml:"kind,omitempty"`
	Priority        int            `yaml:"priority,omitempty"`
	Tags            []string       `yaml:"tags,omitempty"`
	ContextWindow   int            `yaml:"context_window,omitempty"`
	Concurrency     int            `yaml:"concurrency,omitempty"`
	MaxOutputTokens int            `yaml:"max_output_tokens,omitempty"`
	ExtraBody       map[string]any `yaml:"extra_body,omitempty"`
}

// EffectiveOutputCap resolves an entry's output cap: entry → defaults → 0.
func (m *ModelsConfig) EffectiveOutputCap(e ModelEntry) int {
	if e.MaxOutputTokens > 0 {
		return e.MaxOutputTokens
	}
	if m != nil && m.Defaults.MaxOutputTokens > 0 {
		return m.Defaults.MaxOutputTokens
	}
	return 0
}

// Validate checks the parsed config: at least one entry, unique non-empty names,
// non-empty provider + model per entry.
func (c *ModelsConfig) Validate() error {
	if len(c.Entries) == 0 {
		return fmt.Errorf("entries: must have at least one entry")
	}
	seen := map[string]bool{}
	for i, e := range c.Entries {
		if strings.TrimSpace(e.Name) == "" {
			return fmt.Errorf("entries[%d].name is empty", i)
		}
		if seen[e.Name] {
			return fmt.Errorf("entries[%d]: duplicate name %q", i, e.Name)
		}
		seen[e.Name] = true
		if strings.TrimSpace(e.Provider) == "" {
			return fmt.Errorf("entries[%d] (%s).provider is empty", i, e.Name)
		}
		if strings.TrimSpace(e.Model) == "" {
			return fmt.Errorf("entries[%d] (%s).model is empty", i, e.Name)
		}
		// Kind must be a known routing class (mirror contract.Kind* in
		// contract/72-model-pool.go). Case-insensitive: buildEntry lower-cases it.
		// A typo (e.g. "lmm") would otherwise load "live" but never route + suppress
		// the all-dead alert, so reject the whole config → NullPool floor.
		switch strings.ToLower(strings.TrimSpace(e.Kind)) {
		case "", "llm", "asr", "translate", "ocr", "image":
		default:
			return fmt.Errorf("entries[%d] (%s).kind %q is not one of llm/asr/translate/ocr/image", i, e.Name, e.Kind)
		}
	}
	return nil
}

// LoadModels reads + validates $BOB_HOME/models.yaml. A missing file returns
// (nil, nil) — "no models configured".
func LoadModels(home string) (*ModelsConfig, error) {
	path := filepath.Join(home, "models.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c ModelsConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}
