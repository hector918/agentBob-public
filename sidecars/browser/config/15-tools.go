package config

import (
	"os"
	"strings"
)

// ToolsConfig — runtime configuration for the external tool slice (see
// docs/tools-design.md). Each subsection is per-tool-group.
type ToolsConfig struct {
	Filesystem FilesystemConfig `yaml:"filesystem"`
	Execution  ExecutionConfig  `yaml:"execution"`
	Web        WebConfig        `yaml:"web"`
	Browser    BrowserConfig    `yaml:"browser"`
	URLLib     URLLibConfig     `yaml:"urllib"`
	Policy     PolicyConfig     `yaml:"policy"`
}

// PolicyConfig — audit D-residual. Path / command gate applied by sensitive
// tools (terminal, execute_code, patch, read_file, search_files) on top of
// the filesystem allowlist and the tier system. Empty leaves the policy
// package's built-in defaults:
//
//	banned_paths:      /etc /var /proc /sys /root ~/.ssh ~/.gnupg ~/.aws
//	                   ~/.docker ~/.kube ~/.config/gh
//	approval_paths:    ~/Documents ~/Projects /workspace
//	banned_commands:   "sudo " / "sudo\t" / "su " / "su\t"
//	approval_commands: rm -rf / chmod / chown / curl * | sh / wget * | sh
//	                   / dd / mkfs
//
// An explicit empty list (`banned_paths: []`) disables that category —
// operator's call. Set fields to override defaults per deployment.
type PolicyConfig struct {
	BannedPaths      []string `yaml:"banned_paths,omitempty"`
	ApprovalPaths    []string `yaml:"approval_paths,omitempty"`
	BannedCommands   []string `yaml:"banned_commands,omitempty"`
	ApprovalCommands []string `yaml:"approval_commands,omitempty"`
}

// URLLibConfig — runtime config for the URL memory library. Backed by
// SQLite at $BOB_HOME/sqlite-store/urls.db. Bootstrap seeds (the 5 search engines)
// are compiled in; operators add more via $BOB_HOME/urllib.yaml.
// Slice 7.
type URLLibConfig struct {
	Enabled   *bool  `yaml:"enabled"`    // default true
	DBPath    string `yaml:"db_path"`    // "" → $BOB_HOME/sqlite-store/urls.db
	SeedsYAML string `yaml:"seeds_yaml"` // "" → $BOB_HOME/urllib.yaml
}

func (u URLLibConfig) EnabledEff() bool {
	if u.Enabled == nil {
		return true
	}
	return *u.Enabled
}

