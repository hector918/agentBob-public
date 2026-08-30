package epub

// 00-epub.go — the `epub` tool surface + dispatch + mode=read.
//
// read: take the epub the user sent this turn, parse its reading-order chapter
// list, and extract the chapter documents into the turn's space so the model
// can read them with the file tool (fs cat). No model call. translate/pack are a
// later slice (the translate model is a separate kind:translate backend).

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path"
	"path/filepath"
	"strings"

	"agentbob/contract"
)

const (
	// epubMaxFileBytes caps the uploaded epub (compressed) before it is opened.
	epubMaxFileBytes int64 = 100 << 20 // 100 MB
	// maxUnpackTotalBytes caps total decompressed chapter bytes written to the
	// space — an epub is user-supplied, and a zip bomb's decompressed size is
	// unbounded by its compressed size. 300 MB is far above any real book.
	maxUnpackTotalBytes int64 = 300 << 20
	// maxEntryUnpackBytes caps a SINGLE decompressed entry. zipBytes enforces it so
	// every reader (read/translate/pack) shares one bound — a single highly-
	// compressed entry can't OOM the process before a cumulative check fires.
	maxEntryUnpackBytes int64 = 100 << 20
)

const epubParams = `{
  "type": "object",
  "properties": {
    "mode": {
      "type": "string",
      "enum": ["read", "translate", "pack"],
      "description": "read：解包并列出章节（含 file 路径与 title），把各章抽进工作空间，不调用模型；translate：把整本书逐章翻译成 target_lang 并把译好的章节写入工作空间（不打包、不发送，已译的章重跑会跳过）；pack：把工作空间里（可能已被你修订过的）译好章节打包成新 epub 写入工作空间——再用 deliver_file 发送。"
    },
    "file_name": {
      "type": "string",
      "description": "可选：要处理的 epub 的文件名。本轮只有一个 epub 时可不填。translate 与 pack 须指向同一个原始 epub。"
    },
    "target_lang": {
      "type": "string",
      "description": "目标语言，mode=translate 与 mode=pack 时必填且须一致，例如 \"简体中文\" / \"English\" / \"日本語\"。"
    },
    "style": {
      "type": "string",
      "description": "可选，mode=translate 时生效：译文的文风/语气提示，例如 \"正式书面语\" / \"轻松口语\"。"
    }
  },
  "required": ["mode"]
}`

// ChatFunc runs one translate model call: system + user text → translated text.
// nil → the epub tool has no translate backend wired (read still works).
type ChatFunc func(ctx context.Context, system, user string) (string, error)

type epubTool struct {
	translate ChatFunc // bound to the KindTranslate backend; nil → translate/pack report unavailable
}

// New constructs the `epub` tool. translate is the KindTranslate-bound model call
// (nil → only mode=read works).
func New(translate ChatFunc) contract.Tool { return epubTool{translate: translate} }

func (epubTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "epub",
		Description: "读取或翻译 epub 电子书（epub 是 XHTML 的 ZIP 压缩包，普通读文件工具打不开）。" +
			"mode=read：解包用户本轮发来的 epub，返回按阅读顺序的章节列表（每章含 file 路径和标题），并把各章抽进工作空间——之后用文件工具按 file 逐章读取。不调用模型。" +
			"翻译分两步：先 translate，再 pack。" +
			"mode=translate：把整本书逐章翻译成 target_lang，把译好的章节写入工作空间（不打包、不发送）；结果会给出 work_dir，你应当用文件工具抽查若干章译文，必要时直接修订那些章节文件；已译的章重跑会自动跳过（断点续译）。可选 style 指定文风。" +
			"mode=pack：把工作空间里（含你修订过的内容）的译好章节打包成新 epub（保留 CSS/图片/字体）写入工作空间，给出 output_path——再用 deliver_file 把它发给用户。",
		Parameters:     json.RawMessage(epubParams),
		NoAutoCompress: true, // 章节列表 / 路径要原样返回
		SelectionHint: &contract.SelectionHint{
			When:     `用户发来 .epub 电子书，想读它 / 看有哪些章节 / 读某一章 / 把整本书翻译成另一种语言（"这本书讲什么 / 有哪些章节 / 读第 N 章 / 翻译这本书 / epub"）`,
			Then:     `epub mode=read 列章节并抽进工作空间（再用文件工具逐章读）；翻译走 translate→抽查→pack→deliver_file`,
			Priority: 30,
		},
	}
}

