// SSRF guard (two layers): (1) CheckURLHost pre-validates the INITIAL host before
// launching the subprocess; (2) the subprocess is pointed at a deny-private egress
// proxy (egressProxyURL, 93-denyproxy.go) via HTTP(S)_PROXY env, so every request
// AND every in-browser redirect hop is resolved + checked against isBlockedIP before
// it is dialed — closing the fetch/stealthy-fetch post-redirect hole that the CLI's
// missing `--no-follow-redirects` (playwright full-redirect semantics) left open.
// get / post additionally disable redirects at the CLI. The proxy must be honored by
// the subprocess (httpx for get/post; chromium env-proxy for fetch/stealthy) —
// verify on the deploy box (T3), where scrapling actually runs.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// defaultFetchTimeoutSec is the per-call wall-clock budget for the cheap modes
// (get / post). Trunk has no config.WebConfig section yet, so it's inlined at
// the skeleton default (15s). The browser-backed modes lift this in
// modeWallTimeSeconds.
const defaultFetchTimeoutSec = 15

// This file holds the scrapling CLI subprocess machinery (scrapeFetch and its
// helpers). The model-facing tool surface (description + parameter schema) lives in
// 94-websearch.go (webDescription / webParams) — the merged `web` tool (type webTool)
// dispatches its `url` mode to scrapeFetch here and its `query` mode to the search
// path there.
//
//   - `extract get`            (HTTP, curl_cffi TLS impersonation)
//   - `extract fetch`          (playwright DynamicFetcher — JS rendering)
//   - `extract stealthy-fetch` (playwright StealthyFetcher — Cloudflare bypass)
//   - `extract post`           (HTTP POST/PUT/DELETE)
//
// The browser-backed modes (fetch / stealthy-fetch) require playwright's chromium
// (the Dockerfile installs it). When the `scrapling` CLI is absent, scrapeFetch
// returns a "CLI not found" error so the merged tool degrades gracefully (plain
// engine search still works) rather than vanishing from the catalog.

type scrapeArgs struct {
	URL             string          `json:"url"`
	Mode            string          `json:"mode"`
	Format          string          `json:"format"`
	CSSSelector     string          `json:"css_selector"`
	Impersonate     string          `json:"impersonate"`
	TimeoutMs       int             `json:"timeout_ms"`
	SolveCloudflare *bool           `json:"solve_cloudflare"`
	BodyJSON        json.RawMessage `json:"body_json"`
	Method          string          `json:"method"`
}

// scrapeOutcome is a successful scrapeFetch result: the fetched content (capped to
// defaultMaxChars) plus size metadata. scrapeFetch returns this on success and
// ("", hint) on failure — it owns the subprocess machinery but NOT the urllib
// Record or the response envelope (those are the caller's concern).
//
// ToolFailure is the one field that is meaningful on the FAILURE return: it says the
// fetch never got as far as asking the target — our own side broke (scrapling missing
// or crashed on import, playwright chromium absent, sandbox/tempfile, a refused
// argument) — as opposed to the target answering badly (timeout, DNS, bot wall). The
// search path needs the distinction so an engine is never blamed, and cooled down, for
// a broken local install (docs/web-fleet.md §13.3).
type scrapeOutcome struct {
	Content     string
	ByteCount   int
	Truncated   bool
	ToolFailure bool
}

// toolFail builds the failure return for a bob-side/env-side error — the engine
// breaker must not count these against the host that was being fetched.
func toolFail(msg, hint string) (scrapeOutcome, string, string) {
	return scrapeOutcome{ToolFailure: true}, msg, hint
}

// scrapeModeOrDefault / scrapeFormatOrDefault apply the defaults the merged tool's
// url-mode envelope and scrapeFetch share, so both agree on the effective values.
func scrapeModeOrDefault(m string) string {
	if m = strings.TrimSpace(m); m != "" {
		return m
	}
	return "get"
}

func scrapeFormatOrDefault(f string) string {
	if f == "" {
		return "markdown"
	}
	return f
}