// BrowserConfig — runtime config for the browser_* tools. Powered by
// chromedp + a host Chromium binary. Per-session browser instance is
// created lazily on first browser_navigate; cleaned up by the periodic
// sweep at BrowserTTL idle.
type BrowserConfig struct {
	// Enabled flag removed. PR-1: visibility now
	// controlled by grants — admin hides the browser_* tools by flipping
	// the `tool:use:browser_*` rows for the relevant role to `off` in
	// $BOB_HOME/agora/permissions.yaml (matrix format — wildcards no
	// longer accepted; the reconcile keeps the per-tool list in sync).
	// Tool registration always happens; yaml.v3 silently ignores
	// residual `enabled:` lines.
	Headless           *bool  `yaml:"headless"`             // default true
	ExecPath           string `yaml:"exec_path"`            // path to chromium/chrome binary; "" → auto-detect
	UserDataDirRoot    string `yaml:"user_data_dir_root"`   // "" → $BOB_HOME/sandbox/<session>/chromedp
	PageTimeoutSeconds int    `yaml:"page_timeout_seconds"` // default 30 (per nav / per snapshot)
	IdleTTLSeconds     int    `yaml:"idle_ttl_seconds"`     // default 1800 (30 min) — browser closed if untouched this long
	StealthFlags       *bool  `yaml:"stealth_flags"`        // default true — disable-blink-features=AutomationControlled etc.
	// MaxConcurrentSessions caps how many chromium instances live at once
	// (each ~200 MB RAM). When Acquire would exceed the cap, the LRU
	// session is closed first. 0 → default 8. Set very high (or 1000+) to
	// effectively disable. Audit B1.
	MaxConcurrentSessions int `yaml:"max_concurrent_sessions"`
	// Humanize simulates human input on browser_click / browser_type: a
	// curved jitter-timed mouse trajectory into a randomized in-element
	// landing point, then per-keystroke typing cadence (tools/browser
	// humanize.go). Default true (nil = on); `humanize: false` restores raw
	// instant CDP dispatch. In browserd mode the flag is read from
	// browserd's own config file (actions execute there).
	Humanize *bool `yaml:"humanize"`
	// WindowWidth / WindowHeight set the chromium window (and thus the rendered
	// viewport) size. <=0 → default 1280×1280. Headless chromium otherwise comes
	// up at its 800×600 default, which is small, 4:3, triggers mobile/cramped
	// layouts on many sites, and makes the takeover screencast look wrong.
	WindowWidth  int `yaml:"window_width"`
	WindowHeight int `yaml:"window_height"`
	// TakeoverEveryNthFrame throttles the human-takeover screencast: chromium
	// emits one frame per N it renders (CDP everyNthFrame — the only fps lever
	// CDP exposes). <=0 → default 3 (~1/3 the render rate: smooth enough to
	// drive a login, a third of the bandwidth of every-frame). 1 = every frame.
	TakeoverEveryNthFrame int `yaml:"takeover_every_nth_frame"`
	// TakeoverJPEGQuality is the takeover screencast JPEG quality (1-100).
	// <=0 → default 50 (legible for forms/login at a fraction of q80 bytes).
	// Held CONSTANT across the three bandwidth tiers (high/med/low): JPEG ringing
	// on text edges is the #1 legibility killer, so the tiers save bandwidth via
	// resolution (TakeoverMaxWidth) + frame rate (TakeoverEveryNthFrame), never by
	// crushing quality — text stays crisp at every tier (ResolveCastTier).
	TakeoverJPEGQuality int `yaml:"takeover_jpeg_quality"`
	// TakeoverMaxWidth is the MED-tier server-side downscale cap (px) for the
	// takeover screencast: chromium scales each frame to fit within this box BEFORE
	// JPEG-encoding (CDP maxWidth/maxHeight, aspect preserved, never upscaled). This
	// DECOUPLES the takeover capture resolution from the model-facing window size
	// (WindowSizeEff, kept high so the model's screenshots stay detailed). <=0 →
	// default 1024. The high tier omits the cap (native window size); the low tier
	// uses 3/4 of this (768 by default). See ResolveCastTier.
	TakeoverMaxWidth int `yaml:"takeover_max_width"`
	// MaxTakeoverStreams is the admission cap: the max number of concurrent
	// human-takeover screencast streams browserd will serve at once. A connect
	// beyond it is REFUSED (the in-flight streams are never degraded to make room),
	// giving the box a deterministic upper bound on takeover load so a burst of
	// handoffs can't melt it — the rest of the agent is already isolated in its own
	// container, so this protects browserd's own CPU/bandwidth. <=0 → default 4.
	MaxTakeoverStreams int `yaml:"max_takeover_streams"`
	// BrowserdURL — base URL of the external browserd control-plane service
	// (docs/browserd.md P1), e.g. "http://browserd:8377". Empty (default) =
	// today's in-process mode, behaviour unchanged. Non-empty = the 13
	// browser_* tools become thin HTTP shells over browserd: chromium runs in
	// the browserd container only, this process spawns none. Selection
	// happens once at startup assembly. Human takeover needs NO extra config
	// (docs/browserd.md §4): bob's webui proxies screencast/input to
	// browserd's takeover face, assumed at this URL's host on the default
	// takeover port 8378 — keep BROWSERD_TAKEOVER_LISTEN on its default port
	// (a custom port breaks the proxy's address derivation). Because that
	// derivation keeps only scheme+host (any path prefix is dropped) and the
	// takeover face speaks plain HTTP, this must be a BARE http address —
	// `http://<ip-or-hostname>:8377` — never a TLS reverse proxy or a
	// path-prefixed route (an https scheme would carry over to the derived
	// :8378 address and fail the handshake).
	BrowserdURL string `yaml:"browserd_url"`
	// APIKey — OPTIONAL shared secret for browserd (A1). browserd carries no
	// transport security (docs/browserd.md §6 — run it only inside a trusted
	// subnet / encrypted tunnel, never publish its ports). This adds an app-layer
	// check ON TOP: when set, every request on BOTH the control plane AND the
	// takeover face must carry `Authorization: Bearer <key>` or it is rejected 401;
	// when empty (default) both stay open and browserd logs a startup WARN. (The
	// takeover face used to carry a per-takeover ticket; that was retired — browserd
	// is a dumb service authed only by this key, per-scope takeover authorization
	// stays in bob.) Source: $BROWSERD_API_KEY takes precedence over this field (see
	// BrowserdAPIKeyEff) so the secret can stay out of config.yaml.
	APIKey string `yaml:"api_key"`
}

