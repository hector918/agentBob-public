package providers

import (
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agentbob/contract"
)

// fakeOCRRequest holds what the test sidecar saw on a /ocr POST.
type fakeOCRRequest struct {
	imageLen int
	lang     string
	bbox     string
	enhance  string
}

// newFakeRapidOCR builds an httptest server that captures /ocr POST
// requests + returns the configured response. /healthz returns
// pingStatus. The returned *fakeOCRRequest is mutated in place by the
// /ocr handler; safe for sequential single-call tests.
func newFakeRapidOCR(t *testing.T, ocrStatus int, ocrBody string, pingStatus int) (*httptest.Server, *fakeOCRRequest) {
	t.Helper()
	captured := &fakeOCRRequest{}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(pingStatus)
	})
	mux.HandleFunc("/ocr", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, "parse multipart: "+err.Error(), 400)
			return
		}
		fhs := r.MultipartForm.File["image"]
		if len(fhs) > 0 {
			f, err := fhs[0].Open()
			if err == nil {
				b, _ := io.ReadAll(f)
				captured.imageLen = len(b)
				f.Close()
			}
		}
		if v := r.MultipartForm.Value["lang"]; len(v) > 0 {
			captured.lang = v[0]
		}
		if v := r.MultipartForm.Value["bbox"]; len(v) > 0 {
			captured.bbox = v[0]
		}
		if v := r.MultipartForm.Value["enhance"]; len(v) > 0 {
			captured.enhance = v[0]
		}
		w.WriteHeader(ocrStatus)
		_, _ = w.Write([]byte(ocrBody))
	})
	return httptest.NewServer(mux), captured
}

