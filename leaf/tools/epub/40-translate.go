package epub

// 40-translate.go — mode=translate: the long job. Re-parse the original epub,
// translate every chapter's visible text via the KindTranslate backend, and write
// each translated chapter into the turn's space (a work dir). It does NOT
// repackage or deliver — the agent spot-checks / edits the chapter files, then
// calls mode=pack.
//
// Trunk simplification vs skeleton: NO on-disk chunk ledger. The space work dir is
// the durable state — a re-run skips any chapter whose translated file already
// exists (chapter-level resume). Double-translation is impossible because translate
// always reads each chapter's PRISTINE original from the source zip, never from the
// work dir it writes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"agentbob/contract"

	"golang.org/x/net/html"
)

// chunkTargetBytes is the per-request text budget — no translate request exceeds
// it. splitOversizedNodes first splits any over-budget text node; chunkTextNodes
// then groups nodes into chunks of roughly this size.
const chunkTargetBytes = 2500

// chunkSep separates text nodes within one chunk on the wire — each node becomes
// one line and the model is told to keep the count + order. Unlikely in prose.
const chunkSep = "\n⟦¶⟧\n"

func (t epubTool) runTranslate(ctx context.Context, tc contract.ToolContext, fileName, targetLang, style string) contract.ToolResult {
	if t.translate == nil {
		return contract.ErrResult("epub: 没有翻译后端——需要 models.yaml 配一个 kind=translate 的条目；或改用 mode=read")
	}
	if tc.Channels == nil {
		return contract.ErrResult("epub: 工作空间不可用")
	}
	ch, err := tc.Channels.OpenFile(ctx, "")
	if err != nil {
		return contract.ErrResult("epub: 打开工作空间失败：" + err.Error())
	}
	defer ch.Close()

	att, zr, errRes := t.openEpubZip(ctx, ch, tc.Attachments, fileName)
	if errRes != nil {
		return *errRes
	}
	defer zr.Close()

	book, err := parseBook(&zr.Reader)
	if err != nil {
		return contract.ErrResult("epub: " + err.Error())
	}

	workDir := translateWorkDir(att, targetLang)
	system := buildTranslateSystemPrompt(targetLang, style)
	total := len(book.Chapters)
	written, resumed := 0, 0
	var skipped []string

	for ci, c := range book.Chapters {
		select {
		case <-ctx.Done():
			return contract.ErrResult("epub: 翻译被取消（已译的章节保留在工作空间，重跑会接着译）")
		default:
		}
		if c.File == "" || strings.HasPrefix(c.File, "../") || strings.HasPrefix(c.File, "/") {
			continue
		}
		workChapter := path.Join(workDir, c.File)
		// Chapter-level resume: a translated file already in the space means a prior
		// run (or the agent's edit) — keep it, don't re-translate.
		if data, rerr := ch.Read(ctx, workChapter); rerr == nil && len(data) > 0 {
			resumed++
			continue
		}
		// PRISTINE original from the zip (never the work dir) — no double translation.
		htmlBytes, rerr := zipBytes(&zr.Reader, c.File)
		if rerr != nil {
			slog.Warn("epub translate: spine chapter missing in zip", "chapter", ci+1, "file", c.File)
			skipped = append(skipped, c.File)
			continue
		}
		doc, perr := html.Parse(bytes.NewReader(htmlBytes))
		if perr != nil {
			slog.Warn("epub translate: unparseable chapter", "chapter", ci+1, "file", c.File, "err", perr)
			skipped = append(skipped, c.File)
			continue
		}
		if terr := t.translateDoc(ctx, doc, system, collapseChunk); terr != nil {
			// No translate backend configured: the pool returns "no model available"
			// on the very first call. Surface the clean upfront fix instead of the
			// per-chapter "重跑接着译" wrapper (which misleads — re-running can't help
			// without a kind=translate model). String-match because the model sentinel
			// can't cross the leaf boundary as a typed error.
			if strings.Contains(terr.Error(), "no model available") {
				return contract.ErrResult("epub: 没有翻译后端——需要 models.yaml 配一个 kind=translate 的条目；或改用 mode=read")
			}
			return contract.ErrResult(fmt.Sprintf(
				"epub: 第 %d/%d 章翻译失败：%v（已译的章节已保留，重跑会从这里接着译）", ci+1, total, terr))
		}
		rendered, rerr := renderHTML(doc)
		if rerr != nil {
			slog.Warn("epub translate: chapter failed to render", "chapter", ci+1, "file", c.File, "err", rerr)
			skipped = append(skipped, c.File)
			continue
		}
		rendered = repairXMLDeclaration(htmlBytes, rendered)
		if werr := ch.Write(ctx, workChapter, rendered); werr != nil {
			return contract.ErrResult("epub: 写译文失败：" + werr.Error())
		}
		written++
		if tc.Sink != nil {
			tc.Sink.TraceDelta(fmt.Sprintf("📖 %d/%d", ci+1, total))
		}
	}

	if written+resumed == 0 {
		return contract.ErrResult("epub: 没有可翻译的章节")
	}

	// The TOC pass runs AFTER every chapter is safely on disk, so its failure must
	// not take the chapters down with it: returning an error here would withhold
	// work_dir and the spot-check instructions, and the model would have no way to
	// name the directory holding a book that is, in fact, translated.
	toc, tocErr := t.translateTOC(ctx, ch, &zr.Reader, book, workDir, system)
	if tocErr != nil {
		slog.Warn("epub translate: TOC pass failed — chapters kept", "err", tocErr)
	}

	note := "翻译完成，译好的章节已逐章写入工作空间（见 work_dir）。请用文件工具抽查若干章译文质量，发现问题就直接修订对应章节文件；" +
		"确认无误后调用 mode=pack（file_name 与 target_lang 传与本次相同的值）打包，再 deliver_file 发送。现在还不要打包、不要发送。"
	if len(skipped) > 0 {
		note += fmt.Sprintf("注意：有 %d 章因缺失/损坏被跳过（未翻译，打包时保留原文）：%s。", len(skipped), strings.Join(skipped, "、"))
	}
	if tocErr != nil {
		note += "注意：目录（TOC）没能翻译，成书的目录会保持原文，正文不受影响；重跑 translate 会再试一次。"
	}
	res, _ := json.Marshal(map[string]any{
		"work_dir": workDir, "chapters": total, "translated": written, "resumed": resumed,
		"skipped": skipped, "toc": toc, "note": note,
	})
	return contract.OKResult(string(res))
}

