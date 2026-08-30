package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// MigrateLegacySources moves pre-`gateway.admins` + `gateway.telegram.*`
// configuration into $BOB_HOME/sources/telegram.yaml (the new per-platform
// admin design). Idempotent — once the target file exists, the migrator
// leaves both the legacy fields and the source yaml alone.
//
// Returns (migrated=true) when a write happened so the caller can log a
// notice. cfg is mutated in place: legacy fields are zeroed so the next
// config save drops them from disk.
func MigrateLegacySources(home string, cfg *Config) (bool, error) {
	if cfg == nil {
		return false, nil
	}
	legacyAdmins := cfg.Gateway.Admins
	legacyTG := cfg.Gateway.Telegram
	hasLegacy := legacyAdmins.set || !legacyTG.IsZero()
	if !hasLegacy {
		return false, nil
	}

	target := filepath.Join(home, "sources", "telegram.yaml")
	if _, err := os.Stat(target); err == nil {
		// Already migrated — but the legacy fields are still in
		// config.yaml. Clear them in-memory; the next Save (e.g. via
		// `bob config set ...`) will drop them. Don't auto-save here —
		// "Migration overwrote my hand-edited file" is the worst
		// failure mode.
		slog.Info("legacy gateway.{admins,telegram} fields ignored — sources/telegram.yaml already exists",
			"target", target)
		cfg.Gateway.Admins = AdminList{}
		cfg.Gateway.Telegram = TelegramConfig{}
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat %s: %w", target, err)
	}

	// Translate the legacy shape to the new SourceConfig.
	enabled := legacyTG.Enabled
	sc := SourceConfig{
		Enabled:   &enabled,
		TokenEnv:  legacyTG.TokenEnv,
		AllowAll:  legacyTG.AllowAll,
		Allowlist: append(IDList(nil), legacyTG.Allowlist...),
		Denylist:  append(IDList(nil), legacyTG.Denylist...),
	}
	// Per-chat policy was {allowlist, denylist} only; legacy had no
	// per-chat admins. Carry the lists over.
	if len(legacyTG.Chats) > 0 {
		sc.Groups = map[string]SourceGroup{}
		for chatID, cp := range legacyTG.Chats {
			sc.Groups[chatID] = SourceGroup{
				Allowlist: append(IDList(nil), cp.Allowlist...),
				Denylist:  append(IDList(nil), cp.Denylist...),
			}
		}
	}
	// Source-level admins come from the legacy GLOBAL admins list. "*"
	// (everyone is admin) was the legacy default — but the new design's
	// rule is "no admin unless configured". So map "*" to an empty
	// admin list and log it loud; if the operator actually wanted broad
	// admin, they'll add their id explicitly to sources/telegram.yaml.
	switch {
	case !legacyAdmins.set:
		// nothing — empty Admins
	case legacyAdmins.all:
		slog.Warn("legacy gateway.admins=\"*\" cannot be carried into per-source config (no global-admin concept anymore) — sources/telegram.yaml admins will be empty; add explicit user ids if you want platform admins",
			"target", target)
	default:
		sc.Admins = append(IDList(nil), legacyAdmins.ids...)
	}

	if err := SaveSource(home, "telegram", sc); err != nil {
		return false, fmt.Errorf("write %s: %w", target, err)
	}
	slog.Info("migrated legacy gateway.{admins,telegram.*} to sources/telegram.yaml",
		"target", target,
		"allow_all", sc.AllowAll,
		"allowlist_size", len(sc.Allowlist),
		"denylist_size", len(sc.Denylist),
		"admins_size", len(sc.Admins),
		"groups", len(sc.Groups))

	// Clear the legacy fields in memory so the caller's in-flight cfg view is
	// consistent with what we persist.
	cfg.Gateway.Admins = AdminList{}
	cfg.Gateway.Telegram = TelegramConfig{}
	// Persist the clean config.yaml through the flocked Update path rather than
	// a bare Save: Update is the only writer that takes config.yaml.lock, whose
	// stated purpose is exactly "serialize `bob config set` racing a startup
	// migration". A bare Save here would bypass that lock and could lose-update
	// a concurrent config write. We re-clear the legacy fields inside the
	// callback because Update reloads config.yaml from disk under the lock.
	if err := Update(func(c *Config) error {
		c.Gateway.Admins = AdminList{}
		c.Gateway.Telegram = TelegramConfig{}
		return nil
	}); err != nil {
		return true, fmt.Errorf("save cleaned config.yaml: %w", err)
	}
	return true, nil
}
