package providers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"agentbob/contract"
)

// ComfyUIClient is a thin Chatter that maps Chat calls onto a ComfyUI server's
// ASYNC job API. The pool routes here for KindImage; the `image_create` tool (and, later,
// modelgate's images endpoint) packs one generation request per Chat message.
//
// ComfyUI is not chat-shaped and not even request/response — a job is three hops:
//
//	POST /prompt          submit a workflow, returns {"prompt_id": ...} IMMEDIATELY
//	GET  /history/<id>    poll until the id appears; carries outputs + status
//	GET  /view?filename=  fetch the produced bytes
//
// (image-to-image adds a leading POST /upload/image). This client hides all four
// behind ONE synchronous Chat call — the same shape RapidOCRClient uses for the
// OCR sidecar, so the pool's picking / health / failover machinery is untouched.
//
// Wire shape IN — one message, Content = JSON comfyRequest, Images[0] = the
// optional init image for img2img. Wire shape OUT — ChatResponse.Content = JSON
// comfyReply carrying the image as base64.
//
// The base64 never reaches a transcript: the caller writes the bytes to a space
// and returns a one-line text result (docs/image-create-tool.md §9.3). Nothing in this
// package logs it either — reqlog is only wired into the OpenAI-compat and
// Anthropic clients, never here.
//
// WHAT to draw does not live here: the graph is synthesised per call from the
// capability declaration keyed by style (37-imagecaps.go), so an entry only says
// where it is and which styles it can serve. See that file for why capability
// belongs to the style rather than to the endpoint.
//
// ChatStream is unsupported: generation is a job, not a token stream. Progress
// rides the ctx hook instead (WithImageProgress) — the same ctx-tagging idiom as
// WithEntryName, so the Chatter interface stays unchanged.
type ComfyUIClient struct {
	baseURL string
	cfg     comfyConfig
	http    *http.Client
}

const (
	comfyPromptPath  = "/prompt"
	comfyHistoryPath = "/history/"
	comfyViewPath    = "/view"
	comfyUploadPath  = "/upload/image"
	comfyPingPath    = "/system_stats"
	comfyWSPath      = "/ws"

	// comfyPollInterval is how often /history is asked whether a job finished.
	// Generation is tens of seconds, so a tight poll buys nothing.
	comfyPollInterval = 2 * time.Second
	// comfyDefaultTimeout bounds a job when the variant declares no timeout_s.
	// Deliberately generous: a cold backend reloads several GB before it starts.
	comfyDefaultTimeout = 3 * time.Minute
	// comfyUploadTimeout bounds the init-image upload.
	comfyUploadTimeout = 2 * time.Minute
	// comfyMaxImageBytes bounds a fetched image.
	comfyMaxImageBytes = 32 << 20
)

// Model-facing error strings. Sanitized — no baseURL, no raw backend body.
const (
	comfyErrTransport = "comfyui: backend unavailable"
	comfyErrBadReply  = "comfyui: unexpected response from backend"
)

// comfyConfig is the entry's extra_body — now only PER-INSTANCE overrides.
//
// Everything that describes what to draw moved to the capability table keyed by
// style (37-imagecaps.go). What is left here belongs to this particular renderer
// and its card: a bigger GPU raises the safe sizes, a slower one wants a longer
// deadline. Both are usually absent, and an image entry is then six lines of yaml.
type comfyConfig struct {
	Sizes    map[string][]int `json:"sizes"`
	TimeoutS int              `json:"timeout_s"`
}

// comfyRequest is what a caller puts in Message.Content.
type comfyRequest struct {
	Style  string `json:"style"`            // which variant (== the entry tag the caller matched)
	Prompt string `json:"prompt"`           // what to draw
	Aspect string `json:"aspect,omitempty"` // key into cfg.Sizes; empty → "square"
	Change string `json:"change,omitempty"` // key into cfg.Denoise; img2img only
	Seed   int64  `json:"seed,omitempty"`   // 0 → caller did not pin one

	// Recover switches this call from "submit a new job" to "adopt an existing
	// one": skip submission and go straight to polling this prompt_id. The WAL
	// sweep uses it to finish a job whose submitting process died — routed back
	// through the pool with PinnedEntry so base_url stays the pool's secret
	// (docs/image-create-tool.md §9.5).
	Recover string `json:"recover,omitempty"`
}