// joinChunk renders a chunk's text nodes as one sentinel-separated blob.
func joinChunk(chunk []*html.Node) string {
	parts := make([]string, len(chunk))
	for i, n := range chunk {
		parts[i] = n.Data
	}
	return strings.Join(parts, chunkSep)
}

// reedge re-attaches the ORIGINAL text node's leading/trailing whitespace around a
// translated segment. Inline markup (<em>, <a>, ...) splits one sentence across
// several text nodes, and the inter-word spacing lives in those nodes' own edge
// whitespace ("这是 " / "非常" / " 好的。"). The model's spacing can't be trusted — it
// may echo it, drop it, or invent it — so whatever came back on the edges is
// discarded and the original's is restored. Without this, a space-delimited target
// language comes out glued to the markup ("This is<em>very</em>good."); CJK targets
// never noticed.
func reedge(orig, translated string) string {
	const wsCutset = " \t\n\v\f\r 　" // ASCII whitespace + NEL + nbsp + ideographic space
	lead := orig[:len(orig)-len(strings.TrimLeft(orig, wsCutset))]
	trail := orig[len(strings.TrimRight(orig, wsCutset)):]
	return lead + strings.TrimSpace(translated) + trail
}

// applyChunkTranslation writes the model's translation back onto the chunk's text
// nodes, 1:1 on the sentinel. It reports false and writes NOTHING when the segment
// count doesn't match, leaving the caller to choose between retrying smaller and
// collapsing.
func applyChunkTranslation(chunk []*html.Node, translation string) bool {
	parts := strings.Split(translation, strings.TrimSpace(chunkSep))
	if len(parts) != len(chunk) {
		return false
	}
	for i, n := range chunk {
		n.Data = reedge(n.Data, parts[i])
	}
	return true
}

// collapseChunk is the last resort when the segment count won't line up: put the
// whole translation on the first node and empty the rest. Meaning survives, the
// chunk's layout does not.
func collapseChunk(chunk []*html.Node, translation string) {
	if len(chunk) == 0 {
		return
	}
	chunk[0].Data = reedge(chunk[0].Data, translation)
	for i := 1; i < len(chunk); i++ {
		chunk[i].Data = ""
	}
}

// mismatchFallback decides what happens to a chunk whose segment count the model
// would not respect even at half size. The two callers want opposite things, so
// neither is hardcoded here: prose can afford collapseChunk (layout degrades,
// meaning survives) while structural labels cannot (see keepOriginals).
type mismatchFallback func(chunk []*html.Node, translation string)

// keepOriginals leaves a chunk untranslated. It is the fallback for TOC labels,
// where every node is its own structural entry: collapsing would empty all but the
// first, and an epub whose nav pane lists blank rows is strictly worse than one
// whose nav pane is still in the source language.
func keepOriginals([]*html.Node, string) {}

