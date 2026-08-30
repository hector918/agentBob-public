package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// ModelsConfig is the parsed $BOB_HOME/models.yaml — the input to MultiPool.
//
// When the file is absent, the agent falls back to NullPool. The split (own
// yaml file, not part of config.yaml) was a design decision in §6 — the model
// table grows independently of static gateway/display/cleanup config.
type ModelsConfig struct {
	Entries []ModelEntry `yaml:"entries"`
	// Defaults is the top-of-file global block. Three fields today:
	//   · min_context_window — RETIRED (window-blind picker:
	//     the turn sizes to the serving entry's declared window); parsed
	//     only so editor round-trips don't drop the key from old configs
	//   · max_input_tokens — compression budget (when the assembled
	//     prompt would exceed 95% of this, older history gets summarised)
	//   · max_output_tokens — per-call output cap fallback, used for both
	//     streaming and block calls when an entry sets no per-entry
	//     max_output_tokens of its own.
	//
	// rename + restructure (per user): top-level
	// min_context_window relocated INTO defaults; per-entry output cap is
	// a single per-model max_output_tokens covering both the streaming and
	// block paths (folded the short-lived max_streaming_output_tokens /
	// defaults-only block split back together so translate/asr sidecars get
	// their own ceiling). No legacy compat — old field names are silently
	// ignored by yaml.v3 and surfaced by warnOrphanModelsKeys; admin must
	// update models.yaml on upgrade (sed lines in the orphan-key hints).
	Defaults ModelDefaults `yaml:"defaults,omitempty"`
}

// ModelDefaults — top-of-models.yaml global knobs. See ModelsConfig.Defaults
// doc for the full purpose breakdown of each field.
//
// Fallback is the tag-fallback table (distinct from entry-level failover): an
// ordered list of rules consulted when a request's required tags can't be
// served by any AVAILABLE entry (none configured, or all paused/cooling/dead).
// See FallbackRule.
type ModelDefaults struct {
	MinContextWindow int            `yaml:"min_context_window,omitempty"`
	MaxInputTokens   int            `yaml:"max_input_tokens,omitempty"`
	MaxOutputTokens  int            `yaml:"max_output_tokens,omitempty"`
	Fallback         []FallbackRule `yaml:"fallback,omitempty"`
}

// FallbackRule is one tag-fallback entry under defaults.fallback. When a model
// request requires ALL of Tags but no eligible entry has them (none
// configured, or every match is paused / cooling / dead), the pool retries the
// pick requiring To instead — the request's OTHER required tags are preserved
// (only the Tags are swapped for To). Rules chain (a To set can trigger a
// further rule) and are cycle-guarded; the first matching rule per hop wins
// (config order = precedence). A rule with an empty Tags or empty To is
// ignored. This is graceful degradation, NOT authorization — it only widens
// which model serves the turn. See model/30-pick.go::fallbackChain.
type FallbackRule struct {
	Tags []string `yaml:"tags"`
	To   []string `yaml:"to"`
}

// EffectiveMaxInputTokens returns the global compression-budget cap:
// Defaults.MaxInputTokens when set, 0 otherwise (caller falls through
// to deriving from min(entry.ContextWindow)).
func (m *ModelsConfig) EffectiveMaxInputTokens() int {
	if m == nil {
		return 0
	}
	return m.Defaults.MaxInputTokens
}

// EffectiveOutputCap resolves THIS entry's per-response output cap — the
// single knob that flows into the provider client's max_tokens for both
// the streaming and block paths. Resolution: entry.MaxOutputTokens →
// Defaults.MaxOutputTokens → 0 (provider hardcoded default). Per-entry
// wins so admin can set a model-specific cap (a smaller llama.cpp server
// with --ctx-size 4096, or a translate/asr sidecar, needs a tighter
// budget than a 128K cloud chat model in the same pool).
func (m *ModelsConfig) EffectiveOutputCap(e ModelEntry) int {
	if e.MaxOutputTokens > 0 {
		return e.MaxOutputTokens
	}
	if m != nil && m.Defaults.MaxOutputTokens > 0 {
		return m.Defaults.MaxOutputTokens
	}
	return 0
}

