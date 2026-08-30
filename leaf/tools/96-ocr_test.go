package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/flow/compose"
)

// fakeOCRPool is a minimal contract.ModelPool for the image tool tests: it records
// the last Chat request + messages and returns a canned response. Everything else
// is a no-op.
type fakeOCRPool struct {
	resp     contract.ChatResponse
	err      error
	lastReq  contract.ModelRequest
	lastMsgs []contract.Message
}

func (p *fakeOCRPool) ChatStream(context.Context, contract.ModelRequest, []contract.Message) (<-chan contract.StreamEvent, error) {
	return nil, nil
}
func (p *fakeOCRPool) Chat(_ context.Context, req contract.ModelRequest, msgs []contract.Message) (contract.ChatResponse, error) {
	p.lastReq, p.lastMsgs = req, msgs
	return p.resp, p.err
}
func (p *fakeOCRPool) ChatStreamWatch(context.Context, contract.ModelRequest, []contract.Message, contract.StreamWatcher) (contract.ChatResponse, error) {
	return contract.ChatResponse{}, nil
}
func (p *fakeOCRPool) ContextBudget() int              { return 0 }
func (p *fakeOCRPool) Snapshot() contract.PoolSnapshot { return contract.PoolSnapshot{} }
func (p *fakeOCRPool) FlushUsage(context.Context)      {}
func (p *fakeOCRPool) FlushUsageFinal(context.Context) {}
func (p *fakeOCRPool) Close()                          {}

// newVisionTool builds the merged image tool bound to the given fake pool. Most of
// these tests drive the read leg (task=read); `run` fills the task in.
func newVisionTool(pool contract.ModelPool) visionTool {
	get := func() contract.ModelPool { return pool }
	return visionTool{ocr: kindChat{pool: get, kind: contract.KindOCR}, pool: get}
}

// writePNG writes a w×h png to dir and returns its path.
func writePNG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{1, 2, 3, 255})
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return p
}

// run drives the image tool with a real-fs FileChannel rooted at root (images are read
// through the channel now, anchored to att.Path). reuses realFileChannel / fileOpener
// from 86-deliver_test.go (same package).
func run(t *testing.T, tool visionTool, root string, atts []contract.Attachment, args map[string]any) contract.ToolResult {
	t.Helper()
	if _, ok := args["task"]; !ok {
		args["task"] = "read" // these exercise the transcription leg
	}
	raw, _ := json.Marshal(args)
	opener := fileOpener{ch: &realFileChannel{root: root}}
	tc := contract.ToolContext{
		Sid:         "s1",
		Attachments: compose.NewTurnAttachments([]contract.MessageEvent{{Attachments: atts}}, opener),
		Channels:    opener,
	}
	return tool.Run(context.Background(), tc, raw)
}

func TestDetectImageMIME(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want string
	}{
		{"png", []byte{0x89, 0x50, 0x4e, 0x47, 0x0d}, "image/png"},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0, 0x00}, "image/jpeg"},
		{"webp", append([]byte("RIFF\x10\x00\x00\x00WEBP"), 0x00, 0x00), "image/webp"},
		{"bmp", []byte{0x42, 0x4d, 0x00, 0x00, 0x00}, "image/bmp"},
		{"heic", append([]byte{0, 0, 0, 0x18}, []byte("ftypheic")...), "image/heic"},
		{"heif mif1", append([]byte{0, 0, 0, 0x18}, []byte("ftypmif1")...), "image/heic"},
		{"mp4 ftyp not image", append([]byte{0, 0, 0, 0x18}, []byte("ftypisom")...), ""},
		{"unknown", []byte{0xde, 0xad, 0xbe, 0xef}, ""},
		{"too short", []byte{0x89}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectImageMIME(c.b); got != c.want {
				t.Errorf("detectImageMIME(%x) = %q, want %q", c.b, got, c.want)
			}
		})
	}
}

