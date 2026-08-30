package config

import (
	"os"
	"path/filepath"
	"testing"

	"agentbob/sidecars/browser/core"
)

// writeFeishuYAML drops a sources/feishu.yaml under a fresh temp $BOB_HOME and
// returns that home dir.
func writeFeishuYAML(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sources: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feishu.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write feishu.yaml: %v", err)
	}
	return home
}

func TestLoadFeishuSources_Missing(t *testing.T) {
	bots, err := LoadFeishuSources(t.TempDir())
	if err != nil {
		t.Fatalf("missing file: want nil err, got %v", err)
	}
	if bots != nil {
		t.Fatalf("missing file: want nil bots, got %v", bots)
	}
}

func TestLoadFeishuSources_FlatShape(t *testing.T) {
	home := writeFeishuYAML(t, `enabled: true
allow_all: false
allowlist: ["ou_abc"]
admins: ["ou_abc"]
`)
	bots, err := LoadFeishuSources(home)
	if err != nil {
		t.Fatalf("LoadFeishuSources: %v", err)
	}
	if len(bots) != 1 {
		t.Fatalf("flat shape: want 1 bot, got %d", len(bots))
	}
	b := bots[0]
	if b.Name != "feishu" {
		t.Errorf("flat shape: want name %q, got %q", "feishu", b.Name)
	}
	// Flat primary leaves env names empty → startup defaults to FEISHU_APP_ID/SECRET.
	if b.AppIDEnv != "" || b.AppSecretEnv != "" {
		t.Errorf("flat shape: want empty env names, got id=%q secret=%q", b.AppIDEnv, b.AppSecretEnv)
	}
	if !b.Admins.Contains("ou_abc") {
		t.Errorf("flat shape: admins not parsed, got %v", b.Admins)
	}
}

func TestLoadFeishuSources_ListShape(t *testing.T) {
	home := writeFeishuYAML(t, `bots:
  - name: feishu
    allow_all: false
    admins: ["ou_abc"]
  - name: feishu2
    app_id_env: FEISHU_APP_ID_2
    app_secret_env: FEISHU_APP_SECRET_2
    allow_all: true
`)
	bots, err := LoadFeishuSources(home)
	if err != nil {
		t.Fatalf("LoadFeishuSources: %v", err)
	}
	if len(bots) != 2 {
		t.Fatalf("list shape: want 2 bots, got %d", len(bots))
	}
	if bots[0].Name != "feishu" {
		t.Errorf("bot[0] name = %q, want feishu", bots[0].Name)
	}
	if bots[1].Name != "feishu2" || bots[1].AppIDEnv != "FEISHU_APP_ID_2" || bots[1].AppSecretEnv != "FEISHU_APP_SECRET_2" {
		t.Errorf("bot[1] = %+v, want feishu2 with both env names", bots[1])
	}
}

func TestLoadFeishuSources_InvalidName(t *testing.T) {
	home := writeFeishuYAML(t, `bots:
  - name: feishu
  - name: lark2
    app_id_env: FEISHU_APP_ID_2
    app_secret_env: FEISHU_APP_SECRET_2
`)
	if _, err := LoadFeishuSources(home); err == nil {
		t.Fatal("invalid name (not feishu-prefixed): want error, got nil")
	}
}

func TestLoadFeishuSources_NonPrimaryMissingEnv(t *testing.T) {
	// A non-primary bot with no app_id_env/app_secret_env would collide on the
	// primary's FEISHU_APP_ID/SECRET — must be a loud error.
	home := writeFeishuYAML(t, `bots:
  - name: feishu
  - name: feishu2
    app_id_env: FEISHU_APP_ID_2
`)
	if _, err := LoadFeishuSources(home); err == nil {
		t.Fatal("non-primary missing app_secret_env: want error, got nil")
	}
}

func TestLoadFeishuSources_DuplicateName(t *testing.T) {
	home := writeFeishuYAML(t, `bots:
  - name: feishu
  - name: feishu
    app_id_env: FEISHU_APP_ID_2
    app_secret_env: FEISHU_APP_SECRET_2
`)
	if _, err := LoadFeishuSources(home); err == nil {
		t.Fatal("duplicate bot name: want error, got nil")
	}
}

