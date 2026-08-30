package browserremote

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"agentbob/contract"
)

// browserTool is the single trunk-side browser tool: ONE model-facing tool with
// an `action` enum that fans out to the 13 browserd endpoints the skeleton
// exposed as 13 separate browser_* tools. Collapsing them to one keeps the
// model's tool list short (one item, one warrant cap tool:use:browser), lets the
// tool self-gate interactive actions before a navigate, and serializes concurrent
// actions against the one shared remote page.
//
// IDENTITY AXES (docs/wip-browser-profile-identity.md):
//   - profile_key = the per-MEMBER MASTER (masterKey below) — the login store a worker's
//     copy seeds from. Same role, different members may hold different site logins.
//   - scope = the per-SCOPE COPY (CopyScope below) — the worker's own read-only chromium +
//     dir, seeded from the master. Concurrent workers are different scopes.
//
// A worker action runs on its copy (Mode "copy"); the copy never writes the master. Only a
// human-driven copy (taken over via escalate_to_coo, logged in, then 保存登陆) writes its
// login back to the member's master — future workers seed it. 保存登陆 captures the live login
// WITHOUT closing, so the worker that handed off resumes on the SAME logged-in copy.
type browserTool struct {
	c *Client
	// lib is the OPTIONAL URL library (lazy thunk; nil result → no recording). A
	// successful browser_navigate Records the page (signal ①).
	lib func() contract.URLLibrary
	// navigated tracks which sids have had a SUCCESSFUL navigate this process.
	// Interactive actions refuse (without touching browserd) until the sid is in
	// the set — this is the runtime replacement for skeleton's SpecForSession
	// visibility-gating: the tool is always visible, but refuses pre-navigate.
	mu        sync.Mutex
	navigated map[string]bool
	// holds is the human control-hold gate (browser takeover yield). While a human
	// drives the takeover in control mode, Held(sid) is true and every action yields
	// with holdEnvelope instead of fighting the human. nil → never held (no takeover).
	holds contract.BrowserControlHold
	// vision answers a question about a screenshot via a vision-capable model. browserd
	// only returns the screenshot PNG (it runs no model); the model leg runs bob-side over
	// bob's own pool. nil → no vision model wired (browser_vision saves the PNG + says so).
	vision VisionFunc
}

// VisionFunc answers question about a PNG screenshot using a vision-capable model.
// Wired from the model pool (Requires:["vision"]) at construction — keeps browserremote
// decoupled from the pool. Returns the model's answer, or an error (no live vision entry,
// backend failure) the caller degrades to a screenshot-save fallback on.
type VisionFunc func(ctx context.Context, question string, png []byte, mime string) (string, error)

// holdEnvelope is the model-visible yield message returned while a human holds the
// browser. Directive enough that the model gracefully ends the turn (中文 prompt).
const holdEnvelope = "人正在通过接管手动操控这个浏览器，本轮先暂停浏览器操作；请优雅收尾，等人完成后会收到继续提示。"

// NewTool builds the single browser tool bound to c. lib is the optional URL library
// thunk (nil-safe). holds is the human control-hold gate (nil-safe → never held). vision
// answers browser_vision over a vision-capable model (nil → screenshot-only fallback).
func NewTool(c *Client, lib func() contract.URLLibrary, holds contract.BrowserControlHold, vision VisionFunc) contract.Tool {
	return &browserTool{c: c, lib: lib, navigated: make(map[string]bool), holds: holds, vision: vision}
}

// Serialize is TRUE: all actions drive ONE shared remote page per scope, so
// concurrent actions against it must be ordered (skeleton's 13 tools were each
// non-serialized — flagged as a concern; collapsing to one serialized tool fixes
// it). The browserd side is stateful toward a scope; parallel actions would race
// the same tab.
func (*browserTool) Serialize() bool { return true }