// TakeoverEveryNthFrameEff returns the screencast frame-skip divisor, defaulting
// to 3 when unset/invalid.
func (b BrowserConfig) TakeoverEveryNthFrameEff() int {
	if b.TakeoverEveryNthFrame <= 0 {
		return 3
	}
	return b.TakeoverEveryNthFrame
}

// TakeoverJPEGQualityEff returns the screencast JPEG quality, defaulting to 50
// when unset and clamping to the valid 1-100 range.
func (b BrowserConfig) TakeoverJPEGQualityEff() int {
	if b.TakeoverJPEGQuality <= 0 {
		return 50
	}
	if b.TakeoverJPEGQuality > 100 {
		return 100
	}
	return b.TakeoverJPEGQuality
}

// TakeoverMaxWidthEff returns the MED-tier downscale cap, defaulting to 1024 when
// unset/invalid.
func (b BrowserConfig) TakeoverMaxWidthEff() int {
	if b.TakeoverMaxWidth <= 0 {
		return 1024
	}
	return b.TakeoverMaxWidth
}

// MaxTakeoverStreamsEff returns the concurrent-takeover admission cap, defaulting
// to 4 when unset/invalid.
func (b BrowserConfig) MaxTakeoverStreamsEff() int {
	if b.MaxTakeoverStreams <= 0 {
		return 4
	}
	return b.MaxTakeoverStreams
}

// ResolveCastTier maps a requested bandwidth tier ("high"|"med"|"low"; anything
// else → "med") plus the CURRENT number of active takeover streams to the concrete
// screencast params. Two clamps combine, lower-always-wins:
//
//   - the operator/user's requested tier (the webui button — a REQUEST, not a
//     guarantee), and
//   - a load ceiling the backend imposes to protect the box: >=4 streams → "low",
//     >=3 → "med", else no ceiling. effective = min(requested, ceiling).
//
// Quality is constant (TakeoverJPEGQualityEff) so text stays crisp; the tiers move
// only resolution + frame rate:
//
//	high → native size (no cap),    everyNth-1 (min 1)
//	med  → TakeoverMaxWidthEff,     everyNth
//	low  → 3/4 * TakeoverMaxWidthEff, everyNth+2
//
// maxW==0 means "no downscale" (native window size); maxH always mirrors maxW so
// the cap is a square box (CDP preserves aspect within it, for any window shape).
//
// effTier is the NAME ("high"|"med"|"low"|"slow") of the tier actually applied after
// the load clamp — reported back to the webui so the quality button faithfully shows
// what is running (which may be below the requested tier when the box is busy).
//
// "slow" is a MANUAL-only mode: med resolution but ~1 frame / 5s. The 5s wall-clock
// gate (browserd — see TierMinIntervalMs) is the SOLE bandwidth limiter; capture runs
// at everyNth=1 so any post-click change is grabbed promptly and the gate flushes the
// LATEST frame each tick (everyNth>1 would starve capture on quiet pages — see the const
// note below). It is OUTSIDE the high/med/low ladder, so the load ceiling never
// auto-selects OR clamps it — it is already minimal traffic, so a user's request is
// honored as-is regardless of load.
const (
	slowTierEveryNth   = 1    // capture every frame; the 5s wall-clock gate (below) is the SOLE
	//                          bandwidth limiter. everyNth>1 starves capture on quiet pages:
	//                          chromium parks the compositor when idle, so a post-click change
	//                          produces only a few frames — never enough to reach a high everyNth,
	//                          so the change is never captured and the 5s gate has nothing to flush.
	slowTierIntervalMs = 5000 // "slow": forward at most one (latest) frame per this interval
)

