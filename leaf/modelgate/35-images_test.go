package modelgate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbob/contract"
)

// stubCatalog is the shared style catalog as modelgate sees it.
type stubCatalog struct{}

func (stubCatalog) ImageStyles() []contract.ImageStyleInfo {
	return []contract.ImageStyleInfo{
		{
			Style: "comfyui-klein", Summary: "写实 / 通用。", ETA: "约 30 秒", Guide: "flux2-klein",
			Sizes:   map[string][]int{"square": {768, 768}, "portrait": {768, 1024}},
			Changes: []string{"slight", "moderate", "heavy"},
		},
		{
			Style: "comfyui-anima", Summary: "动漫 / 二次元。", ETA: "约 10 秒", Guide: "anima",
			Sizes:   map[string][]int{"square": {1024, 1024}},
			Changes: []string{"slight", "moderate", "heavy"},
		},
	}
}

func (stubCatalog) ImageGuide(style string) (string, bool) {
	switch style {
	case "comfyui-klein":
		return "KLEIN MANUAL: 用自然语言，100 词以内。", true
	case "comfyui-anima":
		return "ANIMA MANUAL: 逗号分隔的标签。", true
	}
	return "", false
}

func imageServer(pool contract.ModelPool, keys contract.APIKeys) *server {
	s := newTestServer(pool, keys)
	s.images = func() contract.ImageCatalog { return stubCatalog{} }
	return s
}

// imgPool answers like the comfyui provider does: a JSON envelope with base64.
func imgPool() *fakePool {
	body, _ := json.Marshal(map[string]any{
		"image_b64": base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n")),
		"width":     768, "height": 768,
	})
	return &fakePool{
		chatResp: contract.ChatResponse{Content: string(body)},
		entries: []contract.ModelInfo{
			{Name: "comfy-gpu1", Kind: contract.KindImage, State: "live", Tags: []string{"comfyui-klein"}},
			{Name: "comfy-gpu4", Kind: contract.KindImage, State: "live", Tags: []string{"comfyui-anima"}},
		},
	}
}

func imgKeys() fakeKeys {
	return fakeKeys{byToken: map[string]contract.APIKeyInfo{
		"img":  {ID: "k1", Kinds: []string{"image"}},
		"chat": {ID: "k2", Kinds: []string{"llm"}},
		"pin":  {ID: "k3", Models: []string{"comfy-gpu1"}},
	}}
}

func TestImageGenerationsHappyPath(t *testing.T) {
	pool := imgPool()
	s := imageServer(pool, imgKeys())
	w := do(s, "POST", "/v1/images/generations", "img",
		`{"model":"comfyui-klein","prompt":"a red bicycle","size":"portrait"}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Created int64 `json:"created"`
		Data    []struct {
			B64           string `json:"b64_json"`
			Width, Height int
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(got.Data) != 1 || got.Data[0].B64 == "" {
		t.Fatalf("expected one b64 image, got %+v", got.Data)
	}
	// The style must reach the pool as a REQUIRED tag on the image kind — that is
	// what routes it, and a request that lost it would draw in whatever style the
	// pool happened to pick.
	if pool.lastReq.Kind != contract.KindImage {
		t.Errorf("Kind = %q, want image", pool.lastReq.Kind)
	}
	if len(pool.lastReq.Requires) != 1 || pool.lastReq.Requires[0] != "comfyui-klein" {
		t.Errorf("Requires = %v, want the style", pool.lastReq.Requires)
	}
}

// A caller that asked for four pictures and silently received one has no way to
// notice; refusing is the only honest answer for a backend that makes one.
func TestImageGenerationsRefusesMultipleImages(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "POST", "/v1/images/generations", "img",
		`{"model":"comfyui-klein","prompt":"x","n":4}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "n") {
		t.Fatalf("status %d body %s — want a 400 naming n", w.Code, w.Body.String())
	}
}

