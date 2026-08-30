package imagecreate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// --- fakes ----------------------------------------------------------------

// oneAttachment is an AttachmentSet holding a single in-memory picture, which is
// all an edit needs to resolve.
type oneAttachment struct {
	att  contract.Attachment
	data []byte
}

func (o *oneAttachment) Pick(context.Context, func(contract.Attachment) bool, string) []contract.Attachment {
	return []contract.Attachment{o.att}
}
func (o *oneAttachment) Suggest(context.Context, func(contract.Attachment) bool) []contract.Attachment {
	return []contract.Attachment{o.att}
}
func (o *oneAttachment) Read(context.Context, contract.Attachment, int64) ([]byte, error) {
	return o.data, nil
}

// echoPool answers like the ComfyUI provider and records the request it was given,
// so a test can assert on what the tool actually submitted.
type echoPool struct {
	fakePool
	got contract.Message
}

func (p *echoPool) Chat(ctx context.Context, _ contract.ModelRequest, msgs []contract.Message) (contract.ChatResponse, error) {
	p.got = msgs[len(msgs)-1]
	report := contract.ImageProgressFrom(ctx)
	report(contract.ImageEvent{Stage: contract.ImageStageSubmitted, PromptID: "job-1", Entry: "comfy-klein"})
	reply, _ := json.Marshal(map[string]any{
		"image_b64": base64.StdEncoding.EncodeToString([]byte("\x89PNG-not-really")),
		"width":     768, "height": 1024, "seconds": 20, "seed": int64(4242),
	})
	return contract.ChatResponse{Content: string(reply)}, nil
}

// pngOf encodes a w×h picture — only its header is ever read.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func editFixture(t *testing.T, pool contract.ModelPool, data []byte, mime string) (*Tool, contract.ToolContext) {
	t.Helper()
	tl, _, tc := generateFixture(t, pool)
	tc.Attachments = &oneAttachment{
		att:  contract.Attachment{Kind: "image", MIME: mime, Path: "inbox/src.png", FileName: "src.png"},
		data: data,
	}
	return tl, tc
}

func submitted(t *testing.T, m contract.Message) map[string]any {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(m.Content), &req); err != nil {
		t.Fatalf("submitted request is not JSON: %v", err)
	}
	return req
}

// --- tests ----------------------------------------------------------------

// The source picture decides the shape of an edit, overriding the caller. The
// backend centre-crops the init image to the requested aspect, so honouring a
// caller's "square" for a portrait photo silently cuts its top and bottom off —
// and the model cannot choose correctly anyway, having only a filename to go on.
func TestEditTakesAspectFromTheSourcePicture(t *testing.T) {
	for _, tc := range []struct {
		name       string
		w, h       int
		asked      string
		wantAspect string
	}{
		{"portrait source overrides a square ask", 600, 900, "square", "portrait"},
		{"landscape source overrides a portrait ask", 900, 600, "portrait", "landscape"},
		{"square source", 800, 800, "landscape", "square"},
		{"no ask at all still follows the source", 600, 900, "", "portrait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := &echoPool{fakePool: fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}}}
			tl, ctx := editFixture(t, pool, pngOf(t, tc.w, tc.h), "image/png")
			args := `{"style":"comfyui-klein","prompt":"a car","init_image":"inbox/src.png","aspect":"` + tc.asked + `"}`
			if res := tl.Run(context.Background(), ctx, json.RawMessage(args)); !res.OK {
				t.Fatalf("res = %+v, want success", res)
			}
			if got := submitted(t, pool.got)["aspect"]; got != tc.wantAspect {
				t.Errorf("submitted aspect = %v, want %v (source %d×%d, caller asked %q)",
					got, tc.wantAspect, tc.w, tc.h, tc.asked)
			}
		})
	}
}

// An undecodable source (WebP, HEIC — an iPhone shares HEIC by default) must leave
// the caller's aspect alone rather than guess: a wrong shape crops the picture.
func TestEditKeepsTheCallersAspectWhenTheSourceCannotBeRead(t *testing.T) {
	pool := &echoPool{fakePool: fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}}}
	tl, ctx := editFixture(t, pool, []byte("RIFF????WEBPVP8 not-really"), "image/webp")
	res := tl.Run(context.Background(), ctx,
		json.RawMessage(`{"style":"comfyui-klein","prompt":"a car","init_image":"inbox/src.png","aspect":"landscape"}`))
	if !res.OK {
		t.Fatalf("res = %+v, want success — an unreadable header must not fail the edit", res)
	}
	if got := submitted(t, pool.got)["aspect"]; got != "landscape" {
		t.Errorf("submitted aspect = %v, want the caller's landscape", got)
	}
	// And the receipt must not claim a source-derived shape it never derived.
	if strings.Contains(res.Data, "按原图") {
		t.Errorf("receipt claims the shape came from the source: %q", res.Data)
	}
}

