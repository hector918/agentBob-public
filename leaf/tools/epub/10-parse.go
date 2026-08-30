// Package epub holds the `epub` tool — it lets the agent READ (and, in a later
// slice, translate) epub books. An epub is a ZIP of XHTML documents, so the
// text-only file tool can't open one directly.
//
// 10-parse.go is the epub container parser: container.xml → OPF, OPF
// manifest+spine → ordered content documents, best-effort chapter titles from
// the epub3 nav doc or the epub2 toc.ncx. Ported from skeleton's
// tools/epub/30-epubfile.go, but reads every entry straight from the zip
// in-memory (no unpack-to-disk round trip) — the read path extracts content
// documents into the turn's space separately (see 00-epub.go).
package epub

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"
)

// Chapter is one reading-order content document. File is the entry path inside
// the epub zip (forward-slash, archive-root); it doubles as the space-relative
// path under the extract dir, so the model reads <dir>/<File>. Title is
// best-effort — "" when neither nav doc nor toc.ncx names it.
type Chapter struct {
	File  string `json:"file"`
	Title string `json:"title"`
}

// EpubBook is a parsed epub: the format, the ordered chapter list, and the two
// navigation documents. NavDoc (epub3 nav) and NCXDoc (epub2 toc.ncx) are archive
// paths, "" when the book has none — they carry the TOC labels, which live
// OUTSIDE the spine and so would otherwise never reach the translator (a book
// whose chapters read 第一章 while its TOC still says Chapter One).
type EpubBook struct {
	Format   string
	Chapters []Chapter
	NavDoc   string
	NCXDoc   string
}

type containerXML struct {
	Rootfiles []struct {
		FullPath  string `xml:"full-path,attr"`
		MediaType string `xml:"media-type,attr"`
	} `xml:"rootfiles>rootfile"`
}

