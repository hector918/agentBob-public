// Package promptlib is the corpus_import tool: a thin, server-side-authenticated
// client that imports structured literary text into the promptlib (hygpo) 素材库.
// The library chunks + embeds the text; a later semantic search surfaces snippets
// as material for music/image generation. This tool only IMPORTS — search is a
// separate service (/v1/search), not wired here.
//
// WHY A TOOL, not a curl in the model's shell: the import key is a bob-process
// secret, and the model-controlled `terminal` shell builds its env from a
// whitelist that deliberately excludes bob's secrets (leaf/warrant/60-localexec.go,
// docs/security-assumptions.md §5.2/§11). So an authenticated call must be made
// bob-side, where the key lives and never reaches the model — exactly the
// wordpress_content pattern (leaf/tools/wordpress).
//
// CONFIG = process env, NOT a per-identity vault credential: promptlib is ONE
// shared internal service (one endpoint, one import key, same for every caller) —
// infrastructure config like $BROWSERD_URL, not a per-member site. Read at use
// time so an admin can set it without a rebuild:
//
//	PROMPTLIB_BASE        library base URL (default http://127.0.0.1:8099)
//	PROMPTLIB_IMPORT_KEY  import key → X-Import-Key header (REQUIRED; unset → the
//	                      tool reports the feature is unconfigured, never a 401)
//
// The key opens ONLY the import endpoint and never appears in any tool output.
package promptlib

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
)

// defaultBase is the internal library address used when PROMPTLIB_BASE is unset.
// Overridable by env — baked in only so a standard deploy needs to set just the key.
const defaultBase = "http://127.0.0.1:8099"

const (
	// importPath is the single endpoint this tool speaks to (base + this).
	importPath = "/v1/contents/text"
	// maxItems / maxPayloadBytes mirror the server's per-call limits so an oversized
	// batch is rejected here with a "split by 集/卷" message instead of a wire 413.
	maxItems        = 50000
	maxPayloadBytes = 16 << 20 // 16 MiB
	// maxFileBytes bounds a space file read before parsing. The API caps a batch at
	// 16 MiB, but a pretty-printed source file can be larger; allow headroom and let
	// the post-marshal maxPayloadBytes check enforce the real wire limit.
	maxFileBytes = 24 << 20 // 24 MiB
	// requestTimeout caps one import. Upload can be up to 16 MiB; embedding is async
	// server-side (the 201 returns before it runs), so 60s is generous headroom.
	requestTimeout = 60 * time.Second
	// maxRespBytes bounds the response body — it's just the content JSON (id/caption).
	maxRespBytes = 1 << 20
	// maxAttempts bounds 429 retries (imports are idempotent, so retry is safe).
	maxAttempts = 3
	// maxRetryWait caps how long one 429 backoff may sleep, whatever Retry-After says,
	// so a hostile/large Retry-After can't park the turn.
	maxRetryWait = 15 * time.Second
)

// sharedHTTP is one client (one connection pool) reused across calls — a fresh
// client per call would drop keep-alive across a multi-batch import turn.
var sharedHTTP = &http.Client{Timeout: requestTimeout}

// item is one retrieval unit: an optional 「题目 · 作者」heading (embedded with the
// body so "李白写月亮的诗" can hit it) and the verbatim body. One poem/一阕词/one
// self-contained passage = one item — the server never splits an item.
type item struct {
	Heading string `json:"heading,omitempty"`
	Body    string `json:"body"`
}

// importReq is the request body: the whole-batch caption (集名+来源+采集时间) plus
// the items. It doubles as the tool's input shape — the model builds it directly.
type importReq struct {
	Caption string `json:"caption"`
	Items   []item `json:"items"`
}

// corpusImport is the tool. Stateless: config is read from env per call, so an
// admin can set PROMPTLIB_IMPORT_KEY without a rebuild.
type corpusImport struct{}