// TestLoadAllSourcePolicies_FeishuMultiBot pins the BUG1 fix: a bots: list
// surfaces EVERY bot in the policy map (not just "feishu"), and each carries
// its OWN gate — so /reload-sources doesn't push an empty gate to the primary
// (LoadSource-as-flat would have) or miss feishu2 entirely.
func TestLoadAllSourcePolicies_FeishuMultiBot(t *testing.T) {
	home := writeFeishuYAML(t, `bots:
  - name: feishu
    allow_all: false
    allowlist: ["ou_primary"]
  - name: feishu2
    app_id_env: FEISHU_APP_ID_2
    app_secret_env: FEISHU_APP_SECRET_2
    allow_all: false
    allowlist: ["ou_second"]
`)
	pol, err := LoadAllSourcePolicies(home)
	if err != nil {
		t.Fatalf("LoadAllSourcePolicies: %v", err)
	}
	if _, ok := pol["feishu"]; !ok {
		t.Fatal("policy map missing primary feishu")
	}
	if !pol["feishu"].Allowlist.Contains("ou_primary") {
		t.Errorf("primary gate lost its allowlist: %+v", pol["feishu"])
	}
	fb2, ok := pol["feishu2"]
	if !ok {
		t.Fatal("policy map missing feishu2 (the BUG1 regression)")
	}
	if !fb2.Allowlist.Contains("ou_second") {
		t.Errorf("feishu2 gate wrong: %+v", fb2)
	}
}

// TestWriteFeishuBotList_ListMode pins the BUG2 fix: /source-allow on a
// non-primary bot edits its entry INSIDE feishu.yaml's bots: list (not a
// phantom feishu2.yaml), preserves the other bot + the env-var names, and the
// edit is visible through LoadFeishuSources.
func TestWriteFeishuBotList_ListMode(t *testing.T) {
	home := writeFeishuYAML(t, `bots:
  - name: feishu
    allow_all: false
    allowlist: ["ou_primary"]
  - name: feishu2
    app_id_env: FEISHU_APP_ID_2
    app_secret_env: FEISHU_APP_SECRET_2
    allow_all: false
    allowlist: ["ou_second"]
`)
	added, err := WriteSourceList(home, "feishu2", "ou_new", core.ListAllow, true)
	if err != nil || !added {
		t.Fatalf("WriteSourceList(feishu2 add): added=%v err=%v", added, err)
	}
	// No phantom file.
	if _, statErr := os.Stat(filepath.Join(home, "sources", "feishu2.yaml")); statErr == nil {
		t.Fatal("phantom sources/feishu2.yaml was created (BUG2 not fixed)")
	}
	bots, err := LoadFeishuSources(home)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var f2 *NamedFeishuSource
	for i := range bots {
		if bots[i].Name == "feishu2" {
			f2 = &bots[i]
		}
	}
	if f2 == nil {
		t.Fatal("feishu2 vanished from bots: after edit")
	}
	if !f2.Allowlist.Contains("ou_new") || !f2.Allowlist.Contains("ou_second") {
		t.Errorf("feishu2 allowlist wrong after add: %v", f2.Allowlist)
	}
	if f2.AppIDEnv != "FEISHU_APP_ID_2" || f2.AppSecretEnv != "FEISHU_APP_SECRET_2" {
		t.Errorf("feishu2 lost its env-var names on re-marshal: id=%q secret=%q", f2.AppIDEnv, f2.AppSecretEnv)
	}
	// Primary untouched.
	for i := range bots {
		if bots[i].Name == "feishu" && bots[i].Allowlist.Contains("ou_new") {
			t.Error("editing feishu2 leaked into the primary feishu allowlist")
		}
	}
}

// TestWriteFeishuBotList_FlatPreservesEnvFields covers a FLAT feishu.yaml that
// uses non-default app_id_env/app_secret_env: an allowlist write must succeed
// (not be rejected as an unmodelled key) AND must preserve the two env fields
// on the round-trip — previously the flat path routed through editSourceList,
// which can't model those keys and either refused the write or dropped them.
func TestWriteFeishuBotList_FlatPreservesEnvFields(t *testing.T) {
	home := writeFeishuYAML(t, `app_id_env: FEISHU_APP_ID_CUSTOM
app_secret_env: FEISHU_APP_SECRET_CUSTOM
allow_all: false
allowlist: ["ou_existing"]
`)
	added, err := WriteSourceList(home, "feishu", "ou_new", core.ListAllow, true)
	if err != nil {
		t.Fatalf("WriteSourceList(flat feishu add): err=%v", err)
	}
	if !added {
		t.Fatal("WriteSourceList reported no change; want added=true")
	}
	bots, err := LoadFeishuSources(home)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(bots) != 1 || bots[0].Name != "feishu" {
		t.Fatalf("flat feishu reload = %+v, want one bot named feishu", bots)
	}
	b := bots[0]
	if b.AppIDEnv != "FEISHU_APP_ID_CUSTOM" || b.AppSecretEnv != "FEISHU_APP_SECRET_CUSTOM" {
		t.Errorf("flat feishu lost its env-var names on write: id=%q secret=%q", b.AppIDEnv, b.AppSecretEnv)
	}
	if !b.Allowlist.Contains("ou_new") || !b.Allowlist.Contains("ou_existing") {
		t.Errorf("flat feishu allowlist wrong after add: %v", b.Allowlist)
	}
}