func (b BrowserConfig) ResolveCastTier(tier string, activeStreams int) (effTier string, quality, everyNth, maxW, maxH int) {
	quality = b.TakeoverJPEGQualityEff()
	if tier == "slow" {
		med := b.TakeoverMaxWidthEff()
		return "slow", quality, slowTierEveryNth, med, med
	}
	rank := map[string]int{"low": 1, "med": 2, "high": 3}
	name := map[int]string{1: "low", 2: "med", 3: "high"}
	req, ok := rank[tier]
	if !ok {
		req = rank["med"] // unknown/empty → the default tier
	}
	ceiling := rank["high"]
	if activeStreams >= 4 {
		ceiling = rank["low"]
	} else if activeStreams >= 3 {
		ceiling = rank["med"]
	}
	eff := req
	if ceiling < eff {
		eff = ceiling
	}
	effTier = name[eff]

	base := b.TakeoverEveryNthFrameEff()
	med := b.TakeoverMaxWidthEff()
	switch eff {
	case rank["high"]:
		everyNth = base - 1
		if everyNth < 1 {
			everyNth = 1
		}
		maxW = 0 // native — no downscale
	case rank["low"]:
		everyNth = base + 2
		maxW = med * 3 / 4
		if maxW < 320 {
			maxW = 320 // floor: at tiny configured widths med*3/4 can int-divide to 0,
			// which castAcquire reads as "native, no downscale" — inverting low into the
			// HIGHEST-resolution tier. Keep a legible-minimum cap instead.
		}
	default: // med
		everyNth = base
		maxW = med
	}
	maxH = maxW
	return effTier, quality, everyNth, maxW, maxH
}

// TierMinIntervalMs is the minimum wall-clock gap (ms) browserd enforces between
// forwarded frames for an effective tier — the lever for "slow" (med resolution but
// ~1 frame / 5s) that everyNth (render-rate-coupled) cannot express. 0 = no gate
// (the high/med/low ladder streams at chromium's everyNth cadence).
func (b BrowserConfig) TierMinIntervalMs(tier string) int {
	if tier == "slow" {
		return slowTierIntervalMs
	}
	return 0
}

// BrowserdAPIKeyEff resolves the browserd control-plane API key: the
// $BROWSERD_API_KEY env var wins (lets the secret live outside config.yaml),
// otherwise tools.browser.api_key. Empty (both unset) = no control-plane auth.
func (b BrowserConfig) BrowserdAPIKeyEff() string {
	if v := strings.TrimSpace(os.Getenv("BROWSERD_API_KEY")); v != "" {
		return v
	}
	return strings.TrimSpace(b.APIKey)
}

func (b BrowserConfig) HeadlessEff() bool {
	if b.Headless == nil {
		return true
	}
	return *b.Headless
}

