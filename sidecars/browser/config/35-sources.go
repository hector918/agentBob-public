package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"agentbob/sidecars/browser/core"

	"gopkg.in/yaml.v3"
)

// SourceConfig is one IM platform's own config (audit per-platform admin).
// One file per source under $BOB_HOME/sources/<name>.yaml. Platforms with
// no file get the zero value — disabled by default, no admins, no users
// authorized. The local-terminal source ("local") doesn't have a file —
// the binary's user is always admin and is the only one who can talk.
//
// Per the decision: there is NO global admin in config.yaml
// anymore. Admins are configured per-source so a multi-platform deployment
// (Telegram + WhatsApp + …) can have different admin lists per platform.
type SourceConfig struct {
	Enabled   *bool                  `yaml:"enabled"`             // default depends on source; if file exists, defaults to true
	TokenEnv  string                 `yaml:"token_env,omitempty"` // env var with the bot token; default per-source
	AllowAll  bool                   `yaml:"allow_all"`           // when true, anyone whose chat passes per-chat rules may talk; default false (closed) — every seed writes allow_all:false
	Allowlist IDList                 `yaml:"allowlist,omitempty"` // user ids; used when allow_all=false
	Denylist  IDList                 `yaml:"denylist,omitempty"`  // always wins
	Admins    IDList                 `yaml:"admins,omitempty"`    // user ids that are admin on THIS source; empty = no admins
	Groups    map[string]SourceGroup `yaml:"groups,omitempty"`    // per-group overrides keyed by chat id (negative for tg groups/supergroups); DM-scoping happens via global allow/denylist, not here
	// PR-6: the per-source `tools:` / `skills:` /
	// `credentials:` allowlists are gone — those moved to the system-wide
	// `$BOB_HOME/permissions.yaml` (see `permissions.Policy`). admin
	// edits one file to change "what every non-agora caller is allowed
	// to call"; SourceConfig now carries ONLY "who is allowed to talk
	// to bob" (admins / allowlist / denylist / per-group overrides).
}

// SourceGroup overrides allow/deny/admin per group chat. (Field was
// SourceChat → renamed — "chat" was ambiguous since DM
// scoping isn't this map's job; admins use global allow/denylist for
// per-user toggles and this map exclusively for group rooms.)
//
//   - AllowAll: per-chat OPEN — when true, ANYONE may talk in this chat
//     (bypasses the source-level allowlist), even when the source is
//     globally closed. Default off (zero value), so an absent field
//     changes nothing. Lets you open ONE group to the whole tenant
//     without opening DMs or other groups. Denylist still wins, and the
//     group-noise gate (@-mention / reply-to-bot) still applies.
//   - Allowlist: per-chat AND — when populated, ONLY listed users may
//     talk in this chat (further restricts the source-level allow).
//   - AllowlistAdd: per-chat UNION — listed users are granted access
//     in this chat ON TOP of the source-level allow. Lets you keep
//     a tight global allowlist while letting extra members speak in
//     a specific group, without opening their DMs.
//   - Denylist: per-chat OR — listed users are denied in this chat.
//   - Admins: per-chat UNION with source-level Admins.
type SourceGroup struct {
	AllowAll     bool   `yaml:"allow_all,omitempty"` // open THIS group to everyone; default off
	Allowlist    IDList `yaml:"allowlist,omitempty"`
	AllowlistAdd IDList `yaml:"allowlist_add,omitempty"`
	Denylist     IDList `yaml:"denylist,omitempty"`
	Admins       IDList `yaml:"admins,omitempty"`
}

// EnabledEff applies the closed-default. A nil Enabled — whether from no
// file at all or a file that omits `enabled:` — is treated as false; the
// operator must set `enabled: true` explicitly to turn a source on (matches
// the seeded closed-default yamls). AppendSourceAllowlist writes an explicit
// enabled:true when adding an allowlist entry, so files mutated by that path
// are turned on deliberately, not by this default.
func (s SourceConfig) EnabledEff() bool {
	if s.Enabled == nil {
		return false
	}
	return *s.Enabled
}

// IsAdmin reports whether userID is an admin in chatID on this source.
// Source-level admins apply to every chat; per-chat admins overlay
// (additive). The matching is exact-string — IDList is platform-
// agnostic (numeric Telegram ids stringified, string Slack ids
// pass-through).
func (s SourceConfig) IsAdmin(chatID, userID string) bool {
	if s.Admins.Contains(userID) {
		return true
	}
	if cp, ok := s.Groups[chatID]; ok && cp.Admins.Contains(userID) {
		return true
	}
	return false
}

// Denied reports whether userID is on the source-level OR the chat-level
// denylist. It is the "denylist wins over everything" check a source runs
// BEFORE offering a message to the bare-code redeem hook (docs/accounts.md §6.1:
// denylist > 认码 > allowlist) — a denylisted sender must not be able to bind
// via a code. Authorized() re-checks the same denylist, so running Denied first
// then Authorized is harmless (the second check just passes).
func (s SourceConfig) Denied(chatID, userID string) bool {
	if s.Denylist.Contains(userID) {
		return true
	}
	if cp, ok := s.Groups[chatID]; ok && cp.Denylist.Contains(userID) {
		return true
	}
	return false
}

