// Package config defines the agentbob configuration and loads it from
// $BOB_HOME/config.yaml (settings) + $BOB_HOME/.env (secrets only).
package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvExample is the template written to $BOB_HOME/.env.example by `bob config init`.
// Copy it to $BOB_HOME/.env and fill in the secrets you need. .env is gitignored —
// it should NEVER be committed; only secrets (API keys, bot tokens, passwords) go here.
// Slots for not-yet-built channels are kept commented out so you know where they go.
const EnvExample = `# agentbob secrets — copy this file to .env (same directory) and fill in.
# This file lives in $BOB_HOME (default ~/.bob/). .env is gitignored — never commit it.
# Only secrets belong here; non-secret settings go in config.yaml.
#
# Pattern: config.yaml has *_env: NAME entries pointing at variables defined here.
# So to use a different env-var name, change the *_env in config.yaml AND the name below.

# ---- IM platforms ----

# Telegram bot token from @BotFather (needed for ` + "`bob gateway`" + `):
TELEGRAM_BOT_TOKEN=

# Feishu / Lark 自建应用 over WebSocket. Setting BOTH vars enables the source.
#   1. open.feishu.cn → create a 企业自建应用 → copy App ID + App Secret
#   2. 应用功能 → 机器人: enable bot ability
#   3. 权限管理: add im:message, im:message.group_at_msg, im:message:send_as_bot
#   4. 事件订阅: choose 长连接/WebSocket mode → subscribe im.message.receive_v1
#   5. fill both lines, restart bob, then DM the bot or @it in a group
# See docs/sources-feishu-design.md §8 for the full checklist.
# FEISHU_APP_ID=cli_xxxxxxxxxxxxxxxx
# FEISHU_APP_SECRET=

# Future IM channels — uncomment + fill in when those sources land:
# WHATSAPP_TOKEN=
# IMESSAGE_TOKEN=
# WECOM_SECRET=

# ---- Email channel ----
# The email source is configured in sources/email.yaml (imap_host,
# username_env, password_env per account); these are the env slots its
# *_env keys point at:
# EMAIL_USERNAME=
# EMAIL_PASSWORD=

# ---- Model provider keys ----
# Only set the one your models.yaml entries' api_key_env actually point at:
# OPENAI_API_KEY=
# OPENROUTER_API_KEY=
# ANTHROPIC_API_KEY=

# ---- PostgreSQL (optional; required when store.backend=postgres or fallback) ----
# Set store.backend in config.yaml to "postgres" (or "fallback" for pg+sqlite
# dual) and put the libpq DSN here. The name (BOB_POSTGRES_DSN) is the default;
# rename via store.postgres.dsn_env if you prefer another env var name.
# Recommended params: ?connect_timeout=2&statement_timeout=10000&pool_max_conns=10
# (short connect_timeout keeps fallback failover fast.)
# BOB_POSTGRES_DSN=postgres://user:pass@host:5432/dbname?sslmode=disable&connect_timeout=2
`

