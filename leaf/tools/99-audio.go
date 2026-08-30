package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
)

const (
	// audioMaxBytes caps the clip read into memory before ffmpeg sees it — the same
	// budget the video leg uses, and for the same reason: bytes are held in memory AND
	// written to a temp file, on the box that also hosts the model pool.
	audioMaxBytes int64 = 64 * 1024 * 1024

	// audioMaxWindow is the most audio ONE call transcribes (5 minutes).
	//
	// This is the segmentation strategy, and it is deliberately not an engine. The
	// backend handles far longer in a single pass, but a single pass over an hour is
	// ~50 minutes of wall clock — too long to hold a tool call open, and the wrong shape
	// besides: nobody wants an hour of transcript in one lump. Capping the window and
	// REPORTING the remaining range makes the model itself the segmenter — it calls
	// again with the next range if it still needs more, and stops when it has what it
	// came for. A cut-into-chunks-and-stitch engine would have been ~120 lines that
	// always transcribes everything, including the 50 minutes nobody asked about.
	audioMaxWindow = 5 * 60.0

	// audioTimeoutFixed / audioTimeoutPerSecond size THIS call's deadline from the window,
	// against the deployed backend's measured cost (~12 s fixed + ~0.83x realtime at the
	// worst measured point) with room to spare.
	//
	// Having our own deadline is not politeness, it is damage control. Without one the
	// only bound is the provider's streamHardTimeout, and that error is DELIBERATELY
	// counted against entry health (leaf/model/15-errors.go) — so two slow max-window
	// calls would cool the single kind=asr entry and take INGESTION's voice transcription
	// down with them. A context deadline expires as a plain context error instead, which
	// costs this one call and nothing else. The ingestion path has always had its own
	// (transcribeTimeout); this one was the gap.
	audioTimeoutFixed     = 90 * time.Second
	audioTimeoutPerSecond = 1200 * time.Millisecond
)

// audioCandidate is the audio tool's APPETITE: anything with a soundtrack.
//
// Video is IN on purpose. Refusing it would leave "这段视频里的人说了什么" with no home —
// a user is never going to demux the audio track before sending, and the seam between
// "audio tool" and "video tool" is exactly the kind of gap requests fall through (the
// lesson the ocr/vision merge already paid for). Cost is zero: ffmpeg selects the audio
// stream out of a container either way.
func audioCandidate(a contract.Attachment) bool {
	return a.IsAudioContent() || a.IsVideoContent()
}

const audioParams = `{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "要转写的音频或视频 —— 照抄本轮附件列表里给出的路径（如 inbox/voice.ogg、inbox/meeting.m4a、inbox/video.mp4）；只写文件名也行。视频会自动取音轨。"
    },
    "time_start": {
      "type": "number",
      "description": "可选：从第几秒开始转（默认从头）。上一次调用告诉你还剩哪一段时，用它接着往下转，例如 time_start=300。"
    },
    "time_window": {
      "type": "number",
      "description": "可选：转多长一段（秒）。默认尽量一次转完，单次最多 300 秒；只想听某一小段时缩小它，例如 time_start=120 配 time_window=60 只转第 120 到 180 秒。"
    }
  },
  "required": ["file_path"]
}`

type audioArgs struct {
	FilePath   string  `json:"file_path"`
	TimeStart  float64 `json:"time_start,omitempty"`
	TimeWindow float64 `json:"time_window,omitempty"`
}

// audioTool transcribes a clip ON DEMAND — the pull half of bob's two-track handling of
// spoken content.
//
// The push half is ingestion (leaf/asr): a short voice note IS the message, so its words
// have to be in the text before the turn can start, and they are. This tool exists for
// everything ingestion declines — anything past its latency budget, which arrives
// announced as 「（时长 4 分 12 秒，未自动转写）」 and stays retrievable instead of lost.
//
// So the division is not "short tool / long tool", it is WHOSE WORDS and WHO IS WAITING:
// the message's own words are paid for up front because the turn needs them; an attached
// recording is paid for only if the model actually needs it, and then in the size it
// asks for.
type audioTool struct {
	pool func() contract.ModelPool
}