func TestStripLines(t *testing.T) {
	raw := `{"images":[{"lines":[{"bbox":[1,2,3,4],"text":"hi","conf":0.9}],"text":"hi","error":""}],"text":"hi"}`
	got := stripLines(raw)
	if strings.Contains(got, "lines") || strings.Contains(got, "bbox") || strings.Contains(got, "conf") {
		t.Errorf("stripLines kept per-line metadata: %s", got)
	}
	if !strings.Contains(got, `"text":"hi"`) {
		t.Errorf("stripLines dropped the text: %s", got)
	}
	// Unparseable input falls through unchanged.
	if stripLines("not json") != "not json" {
		t.Error("stripLines should return raw on parse failure")
	}
}

// TestOCRSoleImage: one image this turn → it's the subject whatever image_path says.
func TestOCRSoleImage(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "shot.png", 4, 4)
	pool := &fakeOCRPool{resp: contract.ChatResponse{Content: `{"images":[{"text":"hello","lines":[1]}],"text":"hello"}`}}
	tool := newVisionTool(pool)
	atts := []contract.Attachment{{Kind: "image", Path: "shot.png", FileName: "shot.png"}}

	res := run(t, tool, dir, atts, map[string]any{"file_path": "the model guessed wrong"})
	if !res.OK {
		t.Fatalf("expected OK, got error: %s", res.Error)
	}
	if pool.lastReq.Kind != contract.KindOCR {
		t.Errorf("expected KindOCR, got %q", pool.lastReq.Kind)
	}
	if len(pool.lastMsgs) != 1 || len(pool.lastMsgs[0].Images) != 1 {
		t.Fatalf("expected one message with one image, got %+v", pool.lastMsgs)
	}
	if strings.Contains(res.Data, "lines") {
		t.Errorf("include_lines defaults false — lines should be stripped: %s", res.Data)
	}
	if !strings.Contains(res.Data, "hello") {
		t.Errorf("missing transcript: %s", res.Data)
	}
}

// TestOCRMultiImageNeedsName: several images → the model must name one.
func TestOCRMultiImageNeedsName(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "a.png", 4, 4)
	writePNG(t, dir, "b.png", 4, 4)
	tool := newVisionTool(&fakeOCRPool{})
	atts := []contract.Attachment{
		{Kind: "image", Path: "a.png", FileName: "a.png"},
		{Kind: "image", Path: "b.png", FileName: "b.png"},
	}
	res := run(t, tool, dir, atts, map[string]any{})
	if res.OK {
		t.Fatal("expected error for ambiguous multi-image")
	}
	if !strings.Contains(res.Error, "a.png") || !strings.Contains(res.Error, "b.png") {
		t.Errorf("error should list both names: %s", res.Error)
	}
}

// TestOCRNoImage: nothing sent this turn → ask the user.
func TestOCRNoImage(t *testing.T) {
	tool := newVisionTool(&fakeOCRPool{})
	res := run(t, tool, t.TempDir(), nil, map[string]any{"file_path": "x.png"})
	if res.OK {
		t.Fatal("expected error when no attachment present")
	}
	if !strings.Contains(res.Error, "没有图片") {
		t.Errorf("unexpected error: %s", res.Error)
	}
}

func TestOCRBboxValidation(t *testing.T) {
	tool := newVisionTool(&fakeOCRPool{})
	root := t.TempDir()
	atts := []contract.Attachment{{Kind: "image", Path: "x.png", FileName: "x.png"}}
	cases := []struct {
		name string
		bbox []int
	}{
		{"three ints", []int{1, 2, 3}},
		{"negative", []int{-1, 0, 5, 5}},
		{"zero area", []int{5, 5, 5, 9}},
		{"inverted", []int{9, 0, 1, 9}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := run(t, tool, root, atts, map[string]any{"bbox": c.bbox})
			if res.OK {
				t.Errorf("bbox %v should be rejected", c.bbox)
			}
		})
	}
}

