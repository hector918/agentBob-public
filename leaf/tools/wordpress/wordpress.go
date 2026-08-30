package wordpress

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"agentbob/contract"
)

// params is the shared JSON Schema for both tools: a generic REST request shape.
// The detailed endpoint/field knowledge is NOT here (it would load into every
// prompt — tool descriptions have no on-demand tier); it lives in the matching
// skill, read on demand via skill_view.
const params = `{
  "type": "object",
  "properties": {
    "method": {
      "type": "string",
      "enum": ["GET", "POST", "PUT", "DELETE"],
      "description": "HTTP 方法：GET 查、POST 建、PUT 改、DELETE 删。"
    },
    "path": {
      "type": "string",
      "description": "完整 REST 路径，含 /wp-json 前缀，例：/wp-json/wc/v3/products 或 /wp-json/wc/v3/orders/123。端点和字段先查对应技能。"
    },
    "query": {
      "type": "object",
      "description": "可选 URL 查询参数，例：{\"per_page\": 20, \"search\": \"T恤\", \"status\": \"processing\"}。用 per_page / _fields 控制返回体积。"
    },
    "body": {
      "type": "object",
      "description": "可选 JSON 请求体，POST/PUT 时填，例：{\"name\": \"新商品\", \"regular_price\": \"99\"}。上传媒体时改填 {\"file\": \"inbox/图片名\", \"filename\": \"可选，默认取文件名\"}：工具会读空间里的这个文件以二进制上传到 /wp-json/wp/v2/media，返回 JSON 里有新媒体 id（title/alt_text 等元数据上传后再 POST /wp/v2/media/<id> 改）。"
    }
  },
  "required": ["method", "path"]
}`

// args is the decoded tool input, shared by both tools.
type args struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Query  map[string]any  `json:"query"`
	Body   json.RawMessage `json:"body"`
}

// wooTool is the WooCommerce store face: locked to the /wc/ namespace.
type wooTool struct{}

// wpTool is the WordPress core (blog/pages/media) face: locked to the /wp/ namespace.
type wpTool struct{}

// NewWooTool / NewWPTool build the two namespace-locked tools. Each resolves the
// caller's kind=wordpress credential from warrant at use time (via the
// identity-bound CredentialOpener), so the site is per-identity, not baked in.
func NewWooTool() contract.Tool { return wooTool{} }
func NewWPTool() contract.Tool  { return wpTool{} }

func (wooTool) Serialize() bool { return true } // writes to one live store — never concurrent
func (wpTool) Serialize() bool  { return true } // writes to one live site — never concurrent

func (wooTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "woocommerce",
		Description: "WooCommerce 网店 REST 工具：商品 / 订单 / 客户 / 优惠券 / 退款 / 报表等的增删改查。" +
			"给 method + path（如 /wp-json/wc/v3/products）+ 可选 query / body；端点和字段查 woocommerce_operation 技能。仅限 WooCommerce(/wc/) 端点。",
		Parameters:     json.RawMessage(params),
		NoAutoCompress: true, // 订单/商品带精确 ID、价格、库存，别被 summarize 糊掉；用 per_page/_fields 控体积
		SelectionHint: &contract.SelectionHint{
			When: `要操作 WooCommerce 网店：查/改商品、看订单、查客户、改库存价格、优惠券、退款、销售报表`,
			Then: `用 woocommerce 工具发 REST 请求（method+path+query+body）；动手前先 skill_view 读 woocommerce_operation 技能拿端点和字段`,
		},
	}
}

func (wpTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "wordpress_content",
		Description: "WordPress 内容 REST 工具：文章 / 页面 / 媒体 / 分类 / 标签 / 评论 等的增删改查。" +
			"给 method + path（如 /wp-json/wp/v2/posts）+ 可选 query / body；端点和字段查 wordpress_content 技能。仅限 WP core(/wp/) 端点，不含网店。",
		Parameters:     json.RawMessage(params),
		NoAutoCompress: true,
		SelectionHint: &contract.SelectionHint{
			When: `要操作 WordPress 网站内容：发/改博客文章、建页面、传媒体、管理分类标签评论`,
			Then: `用 wordpress_content 工具发 REST 请求（method+path+query+body）；动手前先 skill_view 读 wordpress_content 技能拿端点和字段`,
		},
	}
}

func (wooTool) Run(ctx context.Context, tc contract.ToolContext, raw json.RawMessage) contract.ToolResult {
	return run(ctx, tc, raw, "woocommerce", isWooPath,
		"woocommerce 只接受 WooCommerce(/wc/) 端点，例 /wp-json/wc/v3/products；博客/页面/媒体请用 wordpress_content 工具")
}

