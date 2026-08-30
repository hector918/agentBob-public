package imagecreate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // register jpeg for image.DecodeConfig (init-image orientation)
	_ "image/png"  // register png for image.DecodeConfig
	"log/slog"
	"path"
	"strings"
	"sync"

	"agentbob/contract"
	"agentbob/heartwood/clock"
	"agentbob/trunk"
)

// maxInitImageBytes bounds the source picture read for img2img. The backend
// rescales it anyway (the workflow's scale node), so a huge original buys
// nothing and only risks memory.
const maxInitImageBytes = 24 << 20

// spaceDir is the folder produced pictures land in, inside the turn's space. It
// matches the tool name on purpose: the receipt hands this relative path to the
// model, which may pass it on, and a path that disagrees with the tool that wrote
// it is a small puzzle handed to whoever reads it later.
const spaceDir = "image_create"

// createParams is deliberately small. A tool spec is paid on EVERY request — image
// or not — and leaf/tools/95-image.go:140 records what happened last time the
// image surface grew: three tools ate 21% of the whole tool budget. Everything
// that varies per BACKEND (prompt dialect, tag syntax, tier trade-offs) is pulled
// on demand from the guides instead of carried here (docs/image-create-tool.md §4.2).
const createParams = `{
  "type": "object",
  "properties": {
    "prompt": {
      "type": "string",
      "description": "要画什么 —— 写**最终画面**长什么样，用英文。改图时（填了 init_image）尤其重要：原图里要保留的东西（人物、服装、姿势、背景）必须一起写进来，只写「改成某某风格」会连画面内容一起改掉。"
    },
    "style": {
      "type": "string",
      "description": "画风。留空 = 返回当前可用的风格清单（含各自耗时），先看清单再选。"
    },
    "aspect": {
      "type": "string",
      "enum": ["square", "portrait", "landscape"],
      "description": "画面方向，默认 square。竖构图（手机壁纸、人像）用 portrait，横构图（桌面壁纸、风景）用 landscape。改图时不用填：方向按原图定，填了也不算数。具体像素由引擎按自己的安全上限决定，不用也不能指定。"
    },
    "init_image": {
      "type": "string",
      "description": "要改的那张图 —— 照抄本轮附件列表里给出的路径（如 inbox/xxx.jpg）；只写文件名也行。填了就是在原图基础上改，不填就是从零画。"
    },
    "change": {
      "type": "string",
      "enum": ["slight", "moderate", "heavy"],
      "description": "配合 init_image：改动幅度。slight = 只调气氛/光线，构图人物基本不动；moderate = 明显改动，大结构还在；heavy = 只借个大概轮廓重画。默认 moderate。注意：结果像不像原图，主要看 prompt 有没有把原图内容写全，这个档位只决定改动幅度。"
    }
  }
}`

type createArgs struct {
	Prompt    string `json:"prompt"`
	Style     string `json:"style"`
	Aspect    string `json:"aspect"`
	InitImage string `json:"init_image"`
	Change    string `json:"change"`
}

// Tool is the `image_create` tool. Exported because the owning module also registers
// its recovery sweep on the Housekeeper (HousekeepingTask), which a bare
// contract.Tool cannot express.
type Tool struct {
	pool func() contract.ModelPool
	cat  func() contract.ImageCatalog
	gw   func() contract.Gateway
	home string
	wal  *wal
}

// New builds the image_create tool. pool and gw are lazy for the usual reason — both are
// Optional modules that may start after tools.
func New(pool func() contract.ModelPool, cat func() contract.ImageCatalog, gw func() contract.Gateway, home string) *Tool {
	return &Tool{pool: pool, cat: cat, gw: gw, home: home, wal: newWAL(home)}
}

// HousekeepingTask is the WAL recovery sweep, for the module to register on the
// trunk Housekeeper. Persistent state → the shared scheduler, never a private
// timer (docs/image-create-tool.md §9.5).
func (t *Tool) HousekeepingTask() trunk.Task {
	return trunk.Task{
		Name:     "image_create.recover-inflight",
		Period:   walSweepPeriod,
		Priority: 50,
		Run:      t.sweep,
	}
}