// WindowSizeEff returns the chromium window size, defaulting each dimension to
// 1280 when unset/invalid.
func (b BrowserConfig) WindowSizeEff() (w, h int) {
	w, h = b.WindowWidth, b.WindowHeight
	if w <= 0 {
		w = 1280
	}
	if h <= 0 {
		h = 1280
	}
	return w, h
}

func (b BrowserConfig) PageTimeoutSecondsEff() int {
	if b.PageTimeoutSeconds <= 0 {
		return 30
	}
	return b.PageTimeoutSeconds
}

func (b BrowserConfig) IdleTTLSecondsEff() int {
	if b.IdleTTLSeconds <= 0 {
		return 30 * 60
	}
	return b.IdleTTLSeconds
}

func (b BrowserConfig) MaxConcurrentSessionsEff() int {
	if b.MaxConcurrentSessions <= 0 {
		return 8
	}
	return b.MaxConcurrentSessions
}

func (b BrowserConfig) StealthFlagsEff() bool {
	if b.StealthFlags == nil {
		return true
	}
	return *b.StealthFlags
}

func (b BrowserConfig) HumanizeEff() bool {
	if b.Humanize == nil {
		return true
	}
	return *b.Humanize
}

// WebConfig — backend selection + caps for web_search + scrapling.
//
// web_search needs a backend with a JSON API. v1 ships SearXNG only —
// either self-hosted (recommended; you control rate limits + privacy) or
// a public instance (flaky). Set Backend="searxng" + BackendURL="https://...".
// Other backends (Tavily, Brave) reserved for later — they need API keys
// via APIKeyEnv.
//
// FetchTimeoutSeconds bounds every request: it is the per-engine deadline
// for each web_search SERP fetch (engines fan out concurrently, so it also
// caps web_search's total wall time) and the scrapling subprocess
// wall-time. Default 15.
type WebConfig struct {
	Backend             string `yaml:"backend"`               // "searxng" (default) | "tavily" | "brave" — last two TODO
	BackendURL          string `yaml:"backend_url"`           // SearXNG: https://searx.example.com  ;  Tavily: usually empty
	APIKeyEnv           string `yaml:"api_key_env"`           // env var name with the backend's API key (Tavily/Brave)
	MaxResults          int    `yaml:"max_results"`           // default 10
	FetchTimeoutSeconds int    `yaml:"fetch_timeout_seconds"` // default 15
	// Enabled flag removed — see BrowserConfig comment.
}

func (w WebConfig) BackendEff() string {
	if w.Backend == "" {
		return "searxng"
	}
	return w.Backend
}

func (w WebConfig) MaxResultsEff() int {
	if w.MaxResults <= 0 {
		return 10
	}
	if w.MaxResults > 50 {
		return 50
	}
	return w.MaxResults
}

func (w WebConfig) FetchTimeoutSecondsEff() int {
	if w.FetchTimeoutSeconds <= 0 {
		return 15
	}
	return w.FetchTimeoutSeconds
}