// masterKey is the LOGIN STORE axis — the per-MEMBER master profile a worker's copy is
// seeded from / written back to (docs/wip-browser-profile-identity.md). The flow stamps the
// member id (agora member / normal account, contract.MemberFrom); it falls back to the
// PRINCIPAL (per-role, contract.IdentityFrom — Phase-1's axis) until the per-member stamp is
// wired, then the scope, then the sid. "留空间": same role, different members may hold
// different site logins, so the store is per-member, not per-role.
func masterKey(ctx context.Context, tc contract.ToolContext) string {
	if m, ok := contract.MemberFrom(ctx); ok && m != "" {
		return m
	}
	if id, ok := contract.IdentityFrom(ctx); ok && id != "" {
		return id
	}
	if tc.Scope != "" {
		return tc.Scope
	}
	return tc.Sid
}

// CopyScope is the per-SCOPE instance axis — which worker copy. Concurrent workers are
// different scopes (their own chromium + dir); a scope is single-active/serial in bob. The
// human takes over THIS copy (browserd's takeover is scope-keyed), so escalate_to_coo keys
// its token / hold by CopyScope. Falls back to the sid outside a routed turn.
func CopyScope(tc contract.ToolContext) string {
	if tc.Scope != "" {
		return tc.Scope
	}
	return tc.Sid
}

// common routes a WORKER action: a read-only per-SCOPE COPY (Mode "copy") seeded from the
// member's master profile. Scope = the worker's scope (the copy instance — concurrent
// workers each get their own chromium + dir), ProfileKey = the member (which master to seed
// the login from + write back to). The copy is read-only to the worker; only a human-driven
// copy (taken over via escalate_to_coo, logged in, then 保存登陆) writes back to the master.
func (*browserTool) common(ctx context.Context, tc contract.ToolContext) Common {
	return Common{Scope: CopyScope(tc), ProfileKey: masterKey(ctx, tc), Mode: "copy"}
}

// maxNavigatedSids bounds the navigated set. There is no session-end hook on
// trunk to evict a finished sid's flag, so the map would grow per distinct sid
// for the process lifetime (B-15). At the cap drop it wholesale: the flag is only
// a pre-navigate HINT, so a lost entry costs at most one spurious "navigate first"
// (the model re-navigates and re-sets it) — never correctness. The cap is far
// above any plausible concurrent-session count.
const maxNavigatedSids = 1024

func (t *browserTool) markNavigated(sid string) {
	t.mu.Lock()
	if len(t.navigated) >= maxNavigatedSids {
		t.navigated = make(map[string]bool)
	}
	t.navigated[sid] = true
	t.mu.Unlock()
}

func (t *browserTool) isNavigated(sid string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.navigated[sid]
}

