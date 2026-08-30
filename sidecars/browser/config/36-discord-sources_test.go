package config

import (
	"os"
	"path/filepath"
	"testing"

	"agentbob/sidecars/browser/core"
)

// writeDiscordYAML drops a sources/discord.yaml under a fresh temp $BOB_HOME and
// returns that home dir.
func writeDiscordYAML(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sources: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "discord.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write discord.yaml: %v", err)
	}
	return home
}

func TestLoadDiscordSources_Missing(t *testing.T) {
	bots, err := LoadDiscordSources(t.TempDir())
	if err != nil {
		t.Fatalf("missing file: want nil err, got %v", err)
	}
	if bots != nil {
		t.Fatalf("missing file: want nil bots, got %v", bots)
	}
}

func TestLoadDiscordSources_FlatShape(t *testing.T) {
	home := writeDiscordYAML(t, `enabled: true
allow_all: false
allowlist: ["123"]
admins: ["123"]
`)
	bots, err := LoadDiscordSources(home)
	if err != nil {
		t.Fatalf("LoadDiscordSources: %v", err)
	}
	if len(bots) != 1 {
		t.Fatalf("flat shape: want 1 bot, got %d", len(bots))
	}
	b := bots[0]
	if b.Name != "discord" {
		t.Errorf("flat shape: want name %q, got %q", "discord", b.Name)
	}
	// Flat primary leaves token_env empty → startup defaults to DISCORD_BOT_TOKEN.
	if b.TokenEnv != "" {
		t.Errorf("flat shape: want empty token_env, got %q", b.TokenEnv)
	}
	if !b.Admins.Contains("123") {
		t.Errorf("flat shape: admins not parsed, got %v", b.Admins)
	}
}

func TestLoadDiscordSources_ListShape(t *testing.T) {
	home := writeDiscordYAML(t, `bots:
  - name: discord
    allow_all: false
    admins: ["123"]
  - name: discord2
    token_env: DISCORD_BOT_TOKEN_2
    allow_all: true
`)
	bots, err := LoadDiscordSources(home)
	if err != nil {
		t.Fatalf("LoadDiscordSources: %v", err)
	}
	if len(bots) != 2 {
		t.Fatalf("list shape: want 2 bots, got %d", len(bots))
	}
	if bots[0].Name != "discord" {
		t.Errorf("bot[0] name = %q, want discord", bots[0].Name)
	}
	if bots[1].Name != "discord2" || bots[1].TokenEnv != "DISCORD_BOT_TOKEN_2" {
		t.Errorf("bot[1] = %+v, want discord2 with token_env", bots[1])
	}
}

func TestLoadDiscordSources_InvalidName(t *testing.T) {
	home := writeDiscordYAML(t, `bots:
  - name: discord
  - name: slack2
    token_env: DISCORD_BOT_TOKEN_2
`)
	if _, err := LoadDiscordSources(home); err == nil {
		t.Fatal("invalid name (not discord-prefixed): want error, got nil")
	}
}

func TestLoadDiscordSources_NonPrimaryMissingEnv(t *testing.T) {
	// A non-primary bot with no token_env would collide on the primary's
	// DISCORD_BOT_TOKEN — must be a loud error.
	home := writeDiscordYAML(t, `bots:
  - name: discord
  - name: discord2
`)
	if _, err := LoadDiscordSources(home); err == nil {
		t.Fatal("non-primary missing token_env: want error, got nil")
	}
}

func TestLoadDiscordSources_DuplicateName(t *testing.T) {
	home := writeDiscordYAML(t, `bots:
  - name: discord
  - name: discord
    token_env: DISCORD_BOT_TOKEN_2
`)
	if _, err := LoadDiscordSources(home); err == nil {
		t.Fatal("duplicate bot name: want error, got nil")
	}
}

// TestLoadAllSourcePolicies_DiscordMultiBot pins that a bots: list surfaces
// EVERY bot in the policy map (not just "discord"), each carrying its own gate —
// so /reload-sources doesn't push an empty gate to the primary or miss discord2.
func TestLoadAllSourcePolicies_DiscordMultiBot(t *testing.T) {
	home := writeDiscordYAML(t, `bots:
  - name: discord
    allow_all: false
    allowlist: ["prim"]
  - name: discord2
    token_env: DISCORD_BOT_TOKEN_2
    allow_all: false
    allowlist: ["second"]
`)
	pol, err := LoadAllSourcePolicies(home)
	if err != nil {
		t.Fatalf("LoadAllSourcePolicies: %v", err)
	}
	if _, ok := pol["discord"]; !ok {
		t.Fatal("policy map missing primary discord")
	}
	if !pol["discord"].Allowlist.Contains("prim") {
		t.Errorf("primary gate lost its allowlist: %+v", pol["discord"])
	}
	d2, ok := pol["discord2"]
	if !ok {
		t.Fatal("policy map missing discord2")
	}
	if !d2.Allowlist.Contains("second") {
		t.Errorf("discord2 gate wrong: %+v", d2)
	}
}

// TestWriteDiscordBotList_ListMode pins that /source-allow on a non-primary bot
// edits its entry INSIDE discord.yaml's bots: list (not a phantom discord2.yaml),
// preserves the other bot + token_env, and the edit is visible through
// LoadDiscordSources.
func TestWriteDiscordBotList_ListMode(t *testing.T) {
	home := writeDiscordYAML(t, `bots:
  - name: discord
    allow_all: false
    allowlist: ["prim"]
  - name: discord2
    token_env: DISCORD_BOT_TOKEN_2
    allow_all: false
    allowlist: ["second"]
`)
	added, err := WriteSourceList(home, "discord2", "newid", core.ListAllow, true)
	if err != nil || !added {
		t.Fatalf("WriteSourceList(discord2 add): added=%v err=%v", added, err)
	}
	// No phantom file.
	if _, statErr := os.Stat(filepath.Join(home, "sources", "discord2.yaml")); statErr == nil {
		t.Fatal("phantom sources/discord2.yaml was created")
	}
	bots, err := LoadDiscordSources(home)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var d2 *NamedSourceConfig
	for i := range bots {
		if bots[i].Name == "discord2" {
			d2 = &bots[i]
		}
	}
	if d2 == nil {
		t.Fatal("discord2 vanished from bots: after edit")
	}
	if !d2.Allowlist.Contains("newid") || !d2.Allowlist.Contains("second") {
		t.Errorf("discord2 allowlist wrong after add: %v", d2.Allowlist)
	}
	if d2.TokenEnv != "DISCORD_BOT_TOKEN_2" {
		t.Errorf("discord2 lost its token_env on re-marshal: %q", d2.TokenEnv)
	}
	// Primary untouched.
	for i := range bots {
		if bots[i].Name == "discord" && bots[i].Allowlist.Contains("newid") {
			t.Error("editing discord2 leaked into the primary discord allowlist")
		}
	}
}