// scrapeFetch runs the scrapling CLI subprocess for ONE request under sid's
// sandbox and returns the fetched content (capped), or ("", errMsg, hint) on
// failure. It owns the full subprocess machinery — SSRF host pre-check, per-sid
// sandbox, sandboxed HOME + egress deny-proxy, process-group kill, per-mode wall
// budget, output read, and bot-protection / SPA detection — so BOTH the scrapling
// tool (url path) and the shared webFetch ladder reuse identical, audited behavior.
// It does NOT urllib.Record or build the response envelope (the callers do that,
// differently). p is validated here (callers may build it programmatically).
// maxChars caps the returned body (0 → defaultMaxChars). SERP fetches pass a large
// cap so the FULL page reaches serpClean (the caller then caps the cleaned link list),
// matching the in-process serpGet path; page reads pass 0 (the 16KB default).
func (s webTool) scrapeFetch(ctx context.Context, sid string, p *scrapeArgs, maxChars int) (out scrapeOutcome, errMsg, hint string) {
	target := strings.TrimSpace(p.URL)
	if target == "" {
		return toolFail("scrapling: url is required", "")
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return toolFail("url must start with http:// or https://", "")
	}

	// SSRF guard. Scrapling runs as a subprocess with its own network stack, so
	// an in-process DialContext doesn't reach it. Pre-check the URL host here —
	// if it resolves to a blocked range (loopback, RFC1918, link-local, cloud
	// metadata IP), refuse before launching the subprocess.
	if err := CheckURLHost(hostFromTarget(target)); err != nil {
		return toolFail(err.Error(), "")
	}

	if _, err := osexec.LookPath("scrapling"); err != nil {
		return toolFail("scrapling CLI not found — pip install scrapling on this host or rebuild the docker image", "")
	}

	mode := scrapeModeOrDefault(p.Mode)
	switch mode {
	case "get", "fetch", "stealthy-fetch", "post":
		// OK
	default:
		return toolFail("mode must be one of: get, fetch, stealthy-fetch, post", "")
	}

	format := scrapeFormatOrDefault(p.Format)
	ext := map[string]string{"markdown": "md", "text": "txt", "html": "html"}[format]
	if ext == "" {
		return toolFail("format must be markdown, text, or html", "")
	}

	sandbox, err := s.ensureSandbox(sid)
	if err != nil {
		return toolFail("sandbox: "+err.Error(), "")
	}
	// Unique temp file: one search now fires many concurrent scrapeFetch calls under
	// the SAME sid sandbox (render/stealth engines + auto-fetch pages), so a
	// timestamp-keyed name could collide → cross-read / double-remove. CreateTemp
	// guarantees a distinct path per call.
	tmpf, err := os.CreateTemp(sandbox, "scrapling-*."+ext)
	if err != nil {
		return toolFail("scrapling: temp file: "+err.Error(), "")
	}
	tmpPath := tmpf.Name()
	_ = tmpf.Close() // scrapling (re)writes this path; we only needed a unique name
	defer func() {
		if rerr := os.Remove(tmpPath); rerr != nil && !os.IsNotExist(rerr) {
			slog.Warn("scrape: temp cleanup failed (may accumulate on disk)", "path", tmpPath, "err", rerr)
		}
	}()

	wallSec := modeWallTimeSeconds(mode, defaultFetchTimeoutSec)

	cliArgs, timeoutClamped, argErr := buildScrapeCLIArgs(mode, target, tmpPath, wallSec, p)
	if argErr != nil {
		return toolFail(argErr.Error(), "")
	}
	if timeoutClamped {
		slog.Info("scrapling timeout_ms clamped to wall budget",
			"mode", mode, "url", target, "requested_ms", p.TimeoutMs, "wall_sec", wallSec)
	}

	cctx, cancel := context.WithTimeout(ctx, time.Duration(wallSec)*time.Second)
	defer cancel()

	cmd := osexec.CommandContext(cctx, "scrapling", cliArgs...)
	cmd.Dir = sandbox
	// Process-group kill: fetch / stealthy-fetch spawn playwright node +
	// chromium grandchildren. Setpgid puts the whole tree in one group; Cancel
	// kills the group (negative pid); WaitDelay bounds the pipe-EOF wait in case
	// a descendant escaped the group. Group kill still surfaces as "signal:
	// killed", so the timeout-hint branch below keeps matching.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	// Point HOME at a dedicated dir under the sandbox parent rather than the
	// host's $HOME, so scrapling's python subprocess can't read ~/.ssh/id_rsa,
	// ~/.aws/credentials, etc — same sandbox boundary the sibling exec tools
	// enforce.
	scrapHome := filepath.Join(filepath.Dir(sandbox), "_scrapling-home")
	if err := os.MkdirAll(scrapHome, 0o755); err != nil {
		return toolFail("scrapling: mkdir HOME: "+err.Error(), "")
	}
	// Bump its mtime on every use (same guard as ensureSandbox's per-sid dir):
	// _scrapling-home lives inside by_sessions_scope, which sweepScraplingSandboxes
	// RemoveAll's by mtime with no name filter — without this its mtime freezes and
	// the shared HOME is deleted after the idle TTL (B36).
	now := time.Now()
	_ = os.Chtimes(scrapHome, now, now)
	parentPATH := os.Getenv("PATH")
	if parentPATH == "" {
		parentPATH = "/usr/local/bin:/usr/bin:/bin"
	}
	env := []string{
		"PATH=" + parentPATH,
		"HOME=" + scrapHome,
		"TMPDIR=" + sandbox,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	// PLAYWRIGHT_BROWSERS_PATH: fetch / stealthy-fetch launch chromium via
	// playwright (and patchright for stealth); both honor this env var to locate
	// the browser binary. PYTHONPATH / PYTHONUSERBASE / VIRTUAL_ENV are
	// pass-through so scrapling finds its own install — none expose secrets.
	// Sensitive vars (SSH_AUTH_SOCK, AWS_*, etc) are NOT in the allowlist.
	for _, k := range []string{"PYTHONPATH", "PYTHONUSERBASE", "VIRTUAL_ENV", "PLAYWRIGHT_BROWSERS_PATH"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	// SSRF egress guard for redirect hops: point the subprocess (httpx for get/post,
	// chromium for fetch/stealthy) at bob's deny-private proxy, so every request AND
	// every in-browser redirect is resolved + checked against isBlockedIP before the
	// hop is dialed. CheckURLHost above already covers the initial host; this closes
	// the post-redirect hole. Injected ONLY here (the subprocess env) — bob's own
	// llama/pg traffic never uses it. Empty NO_PROXY so nothing bypasses the proxy.
	if px := egressProxyURL(); px != "" {
		env = append(env,
			"HTTP_PROXY="+px, "HTTPS_PROXY="+px,
			"http_proxy="+px, "https_proxy="+px,
			"NO_PROXY=", "no_proxy=",
		)
	}
	cmd.Env = env

	// Bound the subprocess output. CombinedOutput has no cap and a runaway
	// scrapling spewing tracebacks can OOM the bob process. Cap at 2MB — real
	// scrapling output is <100KB; the page body is written to tmpPath separately.
	const stdoutCap = 2 * 1024 * 1024
	capBuf := newScrapeCapBuf(stdoutCap)
	cmd.Stdout = capBuf
	cmd.Stderr = capBuf
	runErr := cmd.Run()
	combinedOut := capBuf.Bytes()
	if runErr != nil {
		eMsg, h, toolSide := scrapeRunError(cctx, runErr, combinedOut, mode, target, wallSec, timeoutClamped, p.TimeoutMs)
		if toolSide {
			return toolFail(eMsg, h)
		}
		return out, eMsg, h
	}

	body, readErr := os.ReadFile(tmpPath)
	if readErr != nil {
		return toolFail(fmt.Sprintf("read scrapling output %s: %v", tmpPath, readErr), "")
	}
	capChars := maxChars
	if capChars <= 0 {
		capChars = defaultMaxChars
	}
	// Scrub raw binary BEFORE it enters the model context: a scrape can pick up non-text
	// bytes (e.g. an embedded/served WebP image), which the model then echoes into a tool
	// call — corrupting the tool-call JSON and 500-ing a strict server (llama.cpp / the main
	// qwen smart model). scrubToText drops invalid UTF-8 + control chars, keeping real text
	// (incl. CJK/emoji). A genuinely-binary resource collapses to near-empty rather than
	// poisoning the turn.
	capped, truncated, total := applyTextCap(scrubToText(string(body)), capChars)

	// Bot-protection / SPA detection — runs against the markdown/text/html the
	// model would actually receive (post-markdownify).
	if eMsg, h := detectBotProtection(capped, mode); eMsg != "" {
		slog.Info("scrapling detected bot-protection / SPA shell",
			"url", target, "mode", mode, "hint", h, "body_len", len(strings.TrimSpace(capped)))
		return out, eMsg, h
	}

	return scrapeOutcome{Content: capped, ByteCount: total, Truncated: truncated}, "", ""
}

// scrubToText drops bytes that would corrupt the model context or a downstream tool-call
// JSON: invalid UTF-8 (raw binary — e.g. an embedded WebP image a scrape picked up) and
// control chars (keeping \t \n \r). Valid UTF-8 text, including CJK/emoji, is preserved; a
// resource that's actually binary collapses to near-empty instead of feeding the model bytes
// it can echo into a tool call (which 500s a strict tool-call parser like llama.cpp).
func scrubToText(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1 // drop other control chars
		}
		return r
	}, s)
}