// translateChunk translates one chunk in place. A count mismatch is usually the
// model merging two short segments or adding a stray line, and the same nodes asked
// for in a smaller batch usually come back clean — so on a mismatch the chunk is
// halved and each half retried ONCE. Only a half that still won't line up hits
// fallback, which holds the blast radius to half a chunk instead of a whole one.
// depth caps the recursion at that single retry: splitting without a floor would
// fan a model that ALWAYS adds a line out to one request per node.
func (t epubTool) translateChunk(ctx context.Context, chunk []*html.Node, system string, depth int, fallback mismatchFallback) error {
	translated, err := t.translate(ctx, system, joinChunk(chunk))
	if err != nil {
		return err
	}
	if applyChunkTranslation(chunk, translated) {
		return nil
	}
	if depth == 0 && len(chunk) > 1 {
		mid := len(chunk) / 2
		if err := t.translateChunk(ctx, chunk[:mid], system, depth+1, fallback); err != nil {
			return err
		}
		return t.translateChunk(ctx, chunk[mid:], system, depth+1, fallback)
	}
	// A single-node chunk is never split, so say which of the two it was — the log is
	// the only channel a silent fallback has.
	if depth > 0 {
		slog.Warn("epub translate: segment count still mismatched after retry", "nodes", len(chunk))
	} else {
		slog.Warn("epub translate: segment count mismatched, chunk too small to retry", "nodes", len(chunk))
	}
	fallback(chunk, translated)
	return nil
}

// translateDoc translates every visible text node of a parsed document in place.
// Chapters and the epub3 nav doc are both XHTML, so both ride this one path — they
// differ only in what a hopeless chunk falls back to, hence the explicit fallback.
func (t epubTool) translateDoc(ctx context.Context, doc *html.Node, system string, fallback mismatchFallback) error {
	splitOversizedNodes(doc)
	for _, chunk := range chunkTextNodes(collectTextNodes(doc)) {
		if err := t.translateChunk(ctx, chunk, system, 0, fallback); err != nil {
			return err
		}
	}
	return nil
}

// chunkTextNodes groups text nodes into chunks of ~chunkTargetBytes (oversized
// nodes were split upstream, so every chunk stays within the per-request budget).
//
// A node costs its text PLUS the separator that joins it to the chunk — joinChunk
// inserts chunkSep between nodes, and at 10 bytes each those dominate once markup
// is dense (a ruby/poetry chapter of 3-byte nodes billed text-only put 833 nodes
// and 10.8 KB on the wire against a 2500-byte budget: the model's reply got cut
// off, the segment count no longer matched, and applyChunkTranslation collapsed
// the whole chunk into its first node — silently, since the chapter still counts
// as translated and resume then skips it). Billing the separator also bounds the
// node COUNT (~190 here), which is what actually drives the overhead.
func chunkTextNodes(nodes []*html.Node) [][]*html.Node {
	var chunks [][]*html.Node
	var cur []*html.Node
	curSize := 0
	for _, n := range nodes {
		sz := len(n.Data) + len(chunkSep)
		if len(cur) > 0 && curSize+sz > chunkTargetBytes {
			chunks = append(chunks, cur)
			cur, curSize = nil, 0
		}
		cur = append(cur, n)
		curSize += sz
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// buildTranslateSystemPrompt assembles the system message for one translate call.
func buildTranslateSystemPrompt(targetLang, style string) string {
	var sb strings.Builder
	sb.WriteString("你是一名专业的图书译者。把用户消息中的文本翻译成")
	sb.WriteString(targetLang)
	sb.WriteString("。要求：忠实保留原意，译文自然流畅；")
	if strings.TrimSpace(style) != "" {
		sb.WriteString("文风/语气：")
		sb.WriteString(strings.TrimSpace(style))
		sb.WriteString("；")
	}
	sb.WriteString("文本中的 ")
	sb.WriteString(strings.TrimSpace(chunkSep))
	sb.WriteString(" 是段落分隔符，必须原样保留、数量和顺序不变——每个分隔符之间的内容逐段翻译。只返回译文本身，不要添加任何解释、标注或额外内容。")
	return sb.String()
}

// translateWorkDir is the space dir mode=translate writes into and mode=pack reads
// back: epub-translated/<bookKey>-<lang>/. Keyed by target language so different
// languages don't collide.
func translateWorkDir(a contract.Attachment, targetLang string) string {
	return "epub-translated/" + bookKey(a) + "-" + sanitizeLangSegment(targetLang)
}

// sanitizeLangSegment turns a target-language label into a safe path segment.
func sanitizeLangSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "translated"
	}
	out := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '.', ' ', '\t', '\n':
			return '_'
		default:
			return r
		}
	}, s)
	if out == "" || out == "_" {
		return "translated"
	}
	return out
}
