package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentbob/contract"
)

// visionCandidate is the vision tool's APPETITE: a picture or a clip.
//
// A local union rather than a widened contract.Attachment.IsImageContent, because that
// predicate has other holders with a different appetite — imagecreate's i2i init image
// (leaf/tools/imagecreate/00-tool.go) and compose's 图片 label — and a video must keep
// falling out of those. Widening the shared predicate would have handed a 200 MB clip
// to the image generator.
func visionCandidate(a contract.Attachment) bool {
	return a.IsImageContent() || a.IsVideoContent()
}

// resolveAttachment is the appetite-driven core every media tool shares: it turns the
// model's file reference into ONE real attachment plus its bytes, or a ready ErrResult.
//
// The heavy lifting — which attachment, the inbox fallback — lives in the AttachmentSet
// (flow/compose); this is the shared tail: appetite gate, readiness gate, size cap. What
// it deliberately does NOT do is have an opinion about FORMAT — the vision legs sniff
// magic bytes, the audio leg hands the file to ffmpeg and lets it complain. Keeping the
// resolution identical while the format handling differs is exactly the seam that stops
// "which file did the model mean" from drifting between tools.
//
// It resolves ONLY — reading is a separate call (readAttachment), so a caller with a
// narrower appetite than its resolution can refuse a file BEFORE its bytes are pulled
// into memory. That split is what lets the image-only leg resolve a video (to say
// something useful about it) without paying for one.
//
// kindLabel names the appetite in the error messages ("图片或视频" / "音频或视频").
func resolveAttachment(ctx context.Context, tc contract.ToolContext, arg, toolName, kindLabel string,
	want func(contract.Attachment) bool,
) (contract.Attachment, *contract.ToolResult) {
	fail := func(msg string) (contract.Attachment, *contract.ToolResult) {
		r := contract.ErrResult(msg)
		return contract.Attachment{}, &r
	}
	if tc.Attachments == nil {
		return fail(toolName + ": 当前没有可用的文件空间（读不到文件）")
	}
	picks := tc.Attachments.Pick(ctx, want, arg)
	switch {
	case len(picks) == 0:
		if recent := tc.Attachments.Suggest(ctx, want); len(recent) > 0 {
			return fail(fmt.Sprintf("%s: 没找到要处理的%s。最近收到的：%s —— 在 file_path 写出文件名即可;都不是就请用户重新发送。",
				toolName, kindLabel, strings.Join(attPaths(recent), "、")))
		}
		return fail(fmt.Sprintf("%s: 本轮消息没有%s附件。请让用户把要处理的文件直接发来，收到后再处理。", toolName, kindLabel))
	case len(picks) > 1:
		return fail(fmt.Sprintf("%s: 本轮有多个可处理的文件：%s —— 在 file_path 里写出要处理的那个的文件名。",
			toolName, strings.Join(attPaths(picks), "、")))
	}
	a := picks[0]
	if a.Path == "" {
		// Named an attachment that never downloaded / was never placed — actionable, not
		// a confusing "is a directory" read error (Pick matches by name across all kinds).
		return fail(fmt.Sprintf("%s: %q 还没准备好（未下载或未入空间），请让用户重新发送。", toolName, arg))
	}
	if !want(a) {
		msg := fmt.Sprintf("%s: %q 是%s，这一步只处理%s。", toolName, arg, a.Kind, kindLabel)
		if names := attPaths(tc.Attachments.Suggest(ctx, want)); len(names) > 0 {
			msg += "本轮可处理：" + strings.Join(names, "、")
		}
		return fail(msg)
	}
	return a, nil
}

// readAttachment loads one resolved attachment's bytes, capped.
func readAttachment(ctx context.Context, tc contract.ToolContext, a contract.Attachment, toolName string, max int64) ([]byte, *contract.ToolResult) {
	body, err := tc.Attachments.Read(ctx, a, max)
	if err != nil {
		r := contract.ErrResult(toolName + ": 读不到文件：" + err.Error())
		return nil, &r
	}
	return body, nil
}

