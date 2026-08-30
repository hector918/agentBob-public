package epub

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"agentbob/contract"

	"golang.org/x/net/html"
)

// upperTranslate is a fake translate backend: it upper-cases the text, preserving
// the ⟦¶⟧ chunk separators (so applyChunkTranslation still maps 1:1). That makes a
// translation visibly present in the output without a real model.
func upperTranslate(_ context.Context, _ string, user string) (string, error) {
	return strings.ToUpper(user), nil
}

func newReadOnlyEpubCtx(t *testing.T, fch *fakeFileChannel) (contract.ToolContext, string) {
	t.Helper()
	epubPath := writeEpub(t, t.TempDir())
	opener := fakeOpener{ch: fch}
	tc := contract.ToolContext{
		Channels:    opener,
		Attachments: attSet(opener, contract.Attachment{Kind: "document", FileName: "book.epub", Path: epubPath, MIME: "application/epub+zip"}),
	}
	return tc, epubPath
}

func unmarshalResult(t *testing.T, data string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatalf("bad result json: %v (%s)", err, data)
	}
	return m
}

func TestRunTranslateThenPack(t *testing.T) {
	fch := &fakeFileChannel{files: map[string][]byte{}}
	tc, _ := newReadOnlyEpubCtx(t, fch)
	tool := epubTool{translate: upperTranslate}

	// translate
	res := tool.Run(context.Background(), tc, []byte(`{"mode":"translate","target_lang":"English"}`))
	if !res.OK {
		t.Fatalf("translate failed: %s", res.Error)
	}
	out := unmarshalResult(t, res.Data)
	if out["translated"].(float64) != 2 {
		t.Fatalf("want 2 translated, got %v", out["translated"])
	}
	workDir := out["work_dir"].(string)
	got := string(fch.files[workDir+"/OEBPS/ch1.xhtml"])
	if !strings.Contains(got, "HELLO CHAPTER ONE") {
		t.Fatalf("ch1 translation not applied/written: %q", got)
	}

	// re-run translate → chapter-level resume (no re-translation)
	res2 := tool.Run(context.Background(), tc, []byte(`{"mode":"translate","target_lang":"English"}`))
	out2 := unmarshalResult(t, res2.Data)
	if out2["translated"].(float64) != 0 || out2["resumed"].(float64) != 2 {
		t.Fatalf("resume wrong: translated=%v resumed=%v", out2["translated"], out2["resumed"])
	}

	// pack → new epub in the space, with the translated chapter substituted
	resP := tool.Run(context.Background(), tc, []byte(`{"mode":"pack","target_lang":"English"}`))
	if !resP.OK {
		t.Fatalf("pack failed: %s", resP.Error)
	}
	outP := unmarshalResult(t, resP.Data)
	outPath := outP["output_path"].(string)
	if !strings.HasSuffix(outPath, ".English.epub") {
		t.Errorf("unexpected output path %q", outPath)
	}
	packed := fch.files[outPath]
	if len(packed) == 0 {
		t.Fatal("packed epub not written to space")
	}
	// the repackaged epub is a valid zip whose ch1 carries the translation and whose
	// non-chapter entries (mimetype) survive verbatim
	zr, err := zip.NewReader(bytes.NewReader(packed), int64(len(packed)))
	if err != nil {
		t.Fatalf("packed output is not a zip: %v", err)
	}
	entries := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		entries[f.Name] = string(b)
	}
	if entries["mimetype"] != "application/epub+zip" {
		t.Errorf("mimetype not preserved: %q", entries["mimetype"])
	}
	if !strings.Contains(entries["OEBPS/ch1.xhtml"], "HELLO CHAPTER ONE") {
		t.Errorf("packed ch1 not translated: %q", entries["OEBPS/ch1.xhtml"])
	}
	if !strings.Contains(entries["OEBPS/content.opf"], "spine") {
		t.Errorf("packed opf missing/garbled")
	}
}

