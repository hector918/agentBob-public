package sources

import (
	"fmt"
	"log/slog"

	"agentbob/contract"
	"agentbob/heartwood/files"
	"agentbob/leaf/stoma/sources/bridge"
	"agentbob/leaf/stoma/sources/discord"
	"agentbob/leaf/stoma/sources/email"
	"agentbob/leaf/stoma/sources/feishu"
	"agentbob/leaf/stoma/sources/local"
	"agentbob/leaf/stoma/sources/telegram"
)

// botName resolves a bot entry's source name: an explicit Name wins; an empty Name
// defaults to the platform name for the first entry and "<platform>-N" for the rest
// (so a single-bot config needs no name, and multi-bot just lists entries).
func botName(name, platform string, i int) string {
	if name != "" {
		return name
	}
	if i == 0 {
		return platform
	}
	return fmt.Sprintf("%s-%d", platform, i+1)
}

// envOrName returns the configured env-var name, or the platform default when empty.
func envOrName(name, def string) string {
	if name != "" {
		return name
	}
	return def
}

// addSource appends src unless err is non-nil — a misconfigured platform (a
// missing token / secret) logs loudly and is skipped, never taking down the
// other sources.
func addSource(srcs []contract.Source, name string, src contract.Source, err error) []contract.Source {
	if err != nil {
		slog.Error("source disabled — construction failed", "source", name, "err", err)
		return srcs
	}
	return append(srcs, src)
}

// Assemble constructs every enabled source from cfg and returns the set to hand
// to stoma.New. This is the platform-wiring point: each source kind is an
// import + a constructor here, gated by its config. Adding a platform = a config
// sub-struct (00-config.go) + one block below; stoma never learns the platform's
// name. fileStore captures inbound attachments (may be nil to disable capture).
func Assemble(cfg Config, fileStore *files.Store) []contract.Source {
	var srcs []contract.Source
	// The internal source is ALWAYS registered (not config-gated): it is the
	// in-process ingress for send_message / cron / proactive delivery. In-process
	// and cheap; absent any emitter it simply sits idle (its Run blocks on an empty
	// buffer). See docs/agora-port.md §6.3.
	srcs = append(srcs, bridge.New())
	if cfg.Local.Enabled {
		srcs = append(srcs, local.New())
	}
	for i, tc := range cfg.Telegram {
		if !tc.Enabled {
			continue
		}
		name := botName(tc.Name, "telegram", i)
		tg, err := telegram.New(name, envOrName(tc.TokenEnv, "TELEGRAM_BOT_TOKEN"), tc.MaxAttachmentMB<<20, fileStore)
		srcs = addSource(srcs, name, tg, err)
	}
	for i, fc := range cfg.Feishu {
		if !fc.Enabled {
			continue
		}
		name := botName(fc.Name, "feishu", i)
		fs, err := feishu.New(name, envOrName(fc.AppIDEnv, "FEISHU_APP_ID"), envOrName(fc.AppSecretEnv, "FEISHU_APP_SECRET"), fc.MaxAttachmentMB<<20, fileStore)
		srcs = addSource(srcs, name, fs, err)
	}
	for i, dc := range cfg.Discord {
		if !dc.Enabled {
			continue
		}
		name := botName(dc.Name, "discord", i)
		ds, err := discord.New(name, envOrName(dc.TokenEnv, "DISCORD_BOT_TOKEN"), dc.MaxAttachmentMB<<20, fileStore)
		srcs = addSource(srcs, name, ds, err)
	}
	for _, ec := range cfg.Email {
		if ec.Enabled != nil && !*ec.Enabled { // presence-gated: nil = enabled; explicit false = skip
			continue
		}
		es, err := email.New(ec, fileStore)
		srcs = addSource(srcs, ec.SourceID, es, err) // log key = the real Name() (S-34), not "email:"+id
	}
	return srcs
}