// browserParams is the union schema: one `action` enum plus every per-action
// field, each tagged with the action(s) it belongs to ([action X only] style,
// mirroring file_storage's "[verb X only]" and scrapling's per-mode tagging). The
// per-action 中文 guidance is pulled verbatim from the 13 skeleton tools' specs so
// no wording is lost. NOTE: browser_tab's own internal sub-action (list/switch/
// close) is exposed as `tab_action` to avoid colliding with the top-level
// `action` key.
const browserParams = `{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["browser_navigate", "browser_snapshot", "browser_click", "browser_type", "browser_press", "browser_scroll", "browser_back", "browser_console", "browser_get_images", "browser_dialog", "browser_vision", "browser_tab"],
      "description": "要执行的浏览器动作：\n  - browser_navigate：启动/复用本 session 的 chromium，加载 url（取内容的最后手段 / 登录交互的入口）。必须先 browser_navigate 才能用其它动作。\n  - browser_snapshot：读当前页面（format=tree/html/text/image）。\n  - browser_click：点击元素（ref 优先，selector fallback）。\n  - browser_type：向输入框输入 text（ref/selector + replace）。\n  - browser_press：发送单个按键 key（Enter/Tab/Escape/方向键…）。\n  - browser_scroll：滚动页面（direction + pixels，或 selector 滚进视口）。\n  - browser_back：浏览器历史后退一步。\n  - browser_console：读浏览器 console 消息（level + max_entries）。\n  - browser_get_images：列出当前页面所有 <img>（selector + max_images）。\n  - browser_dialog：预设下一个 JS 对话框响应（accept + prompt_text）。\n  - browser_vision：截当前页面。配了 vision 模型时把截图+question 发给它、返回文字回答 answer；同时把截图存到 screenshots/ 并返回 path（有文件空间时）。要把截图发给用户就用这个 path（或用 file read 复用）。视觉判断（验证码 / 图表 / 布局 / 「把页面截图发我」）用它，文字 snapshot 解决不了的才上。\n  - browser_tab：管理标签页（tab_action=list/switch/close + index）。"
    },
    "url": {
      "type": "string",
      "description": "[action=browser_navigate] 要加载的 http:// 或 https:// URL。浏览器单会话，此 URL 替换当前 tab。返回页面 title + 紧凑的 accessibility-tree snapshot，含 @e1 / @e2 / ... ref id 给 click / type 用。"
    },
    "format": {
      "type": "string",
      "enum": ["tree", "html", "text", "image"],
      "description": "[action=browser_snapshot] tree（默认，Hermes 兼容）：accessibility-tree 文本，带 @e1 / @e2 ref id 供 click/type 用。html：原始 outer HTML。text：innerText。image：PNG 存到 screenshots/ 下，返回相对路径（可直接传给 file read）。"
    },
    "ref": {
      "type": "string",
      "description": "[action=browser_click/browser_type/browser_press] 前一个 snapshot / navigate 输出里的 ref id（如 \"@e5\"）。优先用 ref。press 时按键前先聚焦该元素。"
    },
    "selector": {
      "type": "string",
      "description": "CSS selector。[browser_click/browser_type/browser_press] 没 ref 时的 fallback（如 \"#login\"、\"input[name=q]\"）。[browser_snapshot] 把 tree/html/text 限定在某子树（image 模式忽略）。[browser_scroll] 把该元素滚进视口（替代 direction）。[browser_get_images] 限定查找范围。"
    },
    "text": {
      "type": "string",
      "description": "[action=browser_type] 要输入的文本。replace=true 先清空再输；默认追加。"
    },
    "replace": {
      "type": "boolean",
      "description": "[action=browser_type] true = 先清空字段再输入。默认 false。"
    },
    "key": {
      "type": "string",
      "description": "[action=browser_press] 按键名。常用：\"Enter\"、\"Tab\"、\"Escape\"、\"Backspace\"、\"ArrowDown\"、\"PageDown\"。单个可打印字符也可。"
    },
    "direction": {
      "type": "string",
      "enum": ["up", "down", "top", "bottom"],
      "description": "[action=browser_scroll] up/down 按 pixels 滚（默认 500）。top/bottom 跳到页首 / 页尾。"
    },
    "pixels": {
      "type": "integer",
      "description": "[action=browser_scroll] direction=up/down 时滚多少像素。默认 500。top/bottom 时忽略。"
    },
    "level": {
      "type": "string",
      "enum": ["all", "error", "warning", "log"],
      "description": "[action=browser_console] 按级别过滤。默认 \"error\"（最有诊断价值）。"
    },
    "max_entries": {
      "type": "integer",
      "description": "[action=browser_console] 返回条目上限。默认 50。"
    },
    "max_images": {
      "type": "integer",
      "description": "[action=browser_get_images] 返回图片上限。默认 50。"
    },
    "accept": {
      "type": "boolean",
      "description": "[action=browser_dialog] true = OK / Yes；false = Cancel / No。默认 true。"
    },
    "prompt_text": {
      "type": "string",
      "description": "[action=browser_dialog] 仅用于 prompt() 对话框——要填入的文本。"
    },
    "question": {
      "type": "string",
      "description": "[action=browser_vision] 要让 vision 模型回答的问题（如 \"页面上有什么\"、\"价格是多少\"、\"哪个按钮是登录\"）。默认 \"简要描述这张截图\"。"
    },
    "tab_action": {
      "type": "string",
      "enum": ["list", "switch", "close"],
      "description": "[action=browser_tab] list 列出所有标签页；switch 切到 index 指定的标签；close 关闭 index 指定的标签。"
    },
    "index": {
      "type": "integer",
      "description": "[action=browser_tab] 1-based 标签序号（tab_action=switch / close 必填），对应 list 返回的 index。"
    }
  },
  "required": ["action"]
}`