// scrapeRunError maps a non-nil subprocess error into (errMsg, hint, toolSide):
// wall-clock timeout (per-mode escalation hint), DNS failure, missing playwright
// chromium, or a generic exit.
//
// toolSide is true only where the output PROVES our own side is broken rather than the
// target refusing us — today that is the missing/unlaunchable playwright browser. A
// generic non-zero exit stays false on purpose: scrapling raises for remote reasons too
// (connection reset, navigation error), and a traceback alone can't tell "our install is
// broken" from "this site made the fetcher blow up", so guessing there would silence the
// breaker for engines that really are failing. The blanket install-is-broken case is
// caught where it is actually visible — harvestEngines sees every engine come back with
// the same error text (docs/web-fleet.md §13.3).
func scrapeRunError(cctx context.Context, runErr error, combinedOut []byte, mode, target string, wallSec int, timeoutClamped bool, requestedMs int) (errMsg, hint string, toolSide bool) {
	emsg := fmt.Sprintf("scrapling exited %v: %s", runErr, snippet(combinedOut))
	out := string(combinedOut)
	// Wall-clock timeout (cctx fires SIGKILL). Common on stealthy-fetch / fetch
	// when cloudflare turnstile or a heavy JS page exceeds the per-mode budget.
	if errors.Is(cctx.Err(), context.DeadlineExceeded) ||
		strings.Contains(runErr.Error(), "signal: killed") {
		if timeoutClamped {
			h := fmt.Sprintf("你请求的 timeout_ms 超过了 mode=%s 的墙钟预算（约 %ds）— "+
				"去掉 timeout_ms（或设到 %d 以内）后重试；这不是渲染太慢，换 mode 无用",
				mode, wallSec, (wallSec-wallTimeoutMarginSeconds)*1000)
			slog.Info("scrapling wall-time timeout (clamped request)",
				"mode", mode, "url", target, "wall_sec", wallSec, "requested_ms", requestedMs)
			return fmt.Sprintf("scrapling %s 超时 (wall %ds，你的 timeout_ms 超预算被钳制)", mode, wallSec), h, false
		}
		var h string
		switch mode {
		case "stealthy-fetch":
			h = fmt.Sprintf("scrapling stealthy-fetch %ds 超时（cloudflare turnstile bypass 太慢）— "+
				"**禁止**回 mode=get 或试 mode=fetch（vanilla playwright 同样过不了 cloudflare）— "+
				"**立刻** browser_navigate（场景 4 互动栈，chromium + 真 cookies）", wallSec)
		case "fetch":
			h = fmt.Sprintf("scrapling fetch %ds 超时（playwright 渲染太慢）— "+
				"**禁止**回 mode=get — 先试 mode=stealthy-fetch（带 stealth）；还是不行再 browser_navigate", wallSec)
		default:
			h = fmt.Sprintf("scrapling 超时 %ds — 改用 browser_navigate", wallSec)
		}
		slog.Info("scrapling wall-time timeout", "mode", mode, "url", target, "wall_sec", wallSec)
		return fmt.Sprintf("scrapling %s 超时 (wall %ds)", mode, wallSec), h, false
	}
	if isDNSClass(out) {
		return emsg, dnsHint, false
	}
	// Browser-backed modes can fail because playwright chromium isn't installed.
	// "Executable doesn't exist" / "BrowserType.launch" mean the browser never came
	// up — our install, not the target — so those are flagged toolSide; the looser
	// playwright._impl match keeps the hint but can also come from a page-level
	// navigation error, so it does not claim to be our fault.
	if mode != "get" && mode != "post" &&
		(strings.Contains(out, "Executable doesn't exist") ||
			strings.Contains(out, "playwright._impl") ||
			strings.Contains(out, "BrowserType.launch")) {
		broken := strings.Contains(out, "Executable doesn't exist") || strings.Contains(out, "BrowserType.launch")
		return emsg, "playwright chromium 没装好；docker 重 build 应该自动装；或退回 mode=get", broken
	}
	return emsg, "", false
}