// The receipt is what a follow-up ("改狠一点") is measured against, so it has to
// state the strength actually used — including the one nobody set.
func TestEditReceiptReportsTheStrengthAndShapeUsed(t *testing.T) {
	pool := &echoPool{fakePool: fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}}}
	tl, ctx := editFixture(t, pool, pngOf(t, 600, 900), "image/png")
	res := tl.Run(context.Background(), ctx,
		json.RawMessage(`{"style":"comfyui-klein","prompt":"a car","init_image":"inbox/src.png"}`))
	if !res.OK {
		t.Fatalf("res = %+v, want success", res)
	}
	for _, want := range []string{"change=moderate", "按原图取的 portrait", "seed 4242"} {
		if !strings.Contains(res.Data, want) {
			t.Errorf("receipt is missing %q: %q", want, res.Data)
		}
	}
}

// A from-scratch picture has no edit strength and no source shape — saying
// otherwise would put a parameter in the receipt that was never in play.
func TestFromScratchReceiptCarriesSeedButNoEditFields(t *testing.T) {
	pool := &echoPool{fakePool: fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}}}
	tl, _, ctx := generateFixture(t, pool)
	res := tl.Run(context.Background(), ctx, json.RawMessage(`{"style":"comfyui-klein","prompt":"a car"}`))
	if !res.OK {
		t.Fatalf("res = %+v, want success", res)
	}
	if !strings.Contains(res.Data, "seed 4242") {
		t.Errorf("receipt has no seed: %q", res.Data)
	}
	if strings.Contains(res.Data, "change=") || strings.Contains(res.Data, "按原图") {
		t.Errorf("receipt reports edit fields for a from-scratch picture: %q", res.Data)
	}
}

// --- recovery sweep -------------------------------------------------------

// exploding stands in for the two collaborators a sweep needs. Any call means the
// sweep adopted a record it should not have touched.
type explodingGateway struct{ t *testing.T }

func (g explodingGateway) Events() <-chan contract.MessageEvent { return nil }
func (g explodingGateway) SourceNames() []string                { return nil }
func (g explodingGateway) SourceByName(string) contract.Source {
	g.t.Error("sweep tried to route a notice for a job this process is still running")
	return nil
}

type explodingPool struct {
	fakePool
	t *testing.T
}

func (p *explodingPool) Chat(context.Context, contract.ModelRequest, []contract.Message) (contract.ChatResponse, error) {
	p.t.Error("sweep probed a job this process is still running — that probe queues behind the generation itself")
	return contract.ChatResponse{}, nil
}

// A record belonging to a generation running RIGHT NOW is not an orphan. The log
// is written ahead of the work, so such a record is always on disk, and the sweep
// tick lands inside a render often enough to matter (2-minute cadence, ~45-second
// renders). Production: the sweep adopted a live job, its probe expired
// queued behind that same job, and the user was told "没画成" about a picture that
// arrived nine seconds later — with the record already deleted.
func TestSweepSkipsJobsThisProcessIsStillRunning(t *testing.T) {
	home := t.TempDir()
	tl := New(func() contract.ModelPool { return &explodingPool{t: t} },
		testCatalog, func() contract.Gateway { return explodingGateway{t: t} }, home)

	tl.wal.claim(walRecord{
		PromptID: "job-live", Entry: "comfy-anima", Scope: "telegram:dm:7",
		Style: "comfyui-anima", Prompt: "x", Created: clock.Now().Unix(),
	})

	if err := tl.sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// And the record must still be there: dropping it is what would make a genuine
	// crash unrecoverable.
	if n := len(tl.wal.list()); n != 1 {
		t.Errorf("wal holds %d records after the sweep, want the live job's record kept", n)
	}
}

// Against a sink that sends immediately, delivery completes inside the call — so the
// record is retired with it, and a later sweep is free to act on anything left behind.
func TestFinishingACallClearsItsInflightMark(t *testing.T) {
	pool := &echoPool{fakePool: fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}}}
	tl, _, tc := generateFixture(t, pool)
	if res := tl.Run(context.Background(), tc, json.RawMessage(`{"style":"comfyui-klein","prompt":"a car"}`)); !res.OK {
		t.Fatalf("res = %+v, want success", res)
	}
	if tl.wal.isOwned("job-1") {
		t.Error("the in-flight mark outlived the call — a later crash's records would never be recovered")
	}
	if n := len(tl.wal.owned); n != 0 {
		t.Errorf("ownership map holds %d stale entries", n)
	}
	if n := len(tl.wal.list()); n != 0 {
		t.Errorf("wal holds %d records after a delivered picture, want 0", n)
	}
}