// pickMedia is the VISION legs' resolution: resolveAttachment plus the magic-byte sniff
// those backends need. maxVideo == 0 means this leg does not accept video at all (the
// read leg), and the refusal happens BEFORE the read so a clip is never pulled into
// memory just to be rejected.
func pickMedia(ctx context.Context, tc contract.ToolContext, arg, toolName string, maxImage, maxVideo int64) ([]byte, string, *contract.ToolResult) {
	// Resolve with the WIDE appetite even on the image-only leg, then refuse by kind
	// below. Narrowing the appetite here looks equivalent and is not: Pick's no-hint
	// fallback matches against the appetite, so a turn holding only a video would find
	// nothing and be told 「本轮消息没有图片附件，请让用户重新发送」 — false, about a file
	// that is right there, and unactionable. Resolve first, then say the useful thing.
	//
	refuseVideo := maxVideo == 0
	a, errRes := resolveAttachment(ctx, tc, arg, toolName, "图片或视频", visionCandidate)
	if errRes != nil {
		return nil, "", errRes
	}
	if refuseVideo && a.IsVideoContent() {
		name := a.Path
		if name == "" {
			name = a.FileName
		}
		r := contract.ErrResult(fmt.Sprintf("%s: %q 是视频，这一步只处理图片。看视频内容请用 task=answer。", toolName, name))
		return nil, "", &r
	}
	limit := maxImage
	if a.IsVideoContent() {
		limit = maxVideo
	}
	body, errRes := readAttachment(ctx, tc, a, toolName, limit)
	if errRes != nil {
		return nil, "", errRes
	}
	// Image sniff FIRST: ISO-BMFF stamps both HEIC and MP4 with "ftyp", and only the
	// image sniffer knows the HEIF brand list. Reversing this order sends iPhone photos
	// to the frame extractor.
	if mime := detectImageMIME(body); mime != "" {
		return body, mime, nil
	}
	if maxVideo > 0 {
		if mime := detectVideoMIME(body); mime != "" {
			return body, mime, nil
		}
		r := contract.ErrResult(toolName + ": 认不出这个文件的格式（图片请用 PNG / JPEG / WebP / BMP / HEIC，视频请用 MP4 / MOV / WebM）")
		return nil, "", &r
	}
	r := contract.ErrResult(toolName + ": not a recognised image (use PNG / JPEG / WebP / BMP / HEIC)")
	return nil, "", &r
}

// attPaths lists attachments by their space-relative Path — the "inbox/<name>" form the
// prompt's attachment list shows — so an error names files the way the model saw them.
func attPaths(atts []contract.Attachment) []string {
	var out []string
	for _, a := range atts {
		if a.Path != "" {
			out = append(out, a.Path)
		}
	}
	return out
}