func (wpTool) Run(ctx context.Context, tc contract.ToolContext, raw json.RawMessage) contract.ToolResult {
	return run(ctx, tc, raw, "wordpress_content", isWPCorePath,
		"wordpress_content 只接受 WP core(/wp/) 端点，例 /wp-json/wp/v2/posts；网店商品/订单请用 woocommerce 工具")
}

// run is the shared request path: parse → validate method → namespace-lock the
// path → resolve the caller's credential → call → classify status. inNamespace is
// the per-tool lock that keeps the capability name honest (the security-relevant
// gate, not a hint). The *Client comes from warrant per identity (resolveClient),
// so the secret never reaches this code — it only calls c.do().
func run(ctx context.Context, tc contract.ToolContext, raw json.RawMessage, name string, inNamespace func(string) bool, lockMsg string) contract.ToolResult {
	var p args
	if err := json.Unmarshal(raw, &p); err != nil {
		return contract.ErrResult(name + ": 参数不是合法 JSON")
	}
	method := strings.ToUpper(strings.TrimSpace(p.Method))
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete:
	default:
		return contract.ErrResult(name + ": method 必须是 GET / POST / PUT / DELETE")
	}
	if strings.TrimSpace(p.Path) == "" {
		return contract.ErrResult(name + ": 缺少 path（完整路径，含 /wp-json 前缀）")
	}
	// Reject traversal dot-segments BEFORE classification/auth: the namespace lock
	// keys on the first segment, but a front-end normalizes "/wp-json/wp/v2/../wc/…"
	// to reach the other namespace after we've already authed as this one. No real
	// WP REST path has a dot-segment, so reject (never path.Clean-and-send — that
	// would auth as one namespace and hit another).
	if hasDotSegment(p.Path) {
		return contract.ErrResult(name + ": path 不能包含 . 或 .. 路径段（如 /wp-json/wp/v2/../wc/…）")
	}
	if !inNamespace(p.Path) {
		return contract.ErrResult(name + ": 路径不在本工具范围内 —— " + lockMsg)
	}
	c, errMsg := resolveClient(ctx, tc)
	if c == nil {
		return contract.ErrResult(name + ": " + errMsg)
	}

	// Upload fork: a `file` key in body is an instruction TO THE TOOL (which space
	// file to send), not a field to forward to WP — the generic JSON passthrough
	// can't carry binary, so this branch reads the file and sends it as raw bytes.
	if up := parseUpload(p.Body); up.File != "" {
		return runUpload(ctx, tc, c, method, p.Path, name, up)
	}

	res, err := c.do(ctx, method, p.Path, p.Query, p.Body)
	if err != nil {
		return contract.ErrResult(name + ": " + err.Error())
	}
	return classify(res, name)
}

// classify turns one REST outcome into a tool result: non-2xx rides back as an
// error carrying WP's {code,message} body (the model self-corrects from it); a 2xx
// with an empty body becomes a minimal ok envelope. Shared by the JSON passthrough
// and the upload path so status handling can't drift between them.
func classify(res *apiResult, name string) contract.ToolResult {
	bodyStr := strings.TrimSpace(string(res.body))
	if res.status < 200 || res.status >= 300 {
		msg := fmt.Sprintf("%s: HTTP %d", name, res.status)
		if bodyStr != "" {
			msg += " — " + bodyStr
		}
		return contract.ErrResult(msg).WithHint("核对 path / 字段 / 权限（端点与字段可 skill_view 对应技能）")
	}
	if bodyStr == "" {
		bodyStr = fmt.Sprintf(`{"ok":true,"status":%d}`, res.status)
	}
	return contract.OKResult(bodyStr)
}

// maxUploadBytes bounds a media upload. A photo is a few MB; 25 MiB leaves room for
// a high-res image while capping a runaway file before it reaches memory.
const maxUploadBytes = 25 << 20

// uploadArgs is the upload instruction carried in body: which space file to send,
// and an optional display filename (WP derives the attachment name/extension from
// it). These are consumed by the tool, NOT forwarded to WP.
type uploadArgs struct {
	File     string `json:"file"`
	Filename string `json:"filename"`
}

// parseUpload peeks the body for the `file` upload key. A body without it (or no
// body) yields an empty File, so the caller stays on the JSON passthrough.
func parseUpload(body json.RawMessage) uploadArgs {
	var up uploadArgs
	if len(body) > 0 {
		_ = json.Unmarshal(body, &up)
	}
	up.File = strings.TrimSpace(up.File)
	up.Filename = strings.TrimSpace(up.Filename)
	return up
}

