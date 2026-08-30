package epub

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// bracketTranslate wraps every segment in [..], keeping the sentinel count and
// order. Unlike upperTranslate it leaves a visible mark on CJK text too.
func bracketTranslate(_ context.Context, _ string, user string) (string, error) {
	sep := strings.TrimSpace(chunkSep)
	parts := strings.Split(user, sep)
	for i, p := range parts {
		parts[i] = "[" + strings.TrimSpace(p) + "]"
	}
	return strings.Join(parts, sep), nil
}

// The nav doc is in the manifest but NOT in the spine, so the chapter pass never
// reaches it — without the TOC pass the finished book navigates in the original
// language while its pages read in the target one.
func TestTranslateTOCTranslatesNavDocAndPackSubstitutesIt(t *testing.T) {
	fch := &fakeFileChannel{files: map[string][]byte{}}
	tc, _ := newReadOnlyEpubCtx(t, fch)
	tool := epubTool{translate: bracketTranslate}

	res := tool.Run(context.Background(), tc, []byte(`{"mode":"translate","target_lang":"English"}`))
	if !res.OK {
		t.Fatalf("translate failed: %s", res.Error)
	}
	out := unmarshalResult(t, res.Data)
	toc, _ := out["toc"].([]any)
	if len(toc) != 1 || toc[0] != "OEBPS/nav.xhtml" {
		t.Fatalf("want the nav doc reported in toc, got %v", out["toc"])
	}
	workDir := out["work_dir"].(string)
	nav := string(fch.files[workDir+"/OEBPS/nav.xhtml"])
	if !strings.Contains(nav, "[第一章]") || !strings.Contains(nav, "[第二章]") {
		t.Fatalf("nav labels not translated into the work dir: %q", nav)
	}

	// re-run: the nav doc resumes like a chapter rather than being re-translated
	fch.files[workDir+"/OEBPS/nav.xhtml"] = []byte(`<html><body><nav><ol><li><a href="ch1.xhtml">EDITED</a></li></ol></nav></body></html>`)
	res2 := tool.Run(context.Background(), tc, []byte(`{"mode":"translate","target_lang":"English"}`))
	if !res2.OK {
		t.Fatalf("re-translate failed: %s", res2.Error)
	}
	if got := string(fch.files[workDir+"/OEBPS/nav.xhtml"]); !strings.Contains(got, "EDITED") {
		t.Errorf("re-run overwrote an edited nav doc: %q", got)
	}

	// pack must carry the translated nav into the output epub
	resP := tool.Run(context.Background(), tc, []byte(`{"mode":"pack","target_lang":"English"}`))
	if !resP.OK {
		t.Fatalf("pack failed: %s", resP.Error)
	}
	outP := unmarshalResult(t, resP.Data)
	// chapters counts the SPINE, not the nav documents that joined replacements
	if got := outP["chapters"].(float64); got != 2 {
		t.Errorf("chapters should count the 2 spine docs, got %v", got)
	}
	packed := fch.files[outP["output_path"].(string)]
	zr, err := zip.NewReader(bytes.NewReader(packed), int64(len(packed)))
	if err != nil {
		t.Fatalf("packed output is not a zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != "OEBPS/nav.xhtml" {
			continue
		}
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if !strings.Contains(string(b), "EDITED") {
			t.Errorf("packed nav doc is the untranslated original: %q", b)
		}
		return
	}
	t.Error("packed epub has no nav doc")
}

// The TOC pass runs after every chapter is already on disk, so its failure must not
// take the whole job down: the model still needs work_dir to spot-check and pack.
func TestTranslateSurvivesTOCFailure(t *testing.T) {
	fch := &fakeFileChannel{files: map[string][]byte{}}
	tc, _ := newReadOnlyEpubCtx(t, fch)

	res := epubTool{translate: bracketTranslate}.Run(context.Background(), tc, []byte(`{"mode":"translate","target_lang":"English"}`))
	if !res.OK {
		t.Fatalf("first translate failed: %s", res.Error)
	}
	workDir := unmarshalResult(t, res.Data)["work_dir"].(string)
	delete(fch.files, workDir+"/OEBPS/nav.xhtml") // force the TOC pass to run again

	// every chapter now resumes (no model call), and only the TOC pass reaches the
	// backend — which fails
	boom := func(context.Context, string, string) (string, error) { return "", errBoom }
	res2 := epubTool{translate: boom}.Run(context.Background(), tc, []byte(`{"mode":"translate","target_lang":"English"}`))
	if !res2.OK {
		t.Fatalf("a TOC failure must not fail the whole translate: %s", res2.Error)
	}
	out := unmarshalResult(t, res2.Data)
	if out["work_dir"] != workDir {
		t.Errorf("work_dir withheld after a TOC failure: %v", out["work_dir"])
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "目录") {
		t.Errorf("the note should say the TOC was left in the source language: %q", note)
	}
}

var errBoom = errors.New("backend down")

