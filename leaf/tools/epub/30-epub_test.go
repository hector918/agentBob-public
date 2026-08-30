package epub

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/flow/compose"
)

// attSet wraps a batch of attachments into the real AttachmentSet for tool tests.
func attSet(ch contract.ChannelOpener, atts ...contract.Attachment) contract.AttachmentSet {
	return compose.NewTurnAttachments([]contract.MessageEvent{{Attachments: atts}}, ch)
}

// --- a minimal real epub3 -----------------------------------------------------

const (
	tContainer = `<?xml version="1.0"?>
<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
 <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`
	tOPF = `<?xml version="1.0"?>
<package version="3.0" xmlns="http://www.idpf.org/2007/opf">
 <manifest>
  <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
  <item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
  <item id="c2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
 </manifest>
 <spine>
  <itemref idref="c1"/>
  <itemref idref="c2"/>
  <itemref idref="c1"/>
 </spine>
</package>`
	tNav = `<html><body><nav><ol>
 <li><a href="ch1.xhtml">第一章</a></li>
 <li><a href="ch2.xhtml">第二章</a></li>
</ol></nav></body></html>`
	tCh1 = `<html><body><p>hello chapter one</p></body></html>`
	tCh2 = `<html><body><p>第二章正文</p></body></html>`
)