// ensureSandbox returns (and creates) the per-session scratch dir under the
// injected sandbox root, keyed by the turn's sid. The scrapling temp output +
// subprocess cwd / HOME / TMPDIR all live here; the temp file is removed right
// after it's read.
func (s webTool) ensureSandbox(sid string) (string, error) {
	root := s.sandboxRoot
	if root == "" {
		root = "."
	}
	dir := filepath.Join(root, "by_sessions_scope", sanitizeSandboxKey(sid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create sandbox %s: %w", dir, err)
	}
	// Bump the dir mtime on every use (MkdirAll on an existing dir does not) so a
	// concurrent Housekeeper idle-sweep never mistakes a just-reactivated sid for an
	// idle one and RemoveAll's it out from under the in-flight call.
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
	return dir, nil
}

// sanitizeSandboxKey makes a sid filesystem-safe (whitelist [A-Za-z0-9_.-];
// anything else → '_'; empty → "default"; leading '.' defanged).
func sanitizeSandboxKey(s string) string {
	if s == "" {
		return "default"
	}
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			out = append(out, byte(r))
		default:
			out = append(out, '_')
		}
	}
	clean := string(out)
	if clean == "" || clean == "." || clean == ".." {
		return "default"
	}
	if clean[0] == '.' {
		clean = "_" + clean[1:]
	}
	return clean
}