// comfyReply is what lands in ChatResponse.Content.
type comfyReply struct {
	PromptID string `json:"prompt_id"`
	ImageB64 string `json:"image_b64"`
	MIME     string `json:"mime"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Variant  string `json:"variant"`
	Seconds  int    `json:"seconds"`
	// Seed is the number this picture actually came out of — reported so a caller
	// can put it in front of a human, and so the same picture is addressable later.
	// Zero on the recovery path: that job was submitted by a process that is gone,
	// and its seed went with it.
	Seed int64 `json:"seed,omitempty"`
}

// NewComfyUIClient builds a Chatter pointed at a ComfyUI root (no /v1 — ComfyUI
// is not OpenAI-shaped). extraBody carries only this instance's overrides, and is
// normally absent; a malformed one is logged and ignored rather than failing the
// entry, since the capability declarations it does not contain are what actually
// drive generation.
func NewComfyUIClient(baseURL string, extraBody map[string]any) *ComfyUIClient {
	c := &ComfyUIClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{}, // no client timeout — ctx + per-job deadline bound it
	}
	if len(extraBody) == 0 {
		return c
	}
	raw, err := json.Marshal(extraBody)
	if err == nil {
		err = json.Unmarshal(raw, &c.cfg)
	}
	if err != nil {
		slog.Warn("comfyui: extra_body is not a valid override block; using capability defaults", "err", err)
		c.cfg = comfyConfig{}
	}
	return c
}

// Ping asks for /system_stats. Reachability only — a live ComfyUI that cannot
// load a model still answers here, which is exactly the tentative-recovery
// semantics the pool documents for Ping.
func (c *ComfyUIClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+comfyPingPath, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	// The STATUS matters, not just the round-trip: these engines sit behind a
	// reverse proxy that answers 502 while the container is down, and the pool uses
	// Ping only to tentatively re-admit a dead entry. Treating the proxy's error
	// page as recovery would put the entry back in rotation, re-advertise its
	// styles, and make the next user wait out a full generation timeout to fail.
	// Matches RapidOCRClient.Ping.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("comfyui ping: %s → HTTP %d", comfyPingPath, resp.StatusCode)
	}
	return nil
}

// ChatStream is unsupported — image generation is a job, not a token stream.
func (c *ComfyUIClient) ChatStream(context.Context, string, []contract.Message, []contract.ToolSpec) (<-chan contract.StreamEvent, error) {
	return nil, fmt.Errorf("comfyui: streaming is not supported (image generation is a job, not a token stream)")
}

// Chat runs one generation end to end and returns the image as base64.
func (c *ComfyUIClient) Chat(ctx context.Context, _ string, messages []contract.Message, _ []contract.ToolSpec) (contract.ChatResponse, error) {
	req, initImage, err := parseComfyRequest(messages)
	if err != nil {
		return contract.ChatResponse{}, err
	}
	report := contract.ImageProgressFrom(ctx)
	start := time.Now()

	// Recovery path: the job already exists, just finish it.
	if req.Recover != "" {
		return c.awaitAndFetch(ctx, req.Recover, req.Style, comfyDefaultTimeout, start, report)
	}

	cap, ok := lookupImageCap(req.Style)
	if !ok {
		return contract.ChatResponse{}, fmt.Errorf("comfyui: no capability is declared for style %q (declared: %s)",
			req.Style, strings.Join(imageCapStyles(), ", "))
	}
	w, h := c.size(cap, req.Aspect)

	// img2img uploads FIRST, because the graph needs the name it lands under. The
	// name is derived from the CONTENT (sha256) and uploaded with overwrite=true, so
	// the backend's input dir stays bounded no matter how often the same picture is
	// reused — ComfyUI never prunes it (docs/image-create-tool.md §9.4).
	initName := ""
	if len(initImage.Data) > 0 {
		initName, err = c.uploadImage(ctx, initImage)
		if err != nil {
			return contract.ChatResponse{}, err
		}
	}

	// The seed is resolved HERE rather than inside the builder: the reply has to
	// carry the number this picture came out of, and a seed minted deeper down is
	// not visible to anything that could report it.
	seed := req.Seed
	if seed == 0 {
		seed = newSeed()
	}
	wf, err := c.buildGraph(cap, req, w, h, initName, seed)
	if err != nil {
		return contract.ChatResponse{}, err
	}

	pid, err := c.submit(ctx, wf)
	if err != nil {
		return contract.ChatResponse{}, err
	}
	// Announce the id the moment it exists — the caller writes its WAL record here,
	// BEFORE the image can possibly arrive. That ordering is the whole point of the
	// write-ahead log: a crash after this line still leaves a record to recover from.
	report(contract.ImageEvent{
		Stage:    contract.ImageStageSubmitted,
		PromptID: pid,
		Entry:    entryNameFromCtx(ctx), // the pool tagged ctx in chatOne; the caller never knew which entry won
	})

	resp, err := c.awaitAndFetch(ctx, pid, req.Style, c.timeout(cap), start, report)
	if err != nil {
		return resp, err
	}
	// Report the size we ASKED for: the bytes are not decoded here, and the workflow
	// pins the latent dimensions, so requested == produced. The seed rides along for
	// the same reason — both are what we submitted, and the job succeeded.
	return stampRequested(resp, w, h, seed)
}

// awaitAndFetch watches a submitted job to completion and returns its image. Shared
// by the normal path and the WAL recovery path, so a recovered job goes through
// exactly the same completion + error handling as a fresh one.
func (c *ComfyUIClient) awaitAndFetch(ctx context.Context, pid, variant string, timeout time.Duration, start time.Time, report func(contract.ImageEvent)) (contract.ChatResponse, error) {
	jobCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Progress is best-effort decoration: the websocket carries step-level events,
	// but completion is ALWAYS decided by /history. A dead ws therefore costs the
	// progress lines and nothing else.
	stopWS := c.watchProgress(jobCtx, pid, report)
	defer stopWS()

	img, err := c.await(jobCtx, pid)
	if err != nil {
		return contract.ChatResponse{}, err
	}
	report(contract.ImageEvent{Stage: contract.ImageStageFetching})
	body, err := c.fetch(jobCtx, img)
	if err != nil {
		return contract.ChatResponse{}, err
	}
	out, err := json.Marshal(comfyReply{
		PromptID: pid,
		ImageB64: base64.StdEncoding.EncodeToString(body),
		MIME:     "image/png",
		Variant:  variant,
		Seconds:  int(time.Since(start).Seconds()),
	})
	if err != nil {
		return contract.ChatResponse{}, fmt.Errorf("comfyui: marshal reply: %w", err)
	}
	return contract.ChatResponse{Content: string(out)}, nil
}

// size resolves an aspect key to pixel dimensions: this instance's override
// first, then the capability's default, then a conservative square.
//
// Layered that way because the two answer different questions. The capability
// knows what the MODEL wants; the ceiling is what the CARD allows — a 960² limit
// is the 1070's 8 GB talking, and the same workflow on a bigger GPU would take
// more. So an entry may raise or lower it without touching the shared declaration.
//
// The final fallback exists so a typo can never produce a 0×0 latent, which
// ComfyUI accepts and turns into something useless.
func (c *ComfyUIClient) size(cap imageCap, aspect string) (int, int) {
	// Resolve PER KEY, not per map. Nesting it the other way round makes a partial
	// override swallow every aspect it does not mention: an entry that raises only
	// the square ceiling would hit its own "square" fallback before the capability
	// was ever consulted, so a request for portrait came back square — and the reply
	// then reported that square as the size that was asked for.
	for _, k := range []string{aspect, "square"} {
		for _, m := range []map[string][]int{c.cfg.Sizes, cap.Sizes} {
			if wh := m[k]; len(wh) == 2 && wh[0] > 0 && wh[1] > 0 {
				return wh[0], wh[1]
			}
		}
	}
	return 512, 512
}

// timeout resolves this call's deadline as the LONGER of the two declarations,
// not "entry wins".
//
// The two layers answer different questions and stop commuting once an entry
// serves more than one style: an entry override says "this card is slow, give it
// room" (per instance), while a capability's own timeout says "this style takes
// 30 steps" (per style). With a fast and a slow style on one entry — which is
// exactly how the Aesthetic tier is meant to land, §7.6 — letting the entry win
// would clamp the slow style and abort every one of its jobs mid-generation. A
// too-short deadline saves nothing; it only turns a working job into a failure.
func (c *ComfyUIClient) timeout(cap imageCap) time.Duration {
	longest := 0
	for _, s := range []int{c.cfg.TimeoutS, cap.TimeoutS} {
		if s > longest {
			longest = s
		}
	}
	if longest > 0 {
		return time.Duration(longest) * time.Second
	}
	return comfyDefaultTimeout
}

// Graph node ids. The builder owns them, so unlike a hand-written template there
// is no mapping to get wrong — and no way to "patch" a node that isn't there and
// silently render something else.
const (
	nUNet     = "1"
	nCLIP     = "2"
	nVAE      = "3"
	nPositive = "4"
	nNegative = "5"
	nLatent   = "6" // t2i: the empty-latent class; i2i: LoadImage
	nScale    = "7" // i2i only
	nEncode   = "8" // i2i only
	nSampler  = "9"
	nDecode   = "10"
	nOutput   = "11"
)

// buildGraph synthesises this call's ComfyUI workflow from the capability
// declaration plus the request. Nothing is stored as a graph: the shape exists
// only for the duration of this job.
//
// The two shipped engines differ in four node classes and their file names and
// agree on everything else, so a declaration plus a builder expresses them both —
// where two hand-written templates cost 326 lines to say the same thing, half of
// it duplicated between the t2i and i2i variants of the same engine.
//
// The i2i chain replaces the empty latent with LoadImage → ImageScale → VAEEncode.
// The scale step is not optional: an img2img latent takes its size from the INPUT
// picture, so an unscaled phone photo is a guaranteed OOM at the sampler
// (docs/image-create-tool.md §8.2.1). ComfyUI does the resampling (lanczos + centre crop),
// which is why bob never decodes or re-encodes the user's image.
func (c *ComfyUIClient) buildGraph(cap imageCap, req comfyRequest, w, h int, initName string, seed int64) (map[string]json.RawMessage, error) {
	i2i := initName != ""
	denoise := 1.0
	if i2i {
		d, ok := cap.Denoise[req.Change]
		if !ok {
			return nil, fmt.Errorf("comfyui: this engine declares no %q edit strength", req.Change)
		}
		denoise = d
	}

	g := comfyGraph{}
	g.add(nUNet, cap.UNet.Node, merge(map[string]any{"unet_name": cap.UNet.File}, cap.UNet.Inputs))
	clip := map[string]any{"clip_name": cap.CLIP.File}
	if cap.CLIP.Type != "" {
		clip["type"] = cap.CLIP.Type
	}
	g.add(nCLIP, cap.CLIP.Node, merge(clip, cap.CLIP.Inputs))
	g.add(nVAE, "VAELoader", map[string]any{"vae_name": cap.VAE})
	g.add(nPositive, "CLIPTextEncode", map[string]any{"clip": link(nCLIP), "text": req.Prompt})
	// The negative is the capability's fixed string, never the caller's: on a
	// CFG-1.0 engine it does nothing at all, so letting a caller set it would be a
	// control that silently no-ops (docs/image-create-tool.md §3.3).
	g.add(nNegative, "CLIPTextEncode", map[string]any{"clip": link(nCLIP), "text": cap.Negative})

	latentSrc := link(nLatent)
	if i2i {
		g.add(nLatent, "LoadImage", map[string]any{"image": initName})
		g.add(nScale, "ImageScale", map[string]any{
			"image": link(nLatent), "width": w, "height": h,
			"upscale_method": "lanczos", "crop": "center",
		})
		g.add(nEncode, "VAEEncode", map[string]any{"pixels": link(nScale), "vae": link(nVAE)})
		latentSrc = link(nEncode)
	} else {
		g.add(nLatent, cap.Latent, map[string]any{"width": w, "height": h, "batch_size": 1})
	}

	g.add(nSampler, "KSampler", map[string]any{
		"model": link(nUNet), "positive": link(nPositive), "negative": link(nNegative),
		"latent_image": latentSrc, "seed": seed, "denoise": denoise,
		"steps": cap.Sampler.Steps, "cfg": cap.Sampler.CFG,
		"sampler_name": cap.Sampler.Sampler, "scheduler": orDefault(cap.Sampler.Scheduler, "simple"),
	})
	// Tiled decode at EVERY resolution, deliberately: plain VAEDecode OOMs from 896²
	// up (fp32 VAE activations) and tiling leaves no visible seam, so one path beats
	// a conditional that would only ever be exercised near the limit.
	g.add(nDecode, "VAEDecodeTiled", map[string]any{
		"samples": link(nSampler), "vae": link(nVAE),
		"tile_size": 256, "overlap": 64, "temporal_size": 64, "temporal_overlap": 8,
	})
	// PreviewImage, not SaveImage: the result lands in the backend's temp dir, which
	// it clears on restart, instead of accumulating in an output dir it never prunes
	// (docs/image-create-tool.md §9.4). bob has the bytes by then anyway.
	g.add(nOutput, "PreviewImage", map[string]any{"images": link(nDecode)})

	return g.encode()
}

// comfyGraph accumulates nodes in declaration order.
type comfyGraph struct {
	nodes map[string]map[string]any
	err   error
}

func (g *comfyGraph) add(id, class string, inputs map[string]any) {
	if g.nodes == nil {
		g.nodes = map[string]map[string]any{}
	}
	if class == "" {
		g.err = fmt.Errorf("comfyui: capability declares no node class for graph slot %s", id)
		return
	}
	g.nodes[id] = map[string]any{"class_type": class, "inputs": inputs}
}

func (g *comfyGraph) encode() (map[string]json.RawMessage, error) {
	if g.err != nil {
		return nil, g.err
	}
	out := make(map[string]json.RawMessage, len(g.nodes))
	for id, n := range g.nodes {
		raw, err := json.Marshal(n)
		if err != nil {
			return nil, fmt.Errorf("comfyui: encode node %s: %w", id, err)
		}
		out[id] = raw
	}
	return out, nil
}

// link is a ComfyUI edge: [<node id>, <output slot>].
func link(node string) []any { return []any{node, 0} }

// merge overlays extras onto base (extras are the capability's class-specific
// inputs, e.g. UNETLoader's weight_dtype).
func merge(base, extras map[string]any) map[string]any {
	for k, v := range extras {
		base[k] = v
	}
	return base
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// submit POSTs the workflow and returns the job id.
func (c *ComfyUIClient) submit(ctx context.Context, wf map[string]json.RawMessage) (string, error) {
	body, err := json.Marshal(map[string]any{"prompt": wf, "client_id": comfyClientID})
	if err != nil {
		return "", fmt.Errorf("comfyui: marshal workflow: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+comfyPromptPath, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		slog.Warn("comfyui: submit failed", "err", err)
		return "", fmt.Errorf("%s", comfyErrTransport)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// A rejected workflow (node validation) comes back here with a detailed body.
		// Log it for ops, hand the model a short version — the detail is about OUR
		// template, not about anything the model can fix.
		slog.Warn("comfyui: submit rejected", "status", resp.StatusCode, "body", truncate(string(raw), 800))
		return "", fmt.Errorf("comfyui: backend rejected the request (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		PromptID string `json:"prompt_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.PromptID == "" {
		return "", fmt.Errorf("%s", comfyErrBadReply)
	}
	return out.PromptID, nil
}

