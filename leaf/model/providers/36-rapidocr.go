package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
)

// RapidOCRClient is a thin Chatter that maps Chat calls onto a RapidOCR
// sidecar's POST /ocr endpoint (sidecars/pp-ocrv6). The pool routes here for KindOCR;
// the flow's pre-OCR step packs one image (+ optional lang / bbox / prompt /
// enhance) per Chat message.
//
// Wire shape on the way in (cross-message multi-image batch):
//
//   - one image per contract.Message via Images[0]; same-message multi-image
//     errors (one image per Chat message by design; the pool's batch path stacks
//     several messages, each with its own image and its own JSON params in
//     Message.Content)
//   - Message.Content is JSON-encoded rapidocrParams (lang / bbox / prompt /
//     enhance); empty / non-JSON Content is fine — sidecar defaults apply
//   - >10 images per Chat call errors before any HTTP work (a soft batch cap)
//
// Wire shape on the way back (aggregate envelope so single + batch share the same
// shape — single-image is just the N=1 case):
//
//	{
//	  "images": [
//	    {"lines": [...], "text": "...", "error": ""},
//	    {"lines": [...], "text": "...", "error": "rapidocr: …"}
//	  ],
//	  "text": "<all per-image texts joined by \n---\n>"
//	}
//
// Failure is collect-all: a per-image HTTP / parse error populates that image's
// .error field but does NOT abort the rest of the batch. ChatStream is unsupported
// — OCR is request/response.
const (
	rapidocrChatPath = "/ocr"
	rapidocrPingPath = "/healthz"
	// rapidocrMaxImages caps how many images one Chat call can carry (a soft batch
	// limit to keep one OCR turn bounded).
	rapidocrMaxImages = 10
)

// rapidocrPerImageTimeout caps a single /ocr POST so a wedged sidecar can't pin
// the OCR call until the caller's ctx cancels. OCR is quick, so the ceiling is
// tighter than the LLM stream timeout.
const rapidocrPerImageTimeout = 2 * time.Minute

// rapidocrMaxRespBytes bounds the per-image response read.
const rapidocrMaxRespBytes = 4 << 20

// Model-facing per-image error strings. Sanitized — NO baseURL / raw body /
// internal error chain (those go to slog.Warn for ops).
const (
	perImageBuildErr     = "rapidocr: internal build error"
	perImageTransportErr = "rapidocr: backend unavailable"
	perImageReadErr      = "rapidocr: response read failed"
	perImageStatusErrFmt = "rapidocr: HTTP %d" // status code only
	perImageParseErr     = "rapidocr: invalid response shape"
)

// RapidOCRClient — see the package docs above.
type RapidOCRClient struct {
	baseURL string
	http    *http.Client
}

// rapidocrParams is the optional JSON envelope a caller sets on Message.Content to
// forward non-image params through the Chatter interface. Empty values fall
// through to the sidecar's defaults.
type rapidocrParams struct {
	Lang    string `json:"lang,omitempty"`
	BBox    []int  `json:"bbox,omitempty"`    // [x1, y1, x2, y2]
	Prompt  string `json:"prompt,omitempty"`  // transcription steer; empty → backend generic default
	Enhance string `json:"enhance,omitempty"` // pre-OCR image enhancement ("gray"); empty → none
}

// rapidocrInput pairs one image with its per-message params.
type rapidocrInput struct {
	img    contract.ImageRef
	params rapidocrParams
}

// rapidocrLine mirrors one line in the sidecar's per-image response.
type rapidocrLine struct {
	BBox [][]float64 `json:"bbox"`
	Text string      `json:"text"`
	Conf float64     `json:"conf"`
}

// rapidocrPerImage is one image's slot in the aggregate envelope.
type rapidocrPerImage struct {
	Lines []rapidocrLine `json:"lines,omitempty"`
	Text  string         `json:"text,omitempty"`
	Error string         `json:"error,omitempty"`
}

// rapidocrAggregateResponse is what every Chat call's Content carries: one slot
// per input image (in order) + a joined `text`.
type rapidocrAggregateResponse struct {
	Images []rapidocrPerImage `json:"images"`
	Text   string             `json:"text"`
}

// rapidocrSidecarReply is the raw shape one /ocr POST returns (one image's worth).
type rapidocrSidecarReply struct {
	Lines []rapidocrLine `json:"lines"`
	Text  string         `json:"text"`
}