// ConfigYAMLExample is the annotated template written to $BOB_HOME/config.yaml.example
// by `bob config init` (alongside the actual config.yaml, which is what's read at runtime).
// The active config can be edited by hand or via ` + "`bob config set`" + ` — comments are
// NOT preserved when ` + "`bob config set`" + ` re-saves; keep this .example as a reference.
const ConfigYAMLExample = `# agentbob configuration — annotated reference template.
# The actual file the gateway reads is config.yaml (same directory). Edit that one
# (or use ` + "`bob config set <key> <value>`" + `); this .example file is just for documentation.
# Settings only — secrets go in .env (alongside this file).

_config_version: 1

agent:
  name: bob          # how the agent identifies itself in replies
  max_loop_iter: 40  # ABSOLUTE ceiling on tool rounds per user turn. With the
                     # progress budget below, a healthy task can run all the
                     # way up to this; raise it for very long workflows.
  loop_budget: 12    # no-progress tolerance (docs/loop-budget.md): a turn this
                     # many rounds past its last progress (new tool information
                     # or a delivered artifact) gets the way-out nudge, then a
                     # blocked exit. Progress refills it implicitly.

display:
  stream: true   # token-by-token streaming reply

attachments:
  # Each platform gets its own tree at $BOB_HOME/attachments/<platform>/<YYYY-MM-DD>/.
  # Top-level here is the default for ALL platforms; per-platform overrides go under platforms.
  enabled: true
  max_download_mb: 25     # per-file cap (Telegram bots can't getFile >20MB anyway)
  retention_days: 14      # delete day-dirs older than this; 0 disables time-based prune
  max_total_mb: 0         # total disk cap PER PLATFORM tree; 0 = no cap
  # platforms:
  #   telegram:
  #     enabled: true
  #     max_download_mb: 25
  #     retention_days: 14
  #     max_total_mb: 10240   # cap the telegram tree at 10 GB
  #   whatsapp:
  #     max_download_mb: 100
  #     retention_days: 7

memory:
  # Hard cap on the bytes Memory.ReadChain returns (i.e. what gets pre-injected
  # into the system prompt every turn). Once exceeded, the chain truncates
  # root-first-keep + slog.Errors so operators notice. Default 8192 (~2k tokens).
  chain_max_bytes: 8192
  # When true, memory layer paths include bots/<botid>/ — for single-process
  # multi-bot setups. Default false (legacy paths). Migration: mv existing
  # files into bots/<botid>/ before flipping. See docs/memory.md.
  namespace_by_bot: false

tools:
  # External tools — read_file / search_files / patch / execute_code / etc.
  # See docs/tools-design.md for the full design.
  filesystem:
    # Per-session scratch root. Each session_scope gets its own subdir under
    # <root>/by_sessions_scope/<session_scope>/ (auto-created, auto-cleaned 24h
    # after last use). Always read+write. Default: $BOB_HOME/sandbox/
    # (full path: $BOB_HOME/sandbox/by_sessions_scope/<session_scope>/).
    sandbox_root: ""

    # Operator-declared READ paths (recursive). Empty → tools can only see
    # the sandbox. In container deployments, declare what you mounted via
    # the docker-compose volumes setting here. In native dev, list specific project dirs.
    #   read_roots: ["~/Projects/foo", "~/Documents/notes"]
    read_roots: []

    # Operator-declared READ-WRITE paths (recursive). Use sparingly. Empty →
    # tools can only WRITE to the sandbox. patch / write_file land here.
    #   read_write_roots: ["~/work/scratch"]
    read_write_roots: []

    # Single read_file call cap. Default 1 MB.
    max_read_bytes: 1048576

    # Directory names skipped by search_files. Default skips well-known noise
    # dirs: .git, node_modules, __pycache__, target, .venv, venv, vendor,
    # dist, build, .next, .cache. Override to widen or narrow.
    #   search_skip_dirs: [".git", "node_modules", ...]
    search_skip_dirs: []

  execution:
    # cwd for execute_code / terminal is always $BOB_HOME/sandbox + a per-session subdir.
    # Wall-time cap per subprocess call (seconds). Default 30.
    max_seconds: 30
    # Output cap per stdout/stderr stream (bytes). Default 65536 (64 KB).
    # Beyond cap → buffer truncated, truncated_stdout/stderr=true in result.
    max_output_bytes: 65536
    # 想关掉 execute_code / terminal：把 permissions.yaml 里对应角色的
    # tool:use:execute_code / tool:use:terminal 行设为 off（工具的可见性由
    # 授权矩阵控制，不再有 enabled 开关）。
    # Docker-run subprocess mode — RESERVED, not yet implemented.
    # When implemented, enabling this runs each call via "docker run --rm"
    # inside the configured image, capping CPU / memory / network. For now
    # leave enabled false; native (process-group) mode is what runs.
    docker:
      enabled: false
      image: "python:3.12-slim"
      network: "bridge"
      memory_mb: 256
      cpus: "1.0"

  web:
    # Search backend. v1 supports "searxng" only (self-hosted recommended).
    # "tavily" / "brave" reserved for future API-key backends.
    backend: "searxng"
    # Base URL for the search backend. Required for searxng — point at your
    # instance: https://searx.example.com. v1 errors out if unset and the
    # model calls web_search.
    backend_url: ""
    # Env var name with the backend's API key, if needed (Tavily/Brave —
    # not used by SearXNG).
    api_key_env: ""
    # Cap on results returned per call. Default 10, hard cap 50.
    max_results: 10
    # HTTP timeout per call (web_search + scrapling). Default 15s.
    fetch_timeout_seconds: 15
    # 想关掉 web_search / scrapling：把 permissions.yaml 里对应角色的
    # tool:use:web_search / tool:use:scrapling 行设为 off。

  browser:
    # Powered by chromedp + a host Chromium binary. Per-session browser
    # instance, lazy-created on first browser_navigate, closed by the
    # periodic sweep at idle_ttl_seconds (30 min default).
    # 想关掉 browser_* 工具：把 permissions.yaml 里对应角色的
    # tool:use:browser_* 行设为 off。
    # Run Chromium without a window. Default true (you almost never want
    # headed in a server). Set false for debug.
    headless: true
    # Path to the Chromium binary. "" → chromedp auto-detects (looks for
    # google-chrome, chromium, etc. on PATH and OS-conventional places).
    # In docker we install /usr/bin/chromium-browser via apk; chromedp
    # finds it automatically.
    exec_path: ""
    # Per-session user-data dir root. "" → uses the per-session sandbox
    # subdir. Set if you want browser profiles to live elsewhere.
    user_data_dir_root: ""
    # Per-call timeout (navigate, snapshot). Default 30s.
    page_timeout_seconds: 30
    # Close idle browser instances older than this. Default 30 min.
    idle_ttl_seconds: 1800
    # Apply basic anti-detection flags (disable-blink-features=
    # AutomationControlled, custom user-agent, navigator.webdriver=false,
    # etc.). Default true. Won't beat Cloudflare Turnstile reliably —
    # those need rebrowser-playwright-grade stealth.
    stealth_flags: true
    # Simulate human input on click/type: curved mouse trajectory with
    # jittered timing into a randomized in-element landing point, plus a
    # per-keystroke typing cadence. Default true.
    humanize: true

gateway:
  # NOTE: per-platform admin lists + channel allow/denylists are NOT
  # configured here anymore. They live per source in
  # $BOB_HOME/sources/<name>.yaml (e.g. sources/telegram.yaml). The
  # legacy gateway.admins / gateway.telegram / gateway.email keys below
  # are migrated into those files on FIRST boot, then ignored — editing
  # them here after the sources/ files exist is silently dropped. Edit
  # sources/<name>.yaml (or use /allow, /admin) instead.
  #
  # While a turn is running, coalesce up to N new messages into the next turn.
  # Default 10; a running turn is never interrupted.
  max_pending_batch: 10
  # Max size (MB) of a voice/audio attachment to auto-transcribe. Past it the
  # message gets a short "too long" note instead of a transcript. Default 15.
  transcribe_max_mb: 15
  # Max concurrent LLM-turn count per scope (= session_scope). v0.2 multi-
  # session: /new is unlimited, but how many sessions in one scope can
  # run an LLM turn simultaneously is capped here. Default 2. Hitting
  # the cap doesn't drop messages — they queue per-session via the
  # busy-trigger todo path and run as soon as a slot frees. See
  # docs/session-design.md.
  #
  # Recommended >= 2. cap=1 means the scope's active sid always wins
  # the priority semaphore, so a non-active sid (a reply to an older
  # session) only runs when the active sid is fully idle — under
  # sustained activity it can wait indefinitely. cap=2 lets one active
  # + one reply-context turn proceed in parallel, which matches the
  # most common multi-session usage.
  max_concurrent_llm_turns_per_scope: 2
  # admins / telegram / email: LEGACY one-time-migration keys only — see
  # the NOTE above. Configure channels in $BOB_HOME/sources/<name>.yaml.

logging:
  target: file   # "file" | "stdout" | "both"
  level: info    # "debug" | "info" | "warn" | "error"

cleanup:
  logs_max_size_mb: 10        # rotate agent.log past this size
  logs_keep: 3                # how many rotated log files; capped at 10
  sessions_retention_days: 30 # archive cold sessions (last msg older than this)
                              # into bob_sessions_archive — dead OR alive, kept
                              # restorable via reply. Default 30; 0 = never
                              # archive (keep everything in the hot tables).
                              # Floored at the learning settling age so a turn
                              # is always distilled before its session archives.
  sweep_interval_hours: 6     # background housekeeping cadence

# turn-lifecycle: per-turn safety guards + every-N-hours auto-distill of
# ended sessions into "insights" rows that get injected back into tools'
# descriptions and per-user/group memory files. All optional; defaults
# in code via *Eff() getters. The whole housekeeping pass (cleanup +
# learning sweep + tool-insights reload) runs on the same timer above
# (sweep_interval_hours).
turn_lifecycle:
  # Safety guards (per turn, not per session)
  same_tool_args_max: 3       # 同一 tool+同样 args 重复 N 次以上 → 设 stuck-loop exit 终止本轮
  same_tool_fail_max: 6       # 同一 tool 连续失败 N 次以上（不看 args，成功即清零）→ 同上（stuck-loop exit）
  same_domain_fail_max: 4     # fetch 类工具在同一 host（跨 mode/URL）累计失败 N 次以上 → 同上（domain-stuck exit）
  smart_escalation_failures: 4 # 一轮里工具失败累计 ≥ N 次后，下一 iter 把模型偏好从 toolcall 升到 smart
  loop_detection: true        # 关掉这两个 guard（不推荐；调试时可暂时设 false）

  # Sweep-time learning (auto-distill insights from ended sessions)
  min_age_days_for_learning: 3    # 会话结束多少天后才参与 sweep（settling delay）
  learning_batch_token_cap: 8000  # 多 session 合批的 token 上限（按 4 字符=1 token 粗估）
  learning_sweep_limit: 50        # 单次 sweep 最多分析 N 个 session（避免长 downtime 后一拖小时）
  tool_insights_max_per_tool: 20  # 单文件 tools/insights.md 里每个工具段保留的 autoinsight 条数上限；admin 手写行不计不驱逐
  insight_compress_threshold: 12  # 某工具段 autoinsight 条数达到此值才触发压缩整理（省 LLM）

# per-session todo subsystem (设计: docs/todo-design.md)。
# 模型用 todo() 工具创建/更新当前会话的任务列表 → 渲染进 system prompt
# → 完成时根据 mode 走人工/judge 验收。整段都是 optional —— 三个 *Eff()
# 兜底就是下面的默认值。
todo:
  # 新 session 的 default mode：manual (默认) / auto / none
  #   manual = 完成 → pending_review，等用户点 ✅/❌ 按钮
  #   auto   = 每 judge_after_completions 次累积调一次 judge-tagged 模型验收
  #   none   = 模型说完成 = 完成 (荣誉制)
  # list 非空时锁定 mode；要切先清空再带新 mode。
  # 默认 none（普通对话荣誉制，不弹人工验收按钮）；agora 会话不受影响，
  # 由 internal source override 强制 auto。
  default_mode: none
  # auto mode 的批量验收阈值。0 → 走默认 3。
  judge_after_completions: 3
  # FailCount ≥ N → Escalated=true ("stuck")。原 id 上无法清除 stuck，
  # 唯一干净恢复 = cancel + 用新 id 重提。0 → 走默认 3。
  stuck_after_fails: 3

# 持久化后端选择 (设计: docs/store-dual-backend.md)。
# 默认就是 sqlite —— 改 backend 才需要这一段。
store:
  # "sqlite" | "postgres" | "fallback"
  #   sqlite   = 单 sqlite (默认；本地文件)
  #   postgres = pg 主，无 fallback (pg 挂直接报错)
  #   fallback = pg 主 + sqlite 备，运行时自动 failover
  backend: sqlite

  sqlite:
    path: ""                          # 空 → $BOB_HOME/sqlite-store/state.db

  postgres:
    dsn_env: BOB_POSTGRES_DSN         # env var 名；DSN 内容在 .env，永不进 yaml
    health_interval_sec: 30           # FallbackStore 健康检查频率
    failback_success_count: 3         # 连续 N 次健康才 failback (hysteresis)
    # DSN 建议加：?connect_timeout=2&statement_timeout=10000&pool_max_conns=10
    # 短 connect_timeout 让 failover 不卡。

# ── Skills ──────────────────────────────────────────────────────────
skills:
  index_visible_cap: 50    # 每会话 skills-index 最多列几条(角色过滤后);0=不限

# ── WebUI(admin 只读面板)──────────────────────────────────────────
webui:
  addr: "127.0.0.1:9877"   # "off" 关闭;默认 127.0.0.1:9877
  refresh_seconds: 3       # 刷新节奏(秒);0→默认 3

# ── Admin HTTP ──────────────────────────────────────────────────────
admin:
  http_addr: "127.0.0.1:9876"  # "off" 关闭;默认 127.0.0.1:9876

# ── Admin line(运维告警转发）─────────────────────────────────────
admin_line:
  # ""=不转发(仍写审计日志)。"<source>:<chat_id>" 转发到某聊天
  # (如 "telegram:<你的tg id>" 让 bot 私信你);"agora:<inbox_scope>" 投进 agora inbox。
  # 坏值不阻塞启动,降级为不转发。
  outlet: ""

# ── Agora ───────────────────────────────────────────────────────────
agora:
  prune_interval: 1h        # dispatch_queue 清扫间隔;默认 1h
  prune_keep_days: 30       # done/cancelled 行保留天数;默认 30(failed 永留)
  # true = 路由到 agora inbox 的消息,发送者没绑账户就拒 + 引导 /accounts new
  # (docs/accounts.md §13.3)。默认 false=不限制。改后需重启。
  require_account: false

# ── Cold-memory retrieval feed(外部检索叶子 chat-retrieval-system)──
retrieval:
  enabled: false            # 默认关;叶子服务起来后再开(docs/cold-memory-retrieval.md)
  base_url: ""              # 叶子地址,如 "http://10.0.0.5:8000";enabled 时必填
  timeout_sec: 10           # 单次 HTTP 超时;默认 10
  tick_sec: 5               # drainer 轮询间隔;默认 5
  batch: 200                # 每批推送条数;默认+上限 200
  tagging_enabled: false    # Phase 1.5:推送前用 small 模型给消息打标(情绪/承诺/意图);默认关
`