// buildScrapeCLIArgs constructs the scrapling argv for the given mode.
func buildScrapeCLIArgs(mode, target, tmpPath string, wallSec int, p *scrapeArgs) ([]string, bool, error) {
	args := []string{"extract"}
	timeoutClamped := false
	switch mode {
	case "get":
		impersonate := p.Impersonate
		if impersonate == "" {
			impersonate = "chrome"
		}
		args = append(args, "get", target, tmpPath, "--impersonate", impersonate)
		// Disable redirect following so CheckURLHost on the initial host is the
		// only host we ever talk to (a public URL could 302 → 169.254.169.254).
		args = append(args, "--no-follow-redirects")
		if strings.TrimSpace(p.CSSSelector) != "" {
			args = append(args, "--css-selector", p.CSSSelector)
		}
	case "fetch":
		args = append(args, "fetch", target, tmpPath)
		if strings.TrimSpace(p.CSSSelector) != "" {
			args = append(args, "--css-selector", p.CSSSelector)
		}
		// Clamp the model-supplied timeout_ms to the wall budget so the inner
		// playwright timeout fires before cctx's outer SIGKILL.
		if eff, clamped := clampTimeoutMs(p.TimeoutMs, wallSec); eff > 0 {
			args = append(args, "--timeout", fmt.Sprintf("%d", eff))
			timeoutClamped = clamped
		}
	case "stealthy-fetch":
		args = append(args, "stealthy-fetch", target, tmpPath)
		if strings.TrimSpace(p.CSSSelector) != "" {
			args = append(args, "--css-selector", p.CSSSelector)
		}
		if eff, clamped := clampTimeoutMs(p.TimeoutMs, wallSec); eff > 0 {
			args = append(args, "--timeout", fmt.Sprintf("%d", eff))
			timeoutClamped = clamped
		}
		// solve_cloudflare default true — that's the whole point of this mode.
		if p.SolveCloudflare == nil || *p.SolveCloudflare {
			args = append(args, "--solve-cloudflare")
		}
	case "post":
		method := strings.ToUpper(strings.TrimSpace(p.Method))
		if method == "" {
			method = "POST"
		}
		switch method {
		case "POST", "PUT", "DELETE":
			// OK
		default:
			return nil, false, fmt.Errorf("method must be POST, PUT, or DELETE")
		}
		// A body only rides POST/PUT: the CLI hangs its --json option off those two
		// commands, so passing it with DELETE exits 2 on an unknown option — and that
		// exit reads as "the target rejected us" downstream. Refuse it here, where the
		// message can say what to change.
		if method == "DELETE" && len(p.BodyJSON) > 0 {
			return nil, false, fmt.Errorf("method=DELETE takes no body_json — drop body_json, or use POST/PUT")
		}
		args = append(args, strings.ToLower(method), target, tmpPath)
		// Disable redirect following for SSRF symmetry with mode=get.
		args = append(args, "--no-follow-redirects")
		if len(p.BodyJSON) > 0 {
			args = append(args, "--json", string(p.BodyJSON))
		}
		if strings.TrimSpace(p.CSSSelector) != "" {
			args = append(args, "--css-selector", p.CSSSelector)
		}
	}
	return args, timeoutClamped, nil
}

