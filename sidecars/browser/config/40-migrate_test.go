package config

import (
	"os"
	"path/filepath"
	"testing"

	"agentbob/sidecars/browser/bobhome"
)

// TestMigrateLegacySources_GoesThroughFlockedUpdate pins the fix that the
// migrator persists the cleaned config.yaml through the flocked Update path
// instead of a bare Save. Update is the only writer that takes
// config.yaml.lock — whose documented purpose is to serialize `bob config set`
// racing a startup migration. A bare Save would never create that lockfile,
// so its presence after migration proves the locked path ran.
func TestMigrateLegacySources_GoesThroughFlockedUpdate(t *testing.T) {
	home := t.TempDir()
	bobhome.SetHome(home)
	t.Cleanup(func() { bobhome.SetHome("") })

	// Seed a config.yaml on disk so Update's Load (under the lock) has
	// something to reload and clean.
	cfg := Default()
	cfg.Gateway.Admins = NewAdminListIDs([]string{"222222222"})
	cfg.Gateway.Telegram = TelegramConfig{Enabled: true, TokenEnv: "TELEGRAM_BOT_TOKEN"}
	if err := Save(cfg); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	migrated, err := MigrateLegacySources(home, cfg)
	if err != nil {
		t.Fatalf("MigrateLegacySources: %v", err)
	}
	if !migrated {
		t.Fatal("expected migrated=true (legacy fields present)")
	}

	// (1) The flock sentinel must exist — only Update creates it. A bare
	// Save() never touches config.yaml.lock.
	lockPath := filepath.Join(home, "config.yaml.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("config.yaml.lock not created — migration bypassed the flocked Update path: %v", err)
	}

	// (2) sources/telegram.yaml must have been written.
	if _, err := os.Stat(filepath.Join(home, "sources", "telegram.yaml")); err != nil {
		t.Fatalf("sources/telegram.yaml not written: %v", err)
	}

	// (3) The persisted config.yaml must have the legacy fields cleared.
	reloaded, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Gateway.Admins.IDs() != nil && len(reloaded.Gateway.Admins.IDs()) != 0 {
		t.Errorf("legacy admins not cleared on disk: %v", reloaded.Gateway.Admins.IDs())
	}
	if !reloaded.Gateway.Telegram.IsZero() {
		t.Errorf("legacy telegram not cleared on disk: %+v", reloaded.Gateway.Telegram)
	}
}