// New builds the corpus_import tool. ALWAYS registered so the capability stays
// visible; an unset PROMPTLIB_IMPORT_KEY makes Run report "未配置" rather than
// hiding the tool from the catalog.
func New() contract.Tool { return corpusImport{} }

// Serialize: imports are idempotent and additive (independent server-side rows),
// so parallel calls in one round are safe — no shared local state to guard.
func (corpusImport) Serialize() bool { return false }

const paramsSchema = `{
  "type": "object",
  "properties": {
    "file": {
      "type": "string",
      "description": "空间里一个规范 import JSON 文件的路径（内容为 {caption,items} 或 [items] 数组）。成规模的语料走这条：先用脚本把源数据转成这个文件落盘，别把正文内联进参数（会被截断）。"
    },
    "items": {
      "type": "array",
      "description": "内联检索单元数组，仅用于三五条的临时导入；成规模的用 file。一首诗/一阕词/一段独立文本 = 一个 item；别把多首合并成一个 item，也别把一首拆成多个。",
      "items": {
        "type": "object",
        "properties": {
          "heading": {"type": "string", "description": "「题目 · 作者」，会和正文一起被嵌入并作展示出处；没有题目/作者可省略。"},
          "body": {"type": "string", "description": "正文，保留原有换行，原文用字（繁体不转简体）。"}
        },
        "required": ["body"]
      }
    },
    "caption": {
      "type": "string",
      "description": "整包内容在库里的标签：集名 + 来源 + 采集时间，例：全宋词 · 苏轼卷（来源：github.com/chinese-poetry, 2026-07 采集）。file 内已含 caption 时可省略；给了则覆盖。"
    }
  }
}`

func (corpusImport) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "corpus_import",
		Description: "把整理好的文学语料（诗词/歌词/文学片段）导入 promptlib 素材库。" +
			"成规模的语料：先用脚本把源转成规范 import JSON 文件落盘，再给 file=文件路径（正文别内联进参数）；三五条临时的才用内联 items。整理规则和纪律查 corpus-import 技能。库自动切块嵌入，之后可被语义检索。",
		Parameters:     json.RawMessage(paramsSchema),
		NoAutoCompress: true, // 返回内容 id 要原样带给用户汇报，别被 summarize 糊掉
		SelectionHint: &contract.SelectionHint{
			When: `要把采集到的诗词/歌词/文学片段导入 promptlib(hygpo)素材库，为音乐/图片生成积累文字素材`,
			Then: `脚本把源转成规范 import JSON 文件落盘 → 用 corpus_import 工具 file=文件路径 导入；动手前先 skill_view 读 corpus-import 技能`,
		},
	}
}

