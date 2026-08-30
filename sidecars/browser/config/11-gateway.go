package config

import (
	"time"
)

type GatewayConfig struct {
	// MaxPendingBatch is the single "how much may pile up while busy" knob
	// (default 10; clamped to >= 1). It caps BOTH (a) how many messages
	// coalesce into one queued turn for text — a running turn is never
	// interrupted — AND (b) how many busy-todos show as queued before new
	// ones get a "queue full" reply. (Image messages are NOT folded — they
	// drain one-per-turn — so for an image burst this is purely the queue
	// depth: arrivals past it are dropped while a turn runs.)
	MaxPendingBatch int `yaml:"max_pending_batch"`
	// MaxConcurrentLLMTurnsPerScope caps how many sids in the same scope
	// (session_scope) can have an LLM turn running simultaneously (v0.2
	// multi-session model — see docs/session-design.md). Default 2.
	// Per-scope, not global: different users / different group chats each
	// get their own cap. /new is unlimited; this only throttles concurrent
	// LLM work. When the cap is hit, the new turn queues + the user gets
	// a "📥 LLM busy (N/N)" notice.
	//
	// Recommended >= 2. With cap=1 the active sid's priority is absolute
	// (prioritySem prefers the active sid on every release), so a non-
	// active sid — e.g. a reply to an older session — only runs when the
	// active sid is fully idle. Under sustained activity it can starve.
	MaxConcurrentLLMTurnsPerScope int `yaml:"max_concurrent_llm_turns_per_scope"`
	// TranscribeMaxMB caps the size of a voice/audio attachment the gateway
	// will auto-transcribe (read + base64 into one ASR request). Past it the
	// clip is almost certainly longer than the ASR model's context anyway,
	// so the message gets a short "too long" note instead of a transcript.
	// Default 15.
	TranscribeMaxMB int `yaml:"transcribe_max_mb"`
	// OCR tunes the ocr tool's per-call image size cap (see OCRConfig).
	OCR OCRConfig `yaml:"ocr"`
	// SourceHealth tunes the per-source heartbeat. Defaults: enabled,
	// 60s interval, 3 consecutive failures before notify, 10min
	// reminder cooldown. Failures route through the system-level admin
	// line (top-level admin_line — see AdminLineConfig).
	SourceHealth SourceHealthConfig `yaml:"source_health"`
	// Admins / Telegram are legacy fields kept for one-time migration to
	// per-source yaml at $BOB_HOME/sources/<name>.yaml (per-platform admin
	// design). On startup, if either is non-empty and the
	// target sources/telegram.yaml doesn't yet exist, the migrator writes
	// the new file from these fields and clears them; the next config save
	// drops them from disk. Don't read these at runtime — the per-source
	// IsAdmin / Authorized lives on config.SourceConfig now.
	Admins   AdminList      `yaml:"admins,omitempty"`
	Telegram TelegramConfig `yaml:"telegram,omitempty"`
}

// SourceHealthConfig tunes the gateway's per-source heartbeat loop.
// Heartbeat is enabled by default; explicit Enabled=false disables.
// All numeric fields fall back to spec defaults when zero/negative.
// Tuning rationale lives in docs/admin-notify.md.
type SourceHealthConfig struct {
	// Enabled toggles the heartbeat. Pointer so the yaml-absent case
	// can default to true (a yaml that doesn't mention source_health
	// still gets monitoring) while explicit `enabled: false` disables.
	Enabled *bool `yaml:"enabled"`
	// IntervalSec — seconds between probe rounds. Default 60. Each
	// probe is per-source (telegram getMe ≈ 10-50ms HTTP), so 60s is
	// well under any provider rate limit and gives ~minute-resolution
	// outage detection.
	IntervalSec int `yaml:"interval_sec"`
	// FailThreshold — consecutive HealthCheck failures before the
	// healthy→unhealthy transition fires AdminNotify. Default 3 —
	// rides through one transient hiccup but pages on real outages.
	FailThreshold int `yaml:"fail_threshold"`
	// NotifyCooldownMin — minutes between repeat AdminNotify for a
	// source that stays unhealthy. Default 10. Recovery transition
	// always notifies regardless of cooldown.
	NotifyCooldownMin int `yaml:"notify_cooldown_min"`
}