// browserDescription is the single tool's model-facing description. It folds in
// the navigate entry-point guidance (the skeleton browser_navigate spec's two
// use-cases) plus the action map, since navigate is the gate to everything else.
const browserDescription = "远程浏览器：单工具多动作（action 参数）。启动/复用本 session 的 chromium，加载页面并交互。两类用途：\n" +
	"  1. URL 取内容的**最后手段** —— 前面 scrapling 4 mode（get/stealthy-fetch/fetch/post）都过不了才用\n" +
	"  2. 登录站点 / 多步交互的**入口** —— action=browser_navigate 成功后用 browser_snapshot/browser_click/browser_type/browser_scroll/browser_back/browser_dialog/browser_get_images/browser_vision/browser_console/browser_tab 继续操作\n" +
	"**必须先 action=browser_navigate 打开页面，其它动作才可用。** browser_navigate 返回页面 title + 紧凑 a11y-tree snapshot（带 @e1 / @e2 ref id 给后续 browser_click/browser_type 用）。Cookies 持久到显式 `/new` 才清；中间 idle / 重启都保留。\n" +
	"**任何交互后页面可能重渲染——之后必须重新 action=browser_snapshot，ref id 是 per-snapshot 的，不跨重渲染**。"

func (t *browserTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name:           "browser",
		Description:    browserDescription,
		Parameters:     json.RawMessage(browserParams),
		NoAutoCompress: true,
		SelectionHint: &contract.SelectionHint{
			When:     `要取当前页内容（原始 HTML / 文本 / 截图）、读取页面元素，或登录站点 / 多步交互`,
			Then:     `先 browser action=browser_navigate 打开页面，再用 action=browser_snapshot（format=html/text/image，可加 selector 限定子树）/ browser_click / browser_type 等动作`,
			Priority: 60,
		},
	}
}

// browserArgs is the union of every per-action field. Run unmarshals once and the
// per-action switch reads the fields it needs.
type browserArgs struct {
	Action string `json:"action"`

	URL string `json:"url"` // navigate

	Format   string `json:"format"`   // snapshot
	Selector string `json:"selector"` // snapshot/click/type/press/scroll/get_images

	Ref     string `json:"ref"`     // click/type/press
	Text    string `json:"text"`    // type
	Replace bool   `json:"replace"` // type
	Key     string `json:"key"`     // press

	Direction string `json:"direction"` // scroll
	Pixels    int    `json:"pixels"`    // scroll

	Level      string `json:"level"`       // console
	MaxEntries int    `json:"max_entries"` // console
	MaxImages  int    `json:"max_images"`  // get_images

	Accept     *bool  `json:"accept"`      // dialog
	PromptText string `json:"prompt_text"` // dialog

	Question string `json:"question"` // vision

	TabAction string `json:"tab_action"` // tab
	Index     int    `json:"index"`      // tab
}