func (t *Tool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "image_create",
		Description: "生成图片：按文字描述画一张新图，或在用户发来的图上改。提示词一律写英文。" +
			"style 留空 → 返回当前可用的画风清单；只填 style 不填 prompt → 返回那个画风的提示词写法说明。",
		Parameters: json.RawMessage(createParams),
		// A produced picture is production, and a failed send is something the model
		// might otherwise claim as done — the same pair deliver_file carries.
		Delivers:   true,
		SideEffect: true,
		// The result line is a receipt, not prose: compacting it would lose the
		// filename the user may refer to later.
		NoAutoCompress: true,
		SelectionHint: &contract.SelectionHint{
			// This is the ONLY always-resident signpost for the tool: everything about
			// HOW to drive an engine is deferred to the guides, so `When` carries the
			// whole "should I reach for this" decision on its own (§4.5).
			When: `用户想要一张图 —— 让你画/生成/做一张图，或者让你把他发来的图改成别的样子（换背景、换风格、改成夜景之类）。` +
				`只是要看懂或读出图里的内容则不适用`,
			Then: `image_create：第一次用先不填 style 调一次拿到可用画风清单；` +
				`要讲究画质就再只填 style 拿到那个引擎的写法说明，照它写 prompt（一律英文）；` +
				`改图时先用 image（task=answer）看一眼原图，再把附件路径填进 init_image、用 change 说明改多狠，` +
				`并把原图里要保留的内容写进 prompt`,
			Priority: 20,
		},
	}
}

// Serialize keeps two draw calls in one round from stacking: the backends run one
// job at a time anyway, so a parallel pair only turns one wait into two.
func (t *Tool) Serialize() bool { return true }

func (t *Tool) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var a createArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return contract.ErrResult("image_create: 参数解析失败：" + err.Error())
	}
	a.Prompt = strings.TrimSpace(a.Prompt)
	a.Style = strings.TrimSpace(a.Style)
	a.Aspect = strings.TrimSpace(a.Aspect)
	a.Change = strings.TrimSpace(a.Change)

	// A change without an image is a parameter that would be silently dropped —
	// and a dropped parameter reads exactly like an honoured one in the result.
	// Same lesson as image's strayFields (leaf/tools/95-image.go:242).
	if a.Change != "" && strings.TrimSpace(a.InitImage) == "" {
		return contract.ErrResult("image_create: change 只在改图时有意义 —— 要改图请同时填 init_image；从零画一张就把 change 去掉")
	}

	pool, cat := t.pool(), t.cat()
	caps := liveCapabilities(pool, cat)
	if len(caps) == 0 {
		if bare := undocumentedStyles(pool, cat); len(bare) > 0 {
			slog.Warn("image_create: live image tags have no guide", "tags", bare)
		}
		return contract.ErrResult("image_create: 现在没有可用的生图后端")
	}

	// Level 1 — no style: the catalog. Cheap, and the natural first move for a
	// model that has never seen the list (the parameter is optional precisely so
	// discovery happens by itself, without inventing a separate verb).
	if a.Style == "" {
		return contract.OKResult(renderCatalog(caps))
	}
	cap, ok := find(caps, a.Style)
	if !ok {
		return contract.ErrResult(fmt.Sprintf("image_create: 没有 %q 这个画风。当前可用：%s", a.Style, styleNames(caps)))
	}

	// Level 2 — style but no prompt: that backend's full manual. Optional by
	// design: a casual request goes straight to generating, and only a caller that
	// wants to get the prompt right pays this round (§4.2).
	if a.Prompt == "" {
		body, ok := t.cat().ImageGuide(cap.Style)
		if !ok {
			return contract.ErrResult(fmt.Sprintf("image_create: %q 还没有提示词说明", cap.Style))
		}
		return contract.OKResult(body)
	}

	// Level 3 — generate.
	return t.generate(ctx, tc, a, cap)
}