// Serialize: read extracts a whole book's chapters into the space — keep it off
// the parallel path so it doesn't race sibling file writes.
func (epubTool) Serialize() bool { return true }

func (t epubTool) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var p struct {
		Mode       string `json:"mode"`
		FileName   string `json:"file_name"`
		TargetLang string `json:"target_lang"`
		Style      string `json:"style"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return contract.ErrResult("epub: invalid arguments: " + err.Error())
	}
	switch strings.TrimSpace(p.Mode) {
	case "read", "":
		return t.runRead(ctx, tc, p.FileName)
	case "translate":
		if strings.TrimSpace(p.TargetLang) == "" {
			return contract.ErrResult("epub: mode=translate 需要 target_lang")
		}
		return t.runTranslate(ctx, tc, p.FileName, p.TargetLang, p.Style)
	case "pack":
		if strings.TrimSpace(p.TargetLang) == "" {
			return contract.ErrResult("epub: mode=pack 需要 target_lang（须与 translate 一致）")
		}
		return t.runPack(ctx, tc, p.FileName, p.TargetLang)
	default:
		return contract.ErrResult(fmt.Sprintf("epub: unknown mode %q (want read / translate / pack)", p.Mode))
	}
}

// openEpubZip anchors the epub attachment for this turn and opens it as a zip,
// resolving the space-relative Path to a real local file through the FileChannel
// (the epub lives in the scope space, placed at submit). The caller closes the
// returned ReadCloser. On any failure it returns an error ToolResult.
func (t epubTool) openEpubZip(ctx context.Context, ch contract.FileChannel, atts contract.AttachmentSet, fileName string) (contract.Attachment, *zip.ReadCloser, *contract.ToolResult) {
	att, errRes := pickEpub(ctx, atts, fileName)
	if errRes != nil {
		return contract.Attachment{}, nil, errRes
	}
	abs, err := ch.AbsPath(att.Path)
	if err != nil {
		r := contract.ErrResult("epub: 文件读不到")
		return contract.Attachment{}, nil, &r
	}
	if st, err := os.Stat(abs); err != nil {
		r := contract.ErrResult("epub: 文件读不到")
		return contract.Attachment{}, nil, &r
	} else if st.Size() > epubMaxFileBytes {
		r := contract.ErrResult(fmt.Sprintf("epub: 文件太大（上限 %dMB）", epubMaxFileBytes/(1024*1024)))
		return contract.Attachment{}, nil, &r
	}
	zr, err := zip.OpenReader(abs)
	if err != nil {
		r := contract.ErrResult("epub: 不是有效的 epub（打不开 ZIP）")
		return contract.Attachment{}, nil, &r
	}
	return att, zr, nil
}

func (t epubTool) runRead(ctx context.Context, tc contract.ToolContext, fileName string) contract.ToolResult {
	if tc.Channels == nil {
		return contract.ErrResult("epub: 工作空间不可用，无法解包")
	}
	ch, err := tc.Channels.OpenFile(ctx, "")
	if err != nil {
		return contract.ErrResult("epub: 打开工作空间失败：" + err.Error())
	}
	defer ch.Close()

	// Anchor the epub to THIS turn's attachment (like the ocr tool anchors an
	// image): the model can't open the bytes, so the tool picks the real
	// placed file rather than trusting a path the model invented, and reads it
	// through the channel.
	att, zr, errRes := t.openEpubZip(ctx, ch, tc.Attachments, fileName)
	if errRes != nil {
		return *errRes
	}
	defer zr.Close()

	book, err := parseBook(&zr.Reader)
	if err != nil {
		return contract.ErrResult("epub: " + err.Error())
	}

	dir := extractDirFor(att)
	out := make([]Chapter, 0, len(book.Chapters))
	var budget int64 = maxUnpackTotalBytes
	for _, c := range book.Chapters {
		// Zip-slip guard: a chapter path must stay inside the extract dir.
		if c.File == "" || strings.HasPrefix(c.File, "../") || strings.HasPrefix(c.File, "/") {
			continue
		}
		data, rerr := zipBytes(&zr.Reader, c.File)
		if rerr != nil {
			continue // a spine entry with no zip file — skip, keep the rest
		}
		if int64(len(data)) > budget {
			return contract.ErrResult("epub: 解包超出大小上限（疑似 zip bomb）")
		}
		budget -= int64(len(data))
		spacePath := path.Join(dir, c.File)
		if werr := ch.Write(ctx, spacePath, data); werr != nil {
			return contract.ErrResult("epub: 写入工作空间失败：" + werr.Error())
		}
		out = append(out, Chapter{File: spacePath, Title: c.Title})
	}
	if len(out) == 0 {
		return contract.ErrResult("epub: 没有可读取的章节内容")
	}

	res, err := json.Marshal(map[string]any{
		"extract_dir": dir,
		"format":      book.Format,
		"chapters":    out,
	})
	if err != nil {
		return contract.ErrResult("epub: 结果编码失败：" + err.Error())
	}
	return contract.OKResult(string(res)).
		WithHint("用文件工具按 chapters[].file 逐章读取（fs cmd=cat path=<file>）。")
}

// pickEpub resolves which attachment is the epub to read via the turn's AttachmentSet:
// the named one (file_name), the sole epub this turn, or an earlier turn's epub by name;
// else an error result (no epub / ambiguous) for the caller to return. isEpubAttachment
// is the appetite predicate.
func pickEpub(ctx context.Context, atts contract.AttachmentSet, fileName string) (contract.Attachment, *contract.ToolResult) {
	if atts == nil {
		r := contract.ErrResult("epub: 工作空间不可用，无法解包")
		return contract.Attachment{}, &r
	}
	picks := atts.Pick(ctx, isEpubAttachment, fileName)
	switch {
	case len(picks) == 0:
		if want := strings.TrimSpace(fileName); want != "" {
			r := contract.ErrResult(fmt.Sprintf("epub: 本轮没有叫 %q 的 epub 附件。", want))
			return contract.Attachment{}, &r
		}
		r := contract.ErrResult("epub: 本轮没有 epub 附件。请让用户把要读的 .epub 发过来。")
		return contract.Attachment{}, &r
	case len(picks) > 1:
		var names []string
		for _, a := range picks {
			names = append(names, epubAttName(a))
		}
		r := contract.ErrResult(fmt.Sprintf("epub: 本轮有多个 epub：%s —— 在 file_name 里写出要读的那个。", strings.Join(names, "、")))
		return contract.Attachment{}, &r
	}
	// Re-gate on appetite: Pick's named-match spans ALL kinds (so a caller can tell
	// "named the wrong kind" from "named nothing"), so a model that named a non-epub
	// file gets it back here — reject it cleanly instead of handing a PDF/image to
	// zip.OpenReader (which would fail with a misleading "not a valid epub").
	if a := picks[0]; isEpubAttachment(a) {
		return a, nil
	}
	if want := strings.TrimSpace(fileName); want != "" {
		r := contract.ErrResult(fmt.Sprintf("epub: %q 不是 epub 附件。", want))
		return contract.Attachment{}, &r
	}
	r := contract.ErrResult("epub: 本轮没有 epub 附件。请让用户把要读的 .epub 发过来。")
	return contract.Attachment{}, &r
}

// isEpubAttachment reports whether a downloaded attachment is an epub (by
// extension or MIME). Kind is transport-layer (feishu delivers an epub as
// Kind="document"), so identity comes from the name/MIME.
func isEpubAttachment(a contract.Attachment) bool {
	if a.Path == "" {
		return false
	}
	name := a.FileName
	if name == "" {
		name = filepath.Base(a.Path)
	}
	if strings.EqualFold(filepath.Ext(name), ".epub") {
		return true
	}
	return strings.Contains(strings.ToLower(a.MIME), "epub")
}

func epubAttName(a contract.Attachment) string {
	if a.FileName != "" {
		return a.FileName
	}
	return filepath.Base(a.Path)
}

// extractDirFor is the read-mode extract dir: epub/<bookKey>/.
func extractDirFor(a contract.Attachment) string { return "epub/" + bookKey(a) }

// bookKey is the per-book space-path segment: <stem>-<hash>. The hash of the
// space Path keeps two same-named books from colliding (a second run would
// otherwise overwrite the first under paths already handed to the model).
func bookKey(a contract.Attachment) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(a.Path))
	return fmt.Sprintf("%s-%08x", bookStem(a), h.Sum32())
}

// bookStem is the sanitized filename stem (no extension), "book" when empty.
func bookStem(a contract.Attachment) string {
	name := a.FileName
	if name == "" {
		name = filepath.Base(a.Path)
	}
	stem := sanitizeStem(strings.TrimSuffix(name, filepath.Ext(name)))
	if stem == "" {
		stem = "book"
	}
	return stem
}

// sanitizeStem keeps a filename stem to a safe space-path segment.
func sanitizeStem(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r > 0x7f: // keep non-ASCII (CJK book names) as-is
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
