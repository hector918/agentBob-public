package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agentbob/sidecars/browser/core"
)

// TestEditSourceList_ConcurrentAppendsNoLostUpdate pins the flock fix: parallel
// allowlist appends targeting the SAME sources file must all survive. Each
// append is a read→mutate→write→rename round-trip; without the exclusive flock
// in editSourceListMulti, two appends that read the same baseline would each
// re-write it and the later rename would silently drop the earlier entry (the
// "已加白 but still rejected" bug). With the lock, every id lands.
func TestEditSourceList_ConcurrentAppendsNoLostUpdate(t *testing.T) {
	home := t.TempDir()
	const n = 24

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// wechat is a flat source → editSourceList round-trips the whole file.
			if _, err := AppendSourceAllowlist(home, "wechat", fmt.Sprintf("u%02d", i)); err != nil {
				t.Errorf("append u%02d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	cfg, err := LoadSource(home, "wechat")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("u%02d", i)
		if !cfg.Allowlist.Contains(id) {
			t.Fatalf("lost update: %s missing from allowlist (have %d/%d): %+v", id, len(cfg.Allowlist), n, cfg.Allowlist)
		}
	}
}

func TestAppendSourceAllowlist(t *testing.T) {
	home := t.TempDir()
	// No file yet — LoadSource returns the zero config, so the first append
	// creates sources/feishu.yaml with the user on the allowlist.
	added, err := AppendSourceAllowlist(home, "feishu", "ou_abc")
	if err != nil || !added {
		t.Fatalf("first add = (%v, %v), want (true, nil)", added, err)
	}
	// Reload and confirm the user is now authorized (allow_all stays false).
	cfg, err := LoadSource(home, "feishu")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !cfg.Allowlist.Contains("ou_abc") {
		t.Fatalf("allowlist missing ou_abc after append: %+v", cfg.Allowlist)
	}
	if cfg.AllowAll {
		t.Fatal("append must not flip allow_all")
	}

	// Idempotent: a second add of the same id reports not-added.
	added2, err := AppendSourceAllowlist(home, "feishu", "ou_abc")
	if err != nil || added2 {
		t.Fatalf("second add = (%v, %v), want (false, nil)", added2, err)
	}
}

// TestAppendSourceAllowlist_MultiBot pins the multi-bot fix: appending to a
// telegram/feishu/discord deployment that uses a bots: list must edit the named
// bot's entry inside the list (NOT refuse, and NOT write a phantom flat file
// that the live policy never reads — the old false-success bug). The other bot
// + its allowlist must be preserved.
func TestAppendSourceAllowlist_MultiBot(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := "bots:\n  - name: telegram\n    enabled: true\n    allowlist: [\"123\"]\n  - name: telegram2\n    token_env: TELEGRAM2_BOT_TOKEN\n    allowlist: [\"456\"]\n"
	path := filepath.Join(dir, "telegram.yaml")
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	// Add 999 to the primary bot "telegram".
	added, err := AppendSourceAllowlist(home, "telegram", "999")
	if err != nil || !added {
		t.Fatalf("multi-bot add = (added=%v, err=%v), want (true, nil)", added, err)
	}
	// The live policy (read via LoadTelegramSources) must now authorize 999 on
	// the "telegram" bot, and telegram2's 456 must be untouched.
	srcs, err := LoadTelegramSources(home)
	if err != nil {
		t.Fatalf("LoadTelegramSources: %v", err)
	}
	var tg, tg2 *SourceConfig
	for i := range srcs {
		switch srcs[i].Name {
		case "telegram":
			tg = &srcs[i].SourceConfig
		case "telegram2":
			tg2 = &srcs[i].SourceConfig
		}
	}
	if tg == nil || !tg.Allowlist.Contains("999") || !tg.Allowlist.Contains("123") {
		t.Fatalf("telegram bot allowlist = %+v, want it to contain 123 and 999", tg)
	}
	if tg2 == nil || !tg2.Allowlist.Contains("456") {
		t.Fatalf("telegram2 allowlist not preserved: %+v", tg2)
	}
	// No phantom flat file should have been written for the bot name.
	if _, statErr := os.Stat(filepath.Join(dir, "telegram2.yaml")); statErr == nil {
		t.Fatal("phantom telegram2.yaml written — should edit the bots: list in place")
	}
}

// TestAppendSourceAllowlist_RefusesEmailMultiAccount pins the footgun fix:
// email is multi-account (N accounts in one email.yaml, each with its own
// source_id + allowlist), so this flat one-file-one-allowlist tool must refuse
// any "email"/"email-*" source rather than silently writing a phantom
// email-<acct>.yaml the email package never reads (which would falsely report
// "added"). Both the bare "email" id and a per-account "email-work" id refuse,
// and no phantom file is created.
func TestAppendSourceAllowlist_RefusesEmailMultiAccount(t *testing.T) {
	for _, src := range []string{"email", "email-work", "email-personal"} {
		home := t.TempDir()
		added, err := AppendSourceAllowlist(home, src, "client@partner.com")
		if err == nil || added {
			t.Fatalf("%s: want refusal, got (added=%v, err=%v)", src, added, err)
		}
		if _, statErr := os.Stat(filepath.Join(home, "sources", src+".yaml")); statErr == nil {
			t.Fatalf("%s: refusal still wrote a phantom %s.yaml", src, src)
		}
	}
}

// TestWriteSourceListAdmin: the accounts module's admin-list management path
// (config.WriteSourceList with core.ListAdmin — sibling of the allowlist path,
// shared editSourceList guards). Add is idempotent + enables an unset source;
// remove drops the id and does not flip enabled; both refuse the email/multi-bot
// footguns via the shared guard.
func TestWriteSourceListAdmin(t *testing.T) {
	home := t.TempDir()

	added, err := WriteSourceList(home, "feishu", "ou_admin", core.ListAdmin, true)
	if err != nil || !added {
		t.Fatalf("first admin add = (%v, %v), want (true, nil)", added, err)
	}
	cfg, _ := LoadSource(home, "feishu")
	if !cfg.Admins.Contains("ou_admin") {
		t.Fatalf("admins missing ou_admin: %+v", cfg.Admins)
	}
	if !cfg.EnabledEff() {
		t.Fatal("admin add must enable an unset source")
	}

	// Idempotent add.
	if added2, _ := WriteSourceList(home, "feishu", "ou_admin", core.ListAdmin, true); added2 {
		t.Fatal("second admin add reported added=true, want false")
	}

	// Remove.
	removed, err := WriteSourceList(home, "feishu", "ou_admin", core.ListAdmin, false)
	if err != nil || !removed {
		t.Fatalf("admin remove = (%v, %v), want (true, nil)", removed, err)
	}
	if cfg, _ := LoadSource(home, "feishu"); cfg.Admins.Contains("ou_admin") {
		t.Fatalf("admins still has ou_admin after remove: %+v", cfg.Admins)
	}
	// Removing an absent id is a no-op (false, nil).
	if removed2, err := WriteSourceList(home, "feishu", "ou_admin", core.ListAdmin, false); err != nil || removed2 {
		t.Fatalf("remove absent = (%v, %v), want (false, nil)", removed2, err)
	}
}

func TestWriteSourceListAdmin_RefusesEmailMultiAccount(t *testing.T) {
	home := t.TempDir()
	if _, err := WriteSourceList(home, "email-work", "x@y.com", core.ListAdmin, true); err == nil {
		t.Fatal("email-work admin add: want refusal, got nil")
	}
}

func TestAppendSourceAllowlist_RejectsTraversal(t *testing.T) {
	home := t.TempDir()
	if _, err := AppendSourceAllowlist(home, "../../etc/evil", "x"); err == nil {
		t.Fatal("path-traversal source name: want error, got nil")
	}
	// The traversal target must not have been created.
	if _, err := os.Stat(filepath.Join(home, "..", "..", "etc", "evil.yaml")); err == nil {
		t.Fatal("traversal wrote a file outside home")
	}
	// The refusal must fire BEFORE lockSourceFamily joins the raw name into
	// a lockfile path — "../escaped" would otherwise drop a 0-byte .lock at
	// <home>/escaped.yaml.lock, outside sources/.
	if _, err := AppendSourceAllowlist(home, "../escaped", "x"); err == nil {
		t.Fatal("path-traversal source name: want error, got nil")
	}
	if _, err := os.Stat(filepath.Join(home, "escaped.yaml.lock")); err == nil {
		t.Fatal("traversal created a lockfile outside sources/")
	}
}

// TestAppendSourceAllowlist_FeishuPreservesEnvKeys pins decision ⑫: the flat
// feishu writer keeps the app_id_env/app_secret_env keys across an /allow
// round-trip (they are NOT silently dropped), while an UNMODELED top-level key
// (a stray deprecated block) is refused LOUDLY and named rather than silently
// dropped.
func TestAppendSourceAllowlist_FeishuPreservesEnvKeys(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "feishu.yaml")
	if err := os.WriteFile(path, []byte("enabled: true\napp_id_env: FEISHU_APP_ID_CUSTOM\napp_secret_env: FEISHU_SECRET_CUSTOM\nallowlist: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := AppendSourceAllowlist(home, "feishu", "ou_abc")
	if err != nil || !added {
		t.Fatalf("append = (%v, %v), want (true, nil)", added, err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FEISHU_APP_ID_CUSTOM", "FEISHU_SECRET_CUSTOM", "ou_abc"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("feishu.yaml after append missing %q:\n%s", want, out)
		}
	}
}

// TestAppendSourceAllowlist_FeishuRejectsUnmodeledKey: a stray top-level key
// must be refused (naming it) instead of silently dropped on the write.
func TestAppendSourceAllowlist_FeishuRejectsUnmodeledKey(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "feishu.yaml")
	if err := os.WriteFile(path, []byte("enabled: true\ntools:\n  - legacy\nallowlist: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := AppendSourceAllowlist(home, "feishu", "ou_abc")
	if err == nil {
		t.Fatal("append must refuse a feishu.yaml carrying an unmodeled top-level key")
	}
	if !strings.Contains(err.Error(), `"tools"`) {
		t.Fatalf("error must name the offending key, got: %v", err)
	}
}
