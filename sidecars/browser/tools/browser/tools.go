package browser

// 11 ToolSpec wrappers, Hermes-named. Each is a thin shim that:
//   1) unmarshals the JSON args,
//   2) acquires the per-session browser via the Pool,
//   3) calls the matching actions.go function,
//   4) marshals the typed result (or {"error": "..."} on failure).
//
// All registered via constructors named Browser<Verb>. Wired in
// pipeline-startup/10-run.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentbob/sidecars/browser/core"
	"agentbob/sidecars/browser/tools/neterror"
)

// --- Common helpers (post-D1 envelope) ----------------------------------
//
// All browser tools return either a structured OK result for success or
// a structured error result for failure (the dispatcher seals them into
// the D1 envelope). The local aliases below keep the many call sites short.

// errResult emits the structured error form.
func errResult(msg string) core.ToolResult { return core.ErrResult(msg) }

// errResultWithHint emits the structured error with a hint — used by fetch
// tools on DNS-class failures so the model gets a search-instead nudge.
func errResultWithHint(msg, hint string) core.ToolResult { return core.ErrResult(msg, hint) }

// jsonOK marshals v as the success payload (JSON-encoded string).
// Marshal failure returns an error result rather than panicking — keeps
// the agent loop unblockable.
func jsonOK(v any) core.ToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errResult("marshal: " + err.Error())
	}
	return core.OKResult(string(b))
}

// --- browser_navigate ----------------------------------------------------

const browserNavigateParams = `{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "要加载的 http:// 或 https:// URL。浏览器单会话，此 URL 替换当前 tab。返回页面 title + 紧凑的 accessibility-tree snapshot，含 @e1 / @e2 / ... ref id 给 browser_click / browser_type 用。"
    }
  },
  "required": ["url"]
}`

type browserNavigate struct{ pool *Pool }

func BrowserNavigate(pool *Pool) core.Tool { return browserNavigate{pool: pool} }

func (browserNavigate) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name: "browser_navigate",
		Description: "启动/复用本 session 的 chromium，加载 URL。两类用途：\n" +
			"  1. URL 取内容的**最后手段** —— 前面 scrapling 4 mode（get/stealthy-fetch/fetch/post）都过不了才用\n" +
			"  2. 登录站点 / 多步交互的**入口** —— navigate 成功后，本 session 的 browser_snapshot/click/type/scroll/back/dialog/get_images/vision/console/cdp 这 11 个工具才出现（之前隐藏）\n" +
			"返回页面 title + 紧凑 a11y-tree snapshot（带 @e1 / @e2 ref id 给后续 click/type 用）。Cookies 持久到显式 `/new` 才清；中间 idle / 重启都保留。Tier=user。",
		Parameters: json.RawMessage(browserNavigateParams),
		// The result is a structured a11y tree whose @e refs the model must read
		// verbatim to click/type — prose-summarising it destroys the refs. Stale
		// snapshots are elided from history elsewhere so this won't bloat context.
		NoAutoCompress: true,
	}
}

func (browserNavigate) Tier() core.Tier { return core.TierUser }
func (browserNavigate) Serialize() bool { return false }

func (b browserNavigate) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_navigate: invalid arguments: %w", err)
	}
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	res, err := Navigate(ctx, sess, p.URL, b.pool.cfg.PageTimeoutSecondsEff())
	if err != nil {
		if neterror.IsDNSClass(err.Error()) {
			return errResultWithHint(err.Error(), neterror.Hint), nil
		}
		return errResult(err.Error()), nil
	}
	// Slice 7: record to URL library so future web_search calls can
	// recall this URL. Excerpt = first slice of the tree snapshot.
	// Query is best-effort from the user's most recent message.
	recordURL(ctx, actx, res.URL, res.Title, excerptFrom(res.Snapshot))

	// Persist the URL so:
	//   1. Idle close / bob restart can auto-resume this page on next
	//      Acquire (acquireKeyed reads the session dataDir's marker).
	//   2. SpecForSession on the interactive browser_* tools can see the
	//      scope-level used-marker and start advertising those tools.
	// RecordNav writes the auto-resume marker into the SESSION's dataDir
	// (vault for a profile session) and the used-marker under the scope dir —
	// the two consumers diverge on the profile path. See Pool.RecordNav.
	b.pool.RecordNav(sess, actx.SessionScope, res.URL)

	return jsonOK(map[string]any{
		"url":      res.URL,
		"title":    res.Title,
		"ready":    res.Ready,
		"snapshot": res.Snapshot,
	}), nil
}

