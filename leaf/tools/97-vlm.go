package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"agentbob/contract"
)

// visionMaxBytes caps the image read off disk before it reaches memory (10 MB) —
// the same guard the read leg uses.
const visionMaxBytes int64 = 10 * 1024 * 1024

// reversePromptInstruction is the canned instruction behind `image task=reverse_prompt`.
// A CONST, not a map keyed by task: the old code looked this up with the model-supplied
// task string and checked the miss, but the task is now validated by the dispatch switch
// (95-image.go). A map here would only reintroduce a way to send the vision model an
// EMPTY instruction and then deliver whatever came back to the user verbatim. `answer`
// has no canned text at all — it sends the user's own question.
const reversePromptInstruction = "Describe this image in exhaustive detail as an image-generation prompt.\n" +
	"Cover ALL of: main subject, secondary objects, background, colors,\n" +
	"lighting, art style, composition, camera angle, textures, mood, and any text.\n" +
	"Write at least 150 words. Do not omit minor details."

// visionStills resolves the model's file reference to the pictures a vision model will
// actually be shown, plus the line describing them.
//
// ONE resolution point for both media kinds, so "which file" and "what does the model
// see" cannot drift apart between them:
//   - an image → itself, one still, no extra description;
//   - a video → evenly-spaced frames plus the timeline line planFrames wrote.
//
// Returns a ready ToolResult on every refusal path (wrong format, unreadable, no frames).
//
// allowVideo is the CALLER's appetite, not a capability check: reverse_prompt passes
// false because its deliverable is a text-to-image prompt, which a moving scene has no
// single answer for. Refusing through pickMedia rather than after extraction means a
// clip is never read into memory and decoded only to be thrown away.
func visionStills(ctx context.Context, tc contract.ToolContext, ref string, allowVideo bool, start, window float64, toolName string) ([]contract.ImageRef, string, *contract.ToolResult) {
	maxVideo := videoMaxBytes
	if !allowVideo {
		maxVideo = 0
	}
	// Resolve the reference to bytes through the turn's AttachmentSet (this turn's file,
	// or an EARLIER turn's via the space inbox when this turn carries none — never a path
	// the model invents). Shared with the read leg so "which file" can't drift.
	body, mime, errRes := pickMedia(ctx, tc, ref, toolName, visionMaxBytes, maxVideo)
	if errRes != nil {
		return nil, "", errRes
	}
	if strings.HasPrefix(mime, "video/") {
		frames, plan, err := videoFrames(ctx, body, start, window)
		if err != nil {
			r := contract.ErrResult(toolName + ": " + err.Error())
			return nil, "", &r
		}
		return frames, plan.Describe(), nil
	}
	// Vision LLM backends accept a NARROWER format set than the OCR sidecar (which
	// decodes HEIC/BMP via pillow): Anthropic / OpenAI-compatible vision APIs take
	// png / jpeg / webp only. Reject the rest upfront with a clear message instead of
	// forwarding it and surfacing an opaque provider 400 (which the backend-error path
	// below would mislabel as "no vision model"). Video never reaches here — frames are
	// always JPEG by construction.
	switch mime {
	case "image/png", "image/jpeg", "image/webp":
		// supported
	default:
		r := contract.ErrResult(fmt.Sprintf("%s: 视觉模型暂不支持该格式(%s)，请发送 PNG / JPEG / WebP 图片。", toolName, mime))
		return nil, "", &r
	}
	return []contract.ImageRef{{Data: body, MIME: mime}}, "", nil
}

// askVision is the leg both vision TASKS share (answer / reverse_prompt): send the
// resolved stills to a vision-tagged entry and enforce the fail-closed checks. Returns
// the model's answer, or a non-nil ToolResult the caller must return as-is. toolName
// only labels the messages.
//
// Shared on purpose: the two tasks differ in what they ASK FOR and who delivers the
// answer, never in how a missing vision model is handled. Duplicating this leg is how
// those guarantees would drift apart.
//
// images carries ONE picture for an image and the whole frame sequence for a video —
// the backend leg is identical either way, which is the point: a video is not a second
// code path, it is a longer slice.
func askVision(ctx context.Context, tc contract.ToolContext, poolFn func() contract.ModelPool, images []contract.ImageRef, instruction, toolName string) (string, *contract.ToolResult) {
	if len(images) == 0 {
		r := contract.ErrResult(toolName + ": 没有可看的画面")
		return "", &r
	}
	pool := poolFn()
	if pool == nil {
		r := contract.ErrResult(toolName + ": 视觉模型不可用，请稍后再试").
			WithHint("需要 models.yaml 配一个带 vision 标签的模型")
		return "", &r
	}
	resp, err := pool.Chat(ctx, contract.ModelRequest{Requires: []string{"vision"}}, []contract.Message{{
		Role:    "user",
		Content: instruction,
		Images:  images,
	}})
	if err != nil {
		slog.Warn("vision leg: backend error", "tool", toolName, "sid", tc.Sid, "frames", len(images), "err", err)
		// A multi-frame request can also fail because the entry that answered cannot hold
		// that many pictures (a per-prompt image cap) or that much context — indistinguishable
		// from "no vision model" at this layer, so the hint names both ways out. Narrowing
		// the window is the one the MODEL can act on without an operator.
		if len(images) > 1 {
			r := contract.ErrResult(toolName + ": 视觉模型没能处理这段视频 —— 可以用 time_start / time_window 只看其中一小段再试").
				WithHint("多帧请求也可能超出后端的单次图片数或上下文上限")
			return "", &r
		}
		r := contract.ErrResult(toolName + ": 视觉模型不可用，请稍后再试").
			WithHint("需要 models.yaml 配一个带 vision 标签的模型")
		return "", &r
	}
	// FAIL-CLOSED guard: verify the entry that actually answered carries the `vision`
	// tag. If a deployment configured a vision→smart fallback rule, the pool may have
	// served the request on a non-vision model that ignored the image and made the
	// answer up — refuse that rather than return a confident hallucination.
	if !entryHasTag(pool, resp.Model, "vision") {
		slog.Warn("vision leg: answered by non-vision entry, refusing", "tool", toolName, "sid", tc.Sid, "entry", resp.Model)
		r := contract.ErrResult(toolName + ": 没有可用的视觉模型，无法处理图片").
			WithHint("需要 models.yaml 配一个带 vision 标签的模型")
		return "", &r
	}
	// Empty content is an error, not an answer. On task=reverse_prompt it would make
	// the exit a silent stay (round.go reads an empty exit reply as stay_silent); on the
	// view tier it would hand the main model nothing and invite it to fill the gap from
	// the filename — the exact failure this tier exists to close.
	if strings.TrimSpace(resp.Content) == "" {
		slog.Warn("vision leg: empty answer from vision model", "tool", toolName, "sid", tc.Sid, "entry", resp.Model)
		r := contract.ErrResult(toolName + ": 视觉模型没有返回内容，请重试或换一张图片")
		return "", &r
	}
	return resp.Content, nil
}

// entryHasTag reports whether the named pool entry currently carries tag. Used by the
// vision leg to confirm the answering entry was actually vision-capable. Unknown
// entry / empty name → false (fail-closed).
func entryHasTag(pool contract.ModelPool, entryName, tag string) bool {
	if entryName == "" {
		return false
	}
	for _, e := range pool.Snapshot().Entries {
		if e.Name != entryName {
			continue
		}
		for _, t := range e.Tags {
			if t == tag {
				return true
			}
		}
		return false
	}
	return false
}
