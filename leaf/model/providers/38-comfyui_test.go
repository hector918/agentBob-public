package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbob/contract"
)

// nodeOf pulls one synthesised node back out of a graph.
func nodeOf(t *testing.T, g map[string]json.RawMessage, id string) (string, map[string]any) {
	t.Helper()
	raw, ok := g[id]
	if !ok {
		t.Fatalf("graph has no node %q", id)
	}
	var n struct {
		ClassType string         `json:"class_type"`
		Inputs    map[string]any `json:"inputs"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("node %q: %v", id, err)
	}
	return n.ClassType, n.Inputs
}

func testCap() imageCap {
	return imageCap{
		UNet:     imageNodeRef{Node: "UNETLoader", File: "anima-turbo.safetensors", Inputs: map[string]any{"weight_dtype": "default"}},
		CLIP:     imageNodeRef{Node: "CLIPLoader", File: "qwen_3_06b_base.safetensors", Type: "qwen_image"},
		VAE:      "qwen_image_vae.safetensors",
		Latent:   "EmptySD3LatentImage",
		Sampler:  imageSampler{Steps: 10, CFG: 1.0, Sampler: "euler", Scheduler: "simple"},
		Negative: "worst quality",
		Sizes:    map[string][]int{"square": {1024, 1024}, "portrait": {832, 1216}},
		TimeoutS: 60,
		Denoise:  map[string]float64{"slight": 0.45, "moderate": 0.55, "heavy": 0.8},
	}
}

func newTestClient() *ComfyUIClient { return NewComfyUIClient("http://x", nil) }

// --- the shipped declarations -------------------------------------------

// The built-ins are embedded, so a broken one is a build mistake — but only this
// proves the parse happened rather than silently yielding an empty table, which
// would make every style report as undeclared.
func TestShippedCapabilitiesLoad(t *testing.T) {
	for _, style := range []string{"comfyui-klein", "comfyui-anima"} {
		cap, ok := lookupImageCap(style)
		if !ok {
			t.Errorf("no capability declared for %q (have: %v)", style, imageCapStyles())
			continue
		}
		if err := cap.validate(); err != nil {
			t.Errorf("%s: %v", style, err)
		}
		if len(cap.Sizes) == 0 || cap.TimeoutS == 0 || len(cap.Denoise) == 0 {
			t.Errorf("%s: missing defaults (sizes/timeout/denoise)", style)
		}
	}
}

// The two engines really do differ only in the loader classes and the latent —
// the premise the builder is built on. If a future declaration breaks it, the
// builder is the wrong shape for it and should say so here.
func TestShippedCapabilitiesDifferOnlyInLoaders(t *testing.T) {
	k, _ := lookupImageCap("comfyui-klein")
	a, _ := lookupImageCap("comfyui-anima")
	if k.UNet.Node == a.UNet.Node {
		t.Error("expected different unet loader classes")
	}
	if k.Latent == a.Latent {
		t.Error("expected different empty-latent classes")
	}
	if k.CLIP.Type == a.CLIP.Type {
		t.Error("expected different clip types")
	}
}

func TestValidateRejectsIncompleteCapability(t *testing.T) {
	for name, mut := range map[string]func(*imageCap){
		"no unet node": func(c *imageCap) { c.UNet.Node = "" },
		"no unet file": func(c *imageCap) { c.UNet.File = "" },
		"no clip":      func(c *imageCap) { c.CLIP.Node = "" },
		"no vae":       func(c *imageCap) { c.VAE = "" },
		"no latent":    func(c *imageCap) { c.Latent = "" },
		"zero steps":   func(c *imageCap) { c.Sampler.Steps = 0 },
		"no sampler":   func(c *imageCap) { c.Sampler.Sampler = "" },
		// These three have downstream fallbacks, but an override REPLACES a whole
		// declaration — so a short override would pass and then silently drop to 512²
		// and break every edit. Load time is where that has to be caught.
		"no sizes":   func(c *imageCap) { c.Sizes = nil },
		"no square":  func(c *imageCap) { c.Sizes = map[string][]int{"portrait": {8, 8}} },
		"no denoise": func(c *imageCap) { c.Denoise = nil },
		"no timeout": func(c *imageCap) { c.TimeoutS = 0 },
	} {
		c := testCap()
		mut(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: validate accepted it", name)
		}
	}
}

// --- graph synthesis ------------------------------------------------------

func TestBuildGraphT2I(t *testing.T) {
	c := newTestClient()
	g, err := c.buildGraph(testCap(), comfyRequest{Style: "s", Prompt: "a cat"}, 832, 1216, "", 7)
	if err != nil {
		t.Fatalf("buildGraph: %v", err)
	}
	if cls, in := nodeOf(t, g, nUNet); cls != "UNETLoader" || in["unet_name"] != "anima-turbo.safetensors" {
		t.Errorf("unet node = %s %v", cls, in)
	} else if in["weight_dtype"] != "default" {
		t.Errorf("class-specific loader input dropped: %v", in)
	}
	if cls, in := nodeOf(t, g, nCLIP); cls != "CLIPLoader" || in["type"] != "qwen_image" {
		t.Errorf("clip node = %s %v", cls, in)
	}
	if cls, in := nodeOf(t, g, nLatent); cls != "EmptySD3LatentImage" ||
		in["width"] != float64(832) || in["height"] != float64(1216) {
		t.Errorf("latent node = %s %v, want the declared class at the requested size", cls, in)
	}
	if _, in := nodeOf(t, g, nPositive); in["text"] != "a cat" {
		t.Errorf("prompt = %v", in["text"])
	}
	// Fixed per capability, never from the caller.
	if _, in := nodeOf(t, g, nNegative); in["text"] != "worst quality" {
		t.Errorf("negative = %v, want the capability's fixed string", in["text"])
	}
	if _, in := nodeOf(t, g, nSampler); in["denoise"] != 1.0 || in["steps"] != float64(10) {
		t.Errorf("sampler = %v, want a full-strength 10-step pass", in)
	}
	// Tiling is unconditional: plain decode OOMs from 896² up and tiling shows no seam.
	if cls, in := nodeOf(t, g, nDecode); cls != "VAEDecodeTiled" || in["tile_size"] != float64(256) {
		t.Errorf("decode node = %s %v", cls, in)
	}
	// Temp dir, not the output dir the backend never prunes.
	if cls, _ := nodeOf(t, g, nOutput); cls != "PreviewImage" {
		t.Errorf("output node = %s, want PreviewImage", cls)
	}
	if _, present := g[nScale]; present {
		t.Error("t2i graph contains the img2img scale node")
	}
}

func TestBuildGraphI2I(t *testing.T) {
	c := newTestClient()
	g, err := c.buildGraph(testCap(), comfyRequest{Style: "s", Prompt: "x", Change: "slight"}, 832, 1216, "bob-abc.png", 7)
	if err != nil {
		t.Fatalf("buildGraph: %v", err)
	}
	if cls, in := nodeOf(t, g, nLatent); cls != "LoadImage" || in["image"] != "bob-abc.png" {
		t.Errorf("latent source = %s %v, want LoadImage on the uploaded name", cls, in)
	}
	// The scale step is what stands between a user's phone photo and a certain OOM.
	cls, scale := nodeOf(t, g, nScale)
	if cls != "ImageScale" || scale["width"] != float64(832) || scale["height"] != float64(1216) {
		t.Errorf("scale node = %s %v, want the aspect's safe size", cls, scale)
	}
	if scale["crop"] != "center" || scale["upscale_method"] != "lanczos" {
		t.Errorf("scale node lost its resampling settings: %v", scale)
	}
	if _, in := nodeOf(t, g, nEncode); !sameLink(in["pixels"], nScale) {
		t.Errorf("VAEEncode reads %v, want the SCALED image", in["pixels"])
	}
	if _, in := nodeOf(t, g, nSampler); !sameLink(in["latent_image"], nEncode) {
		t.Errorf("sampler reads %v, want the encoded latent", in["latent_image"])
	} else if in["denoise"] != 0.45 {
		t.Errorf("denoise = %v, want 0.45 for change=slight", in["denoise"])
	}
}

func sameLink(v any, node string) bool {
	l, ok := v.([]any)
	return ok && len(l) == 2 && l[0] == node
}

// An unknown edit strength must be refused rather than silently becoming a
// full-strength repaint, which would look like the tool ignored the request.
func TestBuildGraphRejectsUnknownChange(t *testing.T) {
	c := newTestClient()
	if _, err := c.buildGraph(testCap(), comfyRequest{Prompt: "x", Change: "nope"}, 512, 512, "img.png", 7); err == nil {
		t.Fatal("accepted an undeclared edit strength")
	}
}

// Each call gets its own seed, or a repeated prompt comes back byte-identical
// from the backend's node cache instead of as a new take.
// The builder no longer draws the seed — Chat does, so that the reply can report
// the number this picture actually came out of. What the builder owes is fidelity:
// the seed it was handed is the seed the sampler runs.
func TestBuildGraphUsesTheSeedItIsGiven(t *testing.T) {
	c := newTestClient()
	g, err := c.buildGraph(testCap(), comfyRequest{Prompt: "x"}, 512, 512, "", 4242)
	if err != nil {
		t.Fatalf("buildGraph: %v", err)
	}
	_, in := nodeOf(t, g, nSampler)
	if in["seed"] != float64(4242) { // the graph round-trips through JSON
		t.Errorf("sampler seed = %v, want the 4242 it was handed", in["seed"])
	}
}

// Fresh calls still land on different pictures: the randomness moved, it did not
// disappear.
func TestNewSeedVaries(t *testing.T) {
	seen := map[int64]bool{}
	for i := 0; i < 8; i++ {
		seen[newSeed()] = true
	}
	if len(seen) < 2 {
		t.Errorf("got %d distinct seeds over 8 draws, want them to vary", len(seen))
	}
}

// --- size / timeout layering ----------------------------------------------

// The capability knows what the model wants; the entry knows what its card
// allows. An instance override must win so the same declaration can serve a
// bigger GPU.
func TestSizeAndTimeoutLayering(t *testing.T) {
	cap := testCap()
	plain := NewComfyUIClient("http://x", nil)
	if w, h := plain.size(cap, "portrait"); w != 832 || h != 1216 {
		t.Errorf("capability default ignored: %dx%d", w, h)
	}
	if got := plain.timeout(cap); got.Seconds() != 60 {
		t.Errorf("timeout = %v, want the capability's 60s", got)
	}
	over := NewComfyUIClient("http://x", map[string]any{
		"sizes":     map[string]any{"portrait": []any{1536, 2048}},
		"timeout_s": 300,
	})
	if w, h := over.size(cap, "portrait"); w != 1536 || h != 2048 {
		t.Errorf("instance override ignored: %dx%d", w, h)
	}
	if got := over.timeout(cap); got.Seconds() != 300 {
		t.Errorf("timeout override ignored: %v", got)
	}
	// A typo must never yield a 0×0 latent, which ComfyUI accepts.
	if w, h := plain.size(imageCap{}, "square"); w == 0 || h == 0 {
		t.Errorf("empty capability gave %dx%d", w, h)
	}
}

// Regression: resolving per-map instead of per-key made a PARTIAL override
// swallow every aspect it did not mention. An entry that raises only its square
// ceiling — the "bigger card" case the override exists for — hit its own square
// fallback before the capability was consulted, so a request for a portrait
// wallpaper came back square and the reply reported that square as the size asked
// for.
func TestPartialSizeOverrideDoesNotSquashOtherAspects(t *testing.T) {
	cap := testCap() // square 1024², portrait 832×1216
	over := NewComfyUIClient("http://x", map[string]any{
		"sizes": map[string]any{"square": []any{1536, 1536}},
	})
	if w, h := over.size(cap, "square"); w != 1536 || h != 1536 {
		t.Errorf("square = %dx%d, want the override", w, h)
	}
	if w, h := over.size(cap, "portrait"); w != 832 || h != 1216 {
		t.Errorf("portrait = %dx%d, want the capability's own portrait — an override of ONE aspect must not redefine the others", w, h)
	}
}

// Regression: an entry may serve several styles, so an entry-level timeout is not
// allowed to clamp a slow one. The fast/slow pair on one card is exactly how the
// Aesthetic tier is meant to land, and "entry wins" would abort every one of its
// jobs mid-generation.
func TestTimeoutTakesTheLongerOfTheTwoLayers(t *testing.T) {
	slow := testCap()
	slow.TimeoutS = 360 // a 30-step quality tier
	entry := NewComfyUIClient("http://x", map[string]any{"timeout_s": 90})
	if got := entry.timeout(slow); got.Seconds() != 360 {
		t.Errorf("timeout = %v, want 360s — an instance override must not cut a slow style short", got)
	}
	fast := testCap() // 60s
	if got := entry.timeout(fast); got.Seconds() != 90 {
		t.Errorf("timeout = %v, want the entry's longer 90s", got)
	}
}

// --- request handling -----------------------------------------------------

func TestChatRefusesUndeclaredStyle(t *testing.T) {
	c := newTestClient()
	_, err := c.Chat(context.Background(), "", []contract.Message{{Role: "user", Content: `{"style":"nope","prompt":"x"}`}}, nil)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want a complaint naming the undeclared style", err)
	}
}

func TestParseComfyRequest(t *testing.T) {
	req, img, err := parseComfyRequest([]contract.Message{{
		Role:    "user",
		Content: `{"style":"comfyui-anima","prompt":"1girl","aspect":"portrait"}`,
		Images:  []contract.ImageRef{{Data: []byte("png"), MIME: "image/png"}},
	}})
	if err != nil {
		t.Fatalf("parseComfyRequest: %v", err)
	}
	if req.Style != "comfyui-anima" || req.Prompt != "1girl" || req.Aspect != "portrait" {
		t.Errorf("req = %+v", req)
	}
	if string(img.Data) != "png" {
		t.Error("init image not carried through")
	}
}

// A recover request legitimately has no prompt — it adopts a job that already
// carries one.
func TestParseComfyRequestAllowsRecoverWithoutPrompt(t *testing.T) {
	req, _, err := parseComfyRequest([]contract.Message{{Role: "user", Content: `{"recover":"abc-123"}`}})
	if err != nil {
		t.Fatalf("parseComfyRequest: %v", err)
	}
	if req.Recover != "abc-123" {
		t.Errorf("Recover = %q", req.Recover)
	}
}

func TestParseComfyRequestRejectsPromptless(t *testing.T) {
	if _, _, err := parseComfyRequest([]contract.Message{{Role: "user", Content: `{"style":"x"}`}}); err == nil {
		t.Fatal("accepted a request with no prompt and no recover id")
	}
}

// --- backend interaction --------------------------------------------------

// The backend's own OOM text blames batch_size, which is always 1 here. Passing
// it through would send the model tuning a parameter it does not control.
func TestComfyExecErrorRewritesOOM(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`["execution_start", {"prompt_id":"p"}]`),
		json.RawMessage(`["execution_error", {"node_type":"KSampler","exception_type":"torch.OutOfMemoryError","exception_message":"Allocation on device \nTIPS: ... batch_size to a large number."}]`),
	}
	err := comfyExecError(msgs)
	if err == nil {
		t.Fatal("nil error for a failed job")
	}
	if strings.Contains(err.Error(), "batch_size") {
		t.Errorf("err = %q — the misleading tip must not reach the model", err)
	}
	if !strings.Contains(err.Error(), "smaller") {
		t.Errorf("err = %q, want it to point at the size", err)
	}
}

func TestComfyExecErrorNamesFailingNode(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`["execution_error", {"node_type":"VAEDecodeTiled","exception_type":"ValueError","exception_message":"internal detail"}]`),
	}
	err := comfyExecError(msgs)
	if err == nil || !strings.Contains(err.Error(), "VAEDecodeTiled") {
		t.Fatalf("err = %v, want the failing node named", err)
	}
	if strings.Contains(err.Error(), "internal detail") {
		t.Errorf("err = %q — raw backend detail must stay in the log", err)
	}
}

// A reverse proxy answering while the engine behind it is down must NOT count as
// reachable: the pool uses Ping only to tentatively re-admit dead entries.
func TestPingRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<html>502</html>", http.StatusBadGateway)
	}))
	defer srv.Close()
	if err := NewComfyUIClient(srv.URL, nil).Ping(context.Background()); err == nil {
		t.Fatal("Ping accepted an HTTP 502 as reachable")
	}
}

func TestPingAcceptsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"system":{}}`))
	}))
	defer srv.Close()
	if err := NewComfyUIClient(srv.URL, nil).Ping(context.Background()); err != nil {
		t.Fatalf("Ping rejected a healthy backend: %v", err)
	}
}