// modeWallTimeSeconds returns the wall-clock budget for the given mode. Cheap
// modes (get, post) use base as-is; the browser-backed modes need more headroom
// because playwright cold-starts chromium and stealthy-fetch additionally
// solves Turnstile (5-15 s).
func modeWallTimeSeconds(mode string, base int) int {
	switch mode {
	case "fetch":
		if base < 60 {
			return 60
		}
	case "stealthy-fetch":
		if base < 90 {
			return 90
		}
	}
	return base
}

// wallTimeoutMarginSeconds is the headroom subtracted from the wall budget when
// clamping the model-supplied timeout_ms, so the inner playwright timeout fires
// BEFORE cctx's outer SIGKILL.
const wallTimeoutMarginSeconds = 5

// clampTimeoutMs clamps a model-supplied timeout_ms so the inner playwright
// timeout never exceeds (wallSec - margin)*1000. requested<=0 means "use
// scrapling's own default" — return 0 so the caller omits --timeout.
func clampTimeoutMs(requested, wallSec int) (effective int, clamped bool) {
	if requested <= 0 {
		return 0, false
	}
	capMs := (wallSec - wallTimeoutMarginSeconds) * 1000
	if capMs < 1000 {
		capMs = 1000
	}
	if requested > capMs {
		return capMs, true
	}
	return requested, false
}

