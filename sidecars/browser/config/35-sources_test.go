package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTelegramYAML drops a sources/telegram.yaml under a fresh temp
// $BOB_HOME and returns that home dir.
func writeTelegramYAML(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sources: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "telegram.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write telegram.yaml: %v", err)
	}
	return home
}

func TestLoadTelegramSources_FlatShape(t *testing.T) {
	home := writeTelegramYAML(t, `enabled: true
token_env: TELEGRAM_BOT_TOKEN
allow_all: false
admins: ["222222222"]
`)
	bots, err := LoadTelegramSources(home)
	if err != nil {
		t.Fatalf("LoadTelegramSources: %v", err)
	}
	if len(bots) != 1 {
		t.Fatalf("flat shape: want 1 bot, got %d", len(bots))
	}
	b := bots[0]
	if b.Name != "telegram" {
		t.Errorf("flat shape: want name %q, got %q", "telegram", b.Name)
	}
	if b.TokenEnv != "TELEGRAM_BOT_TOKEN" {
		t.Errorf("flat shape: want token_env %q, got %q", "TELEGRAM_BOT_TOKEN", b.TokenEnv)
	}
	if !b.Admins.Contains("222222222") {
		t.Errorf("flat shape: admins not parsed, got %v", b.Admins)
	}
	if b.EnabledEff() != true {
		t.Errorf("flat shape: want enabled true")
	}
}

func TestLoadTelegramSources_ListShape(t *testing.T) {
	home := writeTelegramYAML(t, `bots:
  - name: telegram
    token_env: TELEGRAM_BOT_TOKEN
    allow_all: false
    admins: ["222222222"]
  - name: telegram-personal
    token_env: TELEGRAM_BOT_TOKEN_2
    allow_all: false
`)
	bots, err := LoadTelegramSources(home)
	if err != nil {
		t.Fatalf("LoadTelegramSources: %v", err)
	}
	if len(bots) != 2 {
		t.Fatalf("list shape: want 2 bots, got %d", len(bots))
	}
	want := []struct{ name, tokenEnv string }{
		{"telegram", "TELEGRAM_BOT_TOKEN"},
		{"telegram-personal", "TELEGRAM_BOT_TOKEN_2"},
	}
	for i, w := range want {
		if bots[i].Name != w.name {
			t.Errorf("bot[%d]: want name %q, got %q", i, w.name, bots[i].Name)
		}
		if bots[i].TokenEnv != w.tokenEnv {
			t.Errorf("bot[%d]: want token_env %q, got %q", i, w.tokenEnv, bots[i].TokenEnv)
		}
	}
}

func TestLoadTelegramSources_InvalidName(t *testing.T) {
	home := writeTelegramYAML(t, `bots:
  - name: telegram
    token_env: TELEGRAM_BOT_TOKEN
  - name: not-telegram
    token_env: TELEGRAM_BOT_TOKEN_2
`)
	if _, err := LoadTelegramSources(home); err == nil {
		t.Fatal("invalid name: want error, got nil")
	}
}

func TestLoadTelegramSources_InvalidNameColon(t *testing.T) {
	home := writeTelegramYAML(t, `bots:
  - name: "telegram:bad"
    token_env: TELEGRAM_BOT_TOKEN
`)
	if _, err := LoadTelegramSources(home); err == nil {
		t.Fatal("name with colon: want error, got nil")
	}
}

func TestLoadTelegramSources_DuplicateNames(t *testing.T) {
	home := writeTelegramYAML(t, `bots:
  - name: telegram
    token_env: TELEGRAM_BOT_TOKEN
  - name: telegram
    token_env: TELEGRAM_BOT_TOKEN_2
`)
	if _, err := LoadTelegramSources(home); err == nil {
		t.Fatal("duplicate names: want error, got nil")
	}
}

func TestLoadTelegramSources_NonPrimaryEmptyTokenEnv(t *testing.T) {
	home := writeTelegramYAML(t, `bots:
  - name: telegram
    token_env: TELEGRAM_BOT_TOKEN
  - name: telegram-personal
    allow_all: false
`)
	if _, err := LoadTelegramSources(home); err == nil {
		t.Fatal("non-primary empty token_env: want error, got nil")
	}
}

func TestLoadTelegramSources_PrimaryEmptyTokenEnvOK(t *testing.T) {
	// The flat-mode / primary bot named "telegram" may omit token_env —
	// it falls back to the default TELEGRAM_BOT_TOKEN.
	home := writeTelegramYAML(t, `enabled: true
allow_all: true
`)
	bots, err := LoadTelegramSources(home)
	if err != nil {
		t.Fatalf("primary empty token_env: want no error, got %v", err)
	}
	if len(bots) != 1 || bots[0].Name != "telegram" {
		t.Fatalf("primary empty token_env: want 1 bot named telegram, got %v", bots)
	}
}