// recordURL is a shared helper used by all fetch tools. It threads
// the URL library Record call through the AgentCtx; safe when
// actx.URLLibrary is nil (no-op). originalQuery is sourced from the
// user's most recent message — empty if not obvious.
func recordURL(ctx context.Context, actx core.AgentCtx, url, title, excerpt string) {
	if actx.URLLibrary == nil {
		return
	}
	actx.URLLibrary.Record(ctx, url, title, excerpt, actx.Event.Text)
}

// excerptFrom returns the first ~600 bytes of s (≈200 CJK chars or
// ~600 ASCII chars). The SQLite layer caps to 240 anyway and the
// excerpt is informational only — byte-precise truncation is fine.
// Snaps to the last full UTF-8 boundary so the string stays valid.
func excerptFrom(s string) string {
	const maxBytes = 600
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	// Walk back until we land on a UTF-8 lead byte (avoid mid-rune cut).
	for end > 0 && (s[end]&0xC0) == 0x80 {
		end--
	}
	return s[:end]
}

// --- browser_snapshot ----------------------------------------------------

const browserSnapshotParams = `{
  "type": "object",
  "properties": {
    "format": {
      "type": "string",
      "enum": ["tree", "html", "text", "image"],
      "description": "tree（默认，Hermes 兼容）：accessibility-tree 文本，带 @e1 / @e2 ref id 供 click/type 用。html：原始 outer HTML。text：innerText。image：PNG 存到 screenshots/ 下，返回相对路径（可直接传给 read_file）。"
    },
    "selector": {
      "type": "string",
      "description": "可选 CSS selector，把 tree/html/text 限定在某子树。image 模式忽略（整 viewport）。"
    }
  }
}`

type browserSnapshot struct{ pool *Pool }

func BrowserSnapshot(pool *Pool) core.Tool { return browserSnapshot{pool: pool} }

func (browserSnapshot) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "browser_snapshot",
		Description: "读当前页面。默认 format=tree 返回 a11y-tree 列表，含 [@e1] [@e2] ref id 给 browser_click / browser_type 用。format=html 或 text 取原始页面内容做解析；format=image 取 PNG。**任何交互后页面可能重渲染——之后必须重新 snapshot，ref id 是 per-snapshot 的，不跨重渲染**。Tier=user。",
		Parameters:  json.RawMessage(browserSnapshotParams),
		// The whole point of snapshot is the verbatim a11y tree + @e refs the
		// model clicks/types against; prose-summarising it destroys the refs.
		// Stale snapshots are elided from history elsewhere to bound context.
		NoAutoCompress: true,
	}
}

func (browserSnapshot) Tier() core.Tier { return core.TierUser }
func (browserSnapshot) Serialize() bool { return false }