// Authorized applies the two-tier allow/deny model (was on TelegramConfig;
// generalised here so every source uses the same logic):
//
//	denied if on the global denylist OR the chat's denylist;
//	else allowed if on the chat's allowlist_add OR the chat is allow_all;
//	else, if the chat has an allowlist and the user isn't on it → denied;
//	else allowed iff the source is allow_all OR on the global allowlist.
func (s SourceConfig) Authorized(chatID, userID string) bool {
	if s.Denylist.Contains(userID) {
		return false
	}
	if cp, ok := s.Groups[chatID]; ok {
		if cp.Denylist.Contains(userID) {
			return false
		}
		// Per-chat UNION grant: in THIS chat only, accept the user
		// even if they're absent from the source-level allowlist.
		// Lets admin keep a tight global allow while inviting extra
		// members into one group without opening their DMs. Checked
		// BEFORE the restrictive per-chat allowlist gate below —
		// allowlist_add is an unconditional grant, so a populated
		// per-chat allowlist must not shadow it (a user in allowlist_add
		// only would otherwise be denied at the gate before this grant
		// is ever reached).
		if cp.AllowlistAdd.Contains(userID) {
			return true
		}
		// Per-chat OPEN: this group is open to everyone, bypassing the
		// source-level allowlist (so a globally-closed source can still
		// have one wide-open group). Checked before the restrictive
		// per-chat allowlist gate — allow_all means "anyone", so a
		// stray per-group allowlist must not shadow it. Denylist (above)
		// still wins; the @-mention/reply group-noise gate is separate.
		if cp.AllowAll {
			return true
		}
		if len(cp.Allowlist) > 0 && !cp.Allowlist.Contains(userID) {
			return false
		}
	}
	if s.AllowAll {
		return true
	}
	return s.Allowlist.Contains(userID)
}

// LoadSource reads $BOB_HOME/sources/<name>.yaml. Returns the zero
// SourceConfig (disabled, no admins, no allowlist) when the file is
// absent — that's "this platform isn't configured" without an error.
// A parse error is fatal — operators get a clear "this file is broken"
// signal at startup.
func LoadSource(home, name string) (SourceConfig, error) {
	path := filepath.Join(home, "sources", name+".yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceConfig{}, nil
		}
		return SourceConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c SourceConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return SourceConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	warnDeprecatedSourceFields(path, b)
	return c, nil
}

// warnDeprecatedSourceFields logs a one-shot warning per source yaml
// that still carries the PR-6-retired tools/skills/credentials keys.
// yaml.v3 silently ignores unknown fields, so operators upgrading from
// pre-PR-6 had no signal that these keys are now dead config. D-4
// (review). Top-level keys only — nested usage (e.g.
// telegram chat group overrides) is filtered out by the type assertion.
func warnDeprecatedSourceFields(path string, body []byte) {
	var raw map[string]any
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return
	}
	for _, key := range []string{"tools", "skills", "credentials"} {
		if _, ok := raw[key]; ok {
			slog.Warn("source yaml: deprecated key — moved to $BOB_HOME/permissions.yaml in PR-6; delete this section to silence",
				"path", path, "key", key)
		}
	}
}

// NamedSourceConfig is a SourceConfig that carries its own source name.
// Used by the dual-shape telegram loader: in single-process multi-token
// Telegram, each bot is its own core.Source with a globally-unique name
// (the name flows into session keys, the Hub source map, source health,
// and sourcePolicies). The inlined SourceConfig keeps every existing
// per-source field (admins / allow_all / tools / …) available verbatim.
type NamedSourceConfig struct {
	Name         string `yaml:"name"`
	SourceConfig `yaml:",inline"`
}

// telegramSourceNameRE constrains a telegram bot's source name. It must
// start with "telegram" (so "is this a telegram kind?" can be decided by
// prefix at the gateway) and the remainder is [a-z0-9_-] only — notably
// no ':' since that's the session-key separator. Compiled once.
var telegramSourceNameRE = regexp.MustCompile(`^telegram[a-z0-9_-]*$`)

// telegramSourcesFile is the dual-shape on-disk form of
// sources/telegram.yaml. yaml.v3 fills BOTH the inlined flat
// SourceConfig (old single-bot shape) AND Bots (new multi-bot list);
// LoadTelegramSources picks the mode by whether Bots is populated.
type telegramSourcesFile struct {
	Bots         []NamedSourceConfig `yaml:"bots"`
	SourceConfig `yaml:",inline"`
}