func TestLoadTelegramSources_MissingFile(t *testing.T) {
	home := t.TempDir() // no sources/telegram.yaml
	bots, err := LoadTelegramSources(home)
	if err != nil {
		t.Fatalf("missing file: want nil error, got %v", err)
	}
	if bots != nil {
		t.Fatalf("missing file: want nil slice, got %v", bots)
	}
}

func TestLoadTelegramSources_ParseError(t *testing.T) {
	home := writeTelegramYAML(t, "bots: [ this is : not valid yaml")
	if _, err := LoadTelegramSources(home); err == nil {
		t.Fatal("malformed yaml: want error, got nil")
	}
}

func TestLoadTelegramSources_MixedShape(t *testing.T) {
	// Both a bots: list AND top-level flat fields — the flat fields are
	// ignored (a startup WARN fires; here we assert the list wins).
	home := writeTelegramYAML(t, `token_env: STALE_TOP_LEVEL
admins: ["999"]
bots:
  - name: telegram
    token_env: TELEGRAM_BOT_TOKEN
  - name: telegram-personal
    token_env: TELEGRAM_BOT_TOKEN_2
`)
	bots, err := LoadTelegramSources(home)
	if err != nil {
		t.Fatalf("mixed shape: %v", err)
	}
	if len(bots) != 2 {
		t.Fatalf("mixed shape: want 2 bots (list wins), got %d", len(bots))
	}
	if bots[0].Name != "telegram" || bots[0].TokenEnv != "TELEGRAM_BOT_TOKEN" {
		t.Errorf("mixed shape: bot[0] = %q/%q, want telegram/TELEGRAM_BOT_TOKEN", bots[0].Name, bots[0].TokenEnv)
	}
}