// Config is the full agentbob configuration. Most of it is unused in the
// skeleton — the fields present here are the ones the entry/foundation needs.
type Config struct {
	ConfigVersion int `yaml:"_config_version"`

	Agent         AgentConfig         `yaml:"agent"`
	Display       DisplayConfig       `yaml:"display"`
	Attachments   AttachmentsConfig   `yaml:"attachments"`
	Memory        MemoryConfig        `yaml:"memory"`
	Todo          TodoConfig          `yaml:"todo"`
	Tools         ToolsConfig         `yaml:"tools"`
	Gateway       GatewayConfig       `yaml:"gateway"`
	Logging       LoggingConfig       `yaml:"logging"`
	Cleanup       CleanupConfig       `yaml:"cleanup"`
	Store         StoreConfig         `yaml:"store"`
	TurnLifecycle TurnLifecycleConfig `yaml:"turn_lifecycle"`
	Admin         AdminConfig         `yaml:"admin"`
	WebUI         WebUIConfig         `yaml:"webui"`
	Skills        SkillsConfig        `yaml:"skills"`
	Agora         AgoraConfig         `yaml:"agora"`
	AdminLine     AdminLineConfig     `yaml:"admin_line"`
	Retrieval     RetrievalConfig     `yaml:"retrieval"`
}