func (b browserSnapshot) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Format   string `json:"format"`
		Selector string `json:"selector"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_snapshot: invalid arguments: %w", err)
	}
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// Compute the screenshot base dir the same way read_file resolves a
	// relative path (file.SessionSandbox honours SandboxRoot +
	// namespace_by_bot) so format=image writes where read_file reads.
	res, err := Snapshot(ctx, sess, b.pool.sessionSandbox(actx), SnapshotOpts{Format: p.Format, Selector: p.Selector}, b.pool.cfg.PageTimeoutSecondsEff())
	if err != nil {
		return errResult(err.Error()), nil
	}
	out := map[string]any{
		"format":     res.Format,
		"selector":   res.Selector,
		"byte_count": res.ByteCount,
		"truncated":  res.Truncated,
	}
	if res.Content != "" {
		out["content"] = res.Content
	}
	if res.ScreenshotPath != "" {
		out["path"] = res.ScreenshotPath
	}
	return jsonOK(out), nil
}

// --- browser_click -------------------------------------------------------

const browserClickParams = `{
  "type": "object",
  "properties": {
    "ref": {
      "type": "string",
      "description": "前一个 browser_snapshot / browser_navigate 输出里的 ref id（如 \"@e5\"）。优先用 ref。"
    },
    "selector": {
      "type": "string",
      "description": "可选 CSS selector，没有 ref 时用。例：\"#login\"、\"button[type=submit]\"。"
    }
  }
}`

type browserClick struct{ pool *Pool }

func BrowserClick(pool *Pool) core.Tool { return browserClick{pool: pool} }

func (browserClick) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "browser_click",
		Description: "点击元素。优先用 ref=\"@e5\"（最近 browser_snapshot / browser_navigate 输出里的 id）。没 ref 用 selector= 作为 fallback。Tier=user（注意：点击可能提交表单 / 触发购买 / 退出登录 / 跳转）。",
		Parameters:  json.RawMessage(browserClickParams),
	}
}

func (browserClick) Tier() core.Tier { return core.TierUser }
func (browserClick) Serialize() bool { return false }

func (b browserClick) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Ref      string `json:"ref"`
		Selector string `json:"selector"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_click: invalid arguments: %w", err)
	}
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := Click(ctx, sess, Locator{Ref: p.Ref, Selector: p.Selector}, b.pool.cfg.PageTimeoutSecondsEff()); err != nil {
		return errResult(err.Error()), nil
	}
	return jsonOK(map[string]any{
		"ref":      p.Ref,
		"selector": p.Selector,
		"clicked":  true,
	}), nil
}

// --- browser_type --------------------------------------------------------

const browserTypeParams = `{
  "type": "object",
  "properties": {
    "ref": {
      "type": "string",
      "description": "前一个 browser_snapshot / browser_navigate 输出里的 ref id（如 input 的 \"@e3\"）。优先用 ref。"
    },
    "selector": {
      "type": "string",
      "description": "可选 CSS selector，没 ref 时用。例：\"input[name=q]\"、\"#search\"。"
    },
    "text": {
      "type": "string",
      "description": "要输入的文本。replace=true 先清空再输；默认追加。"
    },
    "replace": {
      "type": "boolean",
      "description": "true = 先清空字段再输入。默认 false。"
    }
  },
  "required": ["text"]
}`

type browserType struct{ pool *Pool }

func BrowserType(pool *Pool) core.Tool { return browserType{pool: pool} }

func (browserType) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "browser_type",
		Description: "向 input / textarea / contenteditable 输入文本。优先用 ref=\"@e3\"（最近 snapshot 里的 id），没 ref 用 selector= 作 fallback。replace=true 先清空。提交配合 browser_press(\"Enter\") 或点击提交按钮。Tier=user。",
		Parameters:  json.RawMessage(browserTypeParams),
	}
}

func (browserType) Tier() core.Tier { return core.TierUser }
func (browserType) Serialize() bool { return false }