// isMediaUploadPath reports whether path is the core media COLLECTION endpoint
// (POST /wp-json/wp/v2/media, no id) — the only place a binary upload makes sense.
// Query/fragment are stripped and a trailing slash tolerated; a path with an id
// (…/media/123, a metadata edit) is excluded so `file` can't hijack it.
func isMediaUploadPath(path string) bool {
	p := strings.TrimSpace(path)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimRight(strings.ToLower(p), "/")
	return strings.HasSuffix(p, "/wp-json/wp/v2/media")
}

// headerSafeFilename strips characters that would corrupt the quoted filename in
// the Content-Disposition header (double-quote, backslash) or inject a new header
// line (CR/LF/control). The filename is model-supplied, so it must be sanitized
// before going on the wire (net/http blocks CR/LF but not quotes/backslashes).
func headerSafeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// runUpload reads the named space file and sends it to the media endpoint as raw
// bytes via doUpload. The file reference is resolved through the turn's FileChannel
// (no raw paths): the user's images live at inbox/<name>; a file the tool produced
// earlier lives wherever it wrote it under the space. Status handling reuses
// classify, so a 2xx returns WP's JSON (with the new media id) verbatim.
func runUpload(ctx context.Context, tc contract.ToolContext, c *Client, method, path, name string, up uploadArgs) contract.ToolResult {
	if method != http.MethodPost {
		return contract.ErrResult(name + ": 上传媒体要用 POST")
	}
	if !isMediaUploadPath(path) {
		return contract.ErrResult(name + ": 上传只用于 POST /wp-json/wp/v2/media；改已有媒体的元数据用 POST /wp/v2/media/<id>（不带 file）")
	}
	if tc.Channels == nil {
		return contract.ErrResult(name + ": 当前没有可用的文件空间（读不到文件）")
	}
	ch, err := tc.Channels.OpenFile(ctx, "") // default space = this turn's scope
	if err != nil {
		return contract.ErrResult(name + ": 打开空间失败：" + err.Error())
	}
	defer ch.Close()

	// Read through the channel (backend-agnostic — works for a remote/sftp space
	// where there is no local path to stat) and bound by the bytes actually
	// returned; checking the real length avoids a stat/read TOCTOU. A directory or
	// missing file surfaces as a read error.
	data, err := ch.Read(ctx, up.File)
	if err != nil {
		return contract.ErrResult(name + ": 文件不存在或读取失败：" + err.Error())
	}
	if len(data) > maxUploadBytes {
		return contract.ErrResult(fmt.Sprintf("%s: 文件太大（上限 %dMB）", name, maxUploadBytes/(1024*1024)))
	}

	// Display name for WP, derived from the model's filename or the file's base.
	// Sanitized for the Content-Disposition header (it's model-supplied — a quote
	// or control char would corrupt/inject the header).
	fname := headerSafeFilename(up.Filename)
	if fname == "" {
		fname = headerSafeFilename(filepath.Base(up.File))
	}
	if fname == "" {
		fname = "upload"
	}

	// Content-Type from the actual bytes (authoritative — a file named .png that is
	// really a PDF must not go up as image/png). DetectContentType only knows the
	// common raster formats; when it can't tell (octet-stream, e.g. HEIC) fall back
	// to the filename extension. The result must be an image — else reject.
	ctype := http.DetectContentType(data)
	if ctype == "application/octet-stream" {
		if byExt := mime.TypeByExtension(filepath.Ext(fname)); byExt != "" {
			ctype = byExt
		}
	}
	if !strings.HasPrefix(ctype, "image/") {
		return contract.ErrResult(name + ": 只支持上传图片（识别到的类型：" + ctype + "）")
	}

	res, err := c.doUpload(ctx, path, data, ctype, fname)
	if err != nil {
		return contract.ErrResult(name + ": " + err.Error())
	}
	return classify(res, name)
}

// resolveClient asks warrant (via the identity-bound CredentialOpener) for the
// caller's kind=wordpress credential and returns the built *Client. On any failure
// it returns (nil, msg) with a model-facing reason — no warrant wired, no/many
// credentials authorized (warrant's by-kind-unique resolution), or a build error.
func resolveClient(ctx context.Context, tc contract.ToolContext) (*Client, string) {
	if tc.Credentials == nil {
		return nil, "凭证后端未接入，请联系管理员配置 WordPress 凭证"
	}
	built, err := tc.Credentials.Build(ctx, "wordpress")
	if err != nil {
		return nil, err.Error()
	}
	c, ok := built.(*Client)
	if !ok {
		return nil, "凭证类型不是 wordpress"
	}
	return c, ""
}