type DisplayConfig struct {
	// Stream controls whether replies are streamed token-by-token (terminal: live
	// text; IM: rate-limited message edits) or sent all at once when complete.
	// Turn it off if streaming misbehaves with your backend. Default: true.
	Stream *bool `yaml:"stream"`
}

// StreamEnabled reports whether streaming is on (default true if unset).
func (d DisplayConfig) StreamEnabled() bool { return d.Stream == nil || *d.Stream }

// AdminList is a "*"-or-list value for the admins setting.
//
//	admins: "*"             -> everyone (the default; also when the key is absent)
//	admins: ["123","a@b"]   -> only these user ids
//	admins: []              -> nobody (explicit)
//
// Unmarshalling is tolerant: numeric list items are accepted and stringified, so
// existing `admins: [222222222]` configs still load.
type AdminList struct {
	set bool     // an explicit value was unmarshalled
	all bool     // "*"
	ids []string // explicit ids (when !all)
}

// All reports whether this is the "*" (or unset) value; IDs returns the explicit list.
func (a AdminList) All() bool     { return !a.set || a.all }
func (a AdminList) IDs() []string { return a.ids }

// NewAdminListIDs builds an explicit-ids value programmatically.
func NewAdminListIDs(ids []string) AdminList { return AdminList{set: true, ids: ids} }