func (b browserType) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Ref      string `json:"ref"`
		Selector string `json:"selector"`
		Text     string `json:"text"`
		Replace  bool   `json:"replace"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_type: invalid arguments: %w", err)
	}
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := Type(ctx, sess, Locator{Ref: p.Ref, Selector: p.Selector}, p.Text, p.Replace, b.pool.cfg.PageTimeoutSecondsEff()); err != nil {
		return errResult(err.Error()), nil
	}
	return jsonOK(map[string]any{
		"ref":         p.Ref,
		"selector":    p.Selector,
		"typed_chars": len(p.Text),
		"replaced":    p.Replace,
	}), nil
}

// --- browser_press -------------------------------------------------------

const browserPressParams = `{
  "type": "object",
  "properties": {
    "key": {
      "type": "string",
      "description": "按键名。常用：\"Enter\"、\"Tab\"、\"Escape\"、\"Backspace\"、\"ArrowDown\"、\"PageDown\"。单个可打印字符也可。"
    },
    "ref": {
      "type": "string",
      "description": "可选 ref，按键前先聚焦（如先聚焦搜索框再按 Enter）。"
    },
    "selector": {
      "type": "string",
      "description": "可选 CSS selector 作聚焦的 fallback，没 ref 时用。"
    }
  },
  "required": ["key"]
}`

type browserPress struct{ pool *Pool }

func BrowserPress(pool *Pool) core.Tool { return browserPress{pool: pool} }

func (browserPress) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "browser_press",
		Description: "发送单个按键（Enter / Tab / Escape / 方向键 / 可打印字符）。常用模式：browser_type 进搜索框 → browser_press(\"Enter\") 提交（找不到明显的提交按钮时）。Tier=user。",
		Parameters:  json.RawMessage(browserPressParams),
	}
}

func (browserPress) Tier() core.Tier { return core.TierUser }
func (browserPress) Serialize() bool { return false }

func (b browserPress) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Key      string `json:"key"`
		Ref      string `json:"ref"`
		Selector string `json:"selector"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_press: invalid arguments: %w", err)
	}
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := Press(ctx, sess, p.Key, Locator{Ref: p.Ref, Selector: p.Selector}, b.pool.cfg.PageTimeoutSecondsEff()); err != nil {
		return errResult(err.Error()), nil
	}
	return jsonOK(map[string]any{
		"key":     p.Key,
		"pressed": true,
	}), nil
}

// --- browser_scroll ------------------------------------------------------

const browserScrollParams = `{
  "type": "object",
  "properties": {
    "direction": {
      "type": "string",
      "enum": ["up", "down", "top", "bottom"],
      "description": "up/down 按 pixels 滚（默认 500）。top/bottom 跳到页首 / 页尾。"
    },
    "pixels": {
      "type": "integer",
      "description": "direction=up/down 时滚多少像素。默认 500。top/bottom 时忽略。"
    },
    "selector": {
      "type": "string",
      "description": "可选 CSS selector：把这个元素滚进视口（替代 direction 用法）。"
    }
  }
}`

type browserScroll struct{ pool *Pool }

func BrowserScroll(pool *Pool) core.Tool { return browserScroll{pool: pool} }

func (browserScroll) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "browser_scroll",
		Description: "滚动当前页面。direction=up/down/top/bottom（up/down 可选 pixels，默认 500），或 selector= 把某元素滚进视口。无限滚 feed：scroll(\"down\") + browser_snapshot 循环。Tier=user。",
		Parameters:  json.RawMessage(browserScrollParams),
	}
}

func (browserScroll) Tier() core.Tier { return core.TierUser }
func (browserScroll) Serialize() bool { return false }

func (b browserScroll) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Direction string `json:"direction"`
		Pixels    int    `json:"pixels"`
		Selector  string `json:"selector"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_scroll: invalid arguments: %w", err)
	}
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := Scroll(ctx, sess, ScrollOpts{Direction: p.Direction, Pixels: p.Pixels, Selector: p.Selector}, b.pool.cfg.PageTimeoutSecondsEff()); err != nil {
		return errResult(err.Error()), nil
	}
	return jsonOK(map[string]any{
		"direction": p.Direction,
		"pixels":    p.Pixels,
		"selector":  p.Selector,
		"scrolled":  true,
	}), nil
}

// --- browser_back --------------------------------------------------------

const browserBackParams = `{
  "type": "object",
  "properties": {
  }
}`

type browserBack struct{ pool *Pool }

func BrowserBack(pool *Pool) core.Tool { return browserBack{pool: pool} }

func (browserBack) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "browser_back",
		Description: "浏览器历史后退一步。返回新 URL + title。点错链接 / 多步流程要退出时用。Tier=user。",
		Parameters:  json.RawMessage(browserBackParams),
	}
}

func (browserBack) Tier() core.Tier { return core.TierUser }
func (browserBack) Serialize() bool { return false }

func (b browserBack) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	res, err := Back(ctx, sess, b.pool.cfg.PageTimeoutSecondsEff())
	if err != nil {
		return errResult(err.Error()), nil
	}
	return jsonOK(map[string]any{
		"url":   res.URL,
		"title": res.Title,
	}), nil
}

// --- browser_console -----------------------------------------------------

const browserConsoleParams = `{
  "type": "object",
  "properties": {
    "level": {
      "type": "string",
      "enum": ["all", "error", "warning", "log"],
      "description": "按级别过滤。默认 \"error\"（最有诊断价值）。"
    },
    "max_entries": {"type": "integer", "description": "返回条目上限。默认 50。"}
  }
}`

type browserConsole struct{ pool *Pool }

func BrowserConsole(pool *Pool) core.Tool { return browserConsole{pool: pool} }

func (browserConsole) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "browser_console",
		Description: "读当前会话的浏览器 console 消息。默认 level=error（JS 异常 + console.error()）。点了没反应 / 页面没渲染时用——JS 堆栈通常说明原因。Tier=user（只读）。",
		Parameters:  json.RawMessage(browserConsoleParams),
	}
}

func (browserConsole) Tier() core.Tier { return core.TierUser }
func (browserConsole) Serialize() bool { return false }

func (b browserConsole) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Level      string `json:"level"`
		MaxEntries int    `json:"max_entries"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_console: invalid arguments: %w", err)
	}
	if p.Level == "" {
		p.Level = "error"
	}
	if p.MaxEntries <= 0 {
		p.MaxEntries = 50
	}
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// Read from the acquired session directly: AcquireFor may key it by
	// ProfileName (profile path), so a scope-keyed Pool lookup would return
	// an empty buffer for profile sessions.
	entries := sess.ConsoleSnapshot(p.Level, p.MaxEntries)
	return jsonOK(map[string]any{
		"level":     p.Level,
		"entries":   entries,
		"entry_cnt": len(entries),
	}), nil
}