func (t *browserTool) Run(ctx context.Context, tc contract.ToolContext, raw json.RawMessage) contract.ToolResult {
	var p browserArgs
	if err := json.Unmarshal(raw, &p); err != nil {
		return contract.ErrResult("browser: invalid arguments: " + err.Error())
	}
	// Backend gate: the tool is ALWAYS registered (visible) so the model knows the
	// capability exists; if browserd was never wired (BROWSERD_URL unset) say so
	// plainly rather than attempting a doomed connection — the honest analogue of the
	// old "register only when configured" that silently hid the tool.
	if !t.c.Configured() {
		return contract.ErrResult("browser: 浏览器后端（browserd）未接入，当前无法使用浏览器。").
			WithHint("需要管理员配置 BROWSERD_URL 指向运行中的 browserd 服务后才能用")
	}
	// Human control-hold gate (browser takeover): while a human drives this sid's
	// browser in control mode, yield rather than fight over the page (advisory — bob
	// is browserd's only client). The lease self-expires, so this can't wedge forever.
	// Checked per ACTION: an already-in-flight action isn't interrupted (a remote
	// browserd op can't be cancelled without browserd cooperation, same as skeleton);
	// the gate stops the NEXT one — fine for the handoff flow, where bob is idle.
	if t.holds != nil && t.holds.Held(CopyScope(tc)) {
		return contract.ErrResult(holdEnvelope)
	}

	action := strings.TrimSpace(p.Action)
	if action == "" {
		return contract.ErrResult("browser: missing action").
			WithHint("action ∈ browser_navigate/browser_snapshot/browser_click/browser_type/browser_press/browser_scroll/browser_back/browser_console/browser_get_images/browser_dialog/browser_vision/browser_tab")
	}

	// SELF-GATING: every action except navigate needs a prior successful navigate
	// on this sid. Refuse WITHOUT touching browserd (replaces skeleton's
	// SpecForSession visibility gate with a runtime refusal — the tool stays
	// visible, but interactive actions are dark until the page is open).
	if action != "browser_navigate" && !t.isNavigated(CopyScope(tc)) {
		return contract.ErrResult("browser: 浏览器尚未打开页面，无法执行 action=" + action).
			WithHint("先用 action=browser_navigate 打开页面")
	}

	switch action {
	case "browser_navigate":
		return t.runNavigate(ctx, tc, p)
	case "browser_snapshot":
		return t.runSnapshot(ctx, tc, p)
	case "browser_click":
		return t.runClick(ctx, tc, p)
	case "browser_type":
		return t.runType(ctx, tc, p)
	case "browser_press":
		return t.runPress(ctx, tc, p)
	case "browser_scroll":
		return t.runScroll(ctx, tc, p)
	case "browser_back":
		return t.runBack(ctx, tc, p)
	case "browser_console":
		return t.runConsole(ctx, tc, p)
	case "browser_get_images":
		return t.runGetImages(ctx, tc, p)
	case "browser_dialog":
		return t.runDialog(ctx, tc, p)
	case "browser_vision":
		return t.runVision(ctx, tc, p)
	case "browser_tab":
		return t.runTab(ctx, tc, p)
	default:
		return contract.ErrResult("browser: unknown action: " + action).
			WithHint("action ∈ browser_navigate/browser_snapshot/browser_click/browser_type/browser_press/browser_scroll/browser_back/browser_console/browser_get_images/browser_dialog/browser_vision/browser_tab")
	}
}

// --- action=navigate -----------------------------------------------------

func (t *browserTool) runNavigate(ctx context.Context, tc contract.ToolContext, p browserArgs) contract.ToolResult {
	var res navigateResult
	if err := t.c.post(ctx, "/navigate", navigateReq{Common: t.common(ctx, tc), URL: p.URL}, &res); err != nil {
		// DNS-hint classification applies to ACTION errors only (browserd answered
		// ok:false with a page-level failure). Transport errors (browserd itself
		// unreachable) carry the same net-error substrings but say nothing about
		// the target URL — hinting there would steer the model into a retry loop
		// against a dead service.
		if !isTransportError(err) && isDNSClass(err.Error()) {
			return contract.ErrResult(err.Error()).WithHint(dnsHint)
		}
		return contract.ErrResult(err.Error())
	}
	// Success → this sid may now run interactive actions (self-gate).
	t.markNavigated(CopyScope(tc))
	// Signal ① — a successful navigate is a real fetch of res.URL. Record it (with
	// the page title) into the URL library so future searches can recall it.
	// excerpt="" — navigate yields a title but no snippet; the query anchor is the
	// user's turn text (skeleton's actx.Event.Text). No-op when the library is absent.
	if t.lib != nil {
		if lib := t.lib(); lib != nil {
			lib.Record(ctx, tc.Sid, res.URL, res.Title, "", tc.UserText)
		}
	}
	return jsonOK(map[string]any{
		"url":      res.URL,
		"title":    res.Title,
		"ready":    res.Ready,
		"snapshot": res.Snapshot,
	})
}