func (a *AdminList) UnmarshalYAML(value *yaml.Node) error {
	a.set, a.all, a.ids = true, false, nil
	switch value.Kind {
	case yaml.ScalarNode:
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		if strings.TrimSpace(s) == "*" {
			a.all = true
		} else if s != "" {
			a.ids = []string{s}
		}
		return nil
	case yaml.SequenceNode:
		var raw []any
		if err := value.Decode(&raw); err != nil {
			return err
		}
		for _, v := range raw {
			a.ids = append(a.ids, fmt.Sprintf("%v", v))
		}
		return nil
	default:
		return fmt.Errorf("admins must be \"*\" or a list of user ids")
	}
}

func (a AdminList) MarshalYAML() (any, error) {
	// Zero value (never unmarshalled, never set programmatically) → nil so
	// the field is fully omitted when paired with `yaml:"...,omitempty"`.
	// Post-migration AdminList is zero, and we don't want a stale `admins:`
	// line round-tripping into config.yaml after the per-source split.
	if !a.set && !a.all && len(a.ids) == 0 {
		return nil, nil
	}
	if a.All() {
		return "*", nil
	}
	if a.ids == nil {
		return []string{}, nil
	}
	return a.ids, nil
}

// ChatPolicy is the per-chat (per-group) part of the two-tier allow/deny model.
type ChatPolicy struct {
	Allowlist IDList `yaml:"allowlist"` // if non-empty, further restricts who in this chat is allowed
	Denylist  IDList `yaml:"denylist"`
}