// TestRapidOCRChat_HappyPath: a chat with one image wraps the
// sidecar's reply into the N=1 aggregate envelope; the multipart body
// carries the image bytes.
func TestRapidOCRChat_HappyPath(t *testing.T) {
	body := `{"lines":[{"bbox":[[1,2],[3,2],[3,4],[1,4]],"text":"hello","conf":0.97}],"text":"hello"}`
	srv, captured := newFakeRapidOCR(t, 200, body, 200)
	defer srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := []contract.Message{{
		Role:   "user",
		Images: []contract.ImageRef{{Data: []byte{0x89, 0x50, 0x4e, 0x47}, MIME: "image/png"}},
	}}

	resp, err := c.Chat(context.Background(), "ppocrv4", msgs, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var agg rapidocrAggregateResponse
	if err := json.Unmarshal([]byte(resp.Content), &agg); err != nil {
		t.Fatalf("Content is not a valid aggregate envelope: %v (got %q)", err, resp.Content)
	}
	if len(agg.Images) != 1 {
		t.Fatalf("Images len = %d, want 1", len(agg.Images))
	}
	if agg.Images[0].Text != "hello" {
		t.Errorf("Images[0].Text=%q, want hello", agg.Images[0].Text)
	}
	if agg.Images[0].Error != "" {
		t.Errorf("Images[0].Error=%q, want empty", agg.Images[0].Error)
	}
	if agg.Text != "hello" {
		t.Errorf("aggregate Text=%q, want hello", agg.Text)
	}
	if captured.imageLen != 4 {
		t.Errorf("captured image bytes = %d, want 4", captured.imageLen)
	}
}

// TestRapidOCRChat_LangAndBBox: rapidocrParams JSON in Message.Content
// is parsed and forwarded as multipart form fields.
func TestRapidOCRChat_LangAndBBox(t *testing.T) {
	srv, captured := newFakeRapidOCR(t, 200, `{"lines":[],"text":""}`, 200)
	defer srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := []contract.Message{{
		Role:    "user",
		Content: `{"lang": "ja", "bbox": [10, 20, 110, 120], "enhance": "gray"}`,
		Images:  []contract.ImageRef{{Data: []byte("imgbytes"), MIME: "image/jpeg"}},
	}}

	if _, err := c.Chat(context.Background(), "", msgs, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if captured.lang != "ja" {
		t.Errorf("lang form field = %q, want ja", captured.lang)
	}
	if captured.bbox != "10,20,110,120" {
		t.Errorf("bbox form field = %q, want 10,20,110,120", captured.bbox)
	}
	if captured.enhance != "gray" {
		t.Errorf("enhance form field = %q, want gray", captured.enhance)
	}
}

// TestRapidOCRChat_NoImage: a chat without an image errors before any
// HTTP call (the sidecar is never contacted).
func TestRapidOCRChat_NoImage(t *testing.T) {
	c := NewRapidOCRClient("http://invalid-host-no-server.test")
	_, err := c.Chat(context.Background(), "", []contract.Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no image") {
		t.Errorf("expected 'no image' error, got: %v", err)
	}
}

// TestRapidOCRChat_MultipleImagesInOneMessage: rejects same-message
// multi-image (the OCR tool's contract is one image per Message; the
// batch path stacks one image per message).
func TestRapidOCRChat_MultipleImagesInOneMessage(t *testing.T) {
	c := NewRapidOCRClient("http://invalid-host-no-server.test")
	msgs := []contract.Message{{
		Role: "user",
		Images: []contract.ImageRef{
			{Data: []byte("a"), MIME: "image/png"},
			{Data: []byte("b"), MIME: "image/png"},
		},
	}}
	_, err := c.Chat(context.Background(), "", msgs, nil)
	if err == nil || !strings.Contains(err.Error(), "2 images") {
		t.Errorf("expected '2 images' error, got: %v", err)
	}
}

// TestRapidOCRChat_NonOKResponse: a 4xx/5xx response is collected as
// the per-image .Error field. For an N=1 batch with that image failing,
// the Chat call also bubbles a top-level err (all-fail → pool's
// liveness-fail). Content still carries
// the per-image .Error field so the caller sees the detail. The
// agent-visible per-image error stays sanitized — status code only,
// NOT the sidecar URL or raw body (info-hygiene §0.7).
func TestRapidOCRChat_NonOKResponse(t *testing.T) {
	srv, _ := newFakeRapidOCR(t, 500, "internal error", 200)
	defer srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := []contract.Message{{
		Role:   "user",
		Images: []contract.ImageRef{{Data: []byte("x"), MIME: "image/png"}},
	}}
	resp, err := c.Chat(context.Background(), "", msgs, nil)
	if err == nil {
		t.Fatalf("expected top-level err on all-images-failed (N=1, 500), got nil")
	}
	var agg rapidocrAggregateResponse
	if jerr := json.Unmarshal([]byte(resp.Content), &agg); jerr != nil {
		t.Fatalf("bad aggregate: %v", jerr)
	}
	if len(agg.Images) != 1 {
		t.Fatalf("Images len=%d, want 1", len(agg.Images))
	}
	gotErr := agg.Images[0].Error
	if !strings.Contains(gotErr, "HTTP 500") {
		t.Errorf("Images[0].Error should mention HTTP 500, got: %q", gotErr)
	}
	// Sanitization: NO URL, NO raw body forwarded to the model.
	if strings.Contains(gotErr, "http://") || strings.Contains(gotErr, "https://") {
		t.Errorf("Images[0].Error must not leak baseURL, got: %q", gotErr)
	}
	if strings.Contains(gotErr, "internal error") {
		t.Errorf("Images[0].Error must not leak raw response body, got: %q", gotErr)
	}
}

// TestRapidOCRChat_ParseError: a 200 with non-JSON body is collected
// as the per-image .Error field with the sanitized "invalid response
// shape" message — the raw body / decoder error stays in slog only.
// N=1 batch with that single image failing → Chat also bubbles a
// top-level err (all-fail). Content still carries the per-image .Error.
func TestRapidOCRChat_ParseError(t *testing.T) {
	srv, _ := newFakeRapidOCR(t, 200, "<html>not json</html>", 200)
	defer srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := []contract.Message{{
		Role:   "user",
		Images: []contract.ImageRef{{Data: []byte("x"), MIME: "image/png"}},
	}}
	resp, err := c.Chat(context.Background(), "", msgs, nil)
	if err == nil {
		t.Fatalf("expected top-level err on all-images-failed (N=1 parse fail), got nil")
	}
	var agg rapidocrAggregateResponse
	if jerr := json.Unmarshal([]byte(resp.Content), &agg); jerr != nil {
		t.Fatalf("bad aggregate: %v", jerr)
	}
	gotErr := agg.Images[0].Error
	if !strings.Contains(gotErr, "invalid response shape") {
		t.Errorf("Images[0].Error should mention 'invalid response shape', got: %q", gotErr)
	}
	// Sanitization: NO raw body excerpt forwarded to the model.
	if strings.Contains(gotErr, "<html>") || strings.Contains(gotErr, "not json") {
		t.Errorf("Images[0].Error must not leak raw response body, got: %q", gotErr)
	}
}

// TestRapidOCRChatStream: streaming returns an explicit unsupported
// error — the pool's ChatStream path is LLM-only and OCR uses Chat.
func TestRapidOCRChatStream(t *testing.T) {
	c := NewRapidOCRClient("http://wherever.test")
	ch, err := c.ChatStream(context.Background(), "", nil, nil)
	if err == nil {
		t.Error("expected ChatStream to return an unsupported error")
	}
	if ch != nil {
		t.Error("expected nil channel on unsupported error")
	}
}

// TestRapidOCRPing: GET /healthz returns 200 → nil; non-200 → error.
func TestRapidOCRPing(t *testing.T) {
	srvOK, _ := newFakeRapidOCR(t, 200, "", 200)
	defer srvOK.Close()
	if err := NewRapidOCRClient(srvOK.URL).Ping(context.Background()); err != nil {
		t.Errorf("ping ok: %v", err)
	}
	srvBad, _ := newFakeRapidOCR(t, 200, "", 503)
	defer srvBad.Close()
	err := NewRapidOCRClient(srvBad.URL).Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("expected ping err with 503, got: %v", err)
	}
}

// TestRapidOCRExtensionForMIME pins the cosmetic filename suffix the
// multipart builder picks per known image MIME.
func TestRapidOCRExtensionForMIME(t *testing.T) {
	cases := map[string]string{
		"image/png":                ".png",
		"image/jpeg":               ".jpg",
		"image/jpg":                ".jpg",
		"IMAGE/PNG":                ".png",
		"image/webp":               ".webp",
		"image/bmp":                ".bmp",
		"application/octet-stream": ".bin",
		"":                         ".bin",
	}
	for mime, want := range cases {
		if got := extensionForMIME(mime); got != want {
			t.Errorf("extensionForMIME(%q)=%q, want %q", mime, got, want)
		}
	}
}

// TestRapidOCRBuildMultipart re-parses the built body to confirm
// structure: image bytes + lang + bbox fields all present with the
// right content.
func TestRapidOCRBuildMultipart(t *testing.T) {
	buf, ct, err := buildRapidOCRMultipart(
		contract.ImageRef{Data: []byte("imgbytes"), MIME: "image/png"},
		rapidocrParams{Lang: "en", BBox: []int{1, 2, 3, 4}},
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	const prefix = "multipart/form-data; boundary="
	if !strings.HasPrefix(ct, prefix) {
		t.Fatalf("Content-Type=%q lacks boundary", ct)
	}
	boundary := ct[len(prefix):]
	reader := multipart.NewReader(buf, boundary)
	gotFields := map[string]string{}
	var imageBytes []byte
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		name := part.FormName()
		b, _ := io.ReadAll(part)
		if name == "image" {
			imageBytes = b
		} else {
			gotFields[name] = string(b)
		}
	}
	if string(imageBytes) != "imgbytes" {
		t.Errorf("image part = %q", imageBytes)
	}
	if gotFields["lang"] != "en" {
		t.Errorf("lang = %q, want en", gotFields["lang"])
	}
	if gotFields["bbox"] != "1,2,3,4" {
		t.Errorf("bbox = %q, want 1,2,3,4", gotFields["bbox"])
	}
}

// newCountingRapidOCR is a /ocr handler whose response is chosen per-
// call from a scripted slice — call i returns (statuses[i], bodies[i]).
// Lets a single test exercise mixed success / failure across N images.
func newCountingRapidOCR(t *testing.T, statuses []int, bodies []string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var count atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/ocr", func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so multipart isn't half-read on the connection.
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, "parse multipart: "+err.Error(), 400)
			return
		}
		idx := int(count.Add(1)) - 1
		status := 200
		body := `{"lines":[],"text":""}`
		if idx < len(statuses) {
			status = statuses[idx]
		}
		if idx < len(bodies) {
			body = bodies[idx]
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	return httptest.NewServer(mux), &count
}

