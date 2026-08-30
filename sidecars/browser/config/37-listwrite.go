package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentbob/sidecars/browser/core"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// WriteSourceList adds/removes uid on a source's allowlist|admins, handling BOTH
// flat single-source yaml AND telegram's multi-bot `bots:` file — the per-family
// writer the accounts module dispatches to (docs/accounts.md §12 L1). Email is
// NOT handled here (multi-account; sources/email owns its file) — callers route
// "email"/"email-*" elsewhere; this refuses them loudly as a guard. Returns
// (changed, err): changed=false when the id was already in/absent from the list.
func WriteSourceList(home, source, uid string, kind core.ListKind, add bool) (bool, error) {
	if strings.TrimSpace(uid) == "" {
		return false, fmt.Errorf("empty user id")
	}
	return editSourceListMulti(home, source, func(c *SourceConfig) bool {
		return applyListMutation(c, uid, kind, add)
	})
}

// editSourceListMulti applies mutate to the right SourceConfig for `source`,
// handling BOTH flat single-source yaml AND the multi-bot `bots:` shape: it
// routes telegram/feishu/discord to their per-family bot-list writer (which
// locates the named bot inside bots:) and everything else to editSourceList.
// This is the single multi-bot-aware dispatch that allowlist, admins, AND
// group-allow writes share — so none of them write a phantom flat file on a
// bots: deployment. Email is refused loudly (sources/email owns its writes).
func editSourceListMulti(home, source string, mutate func(*SourceConfig) bool) (bool, error) {
	if source == "email" || strings.HasPrefix(source, "email-") {
		return false, fmt.Errorf("email is multi-account — sources/email owns its yaml writes, not config.editSourceListMulti")
	}
	// Validate BEFORE lockSourceFamily: it joins the raw name into a .lock
	// path, so an unvalidated name would drop a lockfile outside sources/
	// before editSourceList's own check could refuse it.
	if !sourceNameRe.MatchString(source) {
		return false, fmt.Errorf("invalid source name %q", source)
	}
	// Serialize concurrent writers on the SAME underlying yaml file so a
	// read→mutate→write→rename round-trip can't lose a parallel edit (e.g.
	// a slash /allow racing a claimcode auto-allowlist, both targeting
	// telegram.yaml). Mirrors config.Update / config.UpdateModels' flock
	// pattern. The lock is keyed by the FAMILY file (not the raw source
	// name) because every "telegram-*" bot shares telegram.yaml, etc. —
	// keying by raw name would leave two bots in the same file unserialized.
	// editSourceListMulti is the single chokepoint every public write path
	// (WriteSourceList / AppendSourceAllowlist / AppendSourceGroupAllow)
	// routes through, so locking here covers them all.
	unlock, err := lockSourceFamily(home, source)
	if err != nil {
		return false, err
	}
	defer unlock()
	switch {
	case strings.HasPrefix(source, "telegram"):
		return writeTelegramBotList(home, source, mutate)
	case strings.HasPrefix(source, "feishu"):
		return writeFeishuBotList(home, source, mutate)
	case strings.HasPrefix(source, "discord"):
		return writeDiscordBotList(home, source, mutate)
	default:
		// Flat source (wechat / …): editSourceList round-trips the whole
		// SourceConfig with all guards.
		return editSourceList(home, source, mutate)
	}
}

// sourceFamilyFile maps a source name to the yaml file its writes land in.
// telegram/feishu/discord collapse their per-bot names ("telegram-bot2") onto
// the single family file ("telegram.yaml") that holds the bots: list; every
// other source is flat (file == source name).
func sourceFamilyFile(source string) string {
	switch {
	case strings.HasPrefix(source, "telegram"):
		return "telegram"
	case strings.HasPrefix(source, "feishu"):
		return "feishu"
	case strings.HasPrefix(source, "discord"):
		return "discord"
	default:
		return source
	}
}

