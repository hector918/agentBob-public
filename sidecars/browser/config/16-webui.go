package config

import (
	"strings"
	"time"
)

// AdminLineConfig configures the system-level admin line — the single
// "summon the admin" funnel (see docs/admin-line-design.md). The only
// dial is the static attach target: where the line forwards alerts.
type AdminLineConfig struct {
	// Outlet is the static attach spec — design §5, single attach,
	// applied at boot. Three forms:
	//
	//   ""                     unattached (default): alerts are still
	//                           persisted to the append-only audit log,
	//                           just not forwarded anywhere live.
	//
	//   "<source>:<chat_id>"   forward each alert as a plain message via
	//                           that source to a chat a human reads.
	//                           <source> is a source name — "telegram"
	//                           (the primary bot) or any extra bot such
	//                           as "telegram-2". Typical use: set the chat
	//                           id to your OWN telegram user id so the bot
	//                           DMs the alerts straight to you (start a
	//                           chat with the bot first so it is allowed
	//                           to DM you). Example:
	//                               admin_line:
	//                                 outlet: "telegram-2:222222222"
	//
	//   "agora:<inbox_scope>"  emit each alert into an agora company-inbox
	//                           scope — the agora Member staffing that
	//                           inbox handles it as a normal turn.
	//
	// A bad / unknown outlet never blocks boot: the wiring logs a WARN and
	// degrades to unattached.
	Outlet string `yaml:"outlet"`
}

// OutletEff returns the trimmed outlet spec. Trimming happens here so
// downstream callers (the adminline.NewDeliverer factory) don't need
// their own normalisation.
func (a AdminLineConfig) OutletEff() string {
	return strings.TrimSpace(a.Outlet)
}

// SkillsConfig tunes the skills system (docs/skills-system-design.md).
// Most behaviour is fixed by design; this struct only exposes the dials
// that have to differ per deployment.
type SkillsConfig struct {
	// IndexVisibleCap caps how many skill descriptions land in the
	// per-session skills-index after role filtering. 0 = no cap. When
	// hit, deterministic alphabetical truncation + one-shot admin DM
	// per session (Phase 15). Default 50 — comfortably below the
	// Claude Code 1%-of-context budget at ~80 tok/description.
	// Pointer so an explicit 0 (no cap) is distinguishable from unset
	// (default 50) — same pattern as Enabled *bool knobs.
	IndexVisibleCap *int `yaml:"index_visible_cap"`
}

// IndexVisibleCapEff returns the effective cap with the default applied.
// 0 means "no cap" downstream (NewSkillsIndexSource skips truncation).
func (s SkillsConfig) IndexVisibleCapEff() int {
	if s.IndexVisibleCap == nil {
		return 50
	}
	if *s.IndexVisibleCap <= 0 {
		return 0
	}
	return *s.IndexVisibleCap
}

// AdminConfig — local-only admin HTTP endpoint. Currently used by
// `bob db reattach` CLI to talk to the running bob process; could
// host other ops-only handlers later.
//
// Default 127.0.0.1:9876 — bound to loopback so external network
// access is impossible without explicit reconfiguration. Set http_addr
// to "off" to disable the listener entirely (then only telegram
// slash works).
//
// Multi-bot deployments (multiple bobs on one host via --home /
// separate $BOB_HOME): EACH bot MUST set a different http_addr.
// The second one to start otherwise can't bind 9876 and silently
// falls back to "no admin HTTP" — `bob db reattach` from the second
// BOB_HOME would then end up talking to the first bot. Suggested
// pattern: 127.0.0.1:9876 for bot A, 127.0.0.1:9877 for bot B, etc.
type AdminConfig struct {
	HTTPAddr string `yaml:"http_addr"` // "off" disables; default "127.0.0.1:9876"
}

func (a AdminConfig) HTTPAddrEff() string {
	switch a.HTTPAddr {
	case "":
		return "127.0.0.1:9876"
	case "off":
		return ""
	}
	return a.HTTPAddr
}

// WebUIConfig — the standalone read-only status dashboard (webui package).
// Independent module: removing the whole feature is deleting the webui
// package, its Start call in pipeline-startup, and this struct.
//
// Own HTTP listener, separate from admin.http_addr. The dashboard has
// per-coverage session auth, but the default 127.0.0.1:9877 stays
// loopback as defence-in-depth (keeps it off the network). "off"
// disables it.
type WebUIConfig struct {
	Addr           string `yaml:"addr"`            // "off" disables; default "127.0.0.1:9877"
	RefreshSeconds int    `yaml:"refresh_seconds"` // ticker cadence; 0 → default 3
}

func (w WebUIConfig) AddrEff() string {
	switch w.Addr {
	case "":
		return "127.0.0.1:9877"
	case "off":
		return ""
	}
	return w.Addr
}

func (w WebUIConfig) RefreshEff() time.Duration {
	if w.RefreshSeconds <= 0 {
		return 3 * time.Second
	}
	return time.Duration(w.RefreshSeconds) * time.Second
}