// ModelEntry is one pool entry. Name must be unique across entries.
// BaseURL falls back to the provider preset if empty; APIKeyEnv likewise.
// Concurrency=0 means "no limit"; usually the provider preset overrides.
//
// Priority is a static tie-breaker applied AFTER per-call Prefer-tag matching.
// The enabled range is -10000..10000 (the high end is clamped at pool-build
// time; 0 = neutral default). A value below -10000 (i.e. <= -10001) is NOT
// clamped — it marks the entry DISABLED: the pool never routes to it.
// Kind declares the entry's routing class (see core.KindLLM / KindASR /
// KindTranslate) — "llm" (the default for absent/empty) does chat; "asr"
// and "translate" are non-chat and only ever picked by a same-Kind request.
// MaxOutputTokens caps THIS entry's per-response output, on whichever
// path the entry is invoked — streaming (tool rounds, user-visible chat
// replies) or block (side-LLM callers: translate / asr / compression /
// judge / learning). 0 falls through to defaults.max_output_tokens then
// to the provider's hardcoded default. Per-model so a small-context
// backend (translate 4096, asr 1024) gets its own ceiling instead of
// the global default. ExtraBody is a per-entry raw passthrough (openai-compat entries): every
// key is merged into the /chat/completions request body, overriding bob's
// defaults. For backend-specific knobs without a dedicated field — e.g.
// Qwen3's chat_template_kwargs.enable_thinking, temperature,
// repetition_penalty. See ModelsYAMLExample for the common keys.
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

// UpdateModels runs LoadModels → cb(mc) → SaveModels under an exclusive flock
// on <home>/models.yaml.lock so concurrent `bob model add/edit/remove`
// invocations don't lose updates.
//
// When models.yaml is missing, LoadModels returns (nil, nil); UpdateModels
// seeds cb with an empty *ModelsConfig{} so callers can append a first entry
// uniformly.
//
// The lockfile is a 0-byte sentinel created on demand and never deleted.
// SaveModels itself does not take the lock — UpdateModels is the only path
// that does.
func UpdateModels(home string, cb func(*ModelsConfig) error) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	lockPath := filepath.Join(home, "models.yaml.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)

	mc, err := LoadModels(home)
	if err != nil {
		return err
	}
	if mc == nil {
		mc = &ModelsConfig{}
	}
	if err := cb(mc); err != nil {
		return err
	}
	return SaveModels(home, mc)
}

// SaveModels writes the config to $BOB_HOME/models.yaml atomically. Used by
// `bob model add/edit/remove`.
func SaveModels(home string, c *ModelsConfig) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := filepath.Join(home, "models.yaml.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(home, "models.yaml"))
}

// LoadModels reads $BOB_HOME/models.yaml. Returns (nil, nil) when the file
// doesn't exist — callers treat that as "use NullPool".
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
	warnOrphanModelsKeys(path, b)
	return &c, nil
}

// orphanModelsKeyHints maps each retired models.yaml key path to the
// new home + sed line admin should run. Keys are matched against the
// raw `map[string]any` parse of models.yaml; matches log a one-shot
// WARN at LoadModels time so admin sees the silent-drop on next boot
// or reload rather than discovering the regression months later when
// the higher-than-expected output cap shows up on the cloud bill.
//
// Tracked here (not in a separate migration doc) so adding a new
// retired key tomorrow just means appending an entry — the warner
// machinery + admin pathway stay consistent.
var orphanModelsKeyHints = map[string]string{
	"top-level min_context_window":          "retired — the turn sizes to the serving entry's declared context_window; the key is ignored wherever it sits",
	"top-level max_tokens":                  "never a valid key in models.yaml — likely escaped from config.yaml; set `defaults.max_output_tokens:` instead",
	"top-level max_input_tokens":            "moved from config.yaml → set under `defaults.max_input_tokens:`",
	"defaults.max_tokens":                   "renamed → `defaults.max_output_tokens:`",
	"per-entry max_tokens":                  "renamed → per-entry `max_output_tokens:` (sed: `s|^    max_tokens:|    max_output_tokens:|g`)",
	"per-entry max_streaming_output_tokens": "renamed → per-entry `max_output_tokens:` (one cap now covers stream + block; sed: `s|max_streaming_output_tokens:|max_output_tokens:|g`)",
	"per-entry max_block_output_tokens":     "folded into per-entry `max_output_tokens:` (one per-model cap for both paths)",
}