// TestRapidOCRChat_MultiImage_Success: 2 messages with 1 image each
// → aggregate has 2 Images entries + the per-image texts joined by
// the \n---\n separator in agg.Text.
func TestRapidOCRChat_MultiImage_Success(t *testing.T) {
	bodies := []string{
		`{"lines":[],"text":"first"}`,
		`{"lines":[],"text":"second"}`,
	}
	srv, count := newCountingRapidOCR(t, []int{200, 200}, bodies)
	defer srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := []contract.Message{
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("a"), MIME: "image/png"}}},
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("b"), MIME: "image/png"}}},
	}

	resp, err := c.Chat(context.Background(), "", msgs, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := count.Load(); got != 2 {
		t.Errorf("/ocr POSTs = %d, want 2", got)
	}
	var agg rapidocrAggregateResponse
	if err := json.Unmarshal([]byte(resp.Content), &agg); err != nil {
		t.Fatalf("bad aggregate: %v", err)
	}
	if len(agg.Images) != 2 {
		t.Fatalf("Images len=%d, want 2", len(agg.Images))
	}
	if agg.Images[0].Text != "first" || agg.Images[1].Text != "second" {
		t.Errorf("Images text = (%q, %q), want (first, second)",
			agg.Images[0].Text, agg.Images[1].Text)
	}
	if agg.Text != "first\n---\nsecond" {
		t.Errorf("aggregate Text = %q, want %q", agg.Text, "first\n---\nsecond")
	}
}