// ExecutionConfig — runtime caps for execute_code and terminal. The two
// share spawn/cancel/output-cap infrastructure; this config governs both.
//
// Native mode (default): subprocess runs as bob's UID with PATH=/usr/bin
// + sandbox dirs as HOME/TMPDIR. Process group on spawn (Setpgid:true) so
// SIGTERM/SIGKILL on cancel takes the whole tree.
//
// Docker mode (Docker.Enabled=true): NOT YET IMPLEMENTED. Config fields are
// reserved for the future docker-run path. When Docker.Enabled is true and
// docker mode isn't built, startup logs a warning and falls back to native.
type ExecutionConfig struct {
	// The subprocess cwd is always file.SessionSandbox (the per-session dir
	// under FilesystemConfig.SandboxRoot). The former work_dir key was parsed
	// but never read anywhere, so it was removed (B34) rather than
	// keep silently ignoring an operator's setting.
	MaxSeconds     int `yaml:"max_seconds"`      // default 30
	MaxOutputBytes int `yaml:"max_output_bytes"` // default 65536 (64 KB per stdout, per stderr)
	// Enabled flag removed — see BrowserConfig comment.
	// Path overrides the PATH env var the subprocess sees. Empty → use
	// the conservative default `/usr/local/bin:/usr/bin:/bin`. Set
	// (TD7 fix) when running natively with pyenv / homebrew
	// / `~/bin` etc. so python3 / git / npm resolve correctly; under
	// docker the default fits the base image and admin leaves this
	// blank.
	Path   string       `yaml:"path"`
	Docker DockerConfig `yaml:"docker"`
	// AllowNativeInGateway is the explicit acknowledgement an operator must
	// set to run `bob gateway` (multi-user IM) on the host without docker.
	// Without it, gateway refuses to start when execute_code / terminal are
	// enabled and we're not in a container — because at that point any user
	// the bot accepts can ask the model to spawn a shell as bob's UID.
	// Default false. `bob` (local REPL) ignores this — it's only the user
	// themselves at the terminal, not arbitrary IM users.
	AllowNativeInGateway bool `yaml:"allow_native_in_gateway"`
}

// DockerConfig — reserved for the docker-run subprocess mode (not yet
// implemented). When Enabled=true the future code path will run each
// subprocess via 'docker run --rm' inside Image, with --network/--memory/--cpus.
type DockerConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Image    string `yaml:"image"`     // default "python:3.12-slim"
	Network  string `yaml:"network"`   // default "bridge"
	MemoryMB int    `yaml:"memory_mb"` // default 256
	CPUs     string `yaml:"cpus"`      // default "1.0"
}

func (e ExecutionConfig) MaxSecondsEff() int {
	if e.MaxSeconds <= 0 {
		return 30
	}
	return e.MaxSeconds
}

func (e ExecutionConfig) MaxOutputBytesEff() int {
	if e.MaxOutputBytes <= 0 {
		return 64 * 1024
	}
	return e.MaxOutputBytes
}

// PathEff returns the PATH env var subprocesses should see. Empty
// config falls back to the conservative default
// `/usr/local/bin:/usr/bin:/bin` (fits the docker base image; admin
// running natively with pyenv / homebrew / ~/bin should set
// tools.execution.path explicitly). TD7 fix.
func (e ExecutionConfig) PathEff() string {
	if e.Path == "" {
		return "/usr/local/bin:/usr/bin:/bin"
	}
	return e.Path
}

// FilesystemConfig — sandbox + path-allowlist for read_file / search_files /
// patch (and any future fs tool). Container deployments usually leave
// ReadRoots/ReadWriteRoots set to whatever's mounted under /work; native
// dev keeps them empty so tools only see SandboxRoot.
//
// SandboxRoot defaults to $BOB_HOME/sandbox; per-session subdirs are
// auto-created (and auto-cleaned by the periodic sweep at 24h TTL).
type FilesystemConfig struct {
	SandboxRoot    string   `yaml:"sandbox_root"`     // default: $BOB_HOME/sandbox
	ReadRoots      []string `yaml:"read_roots"`       // default: empty
	ReadWriteRoots []string `yaml:"read_write_roots"` // default: empty
	MaxReadBytes   int      `yaml:"max_read_bytes"`   // default: 1 MB
	SearchSkipDirs []string `yaml:"search_skip_dirs"` // default: noise dirs (see below)
}

// MaxReadBytesEff — default 1 MB.
func (f FilesystemConfig) MaxReadBytesEff() int {
	if f.MaxReadBytes <= 0 {
		return 1 << 20
	}
	return f.MaxReadBytes
}

// SearchSkipDirsEff — default skips well-known noise dirs that bloat search
// without ever holding what the model is looking for.
func (f FilesystemConfig) SearchSkipDirsEff() []string {
	if len(f.SearchSkipDirs) > 0 {
		return f.SearchSkipDirs
	}
	return []string{".git", "node_modules", "__pycache__", "target", ".venv", "venv", "vendor", "dist", "build", ".next", ".cache"}
}

