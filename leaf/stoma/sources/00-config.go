// Package sources assembles the concrete Source set the bus (stoma) fans in.
// It is the ONE place that names platforms — stoma itself stays platform-blind.
// Connection config (which sources are on, and their per-platform connection
// parameters) is the source domain's section of the central config.yaml;
// access policy (who may talk) lives separately and is owned by the gate.
package sources

import (
	"agentbob/config"
	"agentbob/leaf/stoma/sources/email"
)

// Config is the `sources:` section of config.yaml — the connection side of the
// source domain. Each platform gets its own sub-struct; a source absent from the
// file falls back to its coded default. The credential VALUES never live here
// (only the env-var names that hold them).
type Config struct {
	Local LocalConfig `yaml:"local"`
	// Telegram / Feishu / Discord are LISTS — one entry per bot, so several bots
	// run in one process (multi-bot). Each entry's Name is its source name + gate
	// policy file (sources/<name>.yaml); an empty Name defaults to the platform name
	// for the first entry, "<platform>-2", "-3", … for the rest.
	Telegram []TelegramConfig    `yaml:"telegram"`
	Feishu   []FeishuConfig      `yaml:"feishu"`
	Discord  []DiscordConfig     `yaml:"discord"`
	Email    []email.EmailConfig `yaml:"email"` // one entry per mailbox account
}

// FeishuConfig is the Feishu/Lark source's connection config. App credentials
// live in env vars named by AppIDEnv / AppSecretEnv (default FEISHU_APP_ID /
// FEISHU_APP_SECRET); access policy is the gate's.
type FeishuConfig struct {
	Name            string `yaml:"name"` // source name + gate policy file; "" → "feishu"
	Enabled         bool   `yaml:"enabled"`
	AppIDEnv        string `yaml:"app_id_env"`
	AppSecretEnv    string `yaml:"app_secret_env"`
	MaxAttachmentMB int64  `yaml:"max_attachment_mb"`
}

// DiscordConfig is the Discord source's connection config. The bot token is in
// the env var named by TokenEnv (default DISCORD_BOT_TOKEN).
type DiscordConfig struct {
	Name            string `yaml:"name"` // source name + gate policy file; "" → "discord"
	Enabled         bool   `yaml:"enabled"`
	TokenEnv        string `yaml:"token_env"`
	MaxAttachmentMB int64  `yaml:"max_attachment_mb"`
}

// LocalConfig is the local terminal source's connection config. It has no
// credentials — the in-process REPL is reachable whenever bob runs — so Enabled
// is its only knob.
type LocalConfig struct {
	Enabled bool `yaml:"enabled"`
}

// TelegramConfig is the Telegram source's CONNECTION config (access policy —
// who may talk — lives in the gate's sources/telegram.yaml, not here). The bot
// token is held in the env var named by TokenEnv, never in the file.
type TelegramConfig struct {
	// Name is this bot's source name + gate policy file (sources/<name>.yaml); ""
	// → "telegram" for the first entry, "telegram-2"… for the rest.
	Name    string `yaml:"name"`
	Enabled bool   `yaml:"enabled"`
	// TokenEnv is the env var holding the bot token (default TELEGRAM_BOT_TOKEN).
	// A per-bot override lets several bots run in one process.
	TokenEnv string `yaml:"token_env"`
	// MaxAttachmentMB caps a single captured attachment; 0 → store default.
	MaxAttachmentMB int64 `yaml:"max_attachment_mb"`
}

// Defaults is the coded baseline the file overrides. Local defaults ON (the REPL
// is the primary dev surface); Telegram defaults OFF (a daemon deployment opts
// in via config.yaml + sets the token env var).
func Defaults() Config {
	return Config{
		Local: LocalConfig{Enabled: true},
	}
}

// LoadConfig reads the `sources:` section of the config.yaml at path over the
// coded Defaults. A missing file yields the defaults (override-only files); only
// the sources section is consulted — other modules' sections are ignored.
func LoadConfig(path string) (Config, error) {
	type fileShape struct {
		Sources Config `yaml:"sources"`
	}
	fs, err := config.Load[fileShape](path, fileShape{Sources: Defaults()})
	return fs.Sources, err
}