// TestRapidOCRChat_MultiImage_PartialFailure: image 1 succeeds (200),
// image 2 fails (500). Both slots are present in agg.Images; only the
// failed slot has a non-empty .Error. Caller's batch survives.
func TestRapidOCRChat_MultiImage_PartialFailure(t *testing.T) {
	srv, _ := newCountingRapidOCR(t,
		[]int{200, 500},
		[]string{`{"lines":[],"text":"good"}`, "engine crashed"},
	)
	defer srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := []contract.Message{
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("a"), MIME: "image/png"}}},
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("b"), MIME: "image/png"}}},
	}

	resp, err := c.Chat(context.Background(), "", msgs, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var agg rapidocrAggregateResponse
	if err := json.Unmarshal([]byte(resp.Content), &agg); err != nil {
		t.Fatalf("bad aggregate: %v", err)
	}
	if len(agg.Images) != 2 {
		t.Fatalf("Images len=%d, want 2", len(agg.Images))
	}
	if agg.Images[0].Error != "" {
		t.Errorf("Images[0].Error=%q, want empty (success)", agg.Images[0].Error)
	}
	if agg.Images[0].Text != "good" {
		t.Errorf("Images[0].Text=%q, want good", agg.Images[0].Text)
	}
	if agg.Images[1].Error == "" {
		t.Error("Images[1].Error empty, want non-empty (500)")
	}
	if !strings.Contains(agg.Images[1].Error, "500") {
		t.Errorf("Images[1].Error=%q should mention 500", agg.Images[1].Error)
	}
}