func writeEpub(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "book.epub")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	add := func(name, body string, store bool) {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if store {
			hdr.Method = zip.Store
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("mimetype", "application/epub+zip", true)
	add("META-INF/container.xml", tContainer, false)
	add("OEBPS/content.opf", tOPF, false)
	add("OEBPS/nav.xhtml", tNav, false)
	add("OEBPS/ch1.xhtml", tCh1, false)
	add("OEBPS/ch2.xhtml", tCh2, false)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func openEpub(t *testing.T, p string) *zip.ReadCloser {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { zr.Close() })
	return zr
}

func TestParseBook(t *testing.T) {
	zr := openEpub(t, writeEpub(t, t.TempDir()))
	book, err := parseBook(&zr.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if book.Format != "epub3" {
		t.Errorf("format=%q want epub3", book.Format)
	}
	// spine has c1,c2,c1 → deduped to two chapters in reading order.
	if len(book.Chapters) != 2 {
		t.Fatalf("want 2 chapters, got %d: %+v", len(book.Chapters), book.Chapters)
	}
	if book.Chapters[0].File != "OEBPS/ch1.xhtml" || book.Chapters[1].File != "OEBPS/ch2.xhtml" {
		t.Errorf("chapter order/paths wrong: %+v", book.Chapters)
	}
	if book.Chapters[0].Title != "第一章" || book.Chapters[1].Title != "第二章" {
		t.Errorf("nav titles not applied: %+v", book.Chapters)
	}
}

func TestNcxTitlesFromBytes(t *testing.T) {
	ncx := `<ncx><navMap>
 <navPoint><navLabel><text>Ch A</text></navLabel><content src="a.xhtml"/></navPoint>
 <navPoint><navLabel><text>Ch B</text></navLabel><content src="b.xhtml#frag"/>
   <navPoint><navLabel><text>Ch B.1</text></navLabel><content src="c.xhtml"/></navPoint>
 </navPoint>
</navMap></ncx>`
	got := ncxTitlesFromBytes([]byte(ncx), "OEBPS")
	if got["OEBPS/a.xhtml"] != "Ch A" || got["OEBPS/b.xhtml"] != "Ch B" || got["OEBPS/c.xhtml"] != "Ch B.1" {
		t.Errorf("ncx titles wrong (nested + fragment strip): %+v", got)
	}
}

// TestNavTitlesFromHTMLNestedTOC: a nested epub3 TOC points chapter + section
// links at one file with different #fragments. Once the fragment is stripped they
// all collapse onto one key, so the chapter link (first in document order) must
// win — every time. This used to iterate a map, so the winner was random and the
// same book reported different titles on each read.
func TestNavTitlesFromHTMLNestedTOC(t *testing.T) {
	nav := []byte(`<html><body><nav><ol>
 <li><a href="ch1.xhtml">Chapter One</a>
   <ol>
    <li><a href="ch1.xhtml#s1">Section 1.1</a></li>
    <li><a href="ch1.xhtml#s2">Section 1.2</a></li>
    <li><a href="ch1.xhtml#s3">Section 1.3</a></li>
   </ol>
 </li>
 <li><a href="ch2.xhtml#top">Chapter Two</a></li>
</ol></nav></body></html>`)

	// the ordered slice is the fix: document order, one entry per link.
	links := extractNavLinks(nav)
	want := []navLink{
		{"ch1.xhtml", "Chapter One"},
		{"ch1.xhtml#s1", "Section 1.1"},
		{"ch1.xhtml#s2", "Section 1.2"},
		{"ch1.xhtml#s3", "Section 1.3"},
		{"ch2.xhtml#top", "Chapter Two"},
	}
	if len(links) != len(want) {
		t.Fatalf("extractNavLinks got %d links, want %d: %+v", len(links), len(want), links)
	}
	for i := range want {
		if links[i] != want[i] {
			t.Errorf("link %d = %+v, want %+v", i, links[i], want[i])
		}
	}

	// repeat: a map-order bug shows up as a flake, so one call proves nothing.
	for i := 0; i < 200; i++ {
		got := navTitlesFromHTML(nav, "OEBPS")
		if got["OEBPS/ch1.xhtml"] != "Chapter One" {
			t.Fatalf("run %d: ch1 title = %q, want %q (nondeterministic winner)", i, got["OEBPS/ch1.xhtml"], "Chapter One")
		}
		if got["OEBPS/ch2.xhtml"] != "Chapter Two" {
			t.Fatalf("run %d: ch2 title = %q, want %q", i, got["OEBPS/ch2.xhtml"], "Chapter Two")
		}
	}
}

// --- fake channel for the end-to-end read test --------------------------------

type fakeFileChannel struct{ files map[string][]byte }

func (c *fakeFileChannel) Alive() bool  { return true }
func (c *fakeFileChannel) Close() error { return nil }
func (c *fakeFileChannel) List(context.Context, string) ([]contract.FileEntry, error) {
	return nil, nil
}
func (c *fakeFileChannel) Read(_ context.Context, p string) ([]byte, error) { return c.files[p], nil }
func (c *fakeFileChannel) Write(_ context.Context, p string, d []byte) error {
	c.files[p] = append([]byte(nil), d...)
	return nil
}
func (c *fakeFileChannel) Mkdir(context.Context, string) error          { return nil }
func (c *fakeFileChannel) Remove(context.Context, string) error         { return nil }
func (c *fakeFileChannel) Rename(context.Context, string, string) error { return nil }
func (c *fakeFileChannel) AbsPath(p string) (string, error)             { return p, nil }

type fakeOpener struct{ ch *fakeFileChannel }

func (o fakeOpener) OpenFile(context.Context, string) (contract.FileChannel, error) { return o.ch, nil }
func (o fakeOpener) OpenExec(context.Context, string) (contract.ExecChannel, error) {
	return nil, nil
}

func TestRunReadExtractsChapters(t *testing.T) {
	dir := t.TempDir()
	epubPath := writeEpub(t, dir)
	fch := &fakeFileChannel{files: map[string][]byte{}}
	opener := fakeOpener{ch: fch}
	tc := contract.ToolContext{
		Channels:    opener,
		Attachments: attSet(opener, contract.Attachment{Kind: "document", FileName: "book.epub", Path: epubPath, MIME: "application/epub+zip"}),
	}
	raw, _ := json.Marshal(map[string]any{"mode": "read"})
	res := epubTool{}.Run(context.Background(), tc, raw)
	if !res.OK {
		t.Fatalf("expected OK, got: %s", res.Error)
	}
	var out struct {
		ExtractDir string    `json:"extract_dir"`
		Format     string    `json:"format"`
		Chapters   []Chapter `json:"chapters"`
	}
	if err := json.Unmarshal([]byte(res.Data), &out); err != nil {
		t.Fatalf("bad result json: %v", err)
	}
	if len(out.Chapters) != 2 {
		t.Fatalf("want 2 chapters, got %d", len(out.Chapters))
	}
	// chapter file paths are space-relative under the extract dir, and the bytes
	// were written into the (fake) space.
	for _, c := range out.Chapters {
		if !strings.HasPrefix(c.File, out.ExtractDir+"/") {
			t.Errorf("chapter file %q not under extract dir %q", c.File, out.ExtractDir)
		}
		if _, ok := fch.files[c.File]; !ok {
			t.Errorf("chapter %q not extracted into the space", c.File)
		}
	}
	if !strings.Contains(string(fch.files[out.Chapters[0].File]), "hello chapter one") {
		t.Error("ch1 content not written")
	}
}

func TestRunReadNoEpub(t *testing.T) {
	opener := fakeOpener{ch: &fakeFileChannel{files: map[string][]byte{}}}
	res := epubTool{}.Run(context.Background(), contract.ToolContext{Channels: opener, Attachments: attSet(opener)}, []byte(`{"mode":"read"}`))
	if res.OK || !strings.Contains(res.Error, "没有 epub") {
		t.Errorf("expected no-epub error, got OK=%v err=%q", res.OK, res.Error)
	}
}

// TestRunReadNamedNonEpub: the model names a non-epub file (a PDF) that IS present
// this turn → a clean "not an epub" error, not a misleading "invalid epub / can't open
// ZIP" (Pick matches the name across all kinds; pickEpub re-gates on the appetite).
func TestRunReadNamedNonEpub(t *testing.T) {
	dir := t.TempDir()
	epubPath := writeEpub(t, dir)
	opener := fakeOpener{ch: &fakeFileChannel{files: map[string][]byte{}}}
	tc := contract.ToolContext{
		Channels: opener,
		Attachments: attSet(opener,
			contract.Attachment{Kind: "document", FileName: "book.epub", Path: epubPath, MIME: "application/epub+zip"},
			contract.Attachment{Kind: "document", FileName: "report.pdf", Path: "report.pdf", MIME: "application/pdf"},
		),
	}
	raw, _ := json.Marshal(map[string]any{"mode": "read", "file_name": "report.pdf"})
	res := epubTool{}.Run(context.Background(), tc, raw)
	if res.OK || !strings.Contains(res.Error, "不是 epub") {
		t.Errorf("named non-epub: OK=%v err=%q, want a clean '不是 epub' error", res.OK, res.Error)
	}
}