const visionParams = `{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "要处理的图片或视频 —— 照抄本轮附件列表里给出的路径（如 inbox/photo.jpg、inbox/video.mp4）；只写文件名也行。本轮没有附件时，请用户重新发送。read 支持 PNG / JPEG / WebP / BMP / HEIC（iPhone 照片直接传，无需自己转格式）；answer 支持这些图片格式加 MP4 / MOV / WebM 视频；reverse_prompt 支持 PNG / JPEG / WebP。"
    },
    "task": {
      "type": "string",
      "enum": ["read", "answer", "reverse_prompt"],
      "description": "要做的事。read = 走专门的 OCR 后端逐字转写图里的文字（数字 / 订单号 / 验证码 / 报告正文这类要一字不差的，用它）。answer = 让视觉模型看图或看视频并回答 question（问这是什么、什么意思、看画面判断、问视频里发生了什么，用它）。reverse_prompt = 产出一段可复现这张图的文生图提示词（用户原话明确要提示词 / 反推时才用）。"
    },
    "question": {
      "type": "string",
      "description": "task=answer 必填：要问这张图或这段视频的问题 —— 把用户这轮的问题原样带进来（如 \"这个视频内容是什么\"）。问题需要上下文才完整时（如 \"那第三行是什么\"），补成一个不依赖上下文也读得懂的问句，但别改动他要问的东西。"
    },
    "prompt": {
      "type": "string",
      "description": "可选，覆盖该 task 的默认指令。task=read 时是转写指令（例如只转某一区域 / 忽略界面噪音 / 要求某种格式），领域化转写用它传具体指令；task=reverse_prompt 时是自定义看图指令（例如 \"重点描述建筑风格\" / \"输出 Midjourney 格式带 --ar\"）。"
    },
    "time_start": {
      "type": "number",
      "description": "task=answer 且是视频时可选：从第几秒开始看（默认从视频开头）。已经知道要看的事发生在后半段时用它，例如 time_start=40。"
    },
    "time_window": {
      "type": "number",
      "description": "task=answer 且是视频时可选：看多长一段（秒，默认整段视频均匀铺开）。缩小到某一段会让这段的时间分辨率更高，适合看清一个瞬间发生了什么，例如 time_start=40 配 time_window=20 只看第 40 到 60 秒。"
    },
    "lang": {
      "type": "string",
      "enum": ["auto", "ch", "en", "ja", "ko"],
      "description": "task=read：OCR 语言。默认 auto（中英多语，覆盖多数场景）。日文 / 韩文要明确指定 ja / ko。"
    },
    "bbox": {
      "type": "array",
      "description": "task=read：可选裁剪框 [x1, y1, x2, y2]（像素坐标，先裁剪再识别）。不填则整图扫。密集表格 / 局部识别时用。",
      "items": {"type": "integer"},
      "minItems": 4,
      "maxItems": 4
    },
    "include_lines": {
      "type": "boolean",
      "description": "task=read：是否返回每行的 bbox + confidence（默认 false，只返回合并文字）。按位置定位 / 处理表格 / 看识别置信度时设 true。"
    },
    "enhance": {
      "type": "string",
      "enum": ["gray"],
      "description": "task=read：识别前的图像增强。\"gray\" = 灰度 + 对比度拉伸，适合深色底、文字有多种颜色的界面截图（如医疗仪器屏幕）；彩色照片或颜色本身有含义时不要开。不填 = 原图直接识别。"
    }
  },
  "required": ["task"]
}`

// visionArgs is the merged argument set. Fields are per-task (see visionParams); a
// task simply ignores the ones that aren't its own.
type visionArgs struct {
	FilePath     string  `json:"file_path"`
	Task         string  `json:"task"`
	Question     string  `json:"question,omitempty"`
	Prompt       string  `json:"prompt,omitempty"`
	TimeStart    float64 `json:"time_start,omitempty"`
	TimeWindow   float64 `json:"time_window,omitempty"`
	Lang         string  `json:"lang,omitempty"`
	BBox         []int   `json:"bbox,omitempty"`
	IncludeLines bool    `json:"include_lines,omitempty"`
	Enhance      string  `json:"enhance,omitempty"`
}