// warnOrphanModelsKeys re-parses the yaml body into a raw map and logs
// a one-shot WARN for every retired key it finds. Best-effort: parse
// failures are silent (the typed Unmarshal already accepted the body,
// so any raw-parse error here is unexpected — log Debug and skip).
//
// Doesn't error or modify behaviour — the typed Unmarshal silently
// dropped the keys per the "no legacy compat" rule; this just makes
// the drop loud so admin notices the regression.
func warnOrphanModelsKeys(path string, body []byte) {
	var raw map[string]any
	if err := yaml.Unmarshal(body, &raw); err != nil {
		slog.Debug("models.yaml: orphan-key scan: raw parse failed", "path", path, "err", err)
		return
	}
	check := func(label, hint string) {
		slog.Warn("models.yaml: retired key still present — value silently ignored",
			"path", path, "key", label, "hint", hint)
	}
	if _, ok := raw["min_context_window"]; ok {
		check("min_context_window", orphanModelsKeyHints["top-level min_context_window"])
	}
	if _, ok := raw["max_tokens"]; ok {
		check("max_tokens", orphanModelsKeyHints["top-level max_tokens"])
	}
	if _, ok := raw["max_input_tokens"]; ok {
		check("max_input_tokens", orphanModelsKeyHints["top-level max_input_tokens"])
	}
	if defaults, ok := raw["defaults"].(map[string]any); ok {
		if _, ok := defaults["max_tokens"]; ok {
			check("defaults.max_tokens", orphanModelsKeyHints["defaults.max_tokens"])
		}
	}
	if entries, ok := raw["entries"].([]any); ok {
		for i, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			name, _ := em["name"].(string)
			if _, ok := em["max_tokens"]; ok {
				slog.Warn("models.yaml: retired per-entry key still present — value silently ignored",
					"path", path, "entry", name, "entry_index", i,
					"key", "max_tokens", "hint", orphanModelsKeyHints["per-entry max_tokens"])
			}
			if _, ok := em["max_streaming_output_tokens"]; ok {
				slog.Warn("models.yaml: retired per-entry key still present — value silently ignored",
					"path", path, "entry", name, "entry_index", i,
					"key", "max_streaming_output_tokens", "hint", orphanModelsKeyHints["per-entry max_streaming_output_tokens"])
			}
			if _, ok := em["max_block_output_tokens"]; ok {
				slog.Warn("models.yaml: retired per-entry key still present — value silently ignored",
					"path", path, "entry", name, "entry_index", i,
					"key", "max_block_output_tokens", "hint", orphanModelsKeyHints["per-entry max_block_output_tokens"])
			}
		}
	}
}

// Validate checks for required fields and name uniqueness.
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
	}
	return nil
}