func (corpusImport) Run(ctx context.Context, tc contract.ToolContext, raw json.RawMessage) contract.ToolResult {
	var in struct {
		File    string `json:"file"`
		Caption string `json:"caption"`
		Items   []item `json:"items"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return contract.ErrResult("corpus_import: 参数不是合法 JSON")
	}
	in.File = strings.TrimSpace(in.File)

	// Resolve the batch: exactly one of file / items. `file` is the primary path for
	// any real corpus — the model names a canonical import JSON it built with a
	// script, and we read it bob-side, so the (large) content never rides the
	// tool_call arguments (which truncate at the model's token limit). Inline `items`
	// is for a few ad-hoc entries only.
	var p importReq
	switch {
	case in.File != "" && len(in.Items) > 0:
		return contract.ErrResult("corpus_import: file 和 items 二选一 —— 成规模的用 file，三五条临时的用 items")
	case in.File != "":
		b, errRes := readImportFile(ctx, tc, in.File)
		if b == nil {
			return errRes
		}
		p = *b
	case len(in.Items) > 0:
		p = importReq{Items: in.Items}
	default:
		return contract.ErrResult("corpus_import: 要给 file（空间里的规范 import JSON 文件）或 items（内联，仅限少量）")
	}
	// caption from the arg overrides the file's; at least one source must supply it.
	if c := strings.TrimSpace(in.Caption); c != "" {
		p.Caption = c
	}
	p.Caption = strings.TrimSpace(p.Caption)
	if p.Caption == "" {
		return contract.ErrResult("corpus_import: 缺少 caption（集名+来源+采集时间）—— 用 caption 参数给，或写进 file")
	}
	if len(p.Items) == 0 {
		return contract.ErrResult("corpus_import: 没有 items —— 至少要有一个检索单元")
	}
	if len(p.Items) > maxItems {
		return contract.ErrResult(fmt.Sprintf("corpus_import: items 超过单次上限 %d 条，按集/卷分成多次导入", maxItems))
	}
	// Reject a blank body here (the server would 400) so the model fixes the data
	// instead of blind-retrying. Trim only checks blankness — the body sent stays
	// verbatim (the server normalizes internal whitespace; an item is never split).
	for i, it := range p.Items {
		if strings.TrimSpace(it.Body) == "" {
			return contract.ErrResult(fmt.Sprintf("corpus_import: 第 %d 个 item 的 body 是空白 —— 剔除或补齐后再导入", i+1))
		}
	}

	key := strings.TrimSpace(os.Getenv("PROMPTLIB_IMPORT_KEY"))
	if key == "" {
		return contract.ErrResult("corpus_import: 素材库导入未配置（PROMPTLIB_IMPORT_KEY 未设），请联系管理员")
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("PROMPTLIB_BASE")), "/")
	if base == "" {
		base = defaultBase
	}

	// Marshal once: the bytes are both the size check (mirror the server's 16 MiB
	// cap locally → a clean "split" message, not a wire 413) and the request body.
	payload, err := json.Marshal(p)
	if err != nil {
		return contract.ErrResult("corpus_import: 序列化请求失败：" + err.Error())
	}
	if len(payload) > maxPayloadBytes {
		return contract.ErrResult(fmt.Sprintf("corpus_import: 这一包 %.1f MiB 超过单次上限 16 MiB，按集/卷分成多次导入",
			float64(len(payload))/(1<<20)))
	}

	endpoint := base + importPath
	for attempt := 1; ; attempt++ {
		res, err := post(ctx, endpoint, key, payload)
		if err != nil {
			return contract.ErrResult("corpus_import: " + err.Error())
		}
		// 429: back off honoring Retry-After (capped) and retry — safe because the
		// import is idempotent. Exhaust attempts → fall through to classify.
		if res.status == http.StatusTooManyRequests && attempt < maxAttempts {
			wait := retryAfter(res.header, maxRetryWait)
			select {
			case <-ctx.Done():
				return contract.ErrResult("corpus_import: 已取消（等待限流重试时）")
			case <-time.After(wait):
			}
			continue
		}
		return classify(res)
	}
}

// readImportFile reads a canonical import file from the caller's default space and
// parses it into {caption, items}. The file is either an object {caption, items} or
// a bare [items] array (caption then comes from the tool arg). This is the path that
// keeps large content OFF the tool_call arguments: the model builds the file with a
// script and only names it. On any failure it returns (nil, <model-facing result>)
// so Run surfaces the reason verbatim.
func readImportFile(ctx context.Context, tc contract.ToolContext, path string) (*importReq, contract.ToolResult) {
	if tc.Channels == nil {
		return nil, contract.ErrResult("corpus_import: 当前没有可用的文件空间（读不到 file）")
	}
	ch, err := tc.Channels.OpenFile(ctx, "") // default space = this turn's scope
	if err != nil {
		return nil, contract.ErrResult("corpus_import: 打开空间失败：" + err.Error())
	}
	defer ch.Close()
	data, err := ch.Read(ctx, path)
	if err != nil {
		return nil, contract.ErrResult("corpus_import: 读不到文件 " + path + "：" + err.Error()).
			WithHint("先用脚本把源数据转成规范 import JSON 落盘，再把它的路径传给 file")
	}
	if len(data) > maxFileBytes {
		return nil, contract.ErrResult(fmt.Sprintf("corpus_import: 文件 %.1f MiB 太大（上限 24 MiB），按集/卷拆分导入",
			float64(len(data))/(1<<20)))
	}
	// Object form {caption,items} first; fall back to a bare [items] array. A valid
	// object with no items key leaves obj.Items nil → fall through → format error.
	var obj importReq
	if json.Unmarshal(data, &obj) == nil && obj.Items != nil {
		return &obj, contract.ToolResult{}
	}
	var items []item
	if json.Unmarshal(data, &items) == nil {
		return &importReq{Items: items}, contract.ToolResult{}
	}
	return nil, contract.ErrResult("corpus_import: 文件不是合法的 {caption,items} 或 [items] JSON —— 检查脚本产出").
		WithHint(`规范格式：{"caption":"…","items":[{"heading":"…","body":"…"}]}`)
}

// apiResult is one import call's raw outcome.
type apiResult struct {
	status int
	body   []byte
	header http.Header
}

// post sends the JSON payload to the import endpoint with the three required
// headers. A returned error is a transport failure (endpoint unreachable / read
// failure); a non-2xx HTTP reply rides back in apiResult for classify to surface.
func post(ctx context.Context, endpoint, key string, payload []byte) (*apiResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Import-Key", key)
	// The internal rate-limit middleware keys on CF-Connecting-IP; a LAN-direct call
	// has none, so send a placeholder (value only needs to be a valid IP).
	req.Header.Set("CF-Connecting-IP", "9.9.9.9")

	resp, err := sharedHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("素材库不可达（%v）", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败：%v", err)
	}
	return &apiResult{status: resp.StatusCode, body: raw, header: resp.Header}, nil
}

// classify turns one import outcome into a tool result. A 2xx returns the content
// JSON (id/caption) verbatim so the model can report the id; each error status
// carries a targeted hint so the model knows whether to fix-and-retry, split, or
// stop and report.
func classify(res *apiResult) contract.ToolResult {
	body := strings.TrimSpace(string(res.body))
	if res.status >= 200 && res.status < 300 {
		if body == "" {
			body = fmt.Sprintf(`{"ok":true,"status":%d}`, res.status)
		}
		return contract.OKResult(body)
	}
	msg := fmt.Sprintf("corpus_import: HTTP %d", res.status)
	if body != "" {
		msg += " — " + body
	}
	r := contract.ErrResult(msg)
	switch res.status {
	case http.StatusBadRequest: // 400
		return r.WithHint("items 为空 / 某 item body 空白 / JSON 格式错 —— 检查数据后重试，别盲目重发")
	case http.StatusUnauthorized: // 401
		return r.WithHint("导入 key 未配置或不对 —— 报告用户，不要重试")
	case http.StatusRequestEntityTooLarge: // 413
		return r.WithHint("包太大 —— 对半分包重试")
	case http.StatusTooManyRequests: // 429 after retries
		if ra := strings.TrimSpace(res.header.Get("Retry-After")); ra != "" {
			return r.WithHint("限流 —— Retry-After: " + ra + "，稍后重试（导入幂等，重试安全）")
		}
		return r.WithHint("限流 —— 稍后重试（导入幂等，重试安全）")
	}
	if res.status >= 500 {
		return r.WithHint("服务端错误 —— 停止并报告用户")
	}
	return r.WithHint("核对数据后重试（导入幂等）")
}

// retryAfter reads the Retry-After header (integer seconds or an HTTP date) and
// clamps the wait to [0, cap]. A missing/garbage value falls back to the cap so a
// 429 always backs off some bounded amount before retrying.
func retryAfter(h http.Header, limit time.Duration) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return limit
	}
	clamp := func(d time.Duration) time.Duration {
		switch {
		case d < 0:
			return 0
		case d > limit:
			return limit
		default:
			return d
		}
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return clamp(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return clamp(time.Until(t))
	}
	return limit
}