// TestRapidOCRChat_PartialFailureMarker (audit D2): a 2-image batch
// with image 1 OK and image 2 returning 500 — the aggregate `text`
// must embed an "[OCR failed:" inline marker for the failed slot,
// alongside the successful image's text + the \n---\n separator.
// Lets a caller that reads `text` only still see the batch boundary.
func TestRapidOCRChat_PartialFailureMarker(t *testing.T) {
	srv, _ := newCountingRapidOCR(t,
		[]int{200, 500},
		[]string{`{"lines":[],"text":"first ok"}`, "engine boom"},
	)
	defer srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := []contract.Message{
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("a"), MIME: "image/png"}}},
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("b"), MIME: "image/png"}}},
	}

	resp, err := c.Chat(context.Background(), "", msgs, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var agg rapidocrAggregateResponse
	if err := json.Unmarshal([]byte(resp.Content), &agg); err != nil {
		t.Fatalf("bad aggregate: %v", err)
	}
	if !strings.Contains(agg.Text, "[OCR failed:") {
		t.Errorf("aggregate Text should embed '[OCR failed:' marker for the failed slot, got: %q", agg.Text)
	}
	if !strings.Contains(agg.Text, "first ok") {
		t.Errorf("aggregate Text should still carry image 1's text, got: %q", agg.Text)
	}
	if !strings.Contains(agg.Text, "\n---\n") {
		t.Errorf("aggregate Text should keep the \\n---\\n batch separator, got: %q", agg.Text)
	}
}

// TestChat_AllImagesTransportFail_ReturnsError: every image's POST is
// transport-refused (sidecar URL points nowhere) → Chat must bubble a
// top-level err so the pool's chatOne sees it and triggers a
// liveness-fail. Content
// still carries the per-image .Error JSON so the caller sees the
// per-image detail.
func TestChat_AllImagesTransportFail_ReturnsError(t *testing.T) {
	// httptest server we close immediately → every Do() gets connect refused.
	srv, _ := newCountingRapidOCR(t, nil, nil)
	srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := []contract.Message{
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("a"), MIME: "image/png"}}},
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("b"), MIME: "image/png"}}},
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("c"), MIME: "image/png"}}},
	}

	resp, err := c.Chat(context.Background(), "", msgs, nil)
	if err == nil {
		t.Fatalf("expected top-level err when ALL images transport-fail, got nil")
	}
	if !strings.Contains(err.Error(), "3/3 image") {
		t.Errorf("err should mention '3/3 image(s) failed', got: %v", err)
	}
	// Content still carries structured per-image errors so caller sees detail.
	var agg rapidocrAggregateResponse
	if jerr := json.Unmarshal([]byte(resp.Content), &agg); jerr != nil {
		t.Fatalf("Content not parseable as aggregate: %v", jerr)
	}
	if len(agg.Images) != 3 {
		t.Fatalf("Images len=%d, want 3", len(agg.Images))
	}
	for i, img := range agg.Images {
		if img.Error == "" {
			t.Errorf("Images[%d].Error empty, want transport-fail marker", i)
		}
		if !strings.Contains(img.Error, "backend unavailable") {
			t.Errorf("Images[%d].Error=%q, want 'backend unavailable'", i, img.Error)
		}
	}
}