// EnabledEff applies the default (true when Enabled is unset).
func (s SourceHealthConfig) EnabledEff() bool {
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// IntervalEff returns the probe interval, defaulting to 60s when
// unset or non-positive.
func (s SourceHealthConfig) IntervalEff() time.Duration {
	if s.IntervalSec <= 0 {
		return 60 * time.Second
	}
	return time.Duration(s.IntervalSec) * time.Second
}

// FailThresholdEff returns the fail threshold, defaulting to 3.
func (s SourceHealthConfig) FailThresholdEff() int {
	if s.FailThreshold <= 0 {
		return 3
	}
	return s.FailThreshold
}

// CooldownEff returns the notify-cooldown duration, defaulting to 10m.
func (s SourceHealthConfig) CooldownEff() time.Duration {
	if s.NotifyCooldownMin <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(s.NotifyCooldownMin) * time.Minute
}

// MaxConcurrentLLMTurnsPerScopeEff returns the cap with the default
// (2) applied when the yaml leaves it unset or non-positive.
func (g GatewayConfig) MaxConcurrentLLMTurnsPerScopeEff() int {
	if g.MaxConcurrentLLMTurnsPerScope <= 0 {
		return 2
	}
	return g.MaxConcurrentLLMTurnsPerScope
}

// TranscribeMaxBytesEff returns the voice/audio transcription size cap in
// bytes, defaulting to 15 MB when unset.
func (g GatewayConfig) TranscribeMaxBytesEff() int64 {
	mb := g.TranscribeMaxMB
	if mb <= 0 {
		mb = 15
	}
	return int64(mb) << 20
}

// OCRConfig tunes the ocr tool. Zero values use sane defaults so an
// operator's config.yaml can leave the whole `gateway.ocr:` block out.
type OCRConfig struct {
	// MaxBytes caps the size in bytes of an image the ocr tool will
	// read off disk before forwarding to the model pool's KindOCR
	// backend. 0 → default 10 MB. Past it the tool returns a typed
	// error ("ocr: image too large …") without ever loading the file
	// — bounded memory regardless of the image found at the path the
	// agent passed.
	MaxBytes int `yaml:"max_bytes"`
}

// MaxBytesEff returns the effective image-size cap, defaulting to 10 MB
// when MaxBytes is unset or non-positive.
func (c OCRConfig) MaxBytesEff() int {
	if c.MaxBytes <= 0 {
		return 10 * 1024 * 1024
	}
	return c.MaxBytes
}

// AttachmentsConfig controls capturing inbound files/images/voice. Each platform
// gets its own tree under $BOB_HOME/attachments/<platform>/ so retention and a
// total-size cap can be enforced per platform. Top-level fields are the defaults;
// Platforms entries override them per platform (nil pointer = inherit default).
type AttachmentsConfig struct {
	Enabled       *bool                          `yaml:"enabled"`             // default true; platform override possible
	MaxDownloadMB int                            `yaml:"max_download_mb"`     // per-file cap in MB; default 25
	RetentionDays int                            `yaml:"retention_days"`      // delete day-dirs older than this; default 14; <=0 disables time-based prune
	MaxTotalMB    int                            `yaml:"max_total_mb"`        // total disk cap for this platform's tree (MB); 0 = no total cap
	Platforms     map[string]PlatformAttachments `yaml:"platforms,omitempty"` // per-platform overrides (keyed by source name: "telegram", "whatsapp", …)
}

// PlatformAttachments overrides AttachmentsConfig defaults for one platform.
// Every field is a pointer — a nil field means "inherit the default".
type PlatformAttachments struct {
	Enabled       *bool `yaml:"enabled,omitempty"`
	MaxDownloadMB *int  `yaml:"max_download_mb,omitempty"`
	RetentionDays *int  `yaml:"retention_days,omitempty"`
	MaxTotalMB    *int  `yaml:"max_total_mb,omitempty"`
}

// ResolvedAttachments is the effective config for one platform (defaults + override).
type ResolvedAttachments struct {
	Enabled       bool
	MaxDownloadMB int
	RetentionDays int
	MaxTotalMB    int
}

// For returns the effective attachments config for a given platform name.
func (a AttachmentsConfig) For(platform string) ResolvedAttachments {
	r := ResolvedAttachments{
		Enabled:       a.Enabled == nil || *a.Enabled,
		MaxDownloadMB: a.MaxDownloadMB,
		RetentionDays: a.RetentionDays,
		MaxTotalMB:    a.MaxTotalMB,
	}
	if r.MaxDownloadMB <= 0 {
		r.MaxDownloadMB = 25
	}
	// NOTE: RetentionDays is intentionally NOT rewritten here. An explicit
	// top-level 0 (or negative) must pass through so the sweeper's `> 0`
	// disable check ("0 disables time-based prune") works as documented.
	// The real default for an unconfigured install lives in Default()
	// (RetentionDays: 14); rewriting 0→14 here would silently turn the
	// documented "keep forever" into 14-day deletion and would also be
	// asymmetric with per-platform *int overrides, which already pass 0
	// through to disable pruning.
	if p, ok := a.Platforms[platform]; ok {
		if p.Enabled != nil {
			r.Enabled = *p.Enabled
		}
		if p.MaxDownloadMB != nil {
			r.MaxDownloadMB = *p.MaxDownloadMB
		}
		if p.RetentionDays != nil {
			r.RetentionDays = *p.RetentionDays
		}
		if p.MaxTotalMB != nil {
			r.MaxTotalMB = *p.MaxTotalMB
		}
	}
	return r
}

// MaxBytes is the per-file cap in bytes (for io.LimitReader).
func (r ResolvedAttachments) MaxBytes() int64 { return int64(r.MaxDownloadMB) << 20 }

// MaxTotalBytes is the total disk cap in bytes (0 = no cap).
func (r ResolvedAttachments) MaxTotalBytes() int64 {
	if r.MaxTotalMB <= 0 {
		return 0
	}
	return int64(r.MaxTotalMB) << 20
}

// TelegramConfig is the LEGACY shape kept solely for the one-time
// migration to $BOB_HOME/sources/telegram.yaml. Don't add fields here —
// add them to SourceConfig instead. After migration the per-source
// `enabled`-IsZero check below makes this struct yaml-omittable.
type TelegramConfig struct {
	Enabled   bool                  `yaml:"enabled,omitempty"`
	TokenEnv  string                `yaml:"token_env,omitempty"`
	AllowAll  bool                  `yaml:"allow_all,omitempty"`
	Allowlist IDList                `yaml:"allowlist,omitempty"`
	Denylist  IDList                `yaml:"denylist,omitempty"`
	Chats     map[string]ChatPolicy `yaml:"chats,omitempty"`
}

// IsZero returns true when the legacy TelegramConfig has no values set —
// used by the migrator to skip already-migrated configs.
func (t TelegramConfig) IsZero() bool {
	return !t.Enabled && t.TokenEnv == "" && !t.AllowAll &&
		len(t.Allowlist) == 0 && len(t.Denylist) == 0 && len(t.Chats) == 0
}