// TestLoadAllSourcePolicies_AllSourceTypes pins the B1 invariant: a
// single helper feeds both startup and Hub.ReloadSourcePolicies, and it
// MUST cover every source type that has a yaml file. The test seeds
// telegram (multi-bot), wechat, and email under a temp $BOB_HOME and
// asserts every account / bot lands in the returned map with its
// allowlist + admins intact. If a future source type ships and skips
// this loader, the symmetric test pinning per-type presence will fail.
func TestLoadAllSourcePolicies_AllSourceTypes(t *testing.T) {
	home := t.TempDir()
	sourcesDir := filepath.Join(home, "sources")
	if err := os.MkdirAll(sourcesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(sourcesDir, "telegram.yaml"), []byte(`bots:
  - name: telegram
    token_env: TELEGRAM_BOT_TOKEN
    admins: ["tg-admin"]
  - name: telegram-personal
    token_env: TELEGRAM_BOT_TOKEN_2
    admins: ["tg-admin-2"]
`), 0o600); err != nil {
		t.Fatalf("write telegram.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourcesDir, "wechat.yaml"), []byte(`enabled: true
admins: ["wx-admin"]
`), 0o600); err != nil {
		t.Fatalf("write wechat.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourcesDir, "email.yaml"), []byte(`enabled: true
accounts:
  - source_id: email-work
    allow_all: false
    allowlist: ["boss@example.com"]
    admins: ["boss@example.com"]
  - source_id: email-personal
    allow_all: true
    admins: ["me@example.com"]
`), 0o600); err != nil {
		t.Fatalf("write email.yaml: %v", err)
	}

	got, err := LoadAllSourcePolicies(home)
	if err != nil {
		t.Fatalf("LoadAllSourcePolicies: %v", err)
	}

	wantPresent := []string{"telegram", "telegram-personal", "wechat", "email-work", "email-personal"}
	for _, key := range wantPresent {
		if _, ok := got[key]; !ok {
			t.Errorf("LoadAllSourcePolicies: missing key %q (got keys: %v)", key, mapKeys(got))
		}
	}

	if !got["telegram"].Admins.Contains("tg-admin") {
		t.Errorf("telegram admins: %v, want contains tg-admin", got["telegram"].Admins)
	}
	if !got["wechat"].Admins.Contains("wx-admin") {
		t.Errorf("wechat admins: %v, want contains wx-admin", got["wechat"].Admins)
	}
	if !got["email-work"].Admins.Contains("boss@example.com") {
		t.Errorf("email-work admins: %v, want contains boss@example.com", got["email-work"].Admins)
	}
	if got["email-work"].AllowAll {
		t.Errorf("email-work allow_all: got true, want false")
	}
	if !got["email-personal"].AllowAll {
		t.Errorf("email-personal allow_all: got false, want true")
	}
	if !got["email-work"].EnabledEff() {
		t.Errorf("email-work enabled: got false, want true (helper synthesises Enabled=true)")
	}
}

// TestLoadAllSourcePolicies_EmailDisabledOrAbsent verifies email
// policies don't leak through when the file disables itself OR is
// absent — matches the closed-default startup contract.
func TestLoadAllSourcePolicies_EmailDisabledOrAbsent(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		home := t.TempDir()
		got, err := LoadAllSourcePolicies(home)
		if err != nil {
			t.Fatalf("LoadAllSourcePolicies: %v", err)
		}
		for k := range got {
			if strings.HasPrefix(k, "email") {
				t.Errorf("email key %q leaked when file absent", k)
			}
		}
	})
	t.Run("disabled", func(t *testing.T) {
		home := t.TempDir()
		sourcesDir := filepath.Join(home, "sources")
		if err := os.MkdirAll(sourcesDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sourcesDir, "email.yaml"), []byte(`enabled: false
accounts:
  - source_id: email-work
`), 0o600); err != nil {
			t.Fatalf("write email.yaml: %v", err)
		}
		got, err := LoadAllSourcePolicies(home)
		if err != nil {
			t.Fatalf("LoadAllSourcePolicies: %v", err)
		}
		if _, ok := got["email-work"]; ok {
			t.Errorf("email-work present when file enabled=false (closed-default contract broken)")
		}
	})
}

// TestLoadAllSourcePolicies_RegressionEmailDropOnReload is the B1
// regression: prior to the shared loader, /reload-sources rebuilt the
// policy map from telegram + wechat only and silently dropped every
// email account. Without an email entry in sourcePolicies the source-
// side authz path fell back to the open default → email user could
// bypass the per-account allow/denylist. Pinning email's presence in
// LoadAllSourcePolicies prevents the regression.
// (PR-6: tool / skill / credential grants moved to
// $BOB_HOME/permissions.yaml — irrelevant to this test, which only
// covers admins / allowlist carry-through.)
func TestLoadAllSourcePolicies_RegressionEmailDropOnReload(t *testing.T) {
	home := t.TempDir()
	sourcesDir := filepath.Join(home, "sources")
	if err := os.MkdirAll(sourcesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourcesDir, "email.yaml"), []byte(`enabled: true
accounts:
  - source_id: email-locked
    allow_all: false
    allowlist: ["only-me@example.com"]
`), 0o600); err != nil {
		t.Fatalf("write email.yaml: %v", err)
	}
	got, err := LoadAllSourcePolicies(home)
	if err != nil {
		t.Fatalf("LoadAllSourcePolicies: %v", err)
	}
	got2, ok := got["email-locked"]
	if !ok {
		t.Fatal("email-locked missing after LoadAllSourcePolicies — B1 regression")
	}
	if got2.AllowAll {
		t.Error("email-locked allow_all: got true, want false (policy carry-through broken)")
	}
	if !got2.Allowlist.Contains("only-me@example.com") {
		t.Errorf("email-locked allowlist: %v, want contains only-me@example.com", got2.Allowlist)
	}
}

// TestLoadEmailPolicies_DuplicateSourceIDFirstWins pins D14
//: a duplicate source_id no longer silently last-wins. The
// first occurrence is kept and the dup skipped (with a WARN), so a
// typo'd dup can't quietly widen one account's allowlist/admins.
func TestLoadEmailPolicies_DuplicateSourceIDFirstWins(t *testing.T) {
	home := t.TempDir()
	sourcesDir := filepath.Join(home, "sources")
	if err := os.MkdirAll(sourcesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourcesDir, "email.yaml"), []byte(`enabled: true
accounts:
  - source_id: email-work
    allow_all: false
    allowlist: ["first@example.com"]
  - source_id: email-work
    allow_all: true
    allowlist: ["second@example.com"]
`), 0o600); err != nil {
		t.Fatalf("write email.yaml: %v", err)
	}
	got, err := loadEmailPolicies(home)
	if err != nil {
		t.Fatalf("loadEmailPolicies: %v", err)
	}
	cfg, ok := got["email-work"]
	if !ok {
		t.Fatal("email-work missing")
	}
	if cfg.AllowAll {
		t.Error("dup last-wins: allow_all flipped to the SECOND account's true — first must win")
	}
	if !cfg.Allowlist.Contains("first@example.com") || cfg.Allowlist.Contains("second@example.com") {
		t.Errorf("dup last-wins: allowlist=%v, want only first@example.com", cfg.Allowlist)
	}
}

// TestLoadEmailPolicies_ParsesDenylist guards A3: the denylist is the
// highest-priority safety table ("wins over everything") and MUST flow
// through to the SourceConfig snapshot so /reload-sources can hot-reload a
// "block this spammer now" edit. Before the fix emailAccountPolicy didn't
// model denylist at all, so cfg.Denylist was always empty.
func TestLoadEmailPolicies_ParsesDenylist(t *testing.T) {
	home := t.TempDir()
	sourcesDir := filepath.Join(home, "sources")
	if err := os.MkdirAll(sourcesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourcesDir, "email.yaml"), []byte(`enabled: true
accounts:
  - source_id: email-work
    allow_all: true
    denylist: ["spammer@evil.com"]
`), 0o600); err != nil {
		t.Fatalf("write email.yaml: %v", err)
	}
	got, err := loadEmailPolicies(home)
	if err != nil {
		t.Fatalf("loadEmailPolicies: %v", err)
	}
	cfg, ok := got["email-work"]
	if !ok {
		t.Fatal("email-work missing")
	}
	if !cfg.Denylist.Contains("spammer@evil.com") {
		t.Errorf("denylist not parsed into SourceConfig: got %v", cfg.Denylist)
	}
}

func mapKeys(m map[string]SourceConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestLoadTelegramSources_EmptyBotsList(t *testing.T) {
	// bots: [] is not "len > 0" → flat mode → one bot named telegram,
	// carrying the (empty) flat config.
	home := writeTelegramYAML(t, "bots: []\n")
	bots, err := LoadTelegramSources(home)
	if err != nil {
		t.Fatalf("empty bots list: %v", err)
	}
	if len(bots) != 1 || bots[0].Name != "telegram" {
		t.Fatalf("empty bots list: want 1 bot named telegram, got %v", bots)
	}
	if bots[0].EnabledEff() {
		t.Errorf("empty bots list: bot should be disabled (no enabled field)")
	}
}

// TestAuthorized_AllowlistAddBypassesPerChatAllowlist pins the
// ordering invariant: allowlist_add is an unconditional per-chat grant
// that must NOT be shadowed by a populated per-chat allowlist. With
// allowlist:[A] + allowlist_add:[B], B must be authorized even though B
// is absent from the restrictive allowlist.
func TestAuthorized_AllowlistAddBypassesPerChatAllowlist(t *testing.T) {
	// AllowAll:true so the per-chat `allowlist` acts as its intended
	// AND-filter (only listed users in this chat) rather than the whole
	// chat being closed. A is on the restrictive list; B is granted via
	// allowlist_add (the union grant the ordering bug used to shadow); C
	// is on neither and must be filtered out.
	cfg := SourceConfig{
		AllowAll: true,
		Groups: map[string]SourceGroup{
			"-100": {
				Allowlist:    IDList{"A"},
				AllowlistAdd: IDList{"B"},
			},
		},
	}
	if !cfg.Authorized("-100", "A") {
		t.Errorf("A (on allowlist) should be authorized")
	}
	if !cfg.Authorized("-100", "B") {
		t.Errorf("B (on allowlist_add) should be authorized despite a populated per-chat allowlist")
	}
	if cfg.Authorized("-100", "C") {
		t.Errorf("C (on neither list) should NOT be authorized")
	}
}

func TestAuthorized_PerGroupAllowAll(t *testing.T) {
	// Source is globally CLOSED (allow_all:false, empty allowlist) — nobody
	// may DM or talk in an unconfigured group. One group is opened wide via
	// per-group allow_all: anyone may talk THERE, while denylist still wins
	// and other chats stay closed.
	cfg := SourceConfig{
		AllowAll:  false,
		Allowlist: IDList{"alice"}, // only alice is globally allowed
		Groups: map[string]SourceGroup{
			"open":   {AllowAll: true, Denylist: IDList{"banned"}},
			"closed": {}, // no override → follows the closed global
		},
	}
	// Open group: a random non-allowlisted user gets in.
	if !cfg.Authorized("open", "random") {
		t.Errorf("random user should be authorized in an allow_all group")
	}
	// Denylist still wins inside an allow_all group.
	if cfg.Authorized("open", "banned") {
		t.Errorf("denylisted user must stay blocked even in an allow_all group")
	}
	// A group without the override stays closed to non-allowlisted users.
	if cfg.Authorized("closed", "random") {
		t.Errorf("random user must NOT be authorized in a non-allow_all group when source is closed")
	}
	// Global allowlist still works elsewhere (DMs / other chats).
	if !cfg.Authorized("dm-chat", "alice") {
		t.Errorf("globally-allowlisted alice should be authorized outside the open group")
	}
	// allow_all overrides a stray restrictive per-group allowlist (anyone means anyone).
	cfg.Groups["open"] = SourceGroup{AllowAll: true, Allowlist: IDList{"alice"}}
	if !cfg.Authorized("open", "random") {
		t.Errorf("allow_all must override a populated per-group allowlist")
	}
}