// NewRapidOCRClient builds a Chatter pointed at baseURL (e.g. the ocr sidecar's
// URL). No trailing /v1 — the sidecar isn't OpenAI-shaped.
func NewRapidOCRClient(baseURL string) *RapidOCRClient {
	return &RapidOCRClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{}, // no client timeout — ctx bounds it
	}
}

// Chat walks every message that carries an image and POSTs each to {baseURL}/ocr.
// The aggregate envelope is JSON-marshalled into ChatResponse.Content so callers
// see one shape whether they sent one image or N. tools/model are ignored.
func (c *RapidOCRClient) Chat(ctx context.Context, _ string, messages []contract.Message, _ []contract.ToolSpec) (contract.ChatResponse, error) {
	inputs, err := extractRapidOCRRequests(messages)
	if err != nil {
		return contract.ChatResponse{}, err
	}

	agg := rapidocrAggregateResponse{Images: make([]rapidocrPerImage, 0, len(inputs))}
	texts := make([]string, 0, len(inputs))
	var fails []error
	for _, in := range inputs {
		per, perErr := c.chatOneImage(ctx, in)
		if perErr != nil {
			fails = append(fails, perErr)
		}
		agg.Images = append(agg.Images, per)
		if per.Error != "" {
			texts = append(texts, fmt.Sprintf("[OCR failed: %s]", per.Error))
		} else {
			texts = append(texts, per.Text)
		}
	}
	agg.Text = strings.Join(texts, "\n---\n")

	out, err := json.Marshal(agg)
	if err != nil {
		return contract.ChatResponse{}, fmt.Errorf("rapidocr: marshal aggregate: %w", err)
	}
	// A cancelled batch must surface the cancellation regardless of how many images
	// completed — otherwise a context-cancelled OCR with a sub-majority fail count
	// banks a fake success and resets the entry's consecutive-fail count.
	if ctx.Err() != nil {
		return contract.ChatResponse{Content: string(out)}, ctx.Err()
	}
	// MAJORITY-fail (incl. all) = the sidecar is unhealthy, not just one bad image →
	// bubble so the pool cools + fails over rather than banking a fake success (which
	// would reset the entry's consecutive-fail count and hide an intermittently-5xx
	// sidecar). A single bad image in a healthy batch still succeeds (>50% gate).
	// Per-image .Error stays in content regardless.
	failed := 0
	for _, img := range agg.Images {
		if img.Error != "" {
			failed++
		}
	}
	if n := len(agg.Images); n > 0 && failed*2 > n {
		return contract.ChatResponse{Content: string(out)},
			fmt.Errorf("rapidocr: %d/%d image(s) failed (sidecar likely unhealthy; see per-image errors in content): %w",
				failed, n, errors.Join(fails...))
	}
	return contract.ChatResponse{Content: string(out)}, nil
}

// chatOneImage POSTs one image to /ocr and returns the per-image slot plus the
// underlying error (nil on success). Transport / status / parse errors become a
// populated .Error field (collect-all) and the raw error is returned alongside.
func (c *RapidOCRClient) chatOneImage(ctx context.Context, in rapidocrInput) (rapidocrPerImage, error) {
	body, contentType, err := buildRapidOCRMultipart(in.img, in.params)
	if err != nil {
		slog.Warn("rapidocr: build request failed", "err", err)
		return rapidocrPerImage{Error: perImageBuildErr}, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, rapidocrPerImageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST", c.baseURL+rapidocrChatPath, body)
	if err != nil {
		slog.Warn("rapidocr: build http request failed", "err", err)
		return rapidocrPerImage{Error: perImageBuildErr}, err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.http.Do(req)
	if err != nil {
		slog.Warn("rapidocr: transport failed", "url", c.baseURL+rapidocrChatPath, "err", err)
		return rapidocrPerImage{Error: perImageTransportErr}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, rapidocrMaxRespBytes))
	if err != nil {
		slog.Warn("rapidocr: read response failed", "err", err)
		return rapidocrPerImage{Error: perImageReadErr}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("rapidocr: non-2xx response", "status", resp.StatusCode, "body", truncateBody(respBody))
		return rapidocrPerImage{Error: fmt.Sprintf(perImageStatusErrFmt, resp.StatusCode)},
			fmt.Errorf("rapidocr: HTTP %d: %s", resp.StatusCode, truncateBody(respBody))
	}
	var parsed rapidocrSidecarReply
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		slog.Warn("rapidocr: parse response failed", "err", err, "body", truncateBody(respBody))
		return rapidocrPerImage{Error: perImageParseErr}, fmt.Errorf("rapidocr: parse response: %w", err)
	}
	if parsed.Lines == nil && parsed.Text == "" {
		slog.Warn("rapidocr: empty response — verify sidecar configuration", "url", c.baseURL+rapidocrChatPath)
	}
	return rapidocrPerImage{Lines: parsed.Lines, Text: parsed.Text}, nil
}