// TestOCRBboxOutOfFrame: a box past the real dimensions is rejected with the true size.
func TestOCRBboxOutOfFrame(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "small.png", 10, 10)
	tool := newVisionTool(&fakeOCRPool{})
	atts := []contract.Attachment{{Kind: "image", Path: "small.png", FileName: "small.png"}}
	res := run(t, tool, dir, atts, map[string]any{"bbox": []int{0, 0, 50, 50}})
	if res.OK {
		t.Fatal("expected out-of-frame bbox to be rejected")
	}
	if !strings.Contains(res.Error, "10×10") {
		t.Errorf("error should report real dimensions: %s", res.Error)
	}
}

// TestOCRBackendUnavailable: nil pool → a clear "backend unavailable" message.
func TestOCRBackendUnavailable(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "shot.png", 4, 4)
	tool := newVisionTool(nil)
	atts := []contract.Attachment{{Kind: "image", Path: "shot.png", FileName: "shot.png"}}
	res := run(t, tool, dir, atts, map[string]any{})
	if res.OK {
		t.Fatal("expected error when no backend wired")
	}
	if !strings.Contains(res.Error, "不可用") {
		t.Errorf("unexpected error: %s", res.Error)
	}
}

// TestOCRQueueFull: a full wait queue is a BUSY backend, not a broken one — the
// message must say so, or the model reads "unavailable" and substitutes a vision
// description for a transcription (the failure shape).
func TestOCRQueueFull(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "shot.png", 4, 4)
	tool := newVisionTool(&fakeOCRPool{err: fmt.Errorf("model pool: kind %q wait queue full: %w", "ocr", contract.ErrModelQueueFull)})
	atts := []contract.Attachment{{Kind: "image", Path: "shot.png", FileName: "shot.png"}}
	res := run(t, tool, dir, atts, map[string]any{})
	if res.OK {
		t.Fatal("expected an error when the queue is full")
	}
	if !strings.Contains(res.Error, "队列已满") {
		t.Errorf("queue-full must be distinguishable from a dead backend: %s", res.Error)
	}
}

// TestOCRDocumentMIMEImage: an iPhone HEIC delivered as Kind=document with image/* MIME
// is still treated as an OCR-able image.
func TestOCRDocumentIsImageByMIME(t *testing.T) {
	att := contract.Attachment{Kind: "document", MIME: "image/heic", Path: "x", FileName: "IMG.HEIC"}
	if !att.IsImageContent() {
		t.Error("HEIC-as-document should be image content")
	}
	doc := contract.Attachment{Kind: "document", MIME: "application/pdf", Path: "y", FileName: "f.pdf"}
	if doc.IsImageContent() {
		t.Error("a pdf document is not image content")
	}
}

// The merged tool's task dispatch. The three jobs must stay distinguishable at the
// RESULT level, not just internally: task=answer returns evidence the main model
// composes around (no ExitRequest), while task=reverse_prompt hands its product to
// the user verbatim (ExitRequest). Routing an ordinary image question through the
// verbatim door would deliver the vision model's raw words INSTEAD of an answer —
// the failure that made these separate tiers in the first place.
func TestImageTaskDispatch(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "shot.png", 4, 4)
	atts := []contract.Attachment{{Kind: "image", Path: "shot.png", FileName: "shot.png"}}

	t.Run("unknown task is refused", func(t *testing.T) {
		res := run(t, newVisionTool(nil), dir, atts, map[string]any{"task": "describe"})
		if res.OK || !strings.Contains(res.Error, "task") {
			t.Fatalf("want a task error, got OK=%v err=%q", res.OK, res.Error)
		}
	})

	t.Run("answer requires a question", func(t *testing.T) {
		res := run(t, newVisionTool(nil), dir, atts, map[string]any{"task": "answer"})
		if res.OK || !strings.Contains(res.Error, "question") {
			t.Fatalf("want a missing-question error, got OK=%v err=%q", res.OK, res.Error)
		}
	})

	t.Run("answer returns evidence, reverse_prompt returns a product", func(t *testing.T) {
		pool := &fakeVisionPool{reply: "图里是一张地图"}
		tool := visionTool{pool: func() contract.ModelPool { return pool }}

		res := run(t, tool, dir, atts, map[string]any{"task": "answer", "question": "这个图片意思是什么"})
		if !res.OK {
			t.Fatalf("answer failed: %s", res.Error)
		}
		if res.ExitRequest != nil {
			t.Fatal("task=answer must NOT end the turn — its output is evidence for the main model")
		}
		if pool.gotPrompt != "这个图片意思是什么" {
			t.Errorf("vision model was asked %q, want the user's own question", pool.gotPrompt)
		}

		res = run(t, tool, dir, atts, map[string]any{"task": "reverse_prompt"})
		if !res.OK {
			t.Fatalf("reverse_prompt failed: %s", res.Error)
		}
		if res.ExitRequest == nil || res.ExitRequest.Reply != "图里是一张地图" {
			t.Fatalf("task=reverse_prompt must deliver its product verbatim, got %+v", res.ExitRequest)
		}
	})
}