// visionTool is the SINGLE surface for everything done with a picture or a clip the
// user sent: transcribe it (task=read, the Kind-bound OCR backend), answer a question
// about it (task=answer, a vision-tagged llm, over one still or a video's frames), or
// reverse it into a generation prompt (task=reverse_prompt). One tool, three tasks,
// two backend legs — readImageText in 96-ocr.go and askVision in 97-vlm.go — plus the
// frame sampler in 98-video.go.
//
// MERGED, from `ocr` + `vision` (+ a short-lived third tool). Two reasons,
// in order of weight:
//
//  1. The seam between narrow tools is where requests fell through. `ocr` advertised
//     "the text", `vision` advertised "a generation prompt", and "这个图片意思是什么"
//     matched neither — so the model called nothing and described the picture from
//     its FILENAME. A single tool covering all three jobs has no seam to fall
//     through; adding a fourth tool would only have moved the seam.
//  2. Tool specs are paid on EVERY request, image or not. The separate image tools
//     were 21% of the whole tool budget (2269 of 10776 bytes measured on the wire),
//     three of them would have been ~27%, and most of it was the same image_path
//     schema and the same boilerplate written three times.
//
// RENAMED `image` → `vision`, when video joined it. The tool's identity is
// "看东西", not "图片": a clip goes through the same three questions a photo does, and
// the sibling that DRAWS is called image_create — `vision` / `image_create` is the
// unambiguous pair `image` / `image_create` never was. Video arrives as frames rather
// than as a clip on the wire; that choice was measured, see 98-video.go.
//
// The cost accepted for it: one warrant capability now covers transcription AND
// vision (they can no longer be granted apart), and the parameter set is a union
// whose fields are task-specific. The bet is that choosing an enum inside an
// unambiguous tool is easier for a model than choosing between three adjacent
// tools — which is exactly what to watch after it ships.
type visionTool struct {
	ocr  kindChat                  // bound to contract.KindOCR — the read leg
	pool func() contract.ModelPool // the vision legs (Requires:["vision"])
}

func (visionTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "vision",
		Description: "看用户发来的图片或视频：task=read 走专门的 OCR 后端逐字转写图里的文字；" +
			"task=answer 让视觉模型看画面并回答你给的问题（视频会按时间顺序抽帧后一起看）；" +
			"task=reverse_prompt 产出可复现该图的文生图提示词。",
		Parameters:     json.RawMessage(visionParams),
		NoAutoCompress: true, // transcripts and grounded image answers are originals, not summarizable
		// EndsTurn stays FALSE even though reverse_prompt delivers verbatim: the flag
		// only decides whether a DELEGATED sub-turn may hold the tool (95-subloop.go),
		// and transcription must stay available there as it always was. The verbatim
		// delivery is a per-CALL ExitRequest below, not a static property of the tool.
		SelectionHint: &contract.SelectionHint{
			// One scenario for all three jobs — the three-way "which tool" decision that
			// used to live out here is now a `task` argument, chosen after the model has
			// already committed to looking at the picture.
			When:     `本轮(或之前)有图片或视频附件，而你需要用到画面里的内容 —— 读里面的字、回答关于这张图/这段视频的问题、或产出它的生成提示词。文件只是顺带发来的(让你存一下、转发、发群里)则不适用`,
			Then:     `vision 选 task：要一字不差的文字用 read(专门 OCR 后端，数字/订单号/验证码更可靠，并提醒用户复核；该领域有专门的识别技能就先用技能，read 是通用兜底)；要回答关于画面的问题用 answer 并把用户的问题原样放进 question(视频也走 answer，想细看某一段就带 time_start/time_window)；用户原话明确要提示词/反推才用 reverse_prompt`,
			Priority: 20,
		},
	}
}

func (visionTool) Serialize() bool { return false }

