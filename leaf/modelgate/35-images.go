package modelgate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register jpeg for image.DecodeConfig (an edit's source shape)
	_ "image/png"  // register png for image.DecodeConfig
	"io"
	"mime/multipart"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"agentbob/contract"
)

// The images endpoints are a thin pass-through: authenticate, turn the OpenAI
// request into a ModelRequest, hand it to the pool, hand the bytes back. All of
// the interesting work — picking a backend, synthesising the workflow, waiting out
// the job — already lives behind contract.ModelPool and is shared with the
// in-conversation image_create tool.
//
// The part that is NOT a pass-through is the prompt guidance. An external caller
// writes its own prompts, so it needs the same manual the tool reads, and it can
// only get it if there is exactly one copy — hence contract.ImageCatalog and the
// `guide` field on GET /v1/models/<style> (docs/image-create-tool.md §6).
//
// `model` NAMES A STYLE here, unlike /v1/chat/completions where it is a lane
// shorthand. That divergence is deliberate: for generation the thing a caller
// means by "which model" IS the style, and asking anyone to send `model: "image"`
// would be a worse fit for the same field. Styles are capability names, so this
// does not leak entry names outward — that rule still holds.

const (
	// imagesMaxUpload bounds an edit's multipart body. The backend rescales the
	// picture anyway, so a larger original buys nothing.
	imagesMaxUpload = 32 << 20
	// imagesFallbackAspect is the last-resort shape: what a caller gets when it names
	// no size and none can be read off a source picture. Every style declares it
	// (the capability validator requires a square).
	imagesFallbackAspect = "square"
	// imagesFallbackChange is the edit strength an edit request gets when it names
	// none — `change` is not part of the OpenAI shape, so omitting it is the norm.
	imagesFallbackChange = "moderate"
)

// imageRequest is the JSON body of /v1/images/generations — the OpenAI shape,
// plus `change` for edits (OpenAI has no edit-strength field and we need one).
type imageRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Size   string `json:"size,omitempty"`
	N      int    `json:"n,omitempty"`
	Change string `json:"change,omitempty"`
	Format string `json:"response_format,omitempty"`
}

// handleImageGenerations serves POST /v1/images/generations.
func (s *server) handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed", "")
		return
	}
	info, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req imageRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body", "")
		return
	}
	s.runImage(r.Context(), w, info, req, contract.ImageRef{})
}

// handleImageEdits serves POST /v1/images/edits — the same job with a source
// picture, sent as multipart like OpenAI's own edits endpoint.
func (s *server) handleImageEdits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed", "")
		return
	}
	info, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	// ParseMultipartForm's argument is maxMemory, NOT a body limit — anything past
	// it spills to temp files with no ceiling, so a large part fills the container's
	// disk before the size check below ever runs. Bound the body first, the way
	// decodeChatRequest does for JSON.
	r.Body = http.MaxBytesReader(w, r.Body, imagesMaxUpload)
	if err := r.ParseMultipartForm(imagesMaxUpload); err != nil {
		// An over-cap body fails HERE, at the parse, not at the size check below — the
		// reader is already exhausted by then. Told apart so the caller learns its
		// picture was too big, instead of being told its multipart body was malformed.
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("the request body exceeds %d bytes — send a smaller picture (the backend rescales it anyway)", imagesMaxUpload), "")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"edits expects a multipart/form-data body with an `image` file", "")
		return
	}
	file, hdr, err := r.FormFile("image")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "`image` file is required", "")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, imagesMaxUpload+1))
	if err != nil || len(data) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "could not read the `image` file", "")
		return
	}
	if len(data) > imagesMaxUpload {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
			fmt.Sprintf("`image` exceeds %d bytes", imagesMaxUpload), "")
		return
	}
	req := imageRequest{
		Model:  r.FormValue("model"),
		Prompt: r.FormValue("prompt"),
		Size:   r.FormValue("size"),
		Change: r.FormValue("change"),
		Format: r.FormValue("response_format"),
	}
	s.runImage(r.Context(), w, info, req, contract.ImageRef{Data: data, MIME: uploadMIME(hdr)})
}

