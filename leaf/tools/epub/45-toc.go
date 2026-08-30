package epub

// 45-toc.go — mode=translate's second pass: the table of contents.
//
// TOC labels live OUTSIDE the spine (epub3 nav.xhtml, epub2 toc.ncx), so the
// chapter pass never sees them. Left alone they produce a book whose text reads
// 第一章 while the reader's navigation pane still says Chapter One — and tapping
// the entry lands on a page that doesn't match its own name. Book METADATA
// (dc:title, dc:creator in the OPF) is deliberately NOT translated: it is the
// book's identity, and rewriting it makes a reader treat the translation as a
// different book from the original on the same shelf.
//
// The two documents need different handling. nav.xhtml is XHTML, so it rides the
// same DOM pipeline as a chapter. toc.ncx is XML — an HTML parse/render round trip
// would mangle it, and encoding/xml won't reproduce namespaces, DTD and attribute
// order byte-for-byte — so its labels are spliced in place and every other byte is
// copied through untouched.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strings"

	"agentbob/contract"

	"golang.org/x/net/html"
)

// translateTOC translates whichever navigation documents the book has into the
// work dir, and returns the archive paths it covered. Same resume rule as the
// chapters: a file already in the work dir is left as it is.
func (t epubTool) translateTOC(ctx context.Context, ch contract.FileChannel, zr *zip.Reader, book *EpubBook, workDir, system string) ([]string, error) {
	inSpine := make(map[string]bool, len(book.Chapters))
	for _, c := range book.Chapters {
		inSpine[c.File] = true
	}

	docs := []struct {
		file  string
		xhtml bool
	}{
		{book.NavDoc, true},
		{book.NCXDoc, false},
	}

	var done []string
	for _, d := range docs {
		// Skip a nav doc that is also a spine entry — some books make the TOC a
		// readable page, and the chapter pass has already translated it.
		if d.file == "" || inSpine[d.file] {
			continue
		}
		if strings.HasPrefix(d.file, "../") || strings.HasPrefix(d.file, "/") {
			continue
		}
		work := path.Join(workDir, d.file)
		if data, rerr := ch.Read(ctx, work); rerr == nil && len(data) > 0 {
			done = append(done, d.file)
			continue
		}
		src, rerr := zipBytes(zr, d.file)
		if rerr != nil {
			slog.Warn("epub translate: TOC document missing in zip", "file", d.file, "err", rerr)
			continue
		}
		var out []byte
		var terr error
		if d.xhtml {
			out, terr = t.translateNavDoc(ctx, src, system)
		} else {
			out, terr = t.translateNCX(ctx, src, system)
		}
		if terr != nil {
			return done, terr
		}
		if out == nil {
			continue // nothing translatable in there
		}
		if werr := ch.Write(ctx, work, out); werr != nil {
			return done, fmt.Errorf("写工作空间失败：%w", werr)
		}
		done = append(done, d.file)
	}
	return done, nil
}

// translateNavDoc translates an epub3 nav document — plain XHTML, so it is the
// chapter path verbatim.
func (t epubTool) translateNavDoc(ctx context.Context, src []byte, system string) ([]byte, error) {
	doc, err := html.Parse(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	// keepOriginals: every text node here is a TOC entry's label, and a blank entry
	// is worse than an untranslated one.
	if err := t.translateDoc(ctx, doc, system, keepOriginals); err != nil {
		return nil, err
	}
	rendered, err := renderHTML(doc)
	if err != nil {
		return nil, err
	}
	return repairXMLDeclaration(src, rendered), nil
}

// ncxLabelRE and ncxTextRE together pick out epub2 TOC labels: <text> is scoped to
// the <navLabel> blocks that name entries, NOT every <text> in the file. The ncx
// also carries <docTitle>/<docAuthor> in a <text>, and those are metadata — the
// same book identity the OPF's dc:title is left alone for. A bare <text> regex
// translates the book's title in the nav pane while the library still shows the
// original, which is exactly the two-books-on-one-shelf outcome this pass avoids.
//
// Both patterns are deliberately exact: a namespaced or attributed variant
// (<ncx:text>, <text xml:lang=...>) simply doesn't match, which leaves that label
// in the source language rather than risking a bad splice.
var (
	ncxLabelRE = regexp.MustCompile(`(?s)<navLabel>.*?</navLabel>`)
	ncxTextRE  = regexp.MustCompile(`(?s)<text>(.*?)</text>`)
)

// translateNCX translates the labels of an epub2 toc.ncx and splices them back
// into the original bytes. Returns nil when the document has no label to
// translate. The label nodes are real html.Nodes so they ride the same chunking,
// retry and whitespace handling as chapter text.
func (t epubTool) translateNCX(ctx context.Context, src []byte, system string) ([]byte, error) {
	// A label's surrounding whitespace is the file's indentation, and it is spliced
	// back RAW: running it through xml.EscapeText would turn a pretty-printed ncx
	// (what Sigil and calibre emit) into a field of &#xA; character references.
	type label struct {
		start, end  int // byte range of the inner text within src
		lead, trail string
		node        *html.Node
	}
	var labels []label
	for _, block := range ncxLabelRE.FindAllIndex(src, -1) {
		m := ncxTextRE.FindSubmatchIndex(src[block[0]:block[1]])
		if m == nil {
			continue
		}
		start, end := block[0]+m[2], block[0]+m[3]
		raw := string(src[start:end])
		body := strings.TrimSpace(raw)
		if body == "" {
			continue
		}
		lead := raw[:strings.Index(raw, body)]
		labels = append(labels, label{
			start: start, end: end,
			lead: lead, trail: raw[len(lead)+len(body):],
			node: &html.Node{Type: html.TextNode, Data: html.UnescapeString(body)},
		})
	}
	if len(labels) == 0 {
		return nil, nil
	}

	nodes := make([]*html.Node, len(labels))
	for i, l := range labels {
		nodes[i] = l.node
	}
	for _, chunk := range chunkTextNodes(nodes) {
		// keepOriginals, not collapseChunk: each label is its own nav entry, so
		// emptying the others would leave the reader a list of unnamed rows.
		if err := t.translateChunk(ctx, chunk, system, 0, keepOriginals); err != nil {
			return nil, err
		}
	}

	var out bytes.Buffer
	prev := 0
	for _, l := range labels {
		out.Write(src[prev:l.start])
		out.WriteString(l.lead)
		if err := xml.EscapeText(&out, []byte(l.node.Data)); err != nil {
			return nil, err
		}
		out.WriteString(l.trail)
		prev = l.end
	}
	out.Write(src[prev:])
	return out.Bytes(), nil
}
