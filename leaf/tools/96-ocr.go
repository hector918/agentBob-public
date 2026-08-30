package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register jpeg for image.DecodeConfig (bbox-vs-dimensions guard)
	_ "image/png"  // register png for image.DecodeConfig
	"log/slog"

	"agentbob/contract"
)

// kindChat is a model pool bound to a SINGLE Kind: a media tool holds one and can
// only ever reach its own backend (KindOCR → the OCR sidecar), never the general
// LLM. The pool is resolved lazily (it may start after tools in the trunk order);
// a nil pool means no backend is wired yet.
type kindChat struct {
	pool func() contract.ModelPool
	kind string
}

func (k kindChat) chat(ctx context.Context, msgs []contract.Message) (contract.ChatResponse, error) {
	// Guard the THUNK too, not just its result: since the image tool merged the two
	// legs into one struct, a half-wired visionTool (one leg set, the other zero) is
	// constructible — tests do exactly that — and a zero kindChat would panic here.
	if k.pool == nil {
		return contract.ChatResponse{}, fmt.Errorf("model pool not wired")
	}
	p := k.pool()
	if p == nil {
		return contract.ChatResponse{}, fmt.Errorf("model pool not available")
	}
	return p.Chat(ctx, contract.ModelRequest{Kind: k.kind}, msgs)
}

// ocrMaxBytes caps the image size read off disk before it reaches memory (10 MB).
// Past it the tool refuses the image.
const ocrMaxBytes int64 = 10 * 1024 * 1024

// readImageText is the `vision task=read` leg: transcribe one image through the
// Kind-bound OCR backend (KindOCR → the OCR sidecar). It is the sole OCR path —
// images are read on demand here, never pre-extracted.
//
// A LEG, not a tool: the three jobs share one tool surface (`vision`, see
// 95-vision.go) so the model picks a task rather than discriminating between three
// near-identical tools. That fragmentation is what produced the gap —
// `ocr` claimed "the text", `vision` claimed "a prompt", and a plain "what does
// this picture mean" fell between them and was answered from the filename.
//
// IMAGES ONLY. It passes maxVideo=0 to pickMedia, so a clip is refused by name before
// any bytes are read: a per-frame transcript of a moving scene is not what "逐字转写"
// means, and quietly sampling one would answer a question nobody asked.
func readImageText(ctx context.Context, tc contract.ToolContext, chat kindChat, p visionArgs) contract.ToolResult {
	if len(p.BBox) != 0 && len(p.BBox) != 4 {
		return contract.ErrResult("vision: bbox must be 4 ints [x1, y1, x2, y2]")
	}
	if len(p.BBox) == 4 {
		// Cheap geometry pre-check before hitting the backend — a negative coord or
		// a non-positive-area box would otherwise produce a nonsense crop server-side.
		x1, y1, x2, y2 := p.BBox[0], p.BBox[1], p.BBox[2], p.BBox[3]
		if x1 < 0 || y1 < 0 || x2 < 0 || y2 < 0 {
			return contract.ErrResult("vision: bbox coordinates must be non-negative [x1, y1, x2, y2]")
		}
		if x1 >= x2 || y1 >= y2 {
			return contract.ErrResult("vision: bbox must have positive area — need x1 < x2 and y1 < y2 [x1, y1, x2, y2]")
		}
	}
	if p.Lang != "" && !validOCRLang(p.Lang) {
		return contract.ErrResult(fmt.Sprintf(
			"vision: lang %q not supported (use auto / ch / en / ja / ko)", p.Lang))
	}
	if p.Enhance != "" && p.Enhance != "gray" {
		return contract.ErrResult(fmt.Sprintf(
			"vision: enhance %q not supported (use \"gray\" or omit)", p.Enhance))
	}

	// Resolve file_path to bytes through the turn's AttachmentSet (this turn's image, or
	// an EARLIER turn's via the space inbox when this turn carries none — never a path the
	// model invents). Shared with the vision leg so "which file" can't drift.
	body, mime, errRes := pickMedia(ctx, tc, p.FilePath, "vision", ocrMaxBytes, 0)
	if errRes != nil {
		return *errRes
	}

	// bbox-vs-dimensions guard: the model can't see the image, so it guesses crop
	// coordinates in an imagined space that may fall outside the real frame — the
	// backend then crops an off-image region and honestly reports black. Decode just
	// the header (cheap) and reject an out-of-frame box with the REAL dimensions so
	// the model re-aims or drops the bbox. jpeg/png only (stdlib); webp/bmp fall through.
	if len(p.BBox) == 4 {
		if cfg, _, derr := image.DecodeConfig(bytes.NewReader(body)); derr == nil && cfg.Width > 0 && cfg.Height > 0 {
			if p.BBox[2] > cfg.Width || p.BBox[3] > cfg.Height || p.BBox[0] >= cfg.Width || p.BBox[1] >= cfg.Height {
				return contract.ErrResult(fmt.Sprintf(
					"vision: bbox %v 超出图片范围(图片 %d×%d 像素)。坐标要落在 x:0..%d、y:0..%d 内;不确定就不填 bbox,整图识别 + 用 prompt 指定只要哪部分。",
					p.BBox, cfg.Width, cfg.Height, cfg.Width, cfg.Height))
			}
		}
	}

	// Pack the non-image args (lang / bbox / prompt / enhance) as JSON Content — the
	// rapidocr provider extracts them and forwards as multipart form fields.
	paramsBlob, err := json.Marshal(struct {
		Lang    string `json:"lang,omitempty"`
		BBox    []int  `json:"bbox,omitempty"`
		Prompt  string `json:"prompt,omitempty"`
		Enhance string `json:"enhance,omitempty"`
	}{Lang: p.Lang, BBox: p.BBox, Prompt: p.Prompt, Enhance: p.Enhance})
	if err != nil {
		return contract.ErrResult("vision: marshal params: " + err.Error())
	}
	msgs := []contract.Message{{
		Role:    "user",
		Content: string(paramsBlob),
		Images:  []contract.ImageRef{{Data: body, MIME: mime}},
	}}

	resp, err := chat.chat(ctx, msgs)
	if err != nil {
		slog.Warn("vision read leg: backend error", "sid", tc.Sid, "err", err)
		// A full queue is a DIFFERENT answer from a dead backend: the OCR
		// backend is alive and working through a line that is already at
		// capacity. Saying so keeps the model from concluding OCR is broken
		// and substituting a lesser tool (a vision description is not a
		// transcription) — retrying in a moment is the right move.
		if errors.Is(err, contract.ErrModelQueueFull) {
			return contract.ErrResult("vision: 识别队列已满(后端正在处理其他图片),稍等一会儿再试这张图")
		}
		return contract.ErrResult("vision: 识别后端暂时不可用,请稍后再试").
			WithHint("需要 models.yaml 配一个 kind=ocr 的条目")
	}
	// resp.Content is the rapidocr aggregate envelope:
	//   {"images":[{"lines":[...],"text":"...","error":""}, ...], "text":"<joined>"}
	// Default (include_lines=false) strips the per-image `lines` so bbox + conf
	// metadata doesn't blow up the prompt budget.
	out := resp.Content
	if !p.IncludeLines {
		out = stripLines(resp.Content)
	}
	return contract.OKResult(out)
}