// runImage is the shared body of both endpoints.
func (s *server) runImage(ctx context.Context, w http.ResponseWriter, info contract.APIKeyInfo, req imageRequest, src contract.ImageRef) {
	style := strings.TrimSpace(req.Model)
	if style == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"`model` is required — it names the style; GET /v1/models lists them", "")
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "`prompt` is required", "")
		return
	}
	// n>1 is REFUSED rather than quietly served as one picture: a caller that asked
	// for four and got one back with no complaint has no way to notice.
	if req.N > 1 {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"this backend produces one image per request; `n` must be 1 or omitted", "")
		return
	}
	// `change` is an EDIT strength: on a from-scratch generation there is nothing to
	// change, so the value would simply be dropped — and a dropped parameter reads
	// exactly like an honoured one in the reply. Refusing names the endpoint that
	// does take it. Same rule the in-conversation tool applies to change without an
	// init image (leaf/tools/imagecreate/00-tool.go).
	if strings.TrimSpace(req.Change) != "" && len(src.Data) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"`change` is the edit strength — it only applies to POST /v1/images/edits, which takes a source `image`", "")
		return
	}
	if f := strings.TrimSpace(req.Format); f != "" && f != "b64_json" {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"only response_format=b64_json is supported (bob does not host image URLs)", "")
		return
	}
	// The lane check comes FIRST: the style-lookup failure names every drawable
	// style, so running it earlier would hand the whole catalog to a key that may
	// not draw at all — and a 403 on a real name would then confirm it exists.
	// GET /v1/models/<style> already hides styles from such keys; the two must agree.
	if !keyAdmitsImages(info) {
		writeError(w, http.StatusForbidden, "invalid_request_error",
			"this key does not admit the image lane", "")
		return
	}
	styles := s.drawableStyles(s.pool.Snapshot())
	found, ok := findStyle(styles, style)
	if !ok {
		// Declared but not currently served is a DIFFERENT answer from unknown, and
		// the catalog now says so too (it lists the style with a non-usable state).
		// Returning "unknown model" here would contradict it — and send the caller off
		// to fix a name that was never wrong.
		if declared, isDeclared := findStyle(s.imageStyles(), style); isDeclared {
			writeError(w, http.StatusServiceUnavailable, "invalid_request_error",
				fmt.Sprintf("%q is not being served right now — no usable backend carries it; GET /v1/models shows its state",
					declared.Style), "")
			return
		}
		writeError(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("unknown model %q; available: %s", style, styleList(styles)), "model_not_found")
		return
	}
	// Route by the CANONICAL name. The lookup is case-insensitive to be forgiving,
	// but tag matching in the picker is exact — passing the caller's spelling
	// through would sail past this 404 and die later as an unroutable request.
	style = found.Style

	aspect, ok := resolveAspect(found, req.Size, src.Data)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("unsupported size %q for %s; supported: %s", req.Size, style, sizeList(found)), "")
		return
	}
	change, ok := resolveChange(found, req.Change, len(src.Data) > 0)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("unsupported change %q for %s; supported: %s",
				req.Change, style, strings.Join(found.Changes, ", ")), "")
		return
	}

	body, err := json.Marshal(map[string]any{
		"style": style, "prompt": req.Prompt, "aspect": aspect, "change": change,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not build the request", "")
		return
	}
	msg := contract.Message{Role: "user", Content: string(body)}
	if len(src.Data) > 0 {
		msg.Images = []contract.ImageRef{src}
	}

	mr := contract.ModelRequest{Kind: contract.KindImage, Requires: []string{style}}
	// Generation is minutes, not seconds, and the caller's own HTTP client is the
	// only sensible clock — so the request context bounds it, not a deadline of ours.
	resp, err := s.pool.Chat(ctxWithConsumer(ctx, info.ID), mr, []contract.Message{msg})
	if err != nil {
		status, typ, msg := chatErrorStatus(err, mr, s.pool.Snapshot())
		writeError(w, status, typ, msg, "")
		return
	}
	var reply struct {
		ImageB64 string `json:"image_b64"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
	}
	if err := json.Unmarshal([]byte(resp.Content), &reply); err != nil || reply.ImageB64 == "" {
		writeError(w, http.StatusBadGateway, "server_error", "the backend returned no image", "")
		return
	}
	if _, err := base64.StdEncoding.DecodeString(reply.ImageB64); err != nil {
		writeError(w, http.StatusBadGateway, "server_error", "the backend returned an undecodable image", "")
		return
	}
	// The OpenAI envelope, plus the dimensions actually produced. Those are not in
	// the standard shape, but a caller that asked for an aspect rather than pixels
	// otherwise has no way to learn what it got.
	writeJSON(w, map[string]any{
		"created": time.Now().Unix(),
		"data": []map[string]any{{
			"b64_json": reply.ImageB64,
			"width":    reply.Width,
			"height":   reply.Height,
		}},
	})
}

// uploadMIME sniffs the declared content type of a multipart part, defaulting to
// PNG (what the backend assumes when it cannot tell).
func uploadMIME(hdr *multipart.FileHeader) string {
	if hdr == nil {
		return "image/png"
	}
	if ct := hdr.Header.Get("Content-Type"); ct != "" {
		return ct
	}
	return "image/png"
}

// imageCatalog resolves the shared catalog, or nil when none is wired.
func (s *server) imageCatalog() contract.ImageCatalog {
	if s.images == nil {
		return nil
	}
	return s.images()
}

// imageStyles returns the declared styles, or nil when no catalog is wired.
func (s *server) imageStyles() []contract.ImageStyleInfo {
	cat := s.imageCatalog()
	if cat == nil {
		return nil
	}
	return cat.ImageStyles()
}

// drawableStyles is what the pool can ACTUALLY draw right now: a declared style no
// usable image entry carries is not on offer, however well documented. Covers both
// the backend being down and a declaration installed without a matching models.yaml
// tag. Accepting a request for it would 503 on every call, and the caller picked it
// on our word.
//
// Usable is "live" OR "tentative" — a tentative entry has been re-admitted by the
// heartbeat and IS pick-eligible, just not yet proven by a real request
// (leaf/model/20-snapshot.go). Treating it as absent used to make a flapping GPU
// look like a style that does not exist, which is a different (and worse) claim.
func (s *server) drawableStyles(snap contract.PoolSnapshot) []contract.ImageStyleInfo {
	live := map[string]bool{}
	for _, e := range snap.Entries {
		if !strings.EqualFold(e.Kind, contract.KindImage) || (e.State != "live" && e.State != "tentative") {
			continue
		}
		for _, t := range e.Tags {
			live[t] = true
		}
	}
	var out []contract.ImageStyleInfo
	for _, st := range s.imageStyles() {
		if live[st.Style] {
			out = append(out, st)
		}
	}
	return out
}

func findStyle(styles []contract.ImageStyleInfo, name string) (contract.ImageStyleInfo, bool) {
	for _, s := range styles {
		if strings.EqualFold(s.Style, name) {
			return s, true
		}
	}
	return contract.ImageStyleInfo{}, false
}

func styleList(styles []contract.ImageStyleInfo) string {
	out := make([]string, 0, len(styles))
	for _, s := range styles {
		out = append(out, s.Style)
	}
	if len(out) == 0 {
		return "(none — no image backend is live)"
	}
	return strings.Join(out, ", ")
}

// resolveAspect maps the request's `size` onto one of the style's aspects. It
// accepts an aspect word or an exact "WxH" that the style actually offers; src is
// an edit's source picture, which decides the shape when the caller names none.
//
// There is deliberately NO nearest-match: silently serving 1024×1024 to someone
// who asked for 1792×1024 produces a picture that is wrong in a way the caller
// cannot see. Refusing names the alternatives and costs one round trip.
//
// A NAMED size always wins, including on an edit that will crop for it. This is
// where the endpoint parts company with the in-conversation tool, which lets the
// source override the model's choice unconditionally: that rule exists because the
// model only ever sees a filename (leaf/tools/imagecreate/00-tool.go), while an
// HTTP caller posted the bytes itself and can see what shape they are. Overriding
// it here would make `size` a field that is silently dropped on one of the two
// endpoints — exactly what `change` is refused for above.
func resolveAspect(s contract.ImageStyleInfo, size string, src []byte) (string, bool) {
	size = strings.TrimSpace(strings.ToLower(size))
	if size == "" || size == "auto" {
		return defaultAspect(s, src), true
	}
	if _, ok := s.Sizes[size]; ok {
		return size, true
	}
	for aspect, wh := range s.Sizes {
		if len(wh) == 2 && fmt.Sprintf("%dx%d", wh[0], wh[1]) == size {
			return aspect, true
		}
	}
	return "", false
}

// defaultAspect settles the shape for a caller that named no size.
//
// On an EDIT that is the source picture's own shape, not a square: the backend
// scales the source with a CENTRE CROP, so editing a portrait photo at the square
// default comes back with its top and bottom cut off — and the reply would report
// that square as if it were what was asked for. The caller sent no size precisely
// because it had no opinion, so the picture's own proportions are the only answer
// that cannot be wrong.
//
// Falls back to the square when the shape cannot be read (WebP / HEIC — the
// standard decoders do not know them) or when the style does not declare it. Both
// still crop; there is simply nothing better to pick, and the reply's width/height
// says what happened.
func defaultAspect(s contract.ImageStyleInfo, src []byte) string {
	if len(src) == 0 {
		return imagesFallbackAspect
	}
	shape, ok := sourceAspect(src)
	if !ok {
		return imagesFallbackAspect
	}
	if _, declared := s.Sizes[shape]; !declared {
		return imagesFallbackAspect
	}
	return shape
}

// sourceAspect reads a picture's own proportions and maps them onto the aspect
// keys. Only the HEADER is decoded, so it costs nothing on bytes already in memory.
//
// The tool side has its own copy (leaf/tools/imagecreate/aspectOf) and that is
// deliberate: the two are leaf modules that may not import each other, and this is
// six lines of standard library rather than a piece of knowledge that could drift —
// unlike the prompt guidance, which is shared through contract.ImageCatalog exactly
// because it would.
func sourceAspect(data []byte) (string, bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return "", false
	}
	switch {
	case cfg.Width > cfg.Height:
		return "landscape", true
	case cfg.Height > cfg.Width:
		return "portrait", true
	default:
		return "square", true
	}
}

// resolveChange settles the edit strength. An edit with none named DEFAULTS
// rather than failing: the field is ours, not OpenAI's, so a caller following the
// standard shape omits it — and the backend treats an unknown strength as fatal,
// which surfaced as a routing-flavoured 503 with nothing to do with the cause.
// The in-conversation tool has always defaulted here; this is the same rule.
//
// A named-but-unknown strength is refused with the list, for the same reason an
// unsupported size is.
func resolveChange(s contract.ImageStyleInfo, change string, isEdit bool) (string, bool) {
	change = strings.TrimSpace(strings.ToLower(change))
	if !isEdit {
		return "", true // generations carry no edit strength at all
	}
	if change == "" {
		if slices.Contains(s.Changes, imagesFallbackChange) {
			return imagesFallbackChange, true
		}
		if len(s.Changes) > 0 {
			return s.Changes[len(s.Changes)/2], true // middle of what this style declares
		}
		return "", false
	}
	return change, slices.Contains(s.Changes, change)
}

func sizeList(s contract.ImageStyleInfo) string {
	out := make([]string, 0, len(s.Sizes))
	for aspect, wh := range s.Sizes {
		if len(wh) == 2 {
			out = append(out, fmt.Sprintf("%s (%dx%d)", aspect, wh[0], wh[1]))
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// keyAdmitsImages reports whether this key may address the image lane. A
// models-form key pins entry names and never addresses lanes, so it is refused
// here rather than silently routed.
func keyAdmitsImages(info contract.APIKeyInfo) bool {
	if !keyIsLaneForm(info) {
		return false
	}
	for _, k := range info.Kinds {
		if strings.EqualFold(k, contract.KindImage) {
			return true
		}
	}
	return false
}