// TestChat_PartialFail_ReturnsNilErr: 5 images, 3 succeed + 2 fail —
// collect-all semantics preserved (no top-level err); content carries
// both successes and failures. Only "ALL failed" trips the new error
// path; mixed batches stay caller's problem.
func TestChat_PartialFail_ReturnsNilErr(t *testing.T) {
	srv, _ := newCountingRapidOCR(t,
		[]int{200, 500, 200, 200, 500},
		[]string{
			`{"lines":[],"text":"one"}`,
			"boom",
			`{"lines":[],"text":"three"}`,
			`{"lines":[],"text":"four"}`,
			"boom",
		},
	)
	defer srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := make([]contract.Message, 5)
	for i := range msgs {
		msgs[i] = contract.Message{
			Role:   "user",
			Images: []contract.ImageRef{{Data: []byte{byte(i)}, MIME: "image/png"}},
		}
	}

	resp, err := c.Chat(context.Background(), "", msgs, nil)
	if err != nil {
		t.Fatalf("expected nil err on partial fail (collect-all), got: %v", err)
	}
	var agg rapidocrAggregateResponse
	if jerr := json.Unmarshal([]byte(resp.Content), &agg); jerr != nil {
		t.Fatalf("bad aggregate: %v", jerr)
	}
	if len(agg.Images) != 5 {
		t.Fatalf("Images len=%d, want 5", len(agg.Images))
	}
	// Spot-check: 0/2/3 are successes; 1/4 are failures.
	wantTexts := []string{"one", "", "three", "four", ""}
	wantFail := []bool{false, true, false, false, true}
	for i := range agg.Images {
		gotFail := agg.Images[i].Error != ""
		if gotFail != wantFail[i] {
			t.Errorf("Images[%d] fail=%v, want %v (err=%q)", i, gotFail, wantFail[i], agg.Images[i].Error)
		}
		if !wantFail[i] && agg.Images[i].Text != wantTexts[i] {
			t.Errorf("Images[%d].Text=%q, want %q", i, agg.Images[i].Text, wantTexts[i])
		}
	}
}

// TestChat_MajorityFail_ReturnsError (#383): 5 images, 2 succeed + 3 fail — a
// MAJORITY failure now bubbles a top-level err (an unhealthy sidecar must not bank
// a fake success), while content still carries the per-image detail.
func TestChat_MajorityFail_ReturnsError(t *testing.T) {
	srv, _ := newCountingRapidOCR(t,
		[]int{200, 500, 500, 200, 500},
		[]string{`{"lines":[],"text":"one"}`, "boom", "boom", `{"lines":[],"text":"four"}`, "boom"},
	)
	defer srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := make([]contract.Message, 5)
	for i := range msgs {
		msgs[i] = contract.Message{Role: "user", Images: []contract.ImageRef{{Data: []byte{byte(i)}, MIME: "image/png"}}}
	}
	resp, err := c.Chat(context.Background(), "", msgs, nil)
	if err == nil {
		t.Fatalf("expected top-level err when majority (3/5) fail, got nil")
	}
	if !strings.Contains(err.Error(), "3/5 image") {
		t.Errorf("err should mention '3/5 image(s) failed', got: %v", err)
	}
	if !strings.Contains(resp.Content, "[OCR failed:") {
		t.Errorf("content should still carry per-image failure markers, got: %q", resp.Content)
	}
}

// TestChat_AllSuccess_ReturnsNilErr (regression): all images succeed →
// no top-level err. Pin the happy path so the new all-fail check
// doesn't accidentally trip on a green batch.
func TestChat_AllSuccess_ReturnsNilErr(t *testing.T) {
	srv, _ := newCountingRapidOCR(t,
		[]int{200, 200, 200},
		[]string{
			`{"lines":[],"text":"a"}`,
			`{"lines":[],"text":"b"}`,
			`{"lines":[],"text":"c"}`,
		},
	)
	defer srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := []contract.Message{
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("1"), MIME: "image/png"}}},
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("2"), MIME: "image/png"}}},
		{Role: "user", Images: []contract.ImageRef{{Data: []byte("3"), MIME: "image/png"}}},
	}

	resp, err := c.Chat(context.Background(), "", msgs, nil)
	if err != nil {
		t.Fatalf("expected nil err on all-success, got: %v", err)
	}
	var agg rapidocrAggregateResponse
	if jerr := json.Unmarshal([]byte(resp.Content), &agg); jerr != nil {
		t.Fatalf("bad aggregate: %v", jerr)
	}
	for i, img := range agg.Images {
		if img.Error != "" {
			t.Errorf("Images[%d].Error=%q, want empty", i, img.Error)
		}
	}
}