// --- action=snapshot -----------------------------------------------------

func (t *browserTool) runSnapshot(ctx context.Context, tc contract.ToolContext, p browserArgs) contract.ToolResult {
	var res snapshotResult
	req := snapshotReq{Common: t.common(ctx, tc), Format: p.Format, Selector: p.Selector}
	if err := t.c.post(ctx, "/snapshot", req, &res); err != nil {
		return contract.ErrResult(err.Error())
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
	if len(res.Image) > 0 {
		// format=image: browserd returns PNG BYTES (two-step shape, same as
		// /vision). ADAPTATION: skeleton wrote them into bob's per-session
		// file.SessionSandbox/screenshots/; trunk has no sandbox seam in
		// ToolContext, so the bytes are written through the turn's default file
		// CHANNEL under screenshots/ (the same backend `file` read/write uses).
		// The path is returned only AFTER the write succeeds — never a lie.
		if tc.Channels == nil {
			return contract.ErrResult("browser snapshot: format=image needs a file space (none wired this turn)")
		}
		ch, err := tc.Channels.OpenFile(ctx, "")
		if err != nil {
			return contract.ErrResult("browser snapshot: open space: " + err.Error())
		}
		defer ch.Close()
		if err := ch.Mkdir(ctx, "screenshots"); err != nil {
			return contract.ErrResult("browser snapshot: mkdir screenshots: " + err.Error())
		}
		name := fmt.Sprintf("screenshots/snap-%d.png", nowNano())
		if err := ch.Write(ctx, name, res.Image); err != nil {
			return contract.ErrResult("browser snapshot: write screenshot: " + err.Error())
		}
		out["path"] = name
	} else if res.ScreenshotPath != "" {
		out["path"] = res.ScreenshotPath
	}
	return jsonOK(out)
}

// --- action=click --------------------------------------------------------

func (t *browserTool) runClick(ctx context.Context, tc contract.ToolContext, p browserArgs) contract.ToolResult {
	if err := t.c.post(ctx, "/click", clickReq{Common: t.common(ctx, tc), Ref: p.Ref, Selector: p.Selector}, nil); err != nil {
		return contract.ErrResult(err.Error())
	}
	return jsonOK(map[string]any{"ref": p.Ref, "selector": p.Selector, "clicked": true})
}

// --- action=type ---------------------------------------------------------

func (t *browserTool) runType(ctx context.Context, tc contract.ToolContext, p browserArgs) contract.ToolResult {
	req := typeReq{Common: t.common(ctx, tc), Ref: p.Ref, Selector: p.Selector, Text: p.Text, Replace: p.Replace}
	if err := t.c.post(ctx, "/type", req, nil); err != nil {
		return contract.ErrResult(err.Error())
	}
	return jsonOK(map[string]any{
		"ref": p.Ref, "selector": p.Selector, "typed_chars": len(p.Text), "replaced": p.Replace,
	})
}

// --- action=press --------------------------------------------------------

func (t *browserTool) runPress(ctx context.Context, tc contract.ToolContext, p browserArgs) contract.ToolResult {
	req := pressReq{Common: t.common(ctx, tc), Key: p.Key, Ref: p.Ref, Selector: p.Selector}
	if err := t.c.post(ctx, "/press", req, nil); err != nil {
		return contract.ErrResult(err.Error())
	}
	return jsonOK(map[string]any{"key": p.Key, "pressed": true})
}

// --- action=scroll -------------------------------------------------------

func (t *browserTool) runScroll(ctx context.Context, tc contract.ToolContext, p browserArgs) contract.ToolResult {
	req := scrollReq{Common: t.common(ctx, tc), Direction: p.Direction, Pixels: p.Pixels, Selector: p.Selector}
	if err := t.c.post(ctx, "/scroll", req, nil); err != nil {
		return contract.ErrResult(err.Error())
	}
	return jsonOK(map[string]any{
		"direction": p.Direction, "pixels": p.Pixels, "selector": p.Selector, "scrolled": true,
	})
}

// --- action=back ---------------------------------------------------------

func (t *browserTool) runBack(ctx context.Context, tc contract.ToolContext, _ browserArgs) contract.ToolResult {
	var res backResult
	if err := t.c.post(ctx, "/back", backReq{Common: t.common(ctx, tc)}, &res); err != nil {
		return contract.ErrResult(err.Error())
	}
	return jsonOK(map[string]any{"url": res.URL, "title": res.Title})
}

// --- action=console ------------------------------------------------------

func (t *browserTool) runConsole(ctx context.Context, tc contract.ToolContext, p browserArgs) contract.ToolResult {
	level := p.Level
	if level == "" {
		level = "error"
	}
	maxEntries := p.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 50
	}
	var res consoleData
	req := consoleReq{Common: t.common(ctx, tc), Level: level, MaxEntries: maxEntries}
	if err := t.c.post(ctx, "/console", req, &res); err != nil {
		return contract.ErrResult(err.Error())
	}
	entries := res.Entries
	if entries == nil {
		entries = []consoleEntry{}
	}
	return jsonOK(map[string]any{"level": level, "entries": entries, "entry_cnt": len(entries)})
}