// stripLines re-marshals the rapidocr aggregate envelope without the per-image
// `lines` field. On any parse failure it returns the original raw response — better
// a noisy envelope than a silently dropped transcript. Per-image errors and the
// joined top-level text are preserved.
func stripLines(raw string) string {
	var env struct {
		Images []struct {
			Text  string `json:"text"`
			Error string `json:"error,omitempty"`
		} `json:"images"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return raw
	}
	out, err := json.Marshal(env)
	if err != nil {
		return raw
	}
	return string(out)
}

// validOCRLang gates the lang parameter against the supported set.
func validOCRLang(s string) bool {
	switch s {
	case "auto", "ch", "en", "ja", "ko":
		return true
	}
	return false
}

// detectImageMIME sniffs magic bytes for the formats the OCR sidecar accepts. HEIC
// is passed through (bob can't decode it CGO-free, but the sidecar does via
// pillow-heif). Empty return = unrecognised. Ported from skeleton's tools/ocr.
func detectImageMIME(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	switch {
	case b[0] == 0x89 && b[1] == 0x50 && b[2] == 0x4e && b[3] == 0x47:
		return "image/png"
	case b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "image/jpeg"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case b[0] == 0x42 && b[1] == 0x4d:
		return "image/bmp"
	case len(b) >= 12 && string(b[4:8]) == "ftyp" && isHEIFBrand(string(b[8:12])):
		return "image/heic"
	}
	return ""
}

// isHEIFBrand reports whether a 4-char ISOBMFF major brand is a HEIF/HEIC still brand.
func isHEIFBrand(brand string) bool {
	switch brand {
	case "heic", "heix", "heim", "heis", "hevc", "hevx", "hevm", "hevs",
		"mif1", "msf1", "mif2":
		return true
	}
	return false
}