// lockSourceFamily takes an exclusive flock on <home>/sources/<family>.yaml.lock
// and returns an unlock func. The lockfile is a 0-byte sentinel created on
// demand and never deleted (mirrors config.Update / config.UpdateModels).
func lockSourceFamily(home, source string) (func(), error) {
	dir := filepath.Join(home, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	lockPath := filepath.Join(dir, sourceFamilyFile(source)+".yaml.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// writeFeishuBotList edits one feishu bot's allowlist|admins inside the
// multi-bot bots: file, mirroring writeTelegramBotList. A flat feishu.yaml
// (single bot "feishu", no bots:) round-trips through editSourceList. A bots:
// list locates the named entry, mutates its SourceConfig, and re-marshals the
// whole list (preserving each bot's name + app_id_env/app_secret_env, since
// those live on NamedFeishuSource). Re-marshaling drops file comments — same
// tradeoff as the telegram path.
func writeFeishuBotList(home, botName string, mutate func(*SourceConfig) bool) (bool, error) {
	dir := filepath.Join(home, "sources")
	path := filepath.Join(dir, "feishu.yaml")
	raw, rerr := os.ReadFile(path)
	if os.IsNotExist(rerr) {
		if botName != "feishu" {
			return false, fmt.Errorf("feishu bot %q: no sources/feishu.yaml", botName)
		}
		return editFlatFeishuList(home, path, mutate)
	}
	if rerr != nil {
		return false, fmt.Errorf("read feishu.yaml: %w", rerr)
	}
	var f feishuSourcesFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return false, fmt.Errorf("parse feishu.yaml: %w", err)
	}
	if len(f.Bots) == 0 {
		// Flat shape: feishu-aware round-trip (NOT editSourceList) so the
		// top-level app_id_env/app_secret_env survive — those live on
		// feishuSourcesFile, not the bare SourceConfig editSourceList models,
		// so routing through it would either reject the file or drop the two
		// env fields on write.
		if botName != "feishu" {
			return false, fmt.Errorf("sources/feishu.yaml is flat (single bot \"feishu\"); no bot %q", botName)
		}
		return editFlatFeishuList(home, path, mutate)
	}
	idx := -1
	for i := range f.Bots {
		if f.Bots[i].Name == botName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, fmt.Errorf("feishu bot %q not found in sources/feishu.yaml bots:", botName)
	}
	if !mutate(&f.Bots[idx].SourceConfig) {
		return false, nil
	}
	// Marshal ONLY the bots: list (not feishuSourcesFile's inlined flat fields,
	// which are zero in list mode and would emit stray top-level keys).
	data, err := yaml.Marshal(struct {
		Bots []NamedFeishuSource `yaml:"bots"`
	}{Bots: f.Bots})
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return false, fmt.Errorf("write feishu.yaml.tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, fmt.Errorf("rename feishu.yaml: %w", err)
	}
	return true, nil
}

// editFlatFeishuList round-trips a FLAT sources/feishu.yaml (single bot
// "feishu", no bots: list), applying mutate to the inline SourceConfig while
// preserving the top-level app_id_env/app_secret_env that live on
// feishuSourcesFile but NOT on the bare SourceConfig editSourceList models.
// Routing the flat feishu file through editSourceList would either reject it
// (those two keys aren't in its top-level whitelist) or silently drop them on
// the write round-trip — a config-data loss that locks claimcode auto-allowlist
// + /allow out of feishu deployments using non-default env names. A missing
// file is treated as an empty flat config (the auto-allowlist seeds it). The
// write is atomic (tmp + rename); like the other source writers it drops file
// comments.
func editFlatFeishuList(home, path string, mutate func(*SourceConfig) bool) (bool, error) {
	var f feishuSourcesFile
	if raw, rerr := os.ReadFile(path); rerr == nil {
		if err := yaml.Unmarshal(raw, &f); err != nil {
			return false, fmt.Errorf("parse feishu.yaml: %w", err)
		}
		if len(f.Bots) > 0 {
			// Caller guarantees the flat shape; guard anyway so we never
			// clobber a bots: list down this path.
			return false, fmt.Errorf("sources/feishu.yaml has a bots: list; editFlatFeishuList is flat-only")
		}
		// Unknown-top-level-key guard (decision ⑫), mirroring
		// editSourceList: the round-trip below re-marshals ONLY the modeled
		// fields, so any other top-level key (a deprecated tools:/skills:/
		// credentials: block left on disk, or a hand-added note) would be
		// SILENTLY DROPPED on the write. Refuse loudly and name the key so the
		// operator removes/migrates it by hand — fail-loud beats silent data
		// loss. The whitelist is editSourceList's set PLUS the two feishu-only
		// env keys this function exists to preserve.
		var top map[string]yaml.Node
		if yaml.Unmarshal(raw, &top) == nil {
			for k := range top {
				switch k {
				case "enabled", "token_env", "allow_all", "allowlist", "denylist", "admins", "groups",
					"app_id_env", "app_secret_env":
				default:
					return false, fmt.Errorf("sources/feishu.yaml carries a top-level field this tool does not model (%q); edit it by hand (remove or migrate that key) so an /allow or claimcode write doesn't silently drop it", k)
				}
			}
		}
	} else if !os.IsNotExist(rerr) {
		return false, fmt.Errorf("read feishu.yaml: %w", rerr)
	}
	if !mutate(&f.SourceConfig) {
		return false, nil
	}
	// Marshal ONLY the flat shape (inline SourceConfig + the two env fields)
	// — never feishuSourcesFile.Bots, which lacks omitempty and would emit a
	// stray `bots: []` line.
	data, err := yaml.Marshal(struct {
		AppIDEnv     string `yaml:"app_id_env,omitempty"`
		AppSecretEnv string `yaml:"app_secret_env,omitempty"`
		SourceConfig `yaml:",inline"`
	}{
		AppIDEnv:     f.AppIDEnv,
		AppSecretEnv: f.AppSecretEnv,
		SourceConfig: f.SourceConfig,
	})
	if err != nil {
		return false, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return false, fmt.Errorf("write feishu.yaml.tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, fmt.Errorf("rename feishu.yaml: %w", err)
	}
	return true, nil
}

// writeDiscordBotList edits one discord bot's allowlist|admins inside the
// multi-bot bots: file, mirroring writeTelegramBotList (discord shares
// telegram's single-token NamedSourceConfig shape). A flat discord.yaml (single
// bot "discord", no bots:) round-trips through editSourceList. A bots: list
// locates the named entry, mutates its SourceConfig, and re-marshals the whole
// list (preserving each bot's name + token_env). Re-marshaling drops file
// comments — same tradeoff as the telegram/feishu paths.
func writeDiscordBotList(home, botName string, mutate func(*SourceConfig) bool) (bool, error) {
	dir := filepath.Join(home, "sources")
	path := filepath.Join(dir, "discord.yaml")
	raw, rerr := os.ReadFile(path)
	if os.IsNotExist(rerr) {
		if botName != "discord" {
			return false, fmt.Errorf("discord bot %q: no sources/discord.yaml", botName)
		}
		return editSourceList(home, "discord", mutate)
	}
	if rerr != nil {
		return false, fmt.Errorf("read discord.yaml: %w", rerr)
	}
	var f discordSourcesFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return false, fmt.Errorf("parse discord.yaml: %w", err)
	}
	if len(f.Bots) == 0 {
		// Flat shape: editSourceList round-trips the flat SourceConfig safely.
		if botName != "discord" {
			return false, fmt.Errorf("sources/discord.yaml is flat (single bot \"discord\"); no bot %q", botName)
		}
		return editSourceList(home, "discord", mutate)
	}
	idx := -1
	for i := range f.Bots {
		if f.Bots[i].Name == botName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, fmt.Errorf("discord bot %q not found in sources/discord.yaml bots:", botName)
	}
	if !mutate(&f.Bots[idx].SourceConfig) {
		return false, nil
	}
	// Marshal ONLY the bots: list (not discordSourcesFile's inlined flat fields,
	// which are zero in list mode and would emit stray top-level keys).
	data, err := yaml.Marshal(struct {
		Bots []NamedSourceConfig `yaml:"bots"`
	}{Bots: f.Bots})
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return false, fmt.Errorf("write discord.yaml.tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, fmt.Errorf("rename discord.yaml: %w", err)
	}
	return true, nil
}

// applyListMutation adds/removes uid on c's allowlist or admins, returning
// whether anything changed. An add enables an unset source (enableIfUnset);
// a remove never touches enabled.
func applyListMutation(c *SourceConfig, uid string, kind core.ListKind, add bool) bool {
	list := &c.Allowlist
	if kind == core.ListAdmin {
		list = &c.Admins
	}
	if add {
		if list.Contains(uid) {
			return false
		}
		*list = append(*list, uid)
		enableIfUnset(c)
		return true
	}
	for i, id := range *list {
		if id == uid {
			*list = append((*list)[:i], (*list)[i+1:]...)
			return true
		}
	}
	return false
}

// writeTelegramBotList edits one bot's allowlist|admins inside
// sources/telegram.yaml, preserving every OTHER bot + every other field. Handles
// both shapes: a flat telegram.yaml (single implicit bot "telegram") delegates
// to editSourceList; a `bots:` list locates the named entry and re-marshals the
// list (only — never the inlined flat fields, which would emit stray top-level
// nulls).
func writeTelegramBotList(home, botName string, mutate func(*SourceConfig) bool) (bool, error) {
	dir := filepath.Join(home, "sources")
	path := filepath.Join(dir, "telegram.yaml")
	raw, rerr := os.ReadFile(path)
	if os.IsNotExist(rerr) {
		// No file yet → only the implicit flat "telegram" bot is meaningful.
		if botName != "telegram" {
			return false, fmt.Errorf("telegram bot %q: no sources/telegram.yaml", botName)
		}
		return editSourceList(home, "telegram", mutate)
	}
	if rerr != nil {
		return false, fmt.Errorf("read telegram.yaml: %w", rerr)
	}
	var f telegramSourcesFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return false, fmt.Errorf("parse telegram.yaml: %w", err)
	}
	if len(f.Bots) == 0 {
		// Flat shape: editSourceList round-trips the flat SourceConfig safely.
		if botName != "telegram" {
			return false, fmt.Errorf("sources/telegram.yaml is flat (single bot \"telegram\"); no bot %q", botName)
		}
		return editSourceList(home, "telegram", mutate)
	}
	idx := -1
	for i := range f.Bots {
		if f.Bots[i].Name == botName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, fmt.Errorf("telegram bot %q not found in sources/telegram.yaml bots:", botName)
	}
	if !mutate(&f.Bots[idx].SourceConfig) {
		return false, nil
	}
	// Marshal ONLY the bots: list (not telegramSourcesFile's inlined flat fields,
	// which are zero in list mode and would emit stray top-level `enabled: null`).
	data, err := yaml.Marshal(struct {
		Bots []NamedSourceConfig `yaml:"bots"`
	}{Bots: f.Bots})
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return false, fmt.Errorf("write telegram.yaml.tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, fmt.Errorf("rename telegram.yaml: %w", err)
	}
	return true, nil
}