func TestTranslateNoBackend(t *testing.T) {
	fch := &fakeFileChannel{files: map[string][]byte{}}
	tc, _ := newReadOnlyEpubCtx(t, fch)
	tool := epubTool{translate: nil}
	res := tool.Run(context.Background(), tc, []byte(`{"mode":"translate","target_lang":"English"}`))
	if res.OK || !strings.Contains(res.Error, "翻译后端") {
		t.Errorf("want no-backend error, got OK=%v err=%q", res.OK, res.Error)
	}
}

func TestPackBeforeTranslate(t *testing.T) {
	fch := &fakeFileChannel{files: map[string][]byte{}}
	tc, _ := newReadOnlyEpubCtx(t, fch)
	tool := epubTool{translate: upperTranslate}
	res := tool.Run(context.Background(), tc, []byte(`{"mode":"pack","target_lang":"English"}`))
	if res.OK || !strings.Contains(res.Error, "没有译好的章节") {
		t.Errorf("want no-translated-chapters error, got OK=%v err=%q", res.OK, res.Error)
	}
}

// dictTranslate is a fake translate backend for the inline-markup case: it looks
// each segment up in a dictionary and re-joins with the bare sentinel, deliberately
// echoing NO spacing back — the rendered spacing must come from the original nodes.
func dictTranslate(dict map[string]string) func(context.Context, string, string) (string, error) {
	return func(_ context.Context, _ string, user string) (string, error) {
		segs := strings.Split(user, strings.TrimSpace(chunkSep))
		for i, s := range segs {
			if v, ok := dict[strings.TrimSpace(s)]; ok {
				segs[i] = v
			}
		}
		return strings.Join(segs, strings.TrimSpace(chunkSep)), nil
	}
}