// LoadTelegramSources reads $BOB_HOME/sources/telegram.yaml and returns
// one NamedSourceConfig per telegram bot. The file has two valid shapes:
//
//   - Flat (old, zero-migration): a bare SourceConfig (top-level
//     enabled: / token_env: / admins: …, no bots: key). Returns exactly
//     one bot named "telegram".
//   - List (new): a bots: key holding a YAML list of NamedSourceConfig.
//     Returns that list; top-level flat fields are ignored.
//
// A missing file returns (nil, nil) — "no telegram bots configured",
// mirroring LoadSource's zero-value-on-absent contract. A parse error or
// a validation failure is fatal so operators get a loud startup signal.
func LoadTelegramSources(home string) ([]NamedSourceConfig, error) {
	path := filepath.Join(home, "sources", "telegram.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f telegramSourcesFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	warnDeprecatedSourceFields(path, b)

	var bots []NamedSourceConfig
	if len(f.Bots) > 0 {
		// list mode — flat fields ignored. Warn loudly when the file ALSO
		// carries top-level flat fields: that's a mid-migration footgun
		// (operator left stale top-level keys above the bots: list).
		if telegramFlatShapeUsed(f.SourceConfig) {
			slog.Warn("sources/telegram.yaml: top-level fields ignored — a bots: list is present; move any top-level enabled/token_env/admins/… into a bots entry",
				"path", path)
		}
		bots = f.Bots
	} else {
		// flat mode — the whole file is one bot, named "telegram".
		bots = []NamedSourceConfig{{Name: "telegram", SourceConfig: f.SourceConfig}}
	}

	// Validation: source names land in session keys, map keys and file
	// paths, so they must be well-formed, unique, and (for non-primary
	// bots) carry an explicit token_env to avoid colliding on the
	// default TELEGRAM_BOT_TOKEN.
	seen := make(map[string]bool, len(bots))
	for _, bot := range bots {
		if !telegramSourceNameRE.MatchString(bot.Name) {
			return nil, fmt.Errorf("parse %s: invalid telegram bot name %q (must match %s)", path, bot.Name, telegramSourceNameRE.String())
		}
		if seen[bot.Name] {
			return nil, fmt.Errorf("parse %s: duplicate telegram bot name %q", path, bot.Name)
		}
		seen[bot.Name] = true
		if bot.Name != "telegram" && bot.TokenEnv == "" {
			return nil, fmt.Errorf("parse %s: telegram bot %q must set token_env (empty would collide with the primary bot's TELEGRAM_BOT_TOKEN)", path, bot.Name)
		}
	}
	return bots, nil
}

// telegramFlatShapeUsed reports whether the inlined flat SourceConfig of a
// telegram.yaml carries any set field — covers every SourceConfig field.
// Used to warn when a bots: list and top-level flat fields coexist (in
// list mode the flat fields are silently ignored).
func telegramFlatShapeUsed(s SourceConfig) bool {
	return s.Enabled != nil || s.TokenEnv != "" || s.AllowAll ||
		len(s.Allowlist) > 0 || len(s.Denylist) > 0 || len(s.Admins) > 0 ||
		len(s.Groups) > 0
}

// NamedFeishuSource is one Feishu bot (app) in $BOB_HOME/sources/feishu.yaml.
// Feishu needs TWO credentials per bot (app_id + app_secret), so unlike
// telegram's single TokenEnv these env-var names live on a feishu-specific
// type rather than the shared SourceConfig. The names point at $BOB_HOME/.env
// entries; the credential VALUES never live in the yaml (prompt/info hygiene,
// docs/prompt-module-design.md §0.7).
type NamedFeishuSource struct {
	Name         string `yaml:"name"`
	AppIDEnv     string `yaml:"app_id_env,omitempty"`     // env var with the app id; default FEISHU_APP_ID
	AppSecretEnv string `yaml:"app_secret_env,omitempty"` // env var with the app secret; default FEISHU_APP_SECRET
	SourceConfig `yaml:",inline"`
}

// feishuSourceNameRE constrains a Feishu bot's source name — same rule as
// telegram (prefix-decidable kind + no ':' since it separates session keys).
// The accounts platform-kind canonicalizer (core/81-accounts.go) already maps
// feishu* → "feishu", so the prefix is the established convention.
var feishuSourceNameRE = regexp.MustCompile(`^feishu[a-z0-9_-]*$`)

// feishuSourcesFile is the dual-shape on-disk form of sources/feishu.yaml,
// mirroring telegramSourcesFile: a flat single-bot file (top-level gate +
// optional app_id_env/app_secret_env) OR a bots: list.
type feishuSourcesFile struct {
	Bots         []NamedFeishuSource `yaml:"bots"`
	AppIDEnv     string              `yaml:"app_id_env,omitempty"`
	AppSecretEnv string              `yaml:"app_secret_env,omitempty"`
	SourceConfig `yaml:",inline"`
}

// LoadFeishuSources reads $BOB_HOME/sources/feishu.yaml and returns one
// NamedFeishuSource per Feishu bot. Two valid shapes (mirrors
// LoadTelegramSources):
//
//   - Flat (old, zero-migration): a bare gate (top-level enabled: /
//     allow_all: / admins: …, optional app_id_env: / app_secret_env:). Returns
//     exactly one bot named "feishu".
//   - List (new): a bots: key holding a YAML list of NamedFeishuSource.
//
// A missing file returns (nil, nil) — "no feishu bots configured". A parse or
// validation failure is fatal so operators get a loud startup signal.
func LoadFeishuSources(home string) ([]NamedFeishuSource, error) {
	path := filepath.Join(home, "sources", "feishu.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f feishuSourcesFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	warnDeprecatedSourceFields(path, b)

	var bots []NamedFeishuSource
	if len(f.Bots) > 0 {
		if telegramFlatShapeUsed(f.SourceConfig) || f.AppIDEnv != "" || f.AppSecretEnv != "" {
			slog.Warn("sources/feishu.yaml: top-level fields ignored — a bots: list is present; move any top-level enabled/app_id_env/admins/… into a bots entry",
				"path", path)
		}
		bots = f.Bots
	} else {
		bots = []NamedFeishuSource{{
			Name:         "feishu",
			AppIDEnv:     f.AppIDEnv,
			AppSecretEnv: f.AppSecretEnv,
			SourceConfig: f.SourceConfig,
		}}
	}

	// Source names land in session keys, map keys, and the message_index, so
	// they must be well-formed + unique. A non-primary bot MUST set both env
	// names (empty would collide on the primary's FEISHU_APP_ID/SECRET).
	seen := make(map[string]bool, len(bots))
	for _, bot := range bots {
		if !feishuSourceNameRE.MatchString(bot.Name) {
			return nil, fmt.Errorf("parse %s: invalid feishu bot name %q (must match %s)", path, bot.Name, feishuSourceNameRE.String())
		}
		if seen[bot.Name] {
			return nil, fmt.Errorf("parse %s: duplicate feishu bot name %q", path, bot.Name)
		}
		seen[bot.Name] = true
		if bot.Name != "feishu" && (bot.AppIDEnv == "" || bot.AppSecretEnv == "") {
			return nil, fmt.Errorf("parse %s: feishu bot %q must set both app_id_env and app_secret_env (empty would collide with the primary bot's FEISHU_APP_ID/FEISHU_APP_SECRET)", path, bot.Name)
		}
	}
	return bots, nil
}

// discordSourceNameRE constrains a Discord bot's source name — same rule as
// telegram/feishu (prefix-decidable kind + no ':' since it separates session
// keys). The accounts platform-kind canonicalizer (core/81-accounts.go) maps
// discord* → "discord", so the prefix is the established convention.
var discordSourceNameRE = regexp.MustCompile(`^discord[a-z0-9_-]*$`)

// discordSourcesFile is the dual-shape on-disk form of sources/discord.yaml,
// mirroring telegram: a flat single-bot file (top-level gate + optional
// token_env) OR a bots: list. Discord's single-token model is identical to
// telegram's, so it reuses NamedSourceConfig (Name + inline SourceConfig, whose
// TokenEnv carries the bot-token env-var name) rather than a feishu-style type.
type discordSourcesFile struct {
	Bots         []NamedSourceConfig `yaml:"bots"`
	SourceConfig `yaml:",inline"`
}

// LoadDiscordSources reads $BOB_HOME/sources/discord.yaml and returns one
// NamedSourceConfig per Discord bot. Two valid shapes (mirrors
// LoadTelegramSources):
//
//   - Flat (old, zero-migration): a bare gate (top-level enabled: /
//     allow_all: / admins: …, optional token_env:). Returns exactly one bot
//     named "discord".
//   - List (new): a bots: key holding a YAML list of NamedSourceConfig.
//
// A missing file returns (nil, nil) — "no discord bots configured". A parse or
// validation failure is fatal so operators get a loud startup signal.
func LoadDiscordSources(home string) ([]NamedSourceConfig, error) {
	path := filepath.Join(home, "sources", "discord.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f discordSourcesFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	warnDeprecatedSourceFields(path, b)

	var bots []NamedSourceConfig
	if len(f.Bots) > 0 {
		if telegramFlatShapeUsed(f.SourceConfig) {
			slog.Warn("sources/discord.yaml: top-level fields ignored — a bots: list is present; move any top-level enabled/token_env/admins/… into a bots entry",
				"path", path)
		}
		bots = f.Bots
	} else {
		bots = []NamedSourceConfig{{
			Name:         "discord",
			SourceConfig: f.SourceConfig,
		}}
	}

	// Source names land in session keys, map keys, and the message_index, so
	// they must be well-formed + unique. A non-primary bot MUST set token_env
	// (empty would collide on the primary's DISCORD_BOT_TOKEN).
	seen := make(map[string]bool, len(bots))
	for _, bot := range bots {
		if !discordSourceNameRE.MatchString(bot.Name) {
			return nil, fmt.Errorf("parse %s: invalid discord bot name %q (must match %s)", path, bot.Name, discordSourceNameRE.String())
		}
		if seen[bot.Name] {
			return nil, fmt.Errorf("parse %s: duplicate discord bot name %q", path, bot.Name)
		}
		seen[bot.Name] = true
		if bot.Name != "discord" && bot.TokenEnv == "" {
			return nil, fmt.Errorf("parse %s: discord bot %q must set token_env (empty would collide with the primary bot's DISCORD_BOT_TOKEN)", path, bot.Name)
		}
	}
	return bots, nil
}

// SaveSource writes the SourceConfig atomically to
// $BOB_HOME/sources/<name>.yaml (creating the sources/ directory if
// needed). Used by the legacy-config migration on startup + by future
// `bob source set` subcommands.
func SaveSource(home, name string, c SourceConfig) error {
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, name+".yaml.tmp")
	// 0o600 — source yaml holds admin/user IDs (sensitive); match
	// SeedTelegramYAML / editSourceList rather than world-readable 0o644.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name+".yaml"))
}

// telegramSeedYAML is the closed-default `sources/telegram.yaml`
// auto-written on first boot when TELEGRAM_BOT_TOKEN is set but the
// file is missing. Closed-by-default (allow_all: false, empty
// allowlist) — admin must explicitly add user IDs or set allow_all
// before any message is accepted. Paired with the source's startup
// WARN so the misconfiguration is loud rather than silent.
const telegramSeedYAML = `# $BOB_HOME/sources/telegram.yaml — auto-seeded on first boot.
# Closed-by-default: every message is dropped until you either set
#   allow_all: true   (open to anyone — dev / single-user setups)
# or fill the allowlist with telegram user IDs (use @userinfobot to
# look up your own id). admins: are users allowed to run admin-only
# slash commands.
#
# Per-source tool / skill / credential grants moved out of this file
# in PR-6; edit $BOB_HOME/permissions.yaml (admin / user
# roles) to change what a non-agora caller is allowed to call.
#
# See $BOB_HOME/sources/telegram.yaml.example for the full annotated
# template (auto-written by ` + "`bob config init`" + `).

enabled: true
token_env: TELEGRAM_BOT_TOKEN

allow_all: false
allowlist: []
denylist: []
admins: []
groups: {}
`

// SeedTelegramYAML writes the closed-default sources/telegram.yaml at
// home/sources/telegram.yaml when the file is missing AND the
// telegram token env (envName) is set. Idempotent — never overwrites
// an existing file. Returns the absolute path written, or "" when
// nothing was done (file existed, or no token in env).
func SeedTelegramYAML(home, envName string) (string, error) {
	if envName == "" {
		envName = "TELEGRAM_BOT_TOKEN"
	}
	if os.Getenv(envName) == "" {
		return "", nil // no token → user isn't planning to run Telegram; don't litter
	}
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "telegram.yaml")
	if _, err := os.Stat(path); err == nil {
		return "", nil // already there; respect admin's edits
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	// R4Y-18: 0o600 — the telegram allowlist contains
	// admin / user IDs (sensitive: anyone with read access can craft
	// social-engineering payloads tailored to known admins).
	if err := os.WriteFile(path, []byte(telegramSeedYAML), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// wechatSeedYAML is the closed-default `sources/wechat.yaml` auto-written
// on first gateway boot. enabled: false — seeding is functionally inert
// until the operator opts in (WeChat has no allowlist gate in v1, so the
// enabled flag IS the safety boundary).
const wechatSeedYAML = `# $BOB_HOME/sources/wechat.yaml — WeChat (个人微信) source config.
#
# v1 connects through the "文件传输助手" (filehelper) self-channel: type
# into filehelper on your phone and bob, logged in as this WeChat
# account, sees it and replies there. It does NOT touch your friends or
# groups.
#
# Disabled by default. To enable: set enabled: true and restart bob. On
# first start bob pushes a QR-login link through the admin line — scan it
# with the WeChat app to log in. The login credential is then cached
# under sources/wechat/ so restarts re-login without a new scan until it
# expires.
#
# admins: who may run admin-only commands. The filehelper self-channel is
# the logged-in account talking to itself (chat_id == user_id ==
# "filehelper"); list it here to make your own filehelper messages admin.
# Remove it if you don't want admin commands over filehelper.

enabled: false

admins:
  - filehelper
`

// SeedWechatYAML writes the closed-default sources/wechat.yaml when the
// file is missing. Idempotent — never overwrites an existing file.
// Unlike telegram there is no token env to gate on (WeChat logs in by QR
// scan), so the seed is unconditional; the file is enabled: false so
// seeding changes nothing until the operator opts in. Returns the
// absolute path written, or "" when the file already existed.
func SeedWechatYAML(home string) (string, error) {
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "wechat.yaml")
	if _, err := os.Stat(path); err == nil {
		return "", nil // already there; respect operator's edits
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(wechatSeedYAML), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// feishuSeedYAML is the closed-default `sources/feishu.yaml` auto-written on
// first boot when FEISHU_APP_ID is set but the file is missing. Closed-by-
// default (allow_all: false, empty allowlist) — like telegram, the source's
// onMessageReceive drops every inbound until an admin sets allow_all or fills
// allowlist with Feishu user open_id values. Paired with the source's startup
// state so the misconfiguration is loud rather than silent.
const feishuSeedYAML = `# $BOB_HOME/sources/feishu.yaml — Feishu (Lark) source config.
# Auto-seeded on first boot when FEISHU_APP_ID is set but this file was missing.
# Closed-by-default: every message is dropped until you either set
#   allow_all: true   (open to anyone in your Feishu tenant — single-user / dev)
# or fill allowlist with Feishu user open_id values (ou_...). admins: are users
# allowed to run admin-only slash commands. groups: per-chat overrides keyed by
# the Feishu chat_id (oc_...) — e.g. allow_all (open ONE group to everyone while
# DMs stay closed), allowlist / allowlist_add / denylist / admins.
#
# Credentials live in $BOB_HOME/.env (FEISHU_APP_ID / FEISHU_APP_SECRET), not
# here. Per-source tool / skill / credential grants live in
# $BOB_HOME/permissions.yaml.
#
# Multi-bot (run several Feishu apps in one bob process): replace the flat shape
# below with a bots: list, each entry a uniquely-named bot (name must start with
# "feishu") pointing at its own credentials in .env. The primary may omit the
# env names (defaults FEISHU_APP_ID / FEISHU_APP_SECRET); every other bot MUST
# set both app_id_env and app_secret_env. Example:
#   bots:
#     - name: feishu            # primary → FEISHU_APP_ID / FEISHU_APP_SECRET
#       allow_all: false
#       allowlist: ["ou_..."]
#     - name: feishu-support
#       app_id_env: FEISHU_APP_ID_SUPPORT
#       app_secret_env: FEISHU_APP_SECRET_SUPPORT
#       allow_all: false
#       allowlist: ["ou_..."]

enabled: true

allow_all: false
allowlist: []
denylist: []
admins: []
groups: {}
`

// SeedFeishuYAML writes the closed-default sources/feishu.yaml when the file is
// missing AND FEISHU_APP_ID is set. Idempotent — never overwrites an existing
// file. Returns the absolute path written, or "" when nothing was done (file
// existed, or the app id env is unset). Mirrors SeedTelegramYAML; 0o600 because
// the allowlist holds user ids.
func SeedFeishuYAML(home string) (string, error) {
	if os.Getenv("FEISHU_APP_ID") == "" {
		return "", nil // not planning to run Feishu → don't litter
	}
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "feishu.yaml")
	if _, err := os.Stat(path); err == nil {
		return "", nil // already there; respect admin's edits
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(feishuSeedYAML), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// discordSeedYAML is the closed-default `sources/discord.yaml` auto-written on
// first boot when DISCORD_BOT_TOKEN is set but the file is missing. Closed-by-
// default (allow_all: false, empty allowlist) — like telegram/feishu, the
// source's onMessageCreate drops every inbound until an admin sets allow_all or
// fills allowlist with Discord user ids.
const discordSeedYAML = `# $BOB_HOME/sources/discord.yaml — Discord source config.
# Auto-seeded on first boot when DISCORD_BOT_TOKEN is set but this file was missing.
# Closed-by-default: every message is dropped until you either set
#   allow_all: true   (open to anyone who can reach the bot — single-user / dev)
# or fill allowlist with Discord user ids (the numeric snowflake, e.g.
# "123456789012345678"). admins: are users allowed to run admin-only slash
# commands. groups: per-chat overrides keyed by the Discord channel id — e.g.
# allow_all (open ONE channel to everyone while DMs stay closed), allowlist /
# allowlist_add / denylist / admins.
#
# The bot token lives in $BOB_HOME/.env (DISCORD_BOT_TOKEN), not here. Per-source
# tool / skill / credential grants live in $BOB_HOME/permissions.yaml.
#
# Bob reads message content only for DMs, messages that @-mention it, and replies
# that ping it (no privileged MESSAGE CONTENT intent) — in a server, @ the bot to
# start a conversation, then reply to it to continue.
#
# Multi-bot (run several Discord apps in one bob process): replace the flat shape
# below with a bots: list, each entry a uniquely-named bot (name must start with
# "discord") pointing at its own token in .env. The primary may omit the env name
# (default DISCORD_BOT_TOKEN); every other bot MUST set token_env. Example:
#   bots:
#     - name: discord            # primary → DISCORD_BOT_TOKEN
#       allow_all: false
#       allowlist: ["123456789012345678"]
#     - name: discord-support
#       token_env: DISCORD_BOT_TOKEN_SUPPORT
#       allow_all: false
#       allowlist: ["123456789012345678"]

enabled: true

allow_all: false
allowlist: []
denylist: []
admins: []
groups: {}
`

// SeedDiscordYAML writes the closed-default sources/discord.yaml when the file is
// missing AND DISCORD_BOT_TOKEN is set. Idempotent — never overwrites an existing
// file. Returns the absolute path written, or "" when nothing was done (file
// existed, or the token env is unset). Mirrors SeedFeishuYAML; 0o600 because the
// allowlist holds user ids.
func SeedDiscordYAML(home string) (string, error) {
	if os.Getenv("DISCORD_BOT_TOKEN") == "" {
		return "", nil // not planning to run Discord → don't litter
	}
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "discord.yaml")
	if _, err := os.Stat(path); err == nil {
		return "", nil // already there; respect admin's edits
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(discordSeedYAML), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// sourceNameRe constrains a source name to a safe filename token, so an
// attacker-supplied name can't traverse out of $BOB_HOME/sources/.
var sourceNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// editSourceList loads sources/<source>.yaml, applies mutate, and atomically
// rewrites the file iff mutate reports a change (returns true). It is the one
// safe path for every per-source list edit (allowlist add, admin add/remove),
// carrying the shared guards so each caller can't re-derive (and drift on) them:
//
//   - path-traversal: the source name must match sourceNameRe.
//   - email multi-account: "email"/"email-*" are refused — N accounts share one
//     email.yaml with per-account lists this flat round-trip can't target;
//     writing here would create a phantom file the email package never reads.
//   - unmodelled top-level keys: a file carrying keys the flat SourceConfig
//     round-trip can't represent (most importantly telegram's multi-bot `bots:`
//     list) is refused rather than clobbered.
//
// The rewrite is a yaml round-trip: it preserves every config VALUE but not
// comments / formatting (acceptable for this bob-managed file). The write is
// atomic (tmp + rename) so a crash can't leave a partial/corrupt yaml that
// fails to reload and drops the list (fail-open).
func editSourceList(home, source string, mutate func(c *SourceConfig) bool) (bool, error) {
	if !sourceNameRe.MatchString(source) {
		return false, fmt.Errorf("invalid source name %q", source)
	}
	if source == "email" || strings.HasPrefix(source, "email-") {
		return false, fmt.Errorf("email is multi-account — edit the matching account's list under accounts: in %s by hand; this tool can't target a per-account list", filepath.Join("sources", "email.yaml"))
	}
	dir := filepath.Join(home, "sources")
	path := filepath.Join(dir, source+".yaml")
	if raw, rerr := os.ReadFile(path); rerr == nil {
		var top map[string]yaml.Node
		if yaml.Unmarshal(raw, &top) == nil {
			for k := range top {
				switch k {
				case "enabled", "token_env", "allow_all", "allowlist", "denylist", "admins", "groups":
				default:
					return false, fmt.Errorf("%s.yaml carries a field this tool does not model (%q — e.g. a multi-bot bots: list); edit it by hand to avoid clobbering it", source, k)
				}
			}
		}
	}
	cfg, err := LoadSource(home, source)
	if err != nil {
		return false, err
	}
	if !mutate(&cfg) {
		return false, nil
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := filepath.Join(dir, source+".yaml.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return false, fmt.Errorf("write %s.yaml.tmp: %w", source, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, fmt.Errorf("rename %s.yaml: %w", source, err)
	}
	return true, nil
}

// enableIfUnset makes an absent `enabled:` explicit-true before a round-trip —
// adding an allow/admin entry implies intent to use the source, so we set
// enabled:true rather than leave `enabled: null` (which EnabledEff reads as
// disabled under the closed-default).
func enableIfUnset(c *SourceConfig) {
	if c.Enabled == nil {
		t := true
		c.Enabled = &t
	}
}

// AppendSourceAllowlist adds userID to sources/<source>.yaml's allowlist.
// Returns (true, nil) when newly added; (false, nil) when already present.
// Routes through the same multi-bot-aware dispatch as the admin-list path
// (WriteSourceList → editSourceListMulti), so on a telegram/feishu/discord
// `bots:` deployment it edits the named bot's entry instead of refusing (flat
// file) or writing a phantom <source>.yaml that the live policy never reads.
func AppendSourceAllowlist(home, source, userID string) (bool, error) {
	return WriteSourceList(home, source, userID, core.ListAllow, true)
}

// AppendSourceGroupAllow opens a whole group to everyone by setting that group's
// per-chat allow_all override (Groups[chatID].AllowAll) — the group-level analog
// of AppendSourceAllowlist ("招白群"). Denylist still wins and the
// @-mention/reply group-noise gate still applies; this only lifts the
// who-may-talk allowlist for that one chat, leaving DMs and other groups closed.
// Returns added=false if the group is already open. Empty chatID is an error.
func AppendSourceGroupAllow(home, source, chatID string) (bool, error) {
	if strings.TrimSpace(chatID) == "" {
		return false, fmt.Errorf("empty chat id")
	}
	// Same multi-bot-aware dispatch as the allowlist/admin paths so a bots:
	// deployment opens the group on the named bot's entry instead of writing a
	// phantom flat file (false success while the sender stays rejected).
	return editSourceListMulti(home, source, func(c *SourceConfig) bool {
		if c.Groups == nil {
			c.Groups = map[string]SourceGroup{}
		}
		g := c.Groups[chatID]
		if g.AllowAll {
			return false
		}
		g.AllowAll = true
		c.Groups[chatID] = g
		enableIfUnset(c)
		return true
	})
}

// Admin-list writes (add/remove on sources/<source>.yaml's admins:) go through
// config.WriteSourceList(..., core.ListAdmin, ...) — the unified per-family
// writer the accounts module dispatches to. It shares editSourceList's guards
// (traversal / email / multi-bot) and handles telegram's bots: shape too.

// SourceYAMLExample is the annotated template written to
// $BOB_HOME/sources/telegram.yaml.example by `bob config init`.
const SourceYAMLExample = `# $BOB_HOME/sources/telegram.yaml — Telegram-specific config.
# Per the per-platform-admin design: there is no global
# admin list anymore; each source owns its own admin / allow / deny
# settings. The local-terminal user is always admin regardless.
#
# PR-6: tool / skill / credential grants moved OUT of this
# file. Edit $BOB_HOME/permissions.yaml (admin + user roles) to change
# what a non-agora caller may call. This file covers ONLY "who is
# allowed to talk to bob"; the permissions yaml covers "what they may
# do once they're in".

enabled: true

# Env var holding the bot token. Default is TELEGRAM_BOT_TOKEN.
token_env: TELEGRAM_BOT_TOKEN

# Authorization model (two-tier):
#   1. denylist always wins (drop the message, no reply)
#   2. per-chat allowlist further restricts who in that chat may talk
#   3. allow_all=true OR being on the global allowlist → authorized
allow_all: true
allowlist: []
denylist: []

# Admins on this source. Empty = no Telegram-side admin; admin-only
# slash commands refuse. The local terminal user remains admin always.
# admin role grants live in $BOB_HOME/permissions.yaml (admin role).
admins: []

# Per-group overrides keyed by chat_id (Telegram group/supergroup ids
# are negative). DM scoping is NOT done here — use global allow/denylist
# for per-user toggles. Knobs per group:
#   allow_all:     open THIS group to everyone (bypasses the global allowlist;
#                  default off) — denylist still wins, @-mention still required
#   allowlist:     restrict THIS group to only listed users (AND with global)
#   allowlist_add: grant THIS group to extra users not in global allowlist
#                  (UNION) — keep global tight, invite extras into one group
#                  without opening their DMs
#   denylist:      block THIS group for listed users
#   admins:        group-local admins (additive with source-level admins)
groups: {}
#   "-1001234567890":
#     allow_all: true                # this group open to all, DMs stay closed
#     allowlist_add: ["111111111"]   # this user may talk here but not in DMs
#     admins: ["222222222"]
`

// emailAccountPolicy is the slimmed-down on-disk shape of one entry in
// $BOB_HOME/sources/email.yaml's `accounts:` list — only the fields the
// identity resolver consults (allow_all / allowlist / admins / source_id).
// The full EmailAccountConfig (with imap / smtp / poll_interval / …) lives in
// sources/email and is consumed at startup; this struct exists so the
// config package can build per-account SourceConfig snapshots WITHOUT
// importing sources/email (which would cycle back through config).
//
// Yaml fields MUST stay in sync with sources/email.EmailAccountConfig's
// corresponding tags (source_id / allow_all / allowlist / denylist /
// admins); a drift (e.g. a rename) would silently parse empty
// allow/deny/admin lists.
// There is currently no automated test asserting this — keep it manual.
type emailAccountPolicy struct {
	SourceID  string `yaml:"source_id"`
	AllowAll  bool   `yaml:"allow_all"`
	Allowlist IDList `yaml:"allowlist,omitempty"`
	Denylist  IDList `yaml:"denylist,omitempty"`
	Admins    IDList `yaml:"admins,omitempty"`
}

// emailPolicyFile is the on-disk shape of email.yaml as the config
// package sees it — `enabled:` + `accounts:` only. Full validation
// (imap / smtp / pass_env / required fields) is the email package's
// job at startup. This shape parses only what the identity resolver
// needs (account ids + allow/admin lists).
type emailPolicyFile struct {
	Enabled  bool                 `yaml:"enabled"`
	Accounts []emailAccountPolicy `yaml:"accounts"`
}

// loadEmailPolicies parses $BOB_HOME/sources/email.yaml into a
// per-account SourceConfig map. Returns an empty map when the file is
// absent OR `enabled: false` — matches sources/email.LoadEmailAccounts'
// startup-skip contract. A parse error is fatal so admin gets a loud
// signal at reload time rather than silent policy loss.
//
// Account-level validation (source_id non-empty, "email" prefix, etc)
// is NOT repeated here — startup's LoadEmailAccounts already rejects
// broken files. A subsequent reload that encounters a missing source_id
// silently drops that entry rather than failing every other account.
func loadEmailPolicies(home string) (map[string]SourceConfig, error) {
	path := filepath.Join(home, "sources", "email.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]SourceConfig{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f emailPolicyFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := map[string]SourceConfig{}
	if !f.Enabled {
		return out, nil
	}
	enabledTrue := true
	for _, acc := range f.Accounts {
		id := strings.TrimSpace(acc.SourceID)
		if id == "" {
			continue
		}
		// D14: guard against a duplicate source_id silently
		// last-wins (one account's allowlist/admins clobbering another's).
		// Warn + KEEP the first occurrence (skip the dup) — a louder
		// alternative to the prior silent overwrite, and the safe choice on
		// a hot /reload-sources path (don't fail the whole reload over one
		// typo'd dup).
		if _, dup := out[id]; dup {
			slog.Warn("email policy: duplicate source_id — keeping first, skipping later",
				"source_id", id, "path", path)
			continue
		}
		out[id] = SourceConfig{
			Enabled:   &enabledTrue,
			AllowAll:  acc.AllowAll,
			Allowlist: acc.Allowlist,
			Denylist:  acc.Denylist,
			Admins:    acc.Admins,
		}
	}
	return out, nil
}

// LoadAllSourcePolicies is the single entry point for every source-yaml
// → identity-resolver policy snapshot. Replaces the prior per-call-site
// duplication (one loop in pipeline-startup, another in
// pipeline-gateway/Hub.ReloadSourcePolicies). When a new source type is
// added (slack, discord, …), the loader gains one block here and both
// startup + /reload-sources pick it up automatically — preventing the
// privilege-escalation class where /reload-sources drops a source's
// policy and the identity resolver falls back to wildcard.
//
// Sources WITHOUT a yaml file (local / internal / bridge) are
// intentionally absent — identity resolver treats a missing key as the
// open default, preserving their pre-policy behaviour. The wechat key
// is unconditionally recorded (zero SourceConfig on missing file) to
// match the prior startup contract.
//
// Returns a parse error WITHOUT a partial map — callers should leave
// their live policies untouched on error (atomic-swap invariant).
func LoadAllSourcePolicies(home string) (map[string]SourceConfig, error) {
	out := map[string]SourceConfig{}

	tgBots, err := LoadTelegramSources(home)
	if err != nil {
		return nil, fmt.Errorf("telegram: %w", err)
	}
	for _, tg := range tgBots {
		out[tg.Name] = tg.SourceConfig
	}

	wc, err := LoadSource(home, "wechat")
	if err != nil {
		return nil, fmt.Errorf("wechat: %w", err)
	}
	out["wechat"] = wc

	// feishu is multi-bot (like telegram): one NamedFeishuSource per app, keyed
	// by bot name. A flat feishu.yaml yields one bot "feishu"; a bots: list
	// yields N. Without looping here the live feishu source(s) never pick up
	// allowlist edits on /reload-sources, and a bots: list would push an empty
	// gate (LoadSource reads the flat shape, ignoring bots:) — locking the
	// primary out.
	fsBots, err := LoadFeishuSources(home)
	if err != nil {
		return nil, fmt.Errorf("feishu: %w", err)
	}
	for _, fb := range fsBots {
		out[fb.Name] = fb.SourceConfig
	}

	// discord is multi-bot (like telegram/feishu): one NamedSourceConfig per
	// bot, keyed by bot name. A flat discord.yaml yields one bot "discord"; a
	// bots: list yields N. Looping here keeps the live discord source(s) picking
	// up allowlist edits on /reload-sources without a bots: list pushing an empty
	// gate to the primary.
	dcBots, err := LoadDiscordSources(home)
	if err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}
	for _, db := range dcBots {
		out[db.Name] = db.SourceConfig
	}

	emailPolicies, err := loadEmailPolicies(home)
	if err != nil {
		return nil, fmt.Errorf("email: %w", err)
	}
	for id, cfg := range emailPolicies {
		out[id] = cfg
	}

	return out, nil
}