// --- action=get_images ---------------------------------------------------

func (t *browserTool) runGetImages(ctx context.Context, tc contract.ToolContext, p browserArgs) contract.ToolResult {
	var res imagesData
	req := getImagesReq{Common: t.common(ctx, tc), Selector: p.Selector, MaxImages: p.MaxImages}
	if err := t.c.post(ctx, "/get-images", req, &res); err != nil {
		return contract.ErrResult(err.Error())
	}
	imgs := res.Images
	if imgs == nil {
		imgs = []imageInfo{}
	}
	return jsonOK(map[string]any{"selector": p.Selector, "images": imgs, "image_cnt": len(imgs)})
}

// --- action=dialog -------------------------------------------------------

func (t *browserTool) runDialog(ctx context.Context, tc contract.ToolContext, p browserArgs) contract.ToolResult {
	accept := true
	if p.Accept != nil {
		accept = *p.Accept
	}
	req := dialogReq{Common: t.common(ctx, tc), Accept: accept, PromptText: p.PromptText}
	if err := t.c.post(ctx, "/dialog", req, nil); err != nil {
		return contract.ErrResult(err.Error())
	}
	return jsonOK(map[string]any{"accept": accept, "prompt_text": p.PromptText, "primed": true})
}

// --- action=vision -------------------------------------------------------
//
// Two-step: POST /vision to browserd for the screenshot PNG (browserd runs no
// model — it only screenshots), then answer the question with a vision-capable
// model bob-side (t.vision, wired from bob's pool at construction; the skeleton's
// actx.ModelPool leg, re-enabled via an injected seam rather than a ModelPool on
// ToolContext). On no vision model / a model error, degrade to saving the PNG to
// the file space with a clear note so the caller can still read it back.
func (t *browserTool) runVision(ctx context.Context, tc contract.ToolContext, p browserArgs) contract.ToolResult {
	q := strings.TrimSpace(p.Question)
	if q == "" {
		q = "简要描述这张截图。"
	}
	// Step 1 — screenshot from browserd.
	var shot visionData
	if err := t.c.post(ctx, "/vision", visionReq{Common: t.common(ctx, tc)}, &shot); err != nil {
		return contract.ErrResult(err.Error())
	}
	png, err := decodeB64(shot.ImageB64)
	if err != nil {
		return contract.ErrResult("browser vision: malformed screenshot payload: " + err.Error())
	}
	// Step 2 — answer the question over a vision-capable model.
	if t.vision != nil {
		answer, verr := t.vision(ctx, q, png, "image/png")
		if verr != nil {
			slog.Warn("browser vision: model leg failed — falling back to screenshot save", "sid", tc.Sid, "err", verr)
		} else if a := strings.TrimSpace(answer); a != "" {
			// Answer succeeded — ALSO persist the shot so "把这张截图发给我 / 存下来"
			// works end-to-end (the caller hands the path to deliver_file / file read).
			// Best-effort: a missing file space just omits the path; the answer still
			// returns. This is the fix for the vision-only dead loop where the model
			// kept re-calling browser_vision waiting for a file path it never got.
			out := map[string]any{"question": q, "answer": a}
			if name, serr := saveShot(ctx, tc, png); serr == nil {
				out["path"] = name
				out["image_size"] = len(png)
			} else {
				slog.Warn("browser vision: answer ok but screenshot save failed", "sid", tc.Sid, "err", serr)
			}
			return jsonOK(out)
		} else {
			slog.Warn("browser vision: model returned empty answer — falling back to screenshot save", "sid", tc.Sid)
		}
	}
	// Fallback — no model (or it failed/emptied): the path is all the caller gets,
	// so a save failure is fatal here (surface it) rather than best-effort.
	name, serr := saveShot(ctx, tc, png)
	if serr != nil {
		if tc.Channels == nil {
			return contract.ErrResult("browser vision: 截图成功但本轮没有文件空间可写入，且 vision 模型不可用")
		}
		return contract.ErrResult("browser vision: " + serr.Error())
	}
	return jsonOK(map[string]any{
		"question":   q,
		"path":       name,
		"image_size": len(png),
		"note":       "截图已保存到上述路径。vision 模型暂不可用（未接入或调用失败），请用 file read 取回图片自行判断。",
	})
}