func (t visionTool) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var p visionArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return contract.ErrResult("vision: invalid arguments: " + err.Error())
	}
	// Normalise before dispatching: a model carrying habits from the two tools this
	// one replaced reaches for "OCR" or " read ", and burning a whole round on an
	// unrecognised-task error teaches it nothing the enum did not already say.
	switch strings.ToLower(strings.TrimSpace(p.Task)) {
	case "read":
		if bad := p.strayFields("read"); bad != "" {
			return contract.ErrResult("vision: task=read 用不到 " + bad + " —— 去掉它们，或换一个用得上的 task")
		}
		return readImageText(ctx, tc, t.ocr, p)

	case "answer":
		if bad := p.strayFields("answer"); bad != "" {
			return contract.ErrResult("vision: task=answer 用不到 " + bad + " —— 想裁剪或做定向转写请用 task=read")
		}
		question := strings.TrimSpace(p.Question)
		if question == "" {
			return contract.ErrResult("vision: task=answer 需要 question —— 把用户这轮要问这张图或这段视频的问题带进来")
		}
		stills, timeline, errRes := visionStills(ctx, tc, p.FilePath, true, p.TimeStart, p.TimeWindow, "vision")
		if errRes != nil {
			return *errRes
		}
		// The timeline line rides on the INSTRUCTION, not on a separate message: the
		// frames are a sequence, and a model told only "here are 30 pictures" loses the
		// before/after that is the whole reason a video was sampled rather than sampled once.
		answer, errRes := askVision(ctx, tc, t.pool, stills, question+timeline, "vision")
		if errRes != nil {
			return *errRes
		}
		// Plain result: the answer is EVIDENCE. The main model still writes the reply
		// around it, possibly alongside other tools — deliberately unlike reverse_prompt
		// below. Routing an ordinary image question through that verbatim door would
		// deliver the vision model's raw words INSTEAD of an answer.
		return contract.OKResult(answer)

	case "reverse_prompt":
		if bad := p.strayFields("reverse_prompt"); bad != "" {
			return contract.ErrResult("vision: task=reverse_prompt 用不到 " + bad)
		}
		instruction := reversePromptInstruction
		if p.Prompt != "" {
			instruction = p.Prompt
		}
		// allowVideo=false: the deliverable is a TEXT-TO-IMAGE prompt handed to the user
		// verbatim, and a prompt distilled from 30 stills of a moving scene reproduces
		// none of them. pickMedia refuses the clip by name before reading a byte.
		stills, _, errRes := visionStills(ctx, tc, p.FilePath, false, 0, 0, "vision")
		if errRes != nil {
			return *errRes
		}
		answer, errRes := askVision(ctx, tc, t.pool, stills, instruction, "vision")
		if errRes != nil {
			return *errRes
		}
		// The prompt IS the deliverable: hand it to the user verbatim via ExitRequest so
		// the main model never paraphrases, translates or truncates it.
		return contract.ToolResult{OK: true, Data: answer, ExitRequest: &contract.ToolExit{Reply: answer}}

	default:
		return contract.ErrResult(fmt.Sprintf("vision: task %q 不支持(用 read / answer / reverse_prompt)", p.Task))
	}
}

// strayFields names the arguments that belong to a DIFFERENT task, so a mismatch is
// refused instead of silently dropped. The costly case is task=answer carrying a bbox:
// the crop is ignored, the whole image goes to the vision model, and the answer comes
// back sounding exactly as confident as a correct one — the model has no way to notice
// it asked about a region and was answered about the picture. A union parameter set is
// the price of one tool; making the union LOUD is what keeps that price bounded.
//
// time_start / time_window belong to task=answer alone for the same reason: silently
// ignoring a time window on a reverse_prompt would answer about a moment the caller
// never got to choose.
func (p visionArgs) strayFields(task string) string {
	var bad []string
	add := func(cond bool, name string) {
		if cond {
			bad = append(bad, name)
		}
	}
	readOnly := func() {
		add(p.Lang != "", "lang")
		add(len(p.BBox) > 0, "bbox")
		add(p.IncludeLines, "include_lines")
		add(p.Enhance != "", "enhance")
	}
	window := func() {
		add(p.TimeStart != 0, "time_start")
		add(p.TimeWindow != 0, "time_window")
	}
	switch task {
	case "read":
		add(p.Question != "", "question")
		window()
	case "answer":
		readOnly()
		add(p.Prompt != "", "prompt") // the question IS the instruction here
	case "reverse_prompt":
		readOnly()
		window()
		add(p.Question != "", "question")
	}
	return strings.Join(bad, " / ")
}