// MemoryConfig — runtime caps + behavior for the memory subsystem.
//
// chain_max_bytes: hard cap on the bytes Memory.ReadChain returns. Once a
// per-turn injection exceeds it, the chain is truncated root-first-keep
// (constitution / platform get priority; user/group truncate last) and an
// slog.Error fires so operators notice the memory needs consolidation.
//
//: scan_for_injection knob removed — it only gated the
// memory_save tool's pre-write defense, and the tool was deleted with
// the rest of the memory write path.
//: snapshot_cache_size removed — the per-session frozen snapshot
// stopped mattering once injection moved to fresh-disk RenderLayer; ReadChain
// now reads disk every call (see memory.FileMemory doc).
type MemoryConfig struct {
	ChainMaxBytes int `yaml:"chain_max_bytes"`
	// NamespaceByBot inserts `bots/<bot_id>/` into memory layer paths. Off by
	// default → legacy paths (single-bot deployments unchanged). Turn on for
	// single-process multi-bot setups to isolate per-bot memory trees.
	NamespaceByBot *bool `yaml:"namespace_by_bot"`
}

// effective getters apply defaults when fields are zero / nil.

func (m MemoryConfig) ChainMaxBytesEff() int {
	if m.ChainMaxBytes <= 0 {
		return 8 * 1024
	}
	return m.ChainMaxBytes
}

func (m MemoryConfig) NamespaceByBotEff() bool {
	if m.NamespaceByBot == nil {
		return false
	}
	return *m.NamespaceByBot
}

// TodoConfig — runtime knobs for the per-session todo subsystem (#101).
//
// default_mode: which verification regime new sessions start in. When
// unset, DefaultModeEff() falls back to "none" (see its comment) — ordinary
// conversations are honor-system unless the operator opts into review.
//   - manual: completed → pending_review, waits for human button click
//   - auto: judge model batch-verifies every JudgeAfterCompletions completions
//   - none (default when unset): pure honor system (model says done = done)
//
// judge_after_completions: trigger threshold for auto mode. The judge
// model fires when count(completed && !verified && JudgedAt==0) reaches
// this. 0 → default 3.
//
// stuck_after_fails: how many judge / human rejections before a single
// todo gets escalated=true (model is told to ask the user instead of
// retrying). 0 → default 3.
// DefaultTodoMode is THE single fallback for a new session's verification
// regime when config.yaml's todo.default_mode is unset. config.yaml is the
// source of truth (the seed sets it explicitly); every layer references this
// const so no second hardcoded default can disagree. "none" = ordinary
// conversations are honor-system (no review buttons / judge ceremony) unless
// the operator opts in. agora sessions are exempt — they get TodoModeAuto via
// the source==internal override in Manager.ApplyDefaultTodoMode.
const DefaultTodoMode = "none"

type TodoConfig struct {
	DefaultMode           string `yaml:"default_mode"`
	JudgeAfterCompletions int    `yaml:"judge_after_completions"`
	StuckAfterFails       int    `yaml:"stuck_after_fails"`
}

// DefaultModeEff applies the default + validates: an unset or unrecognised
// mode falls back to DefaultTodoMode (config.yaml is the source of truth).
func (t TodoConfig) DefaultModeEff() string {
	switch t.DefaultMode {
	case "manual", "auto", "none":
		return t.DefaultMode
	}
	return DefaultTodoMode
}

func (t TodoConfig) JudgeAfterCompletionsEff() int {
	if t.JudgeAfterCompletions <= 0 {
		return 3
	}
	return t.JudgeAfterCompletions
}

func (t TodoConfig) StuckAfterFailsEff() int {
	if t.StuckAfterFails <= 0 {
		return 3
	}
	return t.StuckAfterFails
}