// saveShot persists a screenshot PNG into THIS turn's file space (the same
// space backend file/deliver_file/read_file use, so the returned path is
// immediately deliverable) under screenshots/, returning the space-relative
// path. Shared by browser_vision's answer path (best-effort) and its no-model
// fallback (which surfaces the error). The bytes are written bob-side on
// purpose: browserd only screenshots — a browserd-side write would land on the
// wrong host and deliver_file could never see it.
func saveShot(ctx context.Context, tc contract.ToolContext, png []byte) (string, error) {
	if len(png) == 0 {
		// browserd returned an empty/absent PNG (screenshot failed but HTTP 200):
		// don't persist a 0-byte file + hand back a path that delivers a corrupt image.
		return "", fmt.Errorf("截图为空，未保存")
	}
	if tc.Channels == nil {
		return "", fmt.Errorf("本轮没有文件空间可写入")
	}
	ch, err := tc.Channels.OpenFile(ctx, "")
	if err != nil {
		return "", fmt.Errorf("open space: %w", err)
	}
	defer ch.Close()
	if err := ch.Mkdir(ctx, "screenshots"); err != nil {
		return "", fmt.Errorf("mkdir screenshots: %w", err)
	}
	name := fmt.Sprintf("screenshots/vision-%d.png", nowNano())
	if err := ch.Write(ctx, name, png); err != nil {
		return "", fmt.Errorf("write screenshot: %w", err)
	}
	return name, nil
}

// --- action=tab ----------------------------------------------------------

func (t *browserTool) runTab(ctx context.Context, tc contract.ToolContext, p browserArgs) contract.ToolResult {
	var raw json.RawMessage
	req := tabReq{Common: t.common(ctx, tc), Action: p.TabAction, Index: p.Index}
	if err := t.c.post(ctx, "/tab", req, &raw); err != nil {
		return contract.ErrResult(err.Error())
	}
	return jsonOK(raw)
}