// TestChat_SingleImageTransportFail_ReturnsError: N=1 batch where that
// one image transport-fails → all-fail predicate trips → top-level err.
// A single-image OCR call is the most common shape (the ocr tool is
// 1-image-per-call by contract), so this is the one that actually
// matters for the real-world sidecar-down bug.
func TestChat_SingleImageTransportFail_ReturnsError(t *testing.T) {
	srv, _ := newCountingRapidOCR(t, nil, nil)
	srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := []contract.Message{{
		Role:   "user",
		Images: []contract.ImageRef{{Data: []byte("x"), MIME: "image/png"}},
	}}

	resp, err := c.Chat(context.Background(), "", msgs, nil)
	if err == nil {
		t.Fatalf("expected top-level err on N=1 transport-fail, got nil")
	}
	if !strings.Contains(err.Error(), "1/1 image") {
		t.Errorf("err should mention '1/1 image(s) failed', got: %v", err)
	}
	var agg rapidocrAggregateResponse
	if jerr := json.Unmarshal([]byte(resp.Content), &agg); jerr != nil {
		t.Fatalf("Content not parseable: %v", jerr)
	}
	if len(agg.Images) != 1 || agg.Images[0].Error == "" {
		t.Errorf("expected 1 image with non-empty .Error, got: %+v", agg.Images)
	}
}

// TestChat_EmptyInputs_PreservesExistingBehavior (regression): a chat
// with no image messages goes through extractRapidOCRRequests's error
// path BEFORE the new all-fail check (len(agg.Images)>0 guard). The
// existing 'no image' err shape stays intact.
func TestChat_EmptyInputs_PreservesExistingBehavior(t *testing.T) {
	c := NewRapidOCRClient("http://no-server.invalid")
	_, err := c.Chat(context.Background(), "", []contract.Message{
		{Role: "user", Content: "no images here"},
	}, nil)
	if err == nil {
		t.Fatal("expected err on no-image chat, got nil")
	}
	if !strings.Contains(err.Error(), "no image") {
		t.Errorf("expected 'no image' err (pre-existing path), got: %v", err)
	}
	// Must NOT be the new all-fail message — extract error path runs first.
	if strings.Contains(err.Error(), "0/0 image") || strings.Contains(err.Error(), "sidecar likely unhealthy") {
		t.Errorf("err should be the 'no image' path, not the all-fail path, got: %v", err)
	}
}

// TestRapidOCRChat_TooManyImages: 11 messages with images → top-level
// error before any HTTP call (the batch cap is a hard limit).
func TestRapidOCRChat_TooManyImages(t *testing.T) {
	srv, count := newCountingRapidOCR(t, nil, nil)
	defer srv.Close()
	c := NewRapidOCRClient(srv.URL)
	msgs := make([]contract.Message, 11)
	for i := range msgs {
		msgs[i] = contract.Message{
			Role:   "user",
			Images: []contract.ImageRef{{Data: []byte{byte(i)}, MIME: "image/png"}},
		}
	}

	_, err := c.Chat(context.Background(), "", msgs, nil)
	if err == nil || !strings.Contains(err.Error(), "11 images") {
		t.Errorf("expected '11 images' error, got: %v", err)
	}
	if got := count.Load(); got != 0 {
		t.Errorf("/ocr POSTs = %d, want 0 (cap is pre-HTTP)", got)
	}
}