// No nearest-match on size: silently serving a square to someone who asked for a
// wide banner produces a picture that is wrong in a way they cannot see. The
// refusal must name the alternatives so the retry can succeed.
func TestImageGenerationsRefusesUnsupportedSize(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "POST", "/v1/images/generations", "img",
		`{"model":"comfyui-klein","prompt":"x","size":"1792x1024"}`)
	if w.Code != 400 {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "768x1024") || !strings.Contains(body, "portrait") {
		t.Errorf("refusal does not list the supported sizes: %s", body)
	}
}

// An exact WxH the style does offer is accepted — an OpenAI SDK sends pixels, not
// aspect words.
func TestImageGenerationsAcceptsExactPixelSize(t *testing.T) {
	pool := imgPool()
	w := do(imageServer(pool, imgKeys()), "POST", "/v1/images/generations", "img",
		`{"model":"comfyui-klein","prompt":"x","size":"768x1024"}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestImageGenerationsRefusesUnknownStyle(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "POST", "/v1/images/generations", "img",
		`{"model":"sdxl","prompt":"x"}`)
	if w.Code != 404 {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "comfyui-klein") {
		t.Errorf("refusal does not list what IS available: %s", w.Body.String())
	}
}

// A key that admits only the chat lane must not draw, and a models-form key
// addresses entry names rather than lanes so it cannot either.
func TestImageGenerationsRequiresTheImageLane(t *testing.T) {
	for _, token := range []string{"chat", "pin"} {
		w := do(imageServer(imgPool(), imgKeys()), "POST", "/v1/images/generations", token,
			`{"model":"comfyui-klein","prompt":"x"}`)
		if w.Code != 403 {
			t.Errorf("token %q: status %d, want 403 — %s", token, w.Code, w.Body.String())
		}
	}
}

func TestImageGenerationsRequiresPrompt(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "POST", "/v1/images/generations", "img",
		`{"model":"comfyui-klein"}`)
	if w.Code != 400 {
		t.Fatalf("status %d, want 400", w.Code)
	}
}

// bob does not host images, so a caller expecting a URL must be told rather than
// handed base64 it will not look for.
func TestImageGenerationsRefusesURLFormat(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "POST", "/v1/images/generations", "img",
		`{"model":"comfyui-klein","prompt":"x","response_format":"url"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "b64_json") {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

// --- the point of the whole exercise: one copy of the guidance, two readers ---

// The listing must carry enough to CHOOSE — an external caller writes its own
// prompts and the standard OpenAI model object says nothing it could choose by.
func TestModelsListingCarriesStyleMetadata(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "GET", "/v1/models", "img", "")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	type row struct {
		ID          string   `json:"id"`
		State       string   `json:"state"`
		Description string   `json:"description"`
		ETA         string   `json:"eta"`
		Sizes       []string `json:"sizes"`
	}
	var got struct {
		Data []row `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	var klein *row
	for i := range got.Data {
		if got.Data[i].ID == "comfyui-klein" {
			klein = &got.Data[i]
		}
	}
	if klein == nil {
		t.Fatalf("image styles missing from the listing: %s", w.Body.String())
	}
	// A style is just a tag with a manual, and the catalog answers "what is there,
	// can I use it now" — nothing about what it is FOR. The manual is one hop away
	// (TestModelDetailCarriesTheGuide), so carrying it here too would be a second
	// copy on every listing.
	if klein.Description != "" || klein.ETA != "" || len(klein.Sizes) != 0 {
		t.Errorf("the listing must not carry the manual: %+v", *klein)
	}
	if klein.State != "live" {
		t.Errorf("state = %q, want live", klein.State)
	}
}

// The full manual is the reason the catalog is shared rather than embedded in the
// tool. If this ever stops returning it, an external caller is back to guessing.
func TestModelDetailCarriesTheGuide(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "GET", "/v1/models/comfyui-klein", "img", "")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	guide, _ := got["guide"].(string)
	if !strings.Contains(guide, "KLEIN MANUAL") {
		t.Errorf("guide missing or wrong: %v", got["guide"])
	}
	if got["id"] != "comfyui-klein" {
		t.Errorf("id = %v", got["id"])
	}
}

// A key without the image lane must not read style guidance either — the listing
// and the detail agree on what it can see.
func TestModelDetailHidesStylesFromNonImageKeys(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "GET", "/v1/models/comfyui-klein", "chat", "")
	if w.Code != 404 {
		t.Errorf("status %d, want 404 for a key with no image lane", w.Code)
	}
}

func TestModelDetailUnknownID(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "GET", "/v1/models/nope", "img", "")
	if w.Code != 404 {
		t.Errorf("status %d, want 404", w.Code)
	}
}

// With no catalog wired (the model module absent or older), the endpoints must
// report that nothing is drawable rather than panicking on a nil interface.
func TestImagesDegradeWithoutCatalog(t *testing.T) {
	s := newTestServer(imgPool(), imgKeys()) // no s.images
	w := do(s, "POST", "/v1/images/generations", "img", `{"model":"comfyui-klein","prompt":"x"}`)
	if w.Code != 404 {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body.String())
	}
	if w2 := do(s, "GET", "/v1/models", "img", ""); w2.Code != 200 {
		t.Errorf("listing broke without a catalog: %d", w2.Code)
	}
}

// "Declared but not being served right now" and "does not exist" are DIFFERENT
// answers, and the catalog and the endpoint have to give the same one. The catalog
// lists the capability with a non-usable state; the request is refused, but refused
// honestly — "unknown model" would send the caller off to fix a name that was never
// wrong.
func TestCoolingStyleIsListedAndRefusedHonestly(t *testing.T) {
	pool := imgPool()
	pool.entries = []contract.ModelInfo{
		{Name: "comfy-gpu1", Kind: contract.KindImage, State: "cooling", Tags: []string{"comfyui-klein"}},
		{Name: "comfy-gpu4", Kind: contract.KindImage, State: "live", Tags: []string{"comfyui-anima"}},
	}
	s := imageServer(pool, imgKeys())
	body := do(s, "GET", "/v1/models", "img", "").Body.String()
	if !strings.Contains(body, `"id":"comfyui-klein","object":"model"`) || !strings.Contains(body, `"state":"cooling"`) {
		t.Errorf("a cooling capability must be listed WITH its state: %s", body)
	}
	if !strings.Contains(body, "comfyui-anima") {
		t.Errorf("dropped a style that IS live: %s", body)
	}
	w := do(s, "POST", "/v1/images/generations", "img", `{"model":"comfyui-klein","prompt":"x"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d for a declared-but-unserved style, want 503: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "unknown model") {
		t.Errorf("refusal must not claim the name is unknown: %s", w.Body.String())
	}
	// A name that really is unknown still gets the 404 it deserves.
	if w2 := do(s, "POST", "/v1/images/generations", "img",
		`{"model":"nope","prompt":"x"}`); w2.Code != http.StatusNotFound {
		t.Errorf("status %d for a genuinely unknown style, want 404", w2.Code)
	}
}

// The catalog is flat, so a chat-only client (a model dropdown) can pick a style off
// it and send it to the chat endpoint — it structurally cannot reach the images one.
// Point at the right endpoint rather than routing a chat payload at a renderer.
func TestChatRefusesAnImageCapabilityWithAPointer(t *testing.T) {
	s := imageServer(imgPool(), imgKeys())
	w := do(s, "POST", "/v1/chat/completions", "img",
		`{"model":"comfyui-klein","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/v1/images/generations") {
		t.Errorf("refusal must name the endpoint that takes it: %s", w.Body.String())
	}
}

// Tags are a general routing facility: an image entry may carry operational ones
// that are not styles. Offering those as capabilities would have the two endpoints
// point at each other over a name the catalog invented — chat says "go draw it",
// images says "unknown model".
func TestCatalogHidesUndeclaredImageTags(t *testing.T) {
	pool := imgPool()
	pool.entries = []contract.ModelInfo{
		{Name: "comfy-gpu1", Kind: contract.KindImage, State: "live", Tags: []string{"comfyui-klein", "gpu1"}},
	}
	s := imageServer(pool, imgKeys())
	body := do(s, "GET", "/v1/models", "img", "").Body.String()
	if strings.Contains(body, "gpu1") {
		t.Errorf("an operational tag was advertised as a capability: %s", body)
	}
	if !strings.Contains(body, "comfyui-klein") {
		t.Errorf("the declared style is missing: %s", body)
	}
	if w := do(s, "GET", "/v1/models/gpu1", "img", ""); w.Code != http.StatusNotFound {
		t.Errorf("detail for an operational tag: got %d, want 404", w.Code)
	}
	// and chat must call it unknown rather than sending the caller to the image endpoint
	w := do(s, "POST", "/v1/chat/completions", "img",
		`{"model":"gpu1","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("chat status %d for an operational tag, want 404: %s", w.Code, w.Body.String())
	}
}

// An image-only key posting to the chat endpoint with no `model` at all still has to
// be told where to go — the refusal must not interpolate an empty name.
func TestChatImagePointerWithoutAModelName(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "POST", "/v1/chat/completions", "img",
		`{"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `\"\"`) || strings.Contains(w.Body.String(), `""  draws`) {
		t.Errorf("refusal interpolated an empty name: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/v1/images/generations") {
		t.Errorf("refusal must name the endpoint that takes it: %s", w.Body.String())
	}
}

// tentative is pick-eligible — the heartbeat re-admitted it, it just has not been
// proven by a real request yet. Treating it as absent made a flapping GPU look like
// a style that does not exist.
func TestTentativeStyleIsDrawable(t *testing.T) {
	pool := imgPool()
	pool.entries = []contract.ModelInfo{
		{Name: "comfy-gpu1", Kind: contract.KindImage, State: "tentative", Tags: []string{"comfyui-klein"}},
	}
	s := imageServer(pool, imgKeys())
	if w := do(s, "POST", "/v1/images/generations", "img",
		`{"model":"comfyui-klein","prompt":"x"}`); w.Code != 200 {
		t.Errorf("status %d for a tentative backend, want 200: %s", w.Code, w.Body.String())
	}
}

// The edit strength is OUR field, not OpenAI's, so a caller following the standard
// shape omits it. Failing on that made every plain edits call die as a
// routing-flavoured 503 that named the wrong cause.
func TestImageEditsDefaultsChange(t *testing.T) {
	pool := imgPool()
	s := imageServer(pool, imgKeys())
	body, ct := multipartEdit(t, map[string]string{"model": "comfyui-klein", "prompt": "make it night"}, []byte("\x89PNG"))
	r := httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Authorization", "Bearer img")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(pool.lastMsgContent), &sent)
	if sent["change"] != "moderate" {
		t.Errorf("change = %v, want the default moderate", sent["change"])
	}
}

// An edit with no `size` takes the SOURCE picture's shape. The backend scales the
// source with a centre crop, so the old square default cut the top and bottom off a
// portrait photo and then reported that square as the size that was asked for.
func TestImageEditsTakeTheShapeFromTheSource(t *testing.T) {
	pool := imgPool()
	s := imageServer(pool, imgKeys())
	body, ct := multipartEdit(t, map[string]string{"model": "comfyui-klein", "prompt": "make it night"}, portraitPNG(t))
	r := httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Authorization", "Bearer img")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(pool.lastMsgContent), &sent)
	if sent["aspect"] != "portrait" {
		t.Errorf("aspect = %v, want portrait (the source's own shape)", sent["aspect"])
	}
}

// A NAMED size still wins, even against the source's shape. Unlike the model in a
// conversation — which only ever sees a filename — an HTTP caller posted the bytes
// itself, so an explicit size is a choice rather than a guess; overriding it would
// make `size` a silently-dropped field on this endpoint.
func TestImageEditsHonourAnExplicitSize(t *testing.T) {
	pool := imgPool()
	s := imageServer(pool, imgKeys())
	body, ct := multipartEdit(t, map[string]string{
		"model": "comfyui-klein", "prompt": "make it night", "size": "square",
	}, portraitPNG(t))
	r := httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Authorization", "Bearer img")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(pool.lastMsgContent), &sent)
	if sent["aspect"] != "square" {
		t.Errorf("aspect = %v, want the square the caller named", sent["aspect"])
	}
}

// A shape the style does not declare (anima offers only a square here) falls back
// rather than routing an aspect the backend has no size for.
func TestImageEditsFallBackWhenTheStyleLacksTheShape(t *testing.T) {
	pool := imgPool()
	s := imageServer(pool, imgKeys())
	body, ct := multipartEdit(t, map[string]string{"model": "comfyui-anima", "prompt": "1girl, night"}, portraitPNG(t))
	r := httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Authorization", "Bearer img")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(pool.lastMsgContent), &sent)
	if sent["aspect"] != "square" {
		t.Errorf("aspect = %v, want square (the style declares no portrait)", sent["aspect"])
	}
}

// filler streams bytes forever without allocating them — an over-cap upload has to
// be produced, not held.
type filler struct{}

func (filler) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// An over-cap body fails inside ParseMultipartForm, where the reader is already
// exhausted — so without telling the two apart the caller is told its multipart body
// was malformed, which sends it looking in the wrong place entirely.
func TestImageEditsRefusesAnOversizedUpload(t *testing.T) {
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	_ = mw.WriteField("model", "comfyui-klein")
	_ = mw.WriteField("prompt", "make it night")
	if _, err := mw.CreateFormFile("image", "huge.png"); err != nil {
		t.Fatal(err)
	}
	// Deliberately left unclosed: the body just keeps going past the cap.
	body := io.MultiReader(bytes.NewReader(head.Bytes()), io.LimitReader(filler{}, imagesMaxUpload+1<<20))
	r := httptest.NewRequest("POST", "/v1/images/edits", body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer img")
	w := httptest.NewRecorder()
	imageServer(imgPool(), imgKeys()).routes().ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("status %d body %s — want 413", w.Code, w.Body.String())
	}
}

// `change` on a from-scratch generation has nothing to change, so it would be
// dropped — and a dropped parameter reads exactly like an honoured one in the reply.
func TestImageGenerationsRefusesChange(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "POST", "/v1/images/generations", "img",
		`{"model":"comfyui-klein","prompt":"x","change":"heavy"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "edits") {
		t.Fatalf("status %d body %s — want a 400 naming the edits endpoint", w.Code, w.Body.String())
	}
}

// A named-but-unknown strength is refused with the list, like an unsupported size.
func TestImageEditsRefusesUnknownChange(t *testing.T) {
	s := imageServer(imgPool(), imgKeys())
	body, ct := multipartEdit(t, map[string]string{
		"model": "comfyui-klein", "prompt": "x", "change": "nuclear",
	}, []byte("\x89PNG"))
	r := httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Authorization", "Bearer img")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "moderate") {
		t.Fatalf("status %d body %s — want a 400 listing the strengths", w.Code, w.Body.String())
	}
}

// A key with no image lane must not learn the style catalog from a failed lookup:
// the 403 has to come first, or a junk model name returns the whole list and a
// real one confirms itself by returning 403 instead of 404.
func TestImagesDoNotLeakStylesToNonImageKeys(t *testing.T) {
	w := do(imageServer(imgPool(), imgKeys()), "POST", "/v1/images/generations", "chat",
		`{"model":"whatever","prompt":"x"}`)
	if w.Code != 403 {
		t.Fatalf("status %d, want 403", w.Code)
	}
	if strings.Contains(w.Body.String(), "comfyui-") {
		t.Errorf("the refusal leaked the style catalog: %s", w.Body.String())
	}
}

// The lookup is forgiving about case, but routing is exact — so the canonical
// name must be what reaches the pool, or the request sails past the 404 and dies
// later as an unroutable one.
func TestImageGenerationsCanonicalisesStyle(t *testing.T) {
	pool := imgPool()
	w := do(imageServer(pool, imgKeys()), "POST", "/v1/images/generations", "img",
		`{"model":"COMFYUI-Klein","prompt":"x"}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(pool.lastReq.Requires) != 1 || pool.lastReq.Requires[0] != "comfyui-klein" {
		t.Errorf("Requires = %v, want the canonical style", pool.lastReq.Requires)
	}
}

// multipartEdit builds an edits body.
// portraitPNG is a real PNG that is taller than it is wide. Tiny on purpose — only
// the header is ever decoded.
func portraitPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 4))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func multipartEdit(t *testing.T, fields map[string]string, img []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	part, err := mw.CreateFormFile("image", "src.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(img)
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}