// --- browser_get_images --------------------------------------------------

const browserGetImagesParams = `{
  "type": "object",
  "properties": {
    "max_images": {"type": "integer", "description": "返回图片上限。默认 50。"},
    "selector": {"type": "string", "description": "可选 CSS selector，限定查找范围。"}
  }
}`

type browserGetImages struct{ pool *Pool }

func BrowserGetImages(pool *Pool) core.Tool { return browserGetImages{pool: pool} }

func (browserGetImages) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "browser_get_images",
		Description: "列出当前页面所有 <img>（绝对 src、alt、原始 width/height）。过滤掉 1x1 追踪像素。找产品图 / 图表 / 验证码时用，不用解析整个 HTML。Tier=user。",
		Parameters:  json.RawMessage(browserGetImagesParams),
	}
}

func (browserGetImages) Tier() core.Tier { return core.TierUser }
func (browserGetImages) Serialize() bool { return false }

func (b browserGetImages) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		MaxImages int    `json:"max_images"`
		Selector  string `json:"selector"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_get_images: invalid arguments: %w", err)
	}
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	imgs, err := GetImages(ctx, sess, p.Selector, p.MaxImages, b.pool.cfg.PageTimeoutSecondsEff())
	if err != nil {
		return errResult(err.Error()), nil
	}
	return jsonOK(map[string]any{
		"selector":  p.Selector,
		"images":    imgs,
		"image_cnt": len(imgs),
	}), nil
}

// --- browser_dialog ------------------------------------------------------

const browserDialogParams = `{
  "type": "object",
  "properties": {
    "accept": {"type": "boolean", "description": "true = OK / Yes；false = Cancel / No。默认 true。"},
    "prompt_text": {"type": "string", "description": "仅用于 prompt() 对话框——要填入的文本。"}
  }
}`

type browserDialog struct{ pool *Pool }

func BrowserDialog(pool *Pool) core.Tool { return browserDialog{pool: pool} }

func (browserDialog) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "browser_dialog",
		Description: "预设页面**下一个** JS 对话框（alert / confirm / prompt）的响应。在**点击会触发对话框的按钮之前**调用。accept=false 取消；prompt_text 填 prompt 输入。一个对话框后预设自动失效——下一个回到默认（accept）。Tier=user。",
		Parameters:  json.RawMessage(browserDialogParams),
	}
}

func (browserDialog) Tier() core.Tier { return core.TierUser }
func (browserDialog) Serialize() bool { return false }

func (b browserDialog) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Accept     *bool  `json:"accept"`
		PromptText string `json:"prompt_text"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_dialog: invalid arguments: %w", err)
	}
	accept := true
	if p.Accept != nil {
		accept = *p.Accept
	}
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// Operate on the acquired session directly: AcquireFor may key it by
	// ProfileName (profile path), so a scope-keyed Pool lookup would miss it.
	sess.SetDialogReply(accept, p.PromptText)
	return jsonOK(map[string]any{
		"accept":      accept,
		"prompt_text": p.PromptText,
		"primed":      true,
	}), nil
}