type opfPackage struct {
	Version  string `xml:"version,attr"`
	Manifest struct {
		Items []opfItem `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		TOC      string `xml:"toc,attr"`
		ItemRefs []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
}

type opfItem struct {
	ID         string `xml:"id,attr"`
	Href       string `xml:"href,attr"`
	MediaType  string `xml:"media-type,attr"`
	Properties string `xml:"properties,attr"`
}

type ncxDoc struct {
	NavMap struct {
		Points []ncxPoint `xml:"navPoint"`
	} `xml:"navMap"`
}

type ncxPoint struct {
	Label struct {
		Text string `xml:"text"`
	} `xml:"navLabel"`
	Content struct {
		Src string `xml:"src,attr"`
	} `xml:"content"`
	Children []ncxPoint `xml:"navPoint"`
}

// zipBytes reads one entry's bytes from the zip by exact name, or an error. A
// per-entry decompression cap (maxEntryUnpackBytes) bounds a single highly-
// compressed entry so the read/translate/pack paths that all funnel through here
// can't be driven to OOM by a zip bomb before a cumulative check fires. The header
// size is only a hint (it can lie), so the LimitReader is the real guard.
func zipBytes(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == name {
			if f.UncompressedSize64 > uint64(maxEntryUnpackBytes) {
				return nil, fmt.Errorf("epub entry %q too large (%d bytes, cap %d)", name, f.UncompressedSize64, maxEntryUnpackBytes)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			b, err := io.ReadAll(io.LimitReader(rc, maxEntryUnpackBytes+1))
			if err != nil {
				return nil, err
			}
			if int64(len(b)) > maxEntryUnpackBytes {
				return nil, fmt.Errorf("epub entry %q exceeds the per-entry size cap (%d bytes)", name, maxEntryUnpackBytes)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("epub entry %q not found", name)
}

// parseBook reads container.xml + OPF + (best-effort) nav/ncx titles straight
// from the zip and returns the ordered chapter list. No extraction.
func parseBook(zr *zip.Reader) (*EpubBook, error) {
	cb, err := zipBytes(zr, "META-INF/container.xml")
	if err != nil {
		return nil, fmt.Errorf("read META-INF/container.xml: %w", err)
	}
	opfRel, err := opfPathFromContainerBytes(cb)
	if err != nil {
		return nil, err
	}
	opfBytes, err := zipBytes(zr, opfRel)
	if err != nil {
		return nil, fmt.Errorf("read OPF %s: %w", opfRel, err)
	}
	pkg, err := parseOPF(opfBytes)
	if err != nil {
		return nil, err
	}
	opfDir := path.Dir(opfRel)
	if opfDir == "." {
		opfDir = ""
	}

	chapters := spineChapters(pkg, opfDir)
	if len(chapters) == 0 {
		return nil, fmt.Errorf("epub spine yields no content documents")
	}

	format := "epub2"
	if strings.HasPrefix(strings.TrimSpace(pkg.Version), "3") {
		format = "epub3"
	}

	navDoc, ncxDoc := navDocPath(pkg, opfDir), ncxDocPath(pkg, opfDir)

	titles := navTitles(zr, navDoc)
	if len(titles) == 0 {
		titles = ncxTitles(zr, ncxDoc)
	}
	for i := range chapters {
		if t, ok := titles[chapters[i].File]; ok {
			chapters[i].Title = t
		}
	}
	return &EpubBook{Format: format, Chapters: chapters, NavDoc: navDoc, NCXDoc: ncxDoc}, nil
}

func opfPathFromContainerBytes(b []byte) (string, error) {
	var c containerXML
	if err := xml.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("parse container.xml: %w", err)
	}
	if len(c.Rootfiles) == 0 {
		return "", fmt.Errorf("container.xml has no rootfile")
	}
	for _, rf := range c.Rootfiles {
		if rf.MediaType == "application/oebps-package+xml" && rf.FullPath != "" {
			return path.Clean(rf.FullPath), nil
		}
	}
	if c.Rootfiles[0].FullPath == "" {
		return "", fmt.Errorf("container.xml rootfile has empty full-path")
	}
	return path.Clean(c.Rootfiles[0].FullPath), nil
}

func parseOPF(b []byte) (*opfPackage, error) {
	var pkg opfPackage
	if err := xml.Unmarshal(b, &pkg); err != nil {
		return nil, fmt.Errorf("parse OPF: %w", err)
	}
	if len(pkg.Manifest.Items) == 0 {
		return nil, fmt.Errorf("OPF manifest is empty")
	}
	if len(pkg.Spine.ItemRefs) == 0 {
		return nil, fmt.Errorf("OPF spine is empty")
	}
	return &pkg, nil
}

// spineChapters walks the spine in reading order, resolves each idref through
// the manifest, and returns the ordered content-document list (each unique
// document once — the spec allows repeats; first occurrence wins position).
func spineChapters(pkg *opfPackage, opfDir string) []Chapter {
	byID := make(map[string]opfItem, len(pkg.Manifest.Items))
	for _, it := range pkg.Manifest.Items {
		byID[it.ID] = it
	}
	var out []Chapter
	seen := make(map[string]bool, len(pkg.Spine.ItemRefs))
	for _, ref := range pkg.Spine.ItemRefs {
		it, ok := byID[ref.IDRef]
		if !ok || it.Href == "" {
			continue
		}
		file := resolveHref(opfDir, it.Href)
		if seen[file] {
			continue
		}
		seen[file] = true
		out = append(out, Chapter{File: file})
	}
	return out
}

// resolveHref joins an OPF-relative href onto the OPF's dir, strips the
// fragment, URL-decodes, and returns a clean forward-slash archive path.
func resolveHref(opfDir, href string) string {
	href = unescapeHref(href)
	joined := href
	if opfDir != "" {
		joined = path.Join(opfDir, href)
	}
	return path.Clean(joined)
}

// unescapeHref strips any #fragment and URL-decodes the href so Chapter.File
// and the title-map keys share one spelling (zip entry names are unencoded).
func unescapeHref(href string) string {
	if i := strings.IndexByte(href, '#'); i >= 0 {
		href = href[:i]
	}
	if u, err := url.PathUnescape(href); err == nil {
		href = u
	}
	return href
}

// navDocPath resolves the epub3 nav doc (manifest item properties="nav") to an
// archive path, "" when the book has none.
func navDocPath(pkg *opfPackage, opfDir string) string {
	for _, it := range pkg.Manifest.Items {
		if it.Href != "" && slices.Contains(strings.Fields(it.Properties), "nav") {
			return resolveHref(opfDir, it.Href)
		}
	}
	return ""
}

// navTitles reads the epub3 nav doc from the zip and returns archive-path→title.
// Best-effort: any miss yields nil.
func navTitles(zr *zip.Reader, navRel string) map[string]string {
	if navRel == "" {
		return nil
	}
	b, err := zipBytes(zr, navRel)
	if err != nil {
		return nil
	}
	return navTitlesFromHTML(b, path.Dir(navRel))
}

// navTitlesFromHTML extracts <a href> link text from a nav doc, resolving
// relative hrefs against navBase back to archive-root paths. Several nav links
// collapse onto one key once the #fragment is stripped (a chapter and its
// sections), so the earliest link in document order names the file — which is
// only well-defined because extractNavLinks returns an ordered slice.
func navTitlesFromHTML(htmlBytes []byte, navBase string) map[string]string {
	links := extractNavLinks(htmlBytes)
	if len(links) == 0 {
		return nil
	}
	out := make(map[string]string, len(links))
	for _, link := range links {
		href := unescapeHref(link.Href)
		if href == "" {
			continue
		}
		key := href
		if navBase != "" && navBase != "." {
			key = path.Join(navBase, href)
		}
		key = path.Clean(key)
		if txt := strings.TrimSpace(link.Text); txt != "" {
			if _, exists := out[key]; !exists {
				out[key] = txt
			}
		}
	}
	return out
}

// ncxDocPath resolves the epub2 toc.ncx (spine toc idref, else first ncx manifest
// item) to an archive path, "" when the book has none.
func ncxDocPath(pkg *opfPackage, opfDir string) string {
	var ncxHref string
	if pkg.Spine.TOC != "" {
		for _, it := range pkg.Manifest.Items {
			if it.ID == pkg.Spine.TOC {
				ncxHref = it.Href
				break
			}
		}
	}
	if ncxHref == "" {
		for _, it := range pkg.Manifest.Items {
			if it.MediaType == "application/x-dtbncx+xml" {
				ncxHref = it.Href
				break
			}
		}
	}
	if ncxHref == "" {
		return ""
	}
	return resolveHref(opfDir, ncxHref)
}

// ncxTitles reads the epub2 toc.ncx from the zip and returns archive-path→title
// from a flattened navMap.
func ncxTitles(zr *zip.Reader, ncxRel string) map[string]string {
	if ncxRel == "" {
		return nil
	}
	b, err := zipBytes(zr, ncxRel)
	if err != nil {
		return nil
	}
	return ncxTitlesFromBytes(b, path.Dir(ncxRel))
}

func ncxTitlesFromBytes(b []byte, ncxBase string) map[string]string {
	var doc ncxDoc
	if err := xml.Unmarshal(b, &doc); err != nil {
		return nil
	}
	out := map[string]string{}
	var walk func(points []ncxPoint)
	walk = func(points []ncxPoint) {
		for _, p := range points {
			src := unescapeHref(p.Content.Src)
			if src != "" {
				key := src
				if ncxBase != "" && ncxBase != "." {
					key = path.Join(ncxBase, src)
				}
				key = path.Clean(key)
				if txt := strings.TrimSpace(p.Label.Text); txt != "" {
					if _, exists := out[key]; !exists {
						out[key] = txt
					}
				}
			}
			walk(p.Children)
		}
	}
	walk(doc.NavMap.Points)
	return out
}