// ChatStream is intentionally unsupported: the pool's streaming path is LLM-only.
func (c *RapidOCRClient) ChatStream(_ context.Context, _ string, _ []contract.Message, _ []contract.ToolSpec) (<-chan contract.StreamEvent, error) {
	return nil, fmt.Errorf("rapidocr: streaming not supported (use Chat)")
}

// Ping GETs {baseURL}/healthz to confirm the sidecar is up.
func (c *RapidOCRClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+rapidocrPingPath, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	// Drain before close so the keep-alive conn can be reused (package discipline,
	// mirrors 00-client.go / 30-anthropic.go).
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rapidocr ping: %s%s → HTTP %d", c.baseURL, rapidocrPingPath, resp.StatusCode)
	}
	return nil
}

// extractRapidOCRRequests collects one rapidocrInput per image-carrying message.
// Errors on same-message multi-image, no images at all, or > rapidocrMaxImages.
func extractRapidOCRRequests(messages []contract.Message) ([]rapidocrInput, error) {
	var inputs []rapidocrInput
	for i, m := range messages {
		if len(m.Images) == 0 {
			continue
		}
		if len(m.Images) > 1 {
			return nil, fmt.Errorf("rapidocr: message[%d] carries %d images; expected exactly 1 per message", i, len(m.Images))
		}
		var params rapidocrParams
		if strings.TrimSpace(m.Content) != "" {
			_ = json.Unmarshal([]byte(m.Content), &params)
		}
		inputs = append(inputs, rapidocrInput{img: m.Images[0], params: params})
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("rapidocr: chat carries no image")
	}
	if len(inputs) > rapidocrMaxImages {
		return nil, fmt.Errorf("rapidocr: chat carries %d images; max %d per batch (split into multiple turns)",
			len(inputs), rapidocrMaxImages)
	}
	return inputs, nil
}

// buildRapidOCRMultipart writes the request body: an `image` file part + optional
// `lang` / `bbox` / `prompt` / `enhance` form fields (the standardized /ocr
// contract — see docs/ocr-vision-stack.md). An old sidecar ignores unknown fields.
func buildRapidOCRMultipart(img contract.ImageRef, params rapidocrParams) (*bytes.Buffer, string, error) {
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)

	filename := "image" + extensionForMIME(img.MIME)
	part, err := w.CreateFormFile("image", filename)
	if err != nil {
		return nil, "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(img.Data); err != nil {
		return nil, "", fmt.Errorf("write image: %w", err)
	}
	if params.Lang != "" {
		if err := w.WriteField("lang", params.Lang); err != nil {
			return nil, "", fmt.Errorf("write lang: %w", err)
		}
	}
	if len(params.BBox) > 0 {
		parts := make([]string, len(params.BBox))
		for i, v := range params.BBox {
			parts[i] = strconv.Itoa(v)
		}
		if err := w.WriteField("bbox", strings.Join(parts, ",")); err != nil {
			return nil, "", fmt.Errorf("write bbox: %w", err)
		}
	}
	if params.Prompt != "" {
		if err := w.WriteField("prompt", params.Prompt); err != nil {
			return nil, "", fmt.Errorf("write prompt: %w", err)
		}
	}
	if params.Enhance != "" {
		if err := w.WriteField("enhance", params.Enhance); err != nil {
			return nil, "", fmt.Errorf("write enhance: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf, w.FormDataContentType(), nil
}

// extensionForMIME maps common image MIMEs to a filename extension (cosmetic).
func extensionForMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/heic", "image/heif":
		return ".heic"
	default:
		return ".bin"
	}
}

// truncateBody clips an HTTP body for inclusion in an error message.
func truncateBody(b []byte) string {
	const n = 200
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