// --- browser_cdp ---------------------------------------------------------

const browserCDPParams = `{
  "type": "object",
  "properties": {
    "method": {
      "type": "string",
      "description": "CDP method 名。支持的观测类方法：Page.captureScreenshot / Page.captureSnapshot / Page.printToPDF、DOM.getDocument / DOM.querySelector / DOM.querySelectorAll / DOM.getOuterHTML、Network.getCookies、Runtime.getProperties、Emulation.setDeviceMetricsOverride / Emulation.setUserAgentOverride。例：\"Page.printToPDF\"。"
    },
    "params": {
      "type": "object",
      "description": "method 参数（JSON 对象）。无参方法传 {}。"
    }
  },
  "required": ["method"]
}`

type browserCDP struct{ pool *Pool }

func BrowserCDP(pool *Pool) core.Tool { return browserCDP{pool: pool} }

func (browserCDP) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "browser_cdp",
		Description: "发送 Chrome DevTools Protocol 命令，提供页面观测与诊断能力：页面快照 / 截图 / 导出 PDF（Page.printToPDF）、DOM 查询与原始 HTML（DOM.getOuterHTML）、读当前页 cookie（Network.getCookies）、视口尺寸与 UA 覆盖（Emulation.*）。method 填 CDP 方法名，params 填该方法的 JSON 参数对象。",
		Parameters:  json.RawMessage(browserCDPParams),
		SelectionHint: &core.SelectionHint{
			When:     `要导出当前页 PDF / 拿原始 HTML、诊断当前页 cookie、或伪装视口尺寸与 UA（响应式排查）`,
			Then:     `browser_cdp method=Page.printToPDF / DOM.getOuterHTML / Network.getCookies / Emulation.setDeviceMetricsOverride，params 传该方法的参数对象（无参传 {}）`,
			Priority: 60,
		},
	}
}

func (browserCDP) Tier() core.Tier { return core.TierUser }
func (browserCDP) Serialize() bool { return false }

func (b browserCDP) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_cdp: invalid arguments: %w", err)
	}
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	res, err := CDPExec(ctx, sess, p.Method, p.Params, b.pool.cfg.PageTimeoutSecondsEff())
	if err != nil {
		return errResult(err.Error()), nil
	}
	return jsonOK(map[string]any{
		"method": res.Method,
		"result": res.Result,
	}), nil
}

// --- browser_vision ------------------------------------------------------
//
// Capture a screenshot of the current page and have a vision-capable model
// describe / answer questions about it. Routes via core.ModelPool with
// Requires=["vision"] — operators must tag a model entry as "vision" in
// $BOB_HOME/models.yaml. No vision-tagged entry → tool returns a clear
// error envelope; the LLM caller sees it and switches strategy.

const browserVisionParams = `{
  "type": "object",
  "properties": {
    "question": {
      "type": "string",
      "description": "要让 vision 模型回答的问题（如 \"页面上有什么\"、\"价格是多少\"、\"哪个按钮是登录\"）。默认 \"简要描述这张截图\"。"
    }
  }
}`

type browserVision struct{ pool *Pool }

// BrowserVision constructs the browser_vision tool. Vision model lookup
// happens at Run-time via actx.ModelPool (the agent populates it).
func BrowserVision(pool *Pool) core.Tool { return browserVision{pool: pool} }

func (browserVision) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "browser_vision",
		Description: "截当前页面 + 把截图和你的问题一起发给 vision-capable 模型，返回模型的回答。文字 snapshot（accessibility tree）解决不了的视觉问题用它——验证码 / 图表 / 设计稿 / 视觉布局判断。需要管理员配置了支持图像输入的 vision 模型；没有则报错。Tier=user。",
		Parameters:  json.RawMessage(browserVisionParams),
		// 截图 PNG 是结构性输入，不该被压缩；模型回答是文本，常规处理。
		NoAutoCompress: false,
	}
}