// The whole point of holding: against a sink that DEFERS the send, the picture is not
// the user's when the call returns, so the record must outlive the call. Retiring it
// on return (what the code used to do) would leave a crash mid-hold unrecoverable —
// a finished picture nobody will ever send and no record pointing at it.
func TestAHeldPictureKeepsItsRecordUntilItIsActuallySent(t *testing.T) {
	pool := &echoPool{fakePool: fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}}}
	tl, _, tc := generateFixture(t, pool)
	holder := &holdingSink{Sink: tc.Sink}
	tc.Sink = holder

	if res := tl.Run(context.Background(), tc, json.RawMessage(`{"style":"comfyui-klein","prompt":"a car"}`)); !res.OK {
		t.Fatalf("res = %+v, want success", res)
	}
	if !tl.wal.isOwned("job-1") {
		t.Fatal("the record was retired while the picture was still held — a crash now loses it silently")
	}
	if n := len(tl.wal.list()); n != 1 {
		t.Fatalf("wal holds %d records while holding, want the picture's record kept", n)
	}

	holder.deliver(nil) // the turn ends and the picture goes out
	if tl.wal.isOwned("job-1") {
		t.Error("the record is still claimed after delivery — the sweep will skip it forever")
	}
	if n := len(tl.wal.list()); n != 0 {
		t.Errorf("wal holds %d records after delivery, want 0 (a leftover re-sends the picture)", n)
	}
}

// A held picture that cannot be sent still retires its record. The sweep re-fetches
// from the BACKEND, and these bytes are already saved — keeping the record would buy
// hours of pointless probing and can end in the sweep announcing 「没画成」 about a
// picture that was drawn. What must never happen is the third option: a record left
// claimed, which blinds the sweep to it forever.
func TestAHeldPictureThatCannotBeSentRetiresItsRecord(t *testing.T) {
	pool := &echoPool{fakePool: fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}}}
	tl, _, tc := generateFixture(t, pool)
	holder := &holdingSink{Sink: tc.Sink}
	tc.Sink = holder

	if res := tl.Run(context.Background(), tc, json.RawMessage(`{"style":"comfyui-klein","prompt":"a car"}`)); !res.OK {
		t.Fatalf("res = %+v, want success", res)
	}
	holder.deliver(errors.New("chat is gone"))

	if tl.wal.isOwned("job-1") {
		t.Error("still claimed after a failed send — the sweep is now blind to this record forever")
	}
	if n := len(tl.wal.list()); n != 0 {
		t.Errorf("wal holds %d records after a failed send, want 0 — the bytes are saved, the sweep can only re-probe the backend for nothing", n)
	}
}

// A picture handed to a sink that sends immediately (every regular turn, every
// sub-turn) must report a failed send in the tool's own result. The model repeats
// this result to the user, so swallowing the error is how "画好了" gets said about a
// picture that never left.
func TestAnImmediateSendFailureIsReportedToTheModel(t *testing.T) {
	pool := &echoPool{fakePool: fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}}}
	tl, _, tc := generateFixture(t, pool)
	tc.Sink = &refusingSink{Sink: tc.Sink}

	res := tl.Run(context.Background(), tc, json.RawMessage(`{"style":"comfyui-klein","prompt":"a car"}`))
	if res.OK {
		t.Fatalf("res = %+v, want a failure — the send did not happen and the model must not claim it did", res)
	}
	if !strings.Contains(res.Error, "发送失败") {
		t.Errorf("error = %q, want it to name the send failure", res.Error)
	}
	if !strings.Contains(res.Error, "image_create/") {
		t.Errorf("error = %q, want the saved path so the user can still be pointed at the file", res.Error)
	}
}

// refusingSink accepts nothing: every attachment send fails.
type refusingSink struct{ contract.Sink }

func (s *refusingSink) SendFile(string, string) error { return errors.New("chat is gone") }

// holdingSink stands in for the looping driver's quietSink: it takes pictures and
// sends nothing until the test says so.
type holdingSink struct {
	contract.Sink
	held []func(error)
}

func (h *holdingSink) HoldPicture(_, _ string, onSent func(error)) {
	h.held = append(h.held, onSent)
}

// deliver reports the given outcome for every held picture, as a real flush would.
func (h *holdingSink) deliver(err error) {
	held := h.held
	h.held = nil
	for _, onSent := range held {
		onSent(err)
	}
}