// IDList is a list of user ids stored as strings. YAML unmarshalling accepts
// both numeric and string items — numeric items are stringified — so existing
// configs like `allowlist: [222222222]` keep loading after the migration from
// []int64, and new platforms with non-numeric ids ("U07ABC123" on Slack,
// "user@matrix.org") work without another config schema change.
type IDList []string

// Contains reports whether id is in the list.
func (l IDList) Contains(id string) bool {
	for _, x := range l {
		if x == id {
			return true
		}
	}
	return false
}

func (l *IDList) UnmarshalYAML(value *yaml.Node) error {
	*l = nil
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("expected a YAML list, got %v", value.Kind)
	}
	var raw []any
	if err := value.Decode(&raw); err != nil {
		return err
	}
	for _, v := range raw {
		*l = append(*l, fmt.Sprintf("%v", v))
	}
	return nil
}

func (l IDList) MarshalYAML() (any, error) {
	if l == nil {
		return []string{}, nil
	}
	return []string(l), nil
}

type LoggingConfig struct {
	Target string `yaml:"target"` // "stdout" | "file" | "both"
	Level  string `yaml:"level"`  // "debug" | "info" | "warn" | "error"
}

// CleanupConfig controls log rotation and periodic housekeeping. Attachment
// retention moved to attachments.retention_days / attachments.platforms.<p>.retention_days
// (per-platform; see AttachmentsConfig).
type CleanupConfig struct {
	LogsMaxSizeMB         int `yaml:"logs_max_size_mb"`        // rotate agent.log past this size; default 10
	LogsKeep              int `yaml:"logs_keep"`               // how many rotated log files to keep; default 3; capped at MaxLogsKeep
	SessionsRetentionDays int `yaml:"sessions_retention_days"` // archive cold sessions (last msg older than this) to bob_sessions_archive; default 30; 0 = never archive (keep all in hot tables)
	SweepIntervalHours    int `yaml:"sweep_interval_hours"`    // background sweep cadence; default 6
}