func (browserVision) Tier() core.Tier { return core.TierUser }
func (browserVision) Serialize() bool { return false }

func (v browserVision) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_vision: invalid arguments: %w", err)
	}
	q := strings.TrimSpace(p.Question)
	if q == "" {
		q = "简要描述这张截图。"
	}
	if actx.ModelPool == nil {
		return errResult("browser_vision: model pool not available (synthetic call?)"), nil
	}

	// 抢浏览器会话 + 截图。AcquireFor 与其余工具一致:profile 授权用户走 profile 路径,
	// 否则 legacy。直接 Acquire 会绕过 profile 路由,截到另一个空白 chromium。
	sess, err := v.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	png, err := Screenshot(ctx, sess, v.pool.cfg.PageTimeoutSecondsEff())
	if err != nil {
		return errResult(err.Error()), nil
	}

	// 调 vision-tagged 模型。Requires=["vision"] —— 没匹配 entry 时
	// MultiPool 返回 "no live pool entry has all required tags: [vision]"，
	// 这里翻译成对模型友好的错误，告诉它该怎么修。
	resp, err := actx.ModelPool.Chat(ctx, core.ModelRequest{
		Requires: []string{"vision"},
	}, []core.Message{
		{
			Role:    "user",
			Content: q,
			Images:  []core.ImageRef{{Data: png, MIME: "image/png"}},
		},
	})
	if err != nil {
		if strings.Contains(err.Error(), "required tags") {
			return errResult(
				"browser_vision: 没有 vision-capable 模型可用 —— 请管理员配置一个支持图像输入的 vision 模型。",
			), nil
		}
		return errResult("browser_vision: model call failed: " + err.Error()), nil
	}
	answer := strings.TrimSpace(resp.Content)
	if answer == "" {
		return errResult("browser_vision: 模型 (" + resp.Model + ") 返回空内容"), nil
	}
	return jsonOK(map[string]any{
		"question":   q,
		"answer":     answer,
		"model":      resp.Model,
		"image_size": len(png),
	}), nil
}

// --- browser_tab ---------------------------------------------------------
//
// Multi-tab / popup management. New windows / popups (window.open,
// target=_blank) auto-appear in the tab list; a lone popup is auto-switched
// to. This tool lets the model list all tabs and explicitly switch / close
// when more than one popped (or to leave a popup and return to the main tab).

const browserTabParams = `{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["list", "switch", "close"],
      "description": "list 列出所有标签页；switch 切到 index 指定的标签；close 关闭 index 指定的标签。"
    },
    "index": {
      "type": "integer",
      "description": "1-based 标签序号（switch / close 必填），对应 list 返回的 index。"
    }
  },
  "required": ["action"]
}`

type browserTab struct{ pool *Pool }

func BrowserTab(pool *Pool) core.Tool { return browserTab{pool: pool} }

func (browserTab) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name: "browser_tab",
		Description: "管理浏览器标签页。action=list 列出所有标签；switch+index 切到某个标签;close+index 关闭某个标签。" +
			"新窗口 / 弹窗会自动出现在列表里;单个弹窗会自动切过去,多个则留给你用 switch 选。Tier=user。",
		Parameters: json.RawMessage(browserTabParams),
	}
}

func (browserTab) Tier() core.Tier { return core.TierUser }
func (browserTab) Serialize() bool { return false }

func (b browserTab) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Action string `json:"action"`
		Index  int    `json:"index"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("browser_tab: invalid arguments: %w", err)
	}
	sess, err := b.pool.AcquireFor(ctx, actx)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// RunTab reconciles first (so a popup the model hasn't acted on yet is
	// visible / addressable) and is shared with the browserd /tab handler.
	out, err := RunTab(sess, p.Action, p.Index)
	if err != nil {
		return errResult(err.Error()), nil
	}
	return jsonOK(out), nil
}