// fakeVisionPool answers any Chat with a canned reply from a `vision`-tagged entry,
// recording the instruction it was given.
type fakeVisionPool struct {
	fakeOCRPool
	reply     string
	gotPrompt string
}

func (p *fakeVisionPool) Chat(_ context.Context, _ contract.ModelRequest, msgs []contract.Message) (contract.ChatResponse, error) {
	if len(msgs) > 0 {
		p.gotPrompt = msgs[0].Content
	}
	return contract.ChatResponse{Content: p.reply, Model: "vis"}, nil
}

func (p *fakeVisionPool) Snapshot() contract.PoolSnapshot {
	return contract.PoolSnapshot{Entries: []contract.ModelInfo{{Name: "vis", Tags: []string{"vision"}}}}
}

// A task must refuse arguments that belong to a DIFFERENT task rather than drop them.
// The expensive case is task=answer with a bbox: the crop is meaningless to the vision
// leg, the whole image goes to the model, and the answer comes back sounding exactly
// as confident as a correct one — nobody can tell it answered about the picture when
// a region was asked about.
func TestImageRejectsCrossTaskFields(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "shot.png", 8, 8)
	atts := []contract.Attachment{{Kind: "image", Path: "shot.png", FileName: "shot.png"}}
	pool := &fakeVisionPool{reply: "irrelevant"}
	tool := visionTool{pool: func() contract.ModelPool { return pool }}

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"answer + bbox", map[string]any{"task": "answer", "question": "这是什么", "bbox": []int{0, 0, 4, 4}}, "bbox"},
		{"answer + enhance", map[string]any{"task": "answer", "question": "这是什么", "enhance": "gray"}, "enhance"},
		{"reverse_prompt + question", map[string]any{"task": "reverse_prompt", "question": "这是什么"}, "question"},
		{"read + question", map[string]any{"task": "read", "question": "这是什么"}, "question"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, tool, dir, atts, tc.args)
			if res.OK {
				t.Fatalf("want refusal, got OK (the stray %s was silently dropped)", tc.want)
			}
			if !strings.Contains(res.Error, tc.want) {
				t.Fatalf("error %q should name the offending field %q", res.Error, tc.want)
			}
		})
	}
}

// Task matching tolerates the shapes a model carrying old habits produces, so a
// stale " OCR " costs a normalisation rather than a whole wasted round.
func TestImageTaskNormalised(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "shot.png", 4, 4)
	atts := []contract.Attachment{{Kind: "image", Path: "shot.png", FileName: "shot.png"}}
	pool := &fakeVisionPool{reply: "一段提示词"}
	tool := visionTool{pool: func() contract.ModelPool { return pool }}

	res := run(t, tool, dir, atts, map[string]any{"task": "  Reverse_Prompt "})
	if !res.OK {
		t.Fatalf("normalised task rejected: %s", res.Error)
	}
	if res.ExitRequest == nil {
		t.Fatal("normalisation must reach the same dispatch arm, ExitRequest and all")
	}
}