func (c CleanupConfig) LogsMaxBytes() int64 {
	mb := c.LogsMaxSizeMB
	if mb <= 0 {
		mb = 10
	}
	return int64(mb) << 20
}

// MaxLogsKeep is the hard cap on rotated log files (mirrors logging.maxLogRotations).
const MaxLogsKeep = 10

func (c CleanupConfig) Keep() int {
	if c.LogsKeep > MaxLogsKeep {
		return MaxLogsKeep
	}
	if c.LogsKeep <= 0 {
		return 3
	}
	return c.LogsKeep
}

// SessionRetentionDays returns the cold-session archive age in days — sessions
// whose last message is older than this are archived (NOT deleted) to
// bob_sessions_archive by the housekeeping sweep (docs/group-session-archive.md
// §3). Default 30. 0 (or negative) disables archival entirely → everything
// stays in the hot tables forever (the pre-behavior).
//
// The archive is non-destructive (payload + restorable via reply), and turn-age
// learning runs long before any session is this old, so nothing is lost.
func (c CleanupConfig) SessionRetentionDays() int {
	if c.SessionsRetentionDays <= 0 {
		return 0 // archival disabled
	}
	return c.SessionsRetentionDays
}

func (c CleanupConfig) SweepInterval() int {
	if c.SweepIntervalHours <= 0 {
		return 6
	}
	return c.SweepIntervalHours
}

// BoolPtr returns a pointer to b (handy for optional bool config fields).
func BoolPtr(b bool) *bool { return &b }

// Default returns the built-in default config.
func Default() *Config {
	return &Config{
		ConfigVersion: 1,
		Agent:         AgentConfig{Name: "bob"},
		Display:       DisplayConfig{Stream: BoolPtr(true)},
		Attachments: AttachmentsConfig{
			Enabled:       BoolPtr(true),
			MaxDownloadMB: 25,
			RetentionDays: 14,
			MaxTotalMB:    0, // unlimited by default; users with disk concerns set this
			Platforms:     map[string]PlatformAttachments{},
		},
		Gateway: GatewayConfig{
			MaxPendingBatch:               10,
			MaxConcurrentLLMTurnsPerScope: 2,
			TranscribeMaxMB:               15,
			// Legacy fields (Admins / Telegram) intentionally left at zero;
			// per-source config moved to $BOB_HOME/sources/<name>.yaml.
		},
		Logging: LoggingConfig{Target: "file", Level: "info"},
		Cleanup: CleanupConfig{LogsMaxSizeMB: 10, LogsKeep: 3, SessionsRetentionDays: 30, SweepIntervalHours: 6},
		// TurnLifecycle defaults written explicitly so `bob config init`'s
		// generated config.yaml shows the keys (admins can find + edit
		// without reading source). Values mirror the *Eff() getters; if
		// you change a default below, change the getter too.
		TurnLifecycle: TurnLifecycleConfig{
			SameToolArgsMax:          3,
			SameToolFailMax:          6,
			SameDomainFailMax:        4,
			SmartEscalationFailures:  4,
			LoopDetection:            BoolPtr(true),
			MinAgeDaysForLearning:    3,
			LearningBatchTokenCap:    8000,
			LearningSweepLimit:       50,
			ToolInsightsMaxPerTool:   20,
			InsightCompressThreshold: 12,
		},
	}
}

// TelegramTokenEnv returns the configured env var name for the Telegram bot token,
// falling back to TELEGRAM_BOT_TOKEN.
func (c *Config) TelegramTokenEnv() string {
	if c.Gateway.Telegram.TokenEnv != "" {
		return c.Gateway.Telegram.TokenEnv
	}
	return "TELEGRAM_BOT_TOKEN"
}