// ModelsYAMLExample is the annotated template written to
// $BOB_HOME/models.yaml.example by `bob config init`. Edit + `cp` to
// models.yaml to enable MultiPool.
const ModelsYAMLExample = `# $BOB_HOME/models.yaml — the pool of LLM endpoints MultiPool routes to.
#
# When this file is present, the agent uses MultiPool (tag-matched routing
# across N entries). When it is absent, the agent has no model configured
# and replies with a "configure me" message until you create it.
#
# Tag matching: the agent's request carries Requires (must all match the
# chosen entry's tags) and Prefer (ties broken among qualifiers). Tags
# are free-form strings — the conventional ones are "small" / "smart" /
# "fast" / "cheap" / "local" / "hosted", but anything goes.

# ─────────────────────────────────────────────────────────────────────
# 全字段参考（reference only — 整段注释，不是真 entry）。一个 entry 能配的
# 所有字段都在这；真 entry 只写你需要的几行，其余留空走默认。
#
# - name: example-full              # 唯一名；/model 列表 + 日志按它显示
#   provider: openai-compat         # ollama|llama-cpp|vllm|sglang|openai|openrouter|anthropic|rapidocr|custom
#                                   #   preset 给 base_url/api_key_env 默认；custom/rapidocr 无默认
#   model: deepseek-chat            # 后端的 served model id（非-chat provider 也必填，Validate 要求）
#   base_url: https://api.x.com/v1  # 覆盖 provider preset 的 base_url
#   api_key_env: MY_API_KEY         # 存 key 的环境变量名；覆盖 preset
#   kind: llm                       # llm(默认) | asr | translate | ocr —— 只被同 kind 的请求选中
#   priority: 0                     # 越大越优先（Prefer-tag 之后的静态 tie-break）；
#                                   #   范围 -10000..10000；0=中性；<= -10001 = 禁用该 entry
#   tags: [smart, toolcall, vision] # 自由字符串；内置约定见文件末尾（toolcall/vision/...）
#   context_window: 131072          # 该后端真实上下文窗口；server 报 n_ctx 时探测值覆盖
#   concurrency: 8                  # 该 entry 并发上限；0 = 不限（通常 preset 覆盖）
#   max_output_tokens: 8192         # 该 entry 单次输出上限（stream+block）；0/omit → defaults.max_output_tokens
#   extra_body:                     # 原样并进 /chat/completions 请求体，覆盖 bob 默认；后端认才生效
#     chat_template_kwargs: {enable_thinking: false}   # Qwen3 关思考
#     reasoning: {enabled: false}                      # OpenRouter 关 reasoning
#     temperature: 0.7              # 还有 top_p/top_k/min_p/repetition_penalty/seed/stop 等
# ─────────────────────────────────────────────────────────────────────

# Top-level defaults — all global knobs live here.
# Remove the block if you don't need any; every field has a sane fallback.
defaults:
  # min_context_window: RETIRED — the turn sizes to the serving entry's
  #                     declared context_window; this key is ignored.
  max_input_tokens: 24000       # compression-budget cap (compress when the
                                # assembled prompt would exceed 95% of this).
                                # GLOBAL — compression happens before model
                                # pick, so per-entry override doesn't apply.
  max_output_tokens: 8192       # per-call output cap fallback. Used for BOTH
                                # streaming (chat replies) and block calls
                                # (side-LLM: translate / asr / compression /
                                # judge / learning) whenever an entry sets no
                                # per-entry max_output_tokens of its own.
  # fallback — tag-fallback rules. When a request needs all of 'tags' but NO
  # eligible entry has them (none configured, or all paused/cooling/dead), the
  # pool retries requiring 'to' instead (the request's other required tags are
  # kept). Rules chain + are cycle-guarded; first match per hop wins. This is
  # graceful degradation, not failover (failover is automatic among same-tag
  # entries). Omit the whole list if you don't want any.
  # fallback:
  #   - tags: [vision]      # no vision model available right now…
  #     to:   [smart]       # …answer with a smart text model instead
  #   - tags: [smart]       # and if no smart model either…
  #     to:   [small]       # …fall back to a small one

# Per-entry rules:
#   max_output_tokens — per-entry output cap for THIS model, on whichever
#                       path it's invoked. Overrides defaults.max_output_tokens
#                       for this entry only; 0 / omit = use the default. Set it
#                       on small-context sidecars (translate 4096, asr 1024).
#   context_window — actual context window of this backend (probe overrides
#                    when the server reports n_ctx).

entries:
  # The local small model (cheap, used by the compressor for summaries).
  - name: small
    provider: ollama
    model: qwen2.5:7b
    # base_url: http://localhost:11434/v1   # optional — provider preset default
    tags: [small, fast, local]
    context_window: 32768
    concurrency: 2
    # max_output_tokens: 8192   # cap this entry's reply length (stream + block);
                                # omit / 0 = fall through to defaults.max_output_tokens
                                # then the provider's hardcoded default.
                                # Doesn't apply to Anthropic (caps via different
                                # mechanism — see model/providers/30-anthropic.go).
    # extra_body:        # raw fields merged into every request (see notes below)
    #   chat_template_kwargs: {enable_thinking: false}   # Qwen3: turn OFF reasoning

  # A bigger model for harder questions.
  # - name: smart
  #   provider: openai
  #   model: gpt-4o
  #   api_key_env: OPENAI_API_KEY
  #   tags: [smart, hosted]
  #   context_window: 128000
  #   concurrency: 10

  # Anthropic. context_window defaults to 200000 from the provider preset, so
  # you can omit it. ANTHROPIC_API_KEY is the env var by default.
  # - name: claude
  #   provider: anthropic
  #   model: claude-sonnet-4-5
  #   tags: [smart, cloud, toolcall, vision]

# Tag conventions (free-form, but these are the agent's built-in
# requirements):
#   - "toolcall"  routing for tool-using turns (the agent prefers entries
#                 with this tag for any tool-emitting call)
#   - "vision"    browser_vision routes here (multimodal image input
#                 required); without any vision-tagged entry, that tool
#                 returns a "no vision-capable model" error
#
# extra_body — per-entry raw passthrough. Every key under it is merged
# verbatim into the /chat/completions request body, OVERRIDING bob's
# defaults. Use it for backend knobs bob has no dedicated field for; each
# field is honored only if the backend recognizes it. Common keys:
#   chat_template_kwargs: {enable_thinking: false}  # Qwen3 etc. — disable reasoning
#   reasoning: {enabled: false}      # OpenRouter — disable reasoning
#   temperature / top_p / top_k / min_p
#   repetition_penalty / presence_penalty / frequency_penalty
#   seed / stop
# Do NOT put model / messages / stream here — those are bob's to set.
`