func (audioTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "audio",
		Description: "把还没有转写的音频或视频转成文字 —— 附件旁标着「未自动转写」的录音，或长录音里你想单独听的某一段。" +
			"附件旁标着「内容已转写在上文」的不用调，那段话已经在消息里了。可以用 time_start / time_window 指定范围，单次最多转 300 秒。",
		Parameters:     json.RawMessage(audioParams),
		NoAutoCompress: true, // a transcript is an original, not something to summarise away
		SelectionHint: &contract.SelectionHint{
			// The gate belongs in WHEN, not in THEN. Shipped the other way round on
			// and watched it cost a redundant round on the very first real
			// voice note: "有音频附件 + 你需要里面说了什么" matches the already-transcribed
			// case too, and the exclusion sitting in Then is only read once the tool is
			// already a candidate.
			When:     `有音频/视频附件，而它说的话【还没有出现在对话里】—— 附件旁写着「未自动转写」就是这一种;或者你要的是长录音里的某一段。附件旁写着「内容已转写在上文」的，话就在正文里，不适用`,
			Then:     `audio 传 file_path;长录音想分段听、或上一次的结果告诉你还剩哪一段，就用 time_start/time_window 接着转`,
			Priority: 20,
		},
	}
}

func (audioTool) Serialize() bool { return false }

func (t audioTool) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var p audioArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return contract.ErrResult("audio: invalid arguments: " + err.Error())
	}
	if p.TimeStart < 0 || p.TimeWindow < 0 {
		return contract.ErrResult("audio: 时间窗不能为负数")
	}
	a, errRes := resolveAttachment(ctx, tc, p.FilePath, "audio", "音频或视频", audioCandidate)
	if errRes != nil {
		return *errRes
	}
	body, errRes := readAttachment(ctx, tc, a, "audio", audioMaxBytes)
	if errRes != nil {
		return *errRes
	}

	clip, plan, err := audioSegment(ctx, body, p.TimeStart, p.TimeWindow)
	if err != nil {
		return contract.ErrResult("audio: " + err.Error())
	}

	pool := t.pool()
	if pool == nil {
		return contract.ErrResult("audio: 转写后端不可用，请稍后再试").
			WithHint("需要 models.yaml 配一个 kind=asr 的条目")
	}
	actx, cancel := context.WithTimeout(ctx,
		audioTimeoutFixed+time.Duration(plan.Window*float64(audioTimeoutPerSecond)))
	defer cancel()
	resp, err := pool.Chat(actx, contract.ModelRequest{Kind: contract.KindASR}, []contract.Message{{
		Role:  "user",
		Audio: []contract.AudioRef{{Data: clip, MIME: "audio/flac"}},
	}})
	if err != nil {
		if errors.Is(actx.Err(), context.DeadlineExceeded) {
			return contract.ErrResult("audio: 这一段转写超时了 —— 用 time_window 换一段更短的再试")
		}
		return contract.ErrResult("audio: 转写后端暂时不可用，请稍后再试").
			WithHint("需要 models.yaml 配一个 kind=asr 的条目")
	}
	text := strings.TrimSpace(resp.Content)
	if text == "" {
		// Not an error: plenty of real audio has no intelligible speech, and saying so is
		// a useful answer. Returning "" would invite the model to fill the gap itself.
		return contract.OKResult(plan.header() + "（这段没有可辨识的说话内容）")
	}
	return contract.OKResult(plan.header() + text + plan.footer())
}

// audioPlan is one call's slice of a recording.
type audioPlan struct {
	Duration float64
	Start    float64
	Window   float64
	// UnknownLength marks a container that carried no duration. The slice was still cut
	// and transcribed; the header just must not claim a length it never read.
	UnknownLength bool
}

// End is where this slice stops.
func (p audioPlan) End() float64 { return math.Min(p.Start+p.Window, p.Duration) }

// header states which slice of what these words came from. Always present, even for a
// whole short clip: the transcript joins a conversation as ordinary text, and without a
// frame it reads as somebody in the room having said it.
func (p audioPlan) header() string {
	if p.UnknownLength {
		if p.Start == 0 {
			return "[转写]\n"
		}
		return fmt.Sprintf("[转写 · 从 %s 起]\n", humanSeconds(p.Start))
	}
	if p.Start == 0 && p.End() >= p.Duration {
		return fmt.Sprintf("[转写 · 全长 %s]\n", humanSeconds(p.Duration))
	}
	return fmt.Sprintf("[转写 · 第 %s–%s 段，全长 %s]\n",
		humanSeconds(p.Start), humanSeconds(p.End()), humanSeconds(p.Duration))
}