func (t *Tool) generate(ctx context.Context, tc contract.ToolContext, a createArgs, cap capability) contract.ToolResult {
	if tc.Sink == nil {
		return contract.ErrResult("image_create: 当前没有可用的发送通道（画好了也发不出去）")
	}
	if tc.Channels == nil {
		return contract.ErrResult("image_create: 当前没有可用的工作空间（画好了没处存）")
	}

	msg := contract.Message{Role: "user"}
	fromSource := false // the shape was taken from the source picture, not the caller
	if strings.TrimSpace(a.InitImage) != "" {
		img, errRes := t.initImage(ctx, tc, a.InitImage)
		if errRes != nil {
			return *errRes
		}
		msg.Images = []contract.ImageRef{img}
		if a.Change == "" {
			a.Change = "moderate"
		}
		// The SOURCE decides the shape when editing, overriding whatever the caller
		// asked for. Not a preference — the backend scales the init image with a
		// centre crop, so a portrait photo edited at "square" comes back with its top
		// and bottom cut off, and the receipt would report that square as if it were
		// what the user got. The model cannot make this call either: the attachment
		// list it reads names files and nothing else (flow/compose describeAttachments),
		// so any aspect it fills in for an edit is a guess (docs/image-create-tool.md §4.7).
		if shape, ok := aspectOf(img.Data); ok {
			a.Aspect, fromSource = shape, true
		}
	}
	body, err := json.Marshal(map[string]any{
		"style":  cap.Style,
		"prompt": a.Prompt,
		"aspect": a.Aspect,
		"change": a.Change,
	})
	if err != nil {
		return contract.ErrResult("image_create: 内部错误：" + err.Error())
	}
	msg.Content = string(body)

	// The WAL record is written from inside the progress hook, on the submitted
	// event — i.e. the moment the job exists and before any image can. Doing it
	// here, around the call, would be too late to survive a crash mid-generation.
	// A SLICE, not one id: pool.Chat may run the backend call more than once for a
	// single tool call — withBusyRetry re-submits to the same entry on a transient
	// error (a 502/503/504 from the proxy in front of the engine counts), and
	// failover can hand the request to a peer. Each attempt writes its own record,
	// so keeping only the last id would leave the earlier ones on disk for the sweep
	// to deliver as duplicate images minutes later.
	// Guarded: the hook is invoked from the provider's goroutines — progress events
	// arrive on the websocket reader while submitted/fetching come from the calling
	// one (contract.WithImageProgress).
	var mu sync.Mutex
	var promptIDs []string
	ctx = contract.WithImageProgress(ctx, func(ev contract.ImageEvent) {
		switch ev.Stage {
		case contract.ImageStageSubmitted:
			mu.Lock()
			promptIDs = append(promptIDs, ev.PromptID)
			mu.Unlock()
			t.wal.claim(walRecord{
				PromptID: ev.PromptID,
				Entry:    ev.Entry,
				Scope:    tc.Scope,
				Sid:      tc.Sid,
				Style:    cap.Style,
				Prompt:   a.Prompt,
				Created:  clock.Now().Unix(),
			})
			tc.Sink.TraceDelta("已提交生成任务，正在处理提示词…")
		case contract.ImageStageProgress:
			// Cur/Max are per-phase (sampling steps, then decode tiles), so this
			// reports a fraction of the CURRENT phase and never pretends to be one
			// continuous bar (contract.ImageStageProgress).
			tc.Sink.TraceDelta(fmt.Sprintf("生成中 %d/%d…", ev.Cur, ev.Max))
		case contract.ImageStageFetching:
			tc.Sink.TraceDelta("生成完成，正在取回图片…")
		}
	})

	resp, err := t.pool().Chat(ctx, contract.ModelRequest{
		Kind:        contract.KindImage,
		Requires:    []string{cap.Style},
		AffinityKey: tc.Sid,
	}, []contract.Message{msg})
	if err != nil {
		// A cancelled turn drops the record: the user stopped waiting, so an image
		// arriving minutes later out of nowhere would be worse than none (§9.5).
		// Any OTHER failure also drops it — the job is already known to have failed,
		// so there is nothing for recovery to finish.
		t.dropAll(snapshotIDs(&mu, &promptIDs))
		return contract.ErrResult("image_create: 没画成 —— " + err.Error())
	}

	var reply struct {
		ImageB64 string `json:"image_b64"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		Seconds  int    `json:"seconds"`
		Seed     int64  `json:"seed"`
	}
	if err := json.Unmarshal([]byte(resp.Content), &reply); err != nil || reply.ImageB64 == "" {
		t.dropAll(snapshotIDs(&mu, &promptIDs))
		return contract.ErrResult("image_create: 后端没有返回图片")
	}
	raw, err := base64.StdEncoding.DecodeString(reply.ImageB64)
	if err != nil || len(raw) == 0 {
		t.dropAll(snapshotIDs(&mu, &promptIDs))
		return contract.ErrResult("image_create: 返回的图片无法解码")
	}

	rel, abs, err := t.save(ctx, tc, cap.Style, raw)
	if err != nil {
		t.dropAll(snapshotIDs(&mu, &promptIDs))
		return contract.ErrResult("image_create: 图存不进空间：" + err.Error())
	}
	// As a PICTURE where the channel distinguishes the two: this is something to
	// look at, and the untouched original stays in the space for anyone who wants
	// it. deliver_file deliberately does NOT do this — a file someone asked for by
	// name must arrive byte-for-byte (contract.PhotoSender).
	//
	// WHEN it goes out is the sink's call, not this tool's (contract.SinkPictureHolder).
	// So the record is NOT retired here — the delivery retires it (releaseOnDelivery),
	// and a crash in between leaves an owned-by-nobody record for the sweep, which is
	// exactly the case the log exists for.
	//
	// A sink that sent it right away already knows the answer, and a failed send is
	// something the model would otherwise claim as done — the failure this tool's
	// Delivers flag exists to surface. Report it while it is still knowable; a HELD
	// picture has no outcome yet, so there is nothing to report but the holding.
	held, err := contract.DeliverPictureWhenReady(tc.Sink, abs, "", t.releaseOnDelivery(snapshotIDs(&mu, &promptIDs)))
	if !held && err != nil {
		// The file IS in the space, so say where — the user can still be pointed at
		// it even though the attachment did not go out.
		return contract.ErrResult(fmt.Sprintf("image_create: 图已生成并存为 %s，但发送失败：%v", rel, err))
	}

	// The model gets a receipt, never the bytes: putting the image (or its base64)
	// in the tool result would push a megabyte through the transcript for nothing
	// (§9.3).
	//
	// The receipt states what was actually USED, not what was asked for: a follow-up
	// like "改狠一点" can only be answered against a known starting point, and the
	// edit strength is a value the caller may never have set (it defaults). The seed
	// carries its own caveat — printing a number that cannot be fed back would be an
	// invitation to promise the user a reproduction this tool cannot do yet (§4.7).
	//
	// It no longer claims the picture has ARRIVED: whether it already went out or is
	// waiting for the turn to end is the sink's business, and the model repeats this
	// line's claims to the user. What it says instead is true on every path — sending
	// is handled — and it keeps the instruction that made the old wording worth
	// having: do not send it again. That double-send is what this receipt is the only
	// guard against.
	//
	// The closing half tells the model what to SAY, because the old "just say it's
	// done" produced four-word deliveries that answered nothing — measured across ten
	// real deliveries, that wording and its two silences caused every rejected one:
	// a reply that never names the ask, a second request in the same message left
	// unanswered while the picture got drawn, and — worst, because it reads as
	// diligence — the model vouching for details it cannot have checked.
	//
	// One reason carries both prohibitions instead of two rules to memorise: it has
	// not seen the picture. That is also why "don't describe it" survives from the
	// old wording — a model asked to describe an image it only knows by filename
	// invents one.
	return contract.OKResult(fmt.Sprintf(
		"已生成：%s（%s，%d×%d，seed %d〈暂不能指定〉，耗时 %d 秒）。%s"+
			"图片已交给发送通道，你不用再发一次。回复里说清这张是按什么要求画的，"+
			"以及用户一并要求的其它事做了没有；画面本身别描述、也别声称哪些细节已经做到"+
			"——你没看过这张图，只有用户看得见。",
		rel, cap.Style, reply.Width, reply.Height, reply.Seed, reply.Seconds, editNote(a, fromSource)))
}

// editNote is the edit half of the receipt: the strength actually used, and — when
// the source picture decided it — the shape. Empty for a from-scratch picture.
func editNote(a createArgs, fromSource bool) string {
	if strings.TrimSpace(a.InitImage) == "" {
		return ""
	}
	if fromSource {
		return fmt.Sprintf("改动幅度 change=%s，方向按原图取的 %s（改图时不看 aspect）。", a.Change, a.Aspect)
	}
	// Undecodable source: the caller's aspect stood, so do not claim otherwise —
	// a receipt that says "按原图" when it isn't is worse than one that says nothing.
	return fmt.Sprintf("改动幅度 change=%s。", a.Change)
}

// aspectOf reads a picture's own proportions and maps them onto the aspect keys.
//
// Only the HEADER is decoded (image.DecodeConfig), so this costs nothing on bytes
// that are already in memory — same trick, and the same pair of blank imports, as
// the ocr tool's bbox guard (leaf/tools/96-ocr.go).
//
// ok=false for anything the standard decoders do not recognise — WebP and HEIC
// among them, and an iPhone shares HEIC by default. That path deliberately leaves
// the caller's aspect alone rather than guessing: a wrong shape crops the user's
// picture, and this function existing is not a reason to be confident about one.
func aspectOf(data []byte) (string, bool) {
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

// initImage resolves the model's init_image reference to bytes. The picking rules
// (which attachment, the ambiguity messages, the inbox fallback) are the image
// tool's, reused rather than reimplemented so "which picture did you mean" cannot
// answer differently on the two sides.
func (t *Tool) initImage(ctx context.Context, tc contract.ToolContext, ref string) (contract.ImageRef, *contract.ToolResult) {
	if tc.Attachments == nil {
		r := contract.ErrResult("image_create: 当前没有可用的文件空间（读不到要改的图）")
		return contract.ImageRef{}, &r
	}
	picks := tc.Attachments.Pick(ctx, contract.Attachment.IsImageContent, ref)
	switch {
	case len(picks) == 0:
		msg := "image_create: 没找到要改的那张图。"
		if recent := tc.Attachments.Suggest(ctx, contract.Attachment.IsImageContent); len(recent) > 0 {
			var names []string
			for _, a := range recent {
				if a.Path != "" {
					names = append(names, a.Path)
				}
			}
			msg += "最近收到的图片：" + strings.Join(names, "、") + " —— 在 init_image 写出文件名即可。"
		} else {
			msg += "请让用户把要改的图直接发来。"
		}
		r := contract.ErrResult(msg)
		return contract.ImageRef{}, &r
	case len(picks) > 1:
		var names []string
		for _, a := range picks {
			names = append(names, a.Path)
		}
		r := contract.ErrResult("image_create: 本轮有多张图片：" + strings.Join(names, "、") + " —— 在 init_image 里写出要改的那张。")
		return contract.ImageRef{}, &r
	}
	a := picks[0]
	if a.Path == "" {
		r := contract.ErrResult(fmt.Sprintf("image_create: %q 还没准备好（未下载或未入空间），请让用户重新发送。", ref))
		return contract.ImageRef{}, &r
	}
	data, err := tc.Attachments.Read(ctx, a, maxInitImageBytes)
	if err != nil {
		r := contract.ErrResult("image_create: 读不到要改的图：" + err.Error())
		return contract.ImageRef{}, &r
	}
	return contract.ImageRef{Data: data, MIME: a.MIME}, nil
}

// save writes the produced image into the turn's default space and returns both
// the space-relative name (for the model / the user) and the absolute path (for
// the sink's one-off file send).
//
// The name is built here, from the clock and the style — never from the prompt or
// any other model-supplied string. A generated filename is a path, and a path
// built from model output is an escape waiting to happen.
func (t *Tool) save(ctx context.Context, tc contract.ToolContext, style string, raw []byte) (string, string, error) {
	ch, err := tc.Channels.OpenFile(ctx, "")
	if err != nil {
		return "", "", err
	}
	defer ch.Close()
	if err := ch.Mkdir(ctx, spaceDir); err != nil {
		return "", "", err
	}
	rel := path.Join(spaceDir, fmt.Sprintf("%s-%s.png", clock.Now().Format("20060102-150405"), safeName(style)))
	if err := ch.Write(ctx, rel, raw); err != nil {
		return "", "", err
	}
	abs, err := ch.AbsPath(rel)
	if err != nil {
		return "", "", err
	}
	return rel, abs, nil
}

// dropAll forgets every record this call created — for the paths where the job is
// known to have FAILED (no backend reply, undecodable bytes, nowhere to save it).
// There is nothing for the recovery sweep to finish, and a leftover record would
// surface later as a bogus "it didn't make it".
//
// The success path no longer ends here: a generated picture is handed to the sink
// and held for the turn's acceptance gate, so it is not the user's yet when this
// call returns. Its record is released by the delivery instead (releaseOnDelivery).
func (t *Tool) dropAll(ids []string) {
	for _, id := range ids {
		t.wal.drop(id)
	}
}

// releaseOnDelivery is the callback the sink runs once it has actually tried to send
// a held picture. Either way the record is retired: the recovery sweep re-fetches
// from the BACKEND, and a picture that reached the save step has its bytes on disk
// already — the one thing the sweep can do for it is the wrong thing. Keeping the
// record would buy hours of pointless probing (worst on a channel whose sink always
// refuses attachments, where every picture would take that path) and can end in the
// sweep telling the user 「没画成」 about a picture that was drawn and saved.
//
// So a failed send is a log line, not a retry. The bytes are in the space and the
// receipt named the file, which is what a follow-up has to work with.
func (t *Tool) releaseOnDelivery(ids []string) func(error) {
	return func(err error) {
		for _, id := range ids {
			t.wal.drop(id)
		}
		if err != nil {
			slog.Warn("image_create: generated picture could not be sent", "err", err)
		}
	}
}

// snapshotIDs copies the ids recorded so far under the hook's lock.
func snapshotIDs(mu *sync.Mutex, ids *[]string) []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), (*ids)...)
}
