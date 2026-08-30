package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestLegacyEnabledFlagsIgnored verifies yaml.v3 silently drops the
// removed `tools.{execution,web,browser}.enabled` fields when admin's
// existing config.yaml still carries them after the audience-
// gate refactor. yaml.v3 by default ignores unknown fields — this test
// pins that contract so a future "strict mode" upgrade can't break
// existing admin yamls without surfacing.
func TestLegacyEnabledFlagsIgnored(t *testing.T) {
	body := []byte(`execution:
  enabled: true
  max_seconds: 60
  allow_native_in_gateway: true
web:
  enabled: false
  backend: searxng
  max_results: 5
browser:
  enabled: true
  headless: false
  page_timeout_seconds: 45
`)
	var tc ToolsConfig
	if err := yaml.Unmarshal(body, &tc); err != nil {
		t.Fatalf("yaml unmarshal should NOT fail on legacy enabled keys: %v", err)
	}
	// Non-enabled fields still flow through — the legacy keys are silently dropped.
	if got := tc.Execution.MaxSeconds; got != 60 {
		t.Fatalf("execution.max_seconds: want 60 got %d", got)
	}
	if !tc.Execution.AllowNativeInGateway {
		t.Fatal("execution.allow_native_in_gateway: want true")
	}
	if got := tc.Web.Backend; got != "searxng" {
		t.Fatalf("web.backend: want searxng got %q", got)
	}
	if got := tc.Web.MaxResults; got != 5 {
		t.Fatalf("web.max_results: want 5 got %d", got)
	}
	if got := tc.Browser.HeadlessEff(); got {
		t.Fatal("browser.headless: want false")
	}
	if got := tc.Browser.PageTimeoutSeconds; got != 45 {
		t.Fatalf("browser.page_timeout_seconds: want 45 got %d", got)
	}
}

// TestAttachmentsRetentionZeroDisables pins the contract documented in
// ConfigYAMLExample ("0 disables time-based prune") and the AttachmentsConfig
// struct doc ("<=0 disables time-based prune"): an explicit top-level
// retention_days=0 must survive For() as 0 so the cleanup sweeper's `> 0`
// disable check actually disables pruning. Previously For() rewrote a
// top-level 0 to 14, silently turning "keep forever" into 14-day deletion.
func TestAttachmentsRetentionZeroDisables(t *testing.T) {
	// (a) Explicit top-level retention_days=0 survives For() as 0 (prune disabled).
	explicitZero := AttachmentsConfig{RetentionDays: 0}
	if got := explicitZero.For("telegram").RetentionDays; got != 0 {
		t.Fatalf("explicit top-level retention_days=0: want 0 (disabled) got %d", got)
	}

	// (b) An unconfigured install gets 14 via Default() (prune enabled).
	if got := Default().Attachments.For("telegram").RetentionDays; got != 14 {
		t.Fatalf("Default() retention_days: want 14 got %d", got)
	}

	// (c) Per-platform override of 0 still disables, even when the top-level
	//     default is a positive value.
	zero := 0
	withOverride := AttachmentsConfig{
		RetentionDays: 14,
		Platforms: map[string]PlatformAttachments{
			"telegram": {RetentionDays: &zero},
		},
	}
	if got := withOverride.For("telegram").RetentionDays; got != 0 {
		t.Fatalf("per-platform retention_days=0 override: want 0 (disabled) got %d", got)
	}
	// Sanity: a platform without an override still inherits the positive default.
	if got := withOverride.For("whatsapp").RetentionDays; got != 14 {
		t.Fatalf("inherited top-level retention_days: want 14 got %d", got)
	}
}

// TestIndexVisibleCapEff pins the *int unset/explicit-zero distinction:
// the struct comment and ConfigYAMLExample promise "0 = no cap", which only
// works if a yaml `index_visible_cap: 0` reaches Eff() as an explicit 0
// (returned as 0 = uncapped downstream) rather than collapsing into the
// unset default of 50.
func TestIndexVisibleCapEff(t *testing.T) {
	var unset SkillsConfig
	if got := unset.IndexVisibleCapEff(); got != 50 {
		t.Fatalf("unset: want default 50 got %d", got)
	}
	var explicitZero SkillsConfig
	if err := yaml.Unmarshal([]byte("index_visible_cap: 0"), &explicitZero); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if got := explicitZero.IndexVisibleCapEff(); got != 0 {
		t.Fatalf("explicit 0: want 0 (no cap) got %d", got)
	}
	neg := -3
	if got := (SkillsConfig{IndexVisibleCap: &neg}).IndexVisibleCapEff(); got != 0 {
		t.Fatalf("negative: want 0 (no cap) got %d", got)
	}
	pos := 7
	if got := (SkillsConfig{IndexVisibleCap: &pos}).IndexVisibleCapEff(); got != 7 {
		t.Fatalf("positive: want 7 got %d", got)
	}
}

// TestBrowserHumanizeEff pins the *bool default-true contract (the email
// MarkSeen pattern): nil = on, explicit false = off, and a yaml
// `humanize: false` actually lands as off.
func TestBrowserHumanizeEff(t *testing.T) {
	var unset BrowserConfig
	if !unset.HumanizeEff() {
		t.Fatal("unset humanize: want default true")
	}
	f := false
	if (BrowserConfig{Humanize: &f}).HumanizeEff() {
		t.Fatal("explicit false: want false")
	}
	tr := true
	if !(BrowserConfig{Humanize: &tr}).HumanizeEff() {
		t.Fatal("explicit true: want true")
	}
	var fromYAML BrowserConfig
	if err := yaml.Unmarshal([]byte("humanize: false"), &fromYAML); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if fromYAML.HumanizeEff() {
		t.Fatal("yaml humanize:false: want false")
	}
}