// comfyHistoryImage is one produced file as /history names it.
type comfyHistoryImage struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// await polls /history until the job finishes. Completion — and failure — are
// ASYNC: POST /prompt returns 200 with an id even for a workflow that will OOM,
// so this is the only place a job's real outcome is known.
func (c *ComfyUIClient) await(ctx context.Context, pid string) (comfyHistoryImage, error) {
	for {
		img, done, err := c.pollOnce(ctx, pid)
		if err != nil {
			return comfyHistoryImage{}, err
		}
		if done {
			return img, nil
		}
		select {
		case <-ctx.Done():
			return comfyHistoryImage{}, fmt.Errorf("comfyui: generation did not finish in time: %w", ctx.Err())
		case <-time.After(comfyPollInterval):
		}
	}
}

// pollOnce asks /history about one job. done=false means "not finished yet".
func (c *ComfyUIClient) pollOnce(ctx context.Context, pid string) (comfyHistoryImage, bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+comfyHistoryPath+url.PathEscape(pid), nil)
	if err != nil {
		return comfyHistoryImage{}, false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A transient poll failure is not a job failure — the job is still running on
		// the backend. Report it as not-done and let the deadline decide.
		slog.Debug("comfyui: history poll failed", "err", err)
		return comfyHistoryImage{}, false, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return comfyHistoryImage{}, false, nil
	}
	var hist map[string]struct {
		Outputs map[string]struct {
			Images []comfyHistoryImage `json:"images"`
		} `json:"outputs"`
		Status struct {
			StatusStr string            `json:"status_str"`
			Completed bool              `json:"completed"`
			Messages  []json.RawMessage `json:"messages"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &hist); err != nil {
		return comfyHistoryImage{}, false, nil
	}
	rec, ok := hist[pid]
	if !ok {
		return comfyHistoryImage{}, false, nil
	}
	if rec.Status.StatusStr == "error" {
		return comfyHistoryImage{}, false, comfyExecError(rec.Status.Messages)
	}
	for _, out := range rec.Outputs {
		if len(out.Images) > 0 {
			return out.Images[0], true, nil
		}
	}
	if rec.Status.Completed {
		return comfyHistoryImage{}, false, fmt.Errorf("comfyui: the job finished without producing an image")
	}
	return comfyHistoryImage{}, false, nil
}

// comfyExecError turns a failed job's message list into one model-facing error.
//
// The backend's own OOM text ends with "you might have accidentally set the
// batch_size to a large number", which is MISLEADING here — batch is always 1 and
// the real cause is the requested size. Passing it through would send the model
// chasing a parameter it does not control, so an OOM is rewritten into the one
// action that helps: ask for a smaller picture.
func comfyExecError(messages []json.RawMessage) error {
	for _, m := range messages {
		var pair []json.RawMessage
		if json.Unmarshal(m, &pair) != nil || len(pair) < 2 {
			continue
		}
		var kind string
		if json.Unmarshal(pair[0], &kind) != nil || kind != "execution_error" {
			continue
		}
		var d struct {
			NodeType      string `json:"node_type"`
			ExceptionType string `json:"exception_type"`
			ExceptionMsg  string `json:"exception_message"`
		}
		if json.Unmarshal(pair[1], &d) != nil {
			continue
		}
		if strings.Contains(strings.ToLower(d.ExceptionType), "outofmemory") {
			slog.Warn("comfyui: job ran out of VRAM", "node", d.NodeType)
			return fmt.Errorf("comfyui: this size is too large for the engine — ask for a smaller one")
		}
		slog.Warn("comfyui: job failed", "node", d.NodeType, "type", d.ExceptionType, "msg", truncate(d.ExceptionMsg, 400))
		return fmt.Errorf("comfyui: generation failed in %s", d.NodeType)
	}
	return fmt.Errorf("comfyui: generation failed")
}

// fetch downloads a produced image.
func (c *ComfyUIClient) fetch(ctx context.Context, img comfyHistoryImage) ([]byte, error) {
	q := url.Values{
		"filename":  {img.Filename},
		"subfolder": {img.Subfolder},
		"type":      {img.Type},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+comfyViewPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		slog.Warn("comfyui: fetch failed", "err", err)
		return nil, fmt.Errorf("%s", comfyErrTransport)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("comfyui: could not retrieve the produced image (HTTP %d)", resp.StatusCode)
	}
	// Read ONE byte past the cap so hitting it is detectable. A plain LimitReader
	// returns a truncated body with a nil error, which would be saved to the user's
	// space and sent to the chat as a corrupt PNG with nothing reported anywhere.
	body, err := io.ReadAll(io.LimitReader(resp.Body, comfyMaxImageBytes+1))
	if err != nil || len(body) == 0 {
		return nil, fmt.Errorf("%s", comfyErrBadReply)
	}
	if len(body) > comfyMaxImageBytes {
		slog.Warn("comfyui: produced image exceeds the size cap", "cap_bytes", comfyMaxImageBytes)
		return nil, fmt.Errorf("comfyui: the produced image is too large to handle")
	}
	return body, nil
}

// uploadImage puts the init image in the backend's input dir and returns the name
// to reference it by. The name is the content hash, and overwrite is set, so the
// same picture reuses one slot forever — the backend prunes that directory never.
func (c *ComfyUIClient) uploadImage(ctx context.Context, img contract.ImageRef) (string, error) {
	sum := sha256.Sum256(img.Data)
	name := "bob-" + hex.EncodeToString(sum[:8]) + comfyExt(img.MIME)

	var buf strings.Builder
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("image", name)
	if err != nil {
		return "", fmt.Errorf("comfyui: build upload: %w", err)
	}
	if _, err := part.Write(img.Data); err != nil {
		return "", fmt.Errorf("comfyui: build upload: %w", err)
	}
	if err := mw.WriteField("overwrite", "true"); err != nil {
		return "", fmt.Errorf("comfyui: build upload: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("comfyui: build upload: %w", err)
	}

	upCtx, cancel := context.WithTimeout(ctx, comfyUploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(upCtx, "POST", c.baseURL+comfyUploadPath, strings.NewReader(buf.String()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		slog.Warn("comfyui: upload failed", "err", err)
		return "", fmt.Errorf("%s", comfyErrTransport)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		slog.Warn("comfyui: upload rejected", "status", resp.StatusCode, "body", truncate(string(raw), 400))
		return "", fmt.Errorf("comfyui: backend refused the source image (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Name == "" {
		return "", fmt.Errorf("%s", comfyErrBadReply)
	}
	if out.Subfolder != "" {
		return out.Subfolder + "/" + out.Name, nil
	}
	return out.Name, nil
}

// comfyExt maps a MIME to the extension ComfyUI needs to recognise the upload.
func comfyExt(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

// parseComfyRequest pulls the request envelope and the optional init image out of
// the message list (last message wins — the pool hands us exactly one).
func parseComfyRequest(messages []contract.Message) (comfyRequest, contract.ImageRef, error) {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		var req comfyRequest
		if err := json.Unmarshal([]byte(m.Content), &req); err != nil {
			return comfyRequest{}, contract.ImageRef{}, fmt.Errorf("comfyui: request is not valid JSON: %w", err)
		}
		if req.Recover == "" && strings.TrimSpace(req.Prompt) == "" {
			return comfyRequest{}, contract.ImageRef{}, fmt.Errorf("comfyui: request has no prompt")
		}
		var img contract.ImageRef
		if len(m.Images) > 0 {
			img = m.Images[0]
		}
		return req, img, nil
	}
	return comfyRequest{}, contract.ImageRef{}, fmt.Errorf("comfyui: no request found in messages")
}

// stampRequested stamps the submitted dimensions and seed onto a reply built by
// awaitAndFetch (which knows neither — it only watches a job id).
func stampRequested(resp contract.ChatResponse, w, h int, seed int64) (contract.ChatResponse, error) {
	var r comfyReply
	if err := json.Unmarshal([]byte(resp.Content), &r); err != nil {
		return resp, nil // never fatal: the image is already in hand
	}
	r.Width, r.Height, r.Seed = w, h, seed
	out, err := json.Marshal(r)
	if err != nil {
		return resp, nil
	}
	resp.Content = string(out)
	return resp, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// --- websocket progress ---------------------------------------------------

// comfyClientID identifies bob's submissions on the backend. ComfyUI stores it in
// /history alongside the workflow, and the websocket only forwards progress for
// its own client id, so the two must agree.
const comfyClientID = "agentbob"

// watchProgress opens the backend's event socket and forwards step progress to
// report until ctx ends. Returns a stop func. Every failure here is silent by
// design: progress is decoration, and completion is decided by /history.
func (c *ComfyUIClient) watchProgress(ctx context.Context, pid string, report func(contract.ImageEvent)) func() {
	wsURL, err := comfyWSURL(c.baseURL)
	if err != nil {
		return func() {}
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, 5*time.Second)
	defer cancelDial()
	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, wsURL, nil)
	if err != nil {
		slog.Debug("comfyui: progress socket unavailable", "err", err)
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer conn.Close()
		go func() { // close on ctx end so ReadMessage unblocks
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-done:
			}
		}()
		for {
			_, data, rErr := conn.ReadMessage()
			if rErr != nil {
				return
			}
			var ev struct {
				Type string `json:"type"`
				Data struct {
					Value    int    `json:"value"`
					Max      int    `json:"max"`
					PromptID string `json:"prompt_id"`
					Node     string `json:"node"`
				} `json:"data"`
			}
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			// Other jobs share this socket; only report our own.
			if ev.Data.PromptID != "" && ev.Data.PromptID != pid {
				continue
			}
			if ev.Type == "progress" && ev.Data.Max > 0 {
				report(contract.ImageEvent{
					Stage: contract.ImageStageProgress,
					Cur:   ev.Data.Value,
					Max:   ev.Data.Max,
				})
			}
		}
	}()
	return func() { _ = conn.Close(); <-done }
}

// comfyWSURL rewrites the entry's http(s) base into the websocket endpoint,
// keeping any path prefix (the engines sit behind a reverse proxy at /klein,
// /anima, … so the prefix is load-bearing).
func comfyWSURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + comfyWSPath
	u.RawQuery = url.Values{"clientId": {comfyClientID}}.Encode()
	return u.String(), nil
}