func TestApplyChunkTranslationKeepsInlineSpacing(t *testing.T) {
	// Inline markup splits one sentence into "这是 " / "非常" / " 好的。" — the spaces
	// around the <em> are node-edge whitespace, not content. A space-delimited target
	// language must not come out as "This is<em>very</em>good."
	doc, err := html.Parse(strings.NewReader(`<p>这是 <em>非常</em> 好的。</p>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tr := dictTranslate(map[string]string{"这是": "This is", "非常": "very", "好的。": "good."})
	for _, chunk := range chunkTextNodes(collectTextNodes(doc)) {
		out, terr := tr(context.Background(), "", joinChunk(chunk))
		if terr != nil {
			t.Fatalf("translate: %v", terr)
		}
		applyChunkTranslation(chunk, out)
	}
	rendered, err := renderHTML(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "<p>This is <em>very</em> good.</p>"; !strings.Contains(string(rendered), want) {
		t.Errorf("inline spacing lost:\n got %s\nwant substring %q", rendered, want)
	}
}

func TestCollapseChunkKeepsEdgeSpacing(t *testing.T) {
	// The last-resort collapse puts everything on the first node; that node's own
	// edge whitespace still has to survive, or the collapsed text glues onto its
	// neighbour.
	doc, err := html.Parse(strings.NewReader(`<p>hello <em>there</em></p>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	chunk := collectTextNodes(doc)
	if len(chunk) != 2 {
		t.Fatalf("want 2 text nodes, got %d", len(chunk))
	}
	if applyChunkTranslation(chunk, "BONJOUR TOI") { // no sentinel → mismatch
		t.Fatal("a sentinel-free translation must not apply 1:1")
	}
	collapseChunk(chunk, "BONJOUR TOI")
	rendered, err := renderHTML(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "<p>BONJOUR TOI <em></em></p>"; !strings.Contains(string(rendered), want) {
		t.Errorf("collapse dropped edge spacing:\n got %s\nwant substring %q", rendered, want)
	}
}

// TestChunkWirePayloadStaysInBudget guards the sizing that matters: what
// joinChunk actually puts on the wire, not the bare text it was billed for. A
// ruby/poetry chapter is thousands of tiny text nodes, and separators — not
// prose — dominate there. Billed text-only, one chunk hit 10.8 KB against a
// 2500-byte budget, which is enough to truncate the translate model's reply and
// silently collapse the chapter.
func TestChunkWirePayloadStaysInBudget(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<html><body><p>")
	for i := 0; i < 1400; i++ {
		sb.WriteString("<span>字</span>")
	}
	sb.WriteString("</p></body></html>")
	doc, err := html.Parse(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	splitOversizedNodes(doc)
	chunks := chunkTextNodes(collectTextNodes(doc))
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	for i, c := range chunks {
		if wire := len(joinChunk(c)); wire > chunkTargetBytes {
			t.Errorf("chunk %d: wire payload %d bytes exceeds the %d-byte budget (%d nodes)",
				i, wire, chunkTargetBytes, len(c))
		}
	}
}

// TestTranslateChunkRetriesHalvesBeforeCollapsing covers the recovery path: a
// model that merges two segments used to cost the whole chunk its layout, silently.
// Now the chunk is halved and retried once, and only a half that still won't line
// up collapses.
func TestTranslateChunkRetriesHalvesBeforeCollapsing(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(`<p>a<em>b</em>c<em>d</em></p>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	chunk := collectTextNodes(doc)
	if len(chunk) != 4 {
		t.Fatalf("want 4 text nodes, got %d", len(chunk))
	}

	sep := strings.TrimSpace(chunkSep)
	calls := 0
	// Merges the last two segments when asked for all four — the classic mismatch —
	// but answers a smaller batch faithfully.
	fake := func(_ context.Context, _ string, user string) (string, error) {
		calls++
		parts := strings.Split(user, sep)
		for i, p := range parts {
			parts[i] = strings.ToUpper(strings.TrimSpace(p))
		}
		if len(parts) == 4 {
			return strings.Join(append(parts[:2], parts[2]+parts[3]), sep), nil
		}
		return strings.Join(parts, sep), nil
	}

	if err := (epubTool{translate: fake}).translateChunk(context.Background(), chunk, "sys", 0, collapseChunk); err != nil {
		t.Fatalf("translateChunk: %v", err)
	}
	if calls != 3 {
		t.Errorf("want 1 full attempt + 2 half retries = 3 calls, got %d", calls)
	}
	rendered, err := renderHTML(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "<p>A<em>B</em>C<em>D</em></p>"; !strings.Contains(string(rendered), want) {
		t.Errorf("retry did not recover the layout:\n got %s\nwant substring %q", rendered, want)
	}
}

// A model that mangles the count no matter how small the batch still has to
// terminate — one retry, then collapse.
func TestTranslateChunkCollapsesAfterFailedRetry(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(`<p>a<em>b</em>c<em>d</em></p>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	chunk := collectTextNodes(doc)
	calls := 0
	fake := func(_ context.Context, _ string, user string) (string, error) {
		calls++
		return "GARBAGE", nil // never carries a sentinel → always mismatches
	}
	if err := (epubTool{translate: fake}).translateChunk(context.Background(), chunk, "sys", 0, collapseChunk); err != nil {
		t.Fatalf("translateChunk: %v", err)
	}
	if calls != 3 {
		t.Errorf("want the retry capped at 3 calls total, got %d", calls)
	}
	rendered, err := renderHTML(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// each half collapsed onto its own first node — half a chunk each, not the whole
	if want := "<p>GARBAGE<em></em>GARBAGE<em></em></p>"; !strings.Contains(string(rendered), want) {
		t.Errorf("collapse blast radius wrong:\n got %s\nwant substring %q", rendered, want)
	}
}

func TestChunkAndSplit(t *testing.T) {
	// splitText keeps join==original and respects the byte budget.
	s := strings.Repeat("一句话。", 2000) // ~12KB of CJK
	pieces := splitText(s, chunkTargetBytes)
	if strings.Join(pieces, "") != s {
		t.Fatal("splitText is not lossless")
	}
	for _, p := range pieces[:len(pieces)-1] {
		if len(p) > chunkTargetBytes {
			t.Fatalf("piece exceeds budget: %d", len(p))
		}
	}
}