// detectBotProtection scans tool output for telltale signs of a Cloudflare
// challenge or an empty SPA shell, and returns a hint telling the model to
// escalate. Runs for ALL modes so the (tool, host) failure counter accumulates.
func detectBotProtection(content, mode string) (errMsg, hint string) {
	trimmed := strings.TrimSpace(content)
	n := len(trimmed)
	// A real page is almost never < 500 chars after markdownify of useful
	// content. Be conservative: only flag suspiciously short bodies.
	if n >= 500 {
		return "", ""
	}
	low := strings.ToLower(trimmed)
	cloudflareSignals := []string{
		"just a moment",
		"checking your browser",
		"cf-chl",
		"cloudflare",
		"ddos protection",
		"attention required",
	}
	cfMatch := false
	for _, sig := range cloudflareSignals {
		if strings.Contains(low, sig) {
			cfMatch = true
			break
		}
	}
	if cfMatch {
		switch mode {
		case "get":
			return "got Cloudflare challenge page (not real content)",
				"重试同 URL，但 mode=stealthy-fetch"
		case "fetch":
			return "fetch mode 拿回 Cloudflare 页（vanilla playwright 未能绕过）",
				"**禁止**回 mode=get；立刻 mode=stealthy-fetch；若 stealthy 也失败 → browser_navigate"
		case "stealthy-fetch":
			return "stealthy-fetch 拿回 Cloudflare 页（stealth 也被识破）",
				"scrapling 在这个 host 上已无路可走；**立刻** browser_navigate（chromium + 真 cookies）"
		case "post":
			return "POST 拿回 Cloudflare 页（终端站点没接受 POST，或被反爬）",
				"attach-download 类 URL 通常是 GET 触发下载；用 browser_navigate 让真浏览器去下"
		}
	}
	// Near-empty body (SPA shell or genuinely empty).
	if n < 100 {
		// mode=post is the API path — a short / empty body ({"ok":true}, a 204,
		// a DELETE ack) is a LEGITIMATE response, not bot protection. Only flag a
		// short POST body when it actually looks like a (suspiciously short)
		// HTML page.
		if mode == "post" {
			if strings.Contains(low, "<html") || strings.Contains(low, "<!doctype") ||
				strings.Contains(low, "<title") || strings.Contains(low, "<body") {
				return "POST 拿回疑似反爬 HTML 空壳（短 body 但含 HTML 标记，非 API 数据）",
					"终端站点可能用 HTML 反爬挡 POST；用 browser_navigate 走真实交互"
			}
			return "", "" // short JSON / empty API response — success
		}
		switch mode {
		case "get":
			return "got near-empty body — likely SPA shell needing JS render, or a real empty page",
				"重试同 URL mode=fetch；如果还是空，URL 可能本来就没内容"
		case "fetch":
			return "fetch mode 拿回空 body（JS 渲染了但没内容，或反爬隐藏）",
				"试 mode=stealthy-fetch；若还空，URL 可能本身没内容或 → browser_navigate"
		case "stealthy-fetch":
			return "stealthy-fetch 拿回空 body",
				"URL 可能本身没内容；如确认有内容 → browser_navigate（最后手段）"
		}
	}
	return "", ""
}

// scrapeCapBuf is a fixed-cap io.Writer for the scrapling subprocess's combined
// output. Once max bytes are buffered it silently discards further Writes — the
// subprocess sees no error and runs until cctx wall-time fires SIGKILL.
type scrapeCapBuf struct {
	buf []byte
	max int
}

func newScrapeCapBuf(max int) *scrapeCapBuf { return &scrapeCapBuf{max: max} }

func (b *scrapeCapBuf) Write(p []byte) (int, error) {
	if len(b.buf) >= b.max {
		return len(p), nil
	}
	remaining := b.max - len(b.buf)
	if len(p) <= remaining {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	b.buf = append(b.buf, p[:remaining]...)
	return len(p), nil
}

func (b *scrapeCapBuf) Bytes() []byte { return b.buf }

// scrapeExcerpt returns the head of fetched content (~300 bytes, rune-safe,
// whitespace-collapsed) for the URL library excerpt.
func scrapeExcerpt(content string) string {
	e := collapseWS(content)
	if len(e) > 300 {
		e = truncateRunes(e, 300)
	}
	return e
}