// A body at the cap used to come back truncated with a nil error, i.e. a corrupt
// PNG saved and sent with nothing reported anywhere.
func TestFetchRejectsOversizedImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, comfyMaxImageBytes+64))
	}))
	defer srv.Close()
	_, err := NewComfyUIClient(srv.URL, nil).fetch(context.Background(), comfyHistoryImage{Filename: "x.png", Type: "temp"})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want an oversize refusal", err)
	}
}

func TestFetchAcceptsNormalImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filename") != "x.png" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
	}))
	defer srv.Close()
	got, err := NewComfyUIClient(srv.URL, nil).fetch(context.Background(), comfyHistoryImage{Filename: "x.png", Type: "temp"})
	if err != nil || len(got) == 0 {
		t.Fatalf("fetch: %v (%d bytes)", err, len(got))
	}
}

// The engines sit behind a reverse proxy at /klein, /anima, … so dropping the
// path prefix would connect to the wrong place.
func TestComfyWSURLKeepsPathPrefix(t *testing.T) {
	got, err := comfyWSURL("http://host:11434/klein")
	if err != nil {
		t.Fatalf("comfyWSURL: %v", err)
	}
	if !strings.HasPrefix(got, "ws://host:11434/klein/ws?") {
		t.Errorf("got %q", got)
	}
	got, err = comfyWSURL("https://host/anima/")
	if err != nil {
		t.Fatalf("comfyWSURL: %v", err)
	}
	if !strings.HasPrefix(got, "wss://host/anima/ws?") {
		t.Errorf("got %q, want wss:// and no doubled slash", got)
	}
}