// The ncx is XML: only the navLabel texts may change, every other byte has to
// survive — including <docTitle>/<docAuthor>, which are the book's identity and
// belong to the same "don't touch the metadata" rule as the OPF's dc:title.
func TestTranslateNCXSplicesNavLabelsOnly(t *testing.T) {
	src := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE ncx PUBLIC "-//NISO//DTD ncx 2005-1//EN" "http://www.daisy.org/z3986/2005/ncx-2005-1.dtd">
<ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1">
 <head><meta name="dtb:uid" content="urn:uuid:x"/></head>
 <docTitle><text>Tom &amp; Jerry</text></docTitle>
 <docAuthor><text>Some Author</text></docAuthor>
 <navMap>
  <navPoint id="n1" playOrder="1"><navLabel><text>Chapter One</text></navLabel><content src="ch1.xhtml"/></navPoint>
  <navPoint id="n2" playOrder="2">
   <navLabel>
    <text>Cats &amp; Dogs</text>
   </navLabel>
   <content src="ch2.xhtml"/>
  </navPoint>
  <navPoint id="n3" playOrder="3"><navLabel><text></text></navLabel><content src="ch3.xhtml"/></navPoint>
 </navMap>
</ncx>`)
	out, err := epubTool{translate: bracketTranslate}.translateNCX(context.Background(), src, "sys")
	if err != nil {
		t.Fatalf("translateNCX: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "<text>[Chapter One]</text>") {
		t.Errorf("label not translated:\n%s", got)
	}
	// metadata stays put — a translated nav-pane title against an original library
	// title is the two-books-on-one-shelf outcome this pass exists to avoid
	if !strings.Contains(got, "<docTitle><text>Tom &amp; Jerry</text></docTitle>") {
		t.Errorf("docTitle must not be translated:\n%s", got)
	}
	if !strings.Contains(got, "<docAuthor><text>Some Author</text></docAuthor>") {
		t.Errorf("docAuthor must not be translated:\n%s", got)
	}
	// entity-bearing label round-trips unescape → translate → re-escape, and its
	// pretty-printed indentation is spliced back RAW (not as &#xA; references)
	if want := "\n    <text>[Cats &amp; Dogs]</text>\n   "; !strings.Contains(got, want) {
		t.Errorf("indented label mangled:\nwant substring %q\ngot\n%s", want, got)
	}
	// an empty label is left exactly as it was, not filled with someone else's text
	if !strings.Contains(got, "<navLabel><text></text></navLabel>") {
		t.Errorf("empty label disturbed:\n%s", got)
	}
	for _, keep := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE ncx PUBLIC "-//NISO//DTD ncx 2005-1//EN" "http://www.daisy.org/z3986/2005/ncx-2005-1.dtd">`,
		`<ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1">`,
		`<navPoint id="n1" playOrder="1">`,
		`<content src="ch1.xhtml"/>`,
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("XML scaffolding not preserved verbatim: %q missing from\n%s", keep, got)
		}
	}
}

// The guarantee for TOC labels: no entry is ever left UNNAMED. The prose fallback
// (collapseChunk) empties every node but the first, which for structural labels
// means a nav pane of blank rows — worse than not translating at all. A label that
// ends up alone in a retried half is a different matter: one node and one answer
// always line up, so it takes the model's word, exactly as chapter text does.
func TestTranslateNCXNeverBlanksAnEntry(t *testing.T) {
	src := []byte(`<ncx><navMap>
  <navPoint><navLabel><text>Chapter One</text></navLabel><content src="ch1.xhtml"/></navPoint>
  <navPoint><navLabel><text>Chapter Two</text></navLabel><content src="ch2.xhtml"/></navPoint>
  <navPoint><navLabel><text>Chapter Three</text></navLabel><content src="ch3.xhtml"/></navPoint>
 </navMap></ncx>`)
	garbage := func(context.Context, string, string) (string, error) { return "GARBAGE", nil }
	out, err := epubTool{translate: garbage}.translateNCX(context.Background(), src, "sys")
	if err != nil {
		t.Fatalf("translateNCX: %v", err)
	}
	if strings.Contains(string(out), "<text></text>") {
		t.Fatalf("a nav entry was blanked:\n%s", out)
	}
	// the half that still didn't line up keeps its labels verbatim
	for _, want := range []string{"<text>Chapter Two</text>", "<text>Chapter Three</text>"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("label lost on mismatch: %q missing from\n%s", want, out)
		}
	}
}

// Same rule for the epub3 nav doc: a mismatch must not leave <a href=...></a>.
func TestTranslateNavDocNeverBlanksAnEntry(t *testing.T) {
	src := []byte(`<html><body><nav><ol>
 <li><a href="ch1.xhtml">Chapter One</a></li>
 <li><a href="ch2.xhtml">Chapter Two</a></li>
 <li><a href="ch3.xhtml">Chapter Three</a></li>
</ol></nav></body></html>`)
	garbage := func(context.Context, string, string) (string, error) { return "GARBAGE", nil }
	out, err := epubTool{translate: garbage}.translateNavDoc(context.Background(), src, "sys")
	if err != nil {
		t.Fatalf("translateNavDoc: %v", err)
	}
	doc, err := html.Parse(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	var anchors, empty int
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			anchors++
			if strings.TrimSpace(nodeText(n)) == "" {
				empty++
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if anchors != 3 || empty != 0 {
		t.Errorf("want 3 named TOC links, got %d links with %d unnamed:\n%s", anchors, empty, out)
	}
}

// A book with no ncx must not be reported as having translated one.
func TestTranslateNCXNoLabels(t *testing.T) {
	out, err := epubTool{translate: bracketTranslate}.translateNCX(context.Background(), []byte(`<ncx><navMap/></ncx>`), "sys")
	if err != nil || out != nil {
		t.Errorf("want (nil, nil) for a label-free ncx, got (%q, %v)", out, err)
	}
}

// pre/code carry identifiers and listings — translating them breaks the code and
// turns inline terms into prose.
func TestCollectTextNodesSkipsCodeAndPre(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(
		`<p>看这段 <code>func main()</code> 代码</p><pre>for i := range xs {}</pre><p>结束</p>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got []string
	for _, n := range collectTextNodes(doc) {
		got = append(got, strings.TrimSpace(n.Data))
	}
	want := []string{"看这段", "代码", "结束"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("collected %v, want %v", got, want)
	}
}
