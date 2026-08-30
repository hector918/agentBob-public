package sources_test

import (
	"os"
	"path/filepath"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/stoma"
	"agentbob/leaf/stoma/sources"
)

func TestAssemble_LocalToggle(t *testing.T) {
	// The internal (bridge) source is ALWAYS present — in-process ingress, not
	// config-gated. Local toggles independently.
	on := sources.Assemble(sources.Config{Local: sources.LocalConfig{Enabled: true}}, nil)
	if len(on) != 2 || !has(on, "internal") || !has(on, "local") {
		t.Fatalf("local enabled: got %v, want [internal local]", names(on))
	}
	off := sources.Assemble(sources.Config{Local: sources.LocalConfig{Enabled: false}}, nil)
	if len(off) != 1 || !has(off, "internal") {
		t.Fatalf("local disabled: got %v, want [internal]", names(off))
	}
}

// TestAssemble_FeedsBus: the assembled set drives stoma.New cleanly — the
// assembly's output is a valid bus input (unique names, etc.).
func TestAssemble_FeedsBus(t *testing.T) {
	m := stoma.New(sources.Assemble(sources.Defaults(), nil))
	if m.SourceByName("local") == nil {
		t.Fatal("assembled bus is missing the local source")
	}
}

func TestLoadConfig(t *testing.T) {
	// missing file → coded defaults (local ON).
	def, err := sources.LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || !def.Local.Enabled {
		t.Fatalf("missing file: got (%+v, %v), want local enabled / nil", def, err)
	}

	// a file overriding only the sources section; other sections are ignored.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "model:\n  pool_size: 4\nsources:\n  local:\n    enabled: false\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := sources.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Local.Enabled {
		t.Fatalf("local.enabled override not applied: %+v", cfg)
	}
}

// TestMultiBot: telegram/feishu parse as LISTS so several bots run in one process.
func TestMultiBot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "sources:\n" +
		"  telegram:\n" +
		"    - {enabled: true, token_env: TELEGRAM_BOT_TOKEN}\n" +
		"    - {name: telegram-2, enabled: true, token_env: TELEGRAM_BOT_TOKEN_2}\n" +
		"  feishu:\n" +
		"    - {name: feishu2, enabled: true, app_id_env: FEISHU_APP_ID_2, app_secret_env: FEISHU_APP_SECRET_2}\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := sources.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Telegram) != 2 || cfg.Telegram[1].Name != "telegram-2" {
		t.Errorf("telegram = %+v, want 2 bots (2nd named telegram-2)", cfg.Telegram)
	}
	if len(cfg.Feishu) != 1 || cfg.Feishu[0].AppIDEnv != "FEISHU_APP_ID_2" {
		t.Errorf("feishu = %+v, want per-bot app_id_env", cfg.Feishu)
	}
}

func names(srcs []contract.Source) []string {
	out := make([]string, len(srcs))
	for i, s := range srcs {
		out[i] = s.Name()
	}
	return out
}

func has(srcs []contract.Source, name string) bool {
	for _, s := range srcs {
		if s.Name() == name {
			return true
		}
	}
	return false
}