// footer names the range still untranscribed, so continuing is a fact the model reads
// rather than arithmetic it has to do. Empty when the slice reached the end.
func (p audioPlan) footer() string {
	if p.UnknownLength {
		// Cannot say what is left when the total was never known — and guessing would be
		// the one thing worse than saying nothing.
		return ""
	}
	if p.End() >= p.Duration-0.5 {
		return ""
	}
	return fmt.Sprintf("\n\n（还剩第 %s–%s 未转写，需要就用 time_start=%d 继续）",
		humanSeconds(p.End()), humanSeconds(p.Duration), int(p.End()))
}

// humanSeconds renders seconds as "4:12" / "0:48" — compact and unambiguous in a
// transcript header.
func humanSeconds(s float64) string {
	n := int(s + 0.5)
	return fmt.Sprintf("%d:%02d", n/60, n%60)
}

// audioSegment writes the clip to a temp file, probes it, and transcodes the requested
// slice to the canonical 16 kHz mono FLAC every ASR backend here expects.
//
// A temp FILE for the same reason the frame extractor uses one: -ss has to seek around
// a container whose index may sit at the end, which stdin cannot do.
func audioSegment(ctx context.Context, data []byte, start, window float64) ([]byte, audioPlan, error) {
	select {
	case videoDecodeSem <- struct{}{}:
		defer func() { <-videoDecodeSem }()
	case <-ctx.Done():
		return nil, audioPlan{}, ctx.Err()
	}

	src, err := os.CreateTemp("", "bob-audio-*.media")
	if err != nil {
		return nil, audioPlan{}, fmt.Errorf("temp file: %w", err)
	}
	srcPath := src.Name()
	defer os.Remove(srcPath)
	if _, err := src.Write(data); err != nil {
		src.Close()
		return nil, audioPlan{}, fmt.Errorf("temp file: %w", err)
	}
	if err := src.Close(); err != nil {
		return nil, audioPlan{}, fmt.Errorf("temp file: %w", err)
	}

	duration, err := probeDuration(ctx, srcPath)
	if err != nil {
		// Duration only feeds the PLAN (header, footer, window clamp). A container that
		// does not carry one — some webm, some streamed captures — must still be
		// transcribable: losing the words because the header would have been prettier is
		// the wrong trade. Fall through with an unknown-length plan and let ffmpeg decide
		// what the requested slice contains.
		return wholeFileSegment(ctx, srcPath, start, window)
	}
	if start >= duration {
		return nil, audioPlan{}, fmt.Errorf("time_start=%.0f 秒超出了这段录音的长度（%s）", start, humanSeconds(duration))
	}
	avail := duration - start
	if window <= 0 || window > avail {
		window = avail
	}
	if window > audioMaxWindow {
		window = audioMaxWindow
	}
	plan := audioPlan{Duration: duration, Start: start, Window: window}

	clip, err := extractFLAC(ctx, srcPath, plan)
	return clip, plan, err
}

// extractFLAC cuts plan's slice out of srcPath as canonical 16 kHz mono FLAC — the shape
// every ASR backend here expects.
func extractFLAC(ctx context.Context, srcPath string, plan audioPlan) ([]byte, error) {
	out, err := os.CreateTemp("", "bob-audio-*.flac")
	if err != nil {
		return nil, fmt.Errorf("temp file: %w", err)
	}
	outPath := out.Name()
	out.Close()
	defer os.Remove(outPath)

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(plan.Start, 'f', 3, 64),
		"-t", strconv.FormatFloat(plan.Window, 'f', 3, 64),
		"-i", srcPath,
		"-vn", "-ar", "16000", "-ac", "1",
		"-f", "flac", outPath,
	)
	if o, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(o))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("取音轨失败：%s", msg)
	}
	clip, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("读不到取出的音轨：%w", err)
	}
	if len(clip) == 0 {
		return nil, fmt.Errorf("这一段里没有音轨")
	}
	return clip, nil
}

// wholeFileSegment is the degraded path for a container with no readable duration: cut
// the requested slice anyway, and mark the plan unknown so the header claims nothing it
// cannot back up. Losing the words because the header would have been prettier is the
// wrong trade — duration only ever fed the PLAN.
func wholeFileSegment(ctx context.Context, srcPath string, start, window float64) ([]byte, audioPlan, error) {
	if window <= 0 || window > audioMaxWindow {
		window = audioMaxWindow
	}
	plan := audioPlan{Start: start, Window: window, UnknownLength: true}
	clip, err := extractFLAC(ctx, srcPath, plan)
	return clip, plan, err
}
