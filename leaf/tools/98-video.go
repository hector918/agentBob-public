package tools

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"agentbob/contract"
)

const (
	// videoMaxBytes caps the clip read into memory before ffmpeg sees it. Deliberately far
	// above the 10 MB image caps — a one-minute phone video is already ~14 MB, so an image
	// cap here would refuse exactly the files this leg exists to handle — but NOT unbounded:
	// the bytes are held in memory AND written to a temp file, so the real peak is twice
	// this, on the box that also hosts the model pool. 64 MB is ~3× the largest file a
	// Telegram bot can even download, and a clip past it fails with a clear message rather
	// than by making the host swap.
	videoMaxBytes int64 = 64 * 1024 * 1024

	// videoMaxFrames is the most stills one clip is ever reduced to. 30 is a MEASURED
	// ceiling, not a round number: the vision entry that can hold them accepts 32 images
	// per prompt, and 32 frames @448 cost ~3.7k prompt tokens there.
	videoMaxFrames = 30

	// videoFrameWidth is the width each still is scaled to (height follows the aspect).
	// 448 was measured as the point where the event in a 720p surveillance clip stays
	// legible while 30 frames still fit comfortably in one request.
	videoFrameWidth = 448

	// videoFrameQuality is ffmpeg's -q:v for the extracted JPEGs (2 = best, 31 = worst).
	videoFrameQuality = "4"
)

// videoDecodeSem bounds in-flight ffmpeg decodes process-wide, the same guard
// leaf/asr puts on transcoding: a burst of videos must not spawn an unbounded number
// of subprocesses on the box the agent shares with everything else.
var videoDecodeSem = make(chan struct{}, 2)

// detectVideoMIME sniffs magic bytes for the containers ffmpeg is asked to decode.
//
// MUST be consulted only AFTER detectImageMIME (96-ocr.go) has come back empty:
// ISO-BMFF stamps BOTH MP4 and HEIC with "ftyp" at offset 4 and only the brand tells
// them apart, so an iPhone photo reaching this check first would be handed to the
// frame extractor. The image sniffer owns the brand list; this one is the fallthrough.
func detectVideoMIME(b []byte) string {
	if len(b) < 12 {
		return ""
	}
	switch {
	case string(b[4:8]) == "ftyp":
		// MP4 / MOV / M4V all land here; ffmpeg demuxes by content, so one label is enough.
		return "video/mp4"
	case b[0] == 0x1a && b[1] == 0x45 && b[2] == 0xdf && b[3] == 0xa3:
		// EBML header — WebM and Matroska share it, and so do Telegram's video stickers.
		return "video/webm"
	}
	return ""
}

// framePlan is the sampling decision for one clip: which slice of it to look at, and
// how many evenly-spaced stills to reduce that slice to.
type framePlan struct {
	Duration float64 // the clip's full length, seconds
	Start    float64 // window start, seconds
	Window   float64 // window length, seconds
	Frames   int     // stills to extract, evenly spaced across Window
}

// FPS is the rate that spreads Frames evenly across Window, never exceeding one frame
// per second. The clamp only ever binds on a sub-second window, where Frames has been
// floored UP to 1 and the raw ratio would read as a rate the sampler does not mean —
// ffmpeg yields the same single still either way, but a plan should not claim 2.5 fps.
func (p framePlan) FPS() float64 {
	if fps := float64(p.Frames) / p.Window; fps < 1 {
		return fps
	}
	return 1
}

// planFrames decides how to sample a clip: ONE FRAME PER SECOND, and when that would
// exceed videoMaxFrames the same budget is spread EVENLY over the whole window instead.
//
// The default window is the WHOLE clip, not its first N seconds. That choice is the
// difference between a right and a wrong answer on real footage: in the clip that
// motivated this leg (a 65 s surveillance recording) nothing happens until ~40 s, so a
// leading fixed-length window returns only the calm half and the model confidently
// describes an ordinary parking lot. Stretching the frame budget across the clip keeps
// the whole timeline in view; a model that wants detail asks for a narrower window and
// gets the full 1 fps inside it.
//
// start/window are the model's optional request; 0 means "unset" for both.
func planFrames(duration, start, window float64) (framePlan, error) {
	if duration <= 0 {
		return framePlan{}, fmt.Errorf("视频时长未知（文件可能损坏或不是视频）")
	}
	if start < 0 || window < 0 {
		return framePlan{}, fmt.Errorf("时间窗不能为负数")
	}
	if start >= duration {
		return framePlan{}, fmt.Errorf("time_start=%.0fs 超出了视频长度（%.0fs）", start, duration)
	}
	avail := duration - start
	if window == 0 || window > avail {
		window = avail
	}
	frames := int(math.Floor(window))
	if frames > videoMaxFrames {
		frames = videoMaxFrames
	}
	if frames < 1 {
		// Sub-second window (or a sub-second clip): one still is all there is to see.
		frames = 1
	}
	return framePlan{Duration: duration, Start: start, Window: window, Frames: frames}, nil
}

// videoGroundingClause keeps the answer on the pixels. MEASURED, not assumed: on a real
// clip at temperature 0.3, four runs without it fabricated a specific date and place for
// the event twice ("2018年8月3日缅甸果敢地区的军事冲突"), four runs with it did so zero
// times. It does NOT settle what the event was — that judgement is unstable at any
// temperature above 0 and no wording fixed it — so the clause claims only what it earns:
// no invented dates, names or off-screen causes. The confabulated-corroboration failure
// (a made-up timestamp offered as evidence FOR a wrong conclusion) is the one worth
// spending 40 characters on.
const videoGroundingClause = "只根据画面里看得见的东西回答，不要推测画面外的原因、时间或事件名称。"

// Describe renders the plan as the one line prepended to the vision instruction, so the
// model reading the stills knows they are a TIMELINE and over what span — without it the
// same frames read as a pile of unrelated pictures and the temporal story is lost.
func (p framePlan) Describe() string {
	span := fmt.Sprintf("（以下是这段视频（全长 %.0f 秒）按时间顺序均匀抽取的 %d 帧。", p.Duration, p.Frames)
	if p.Start != 0 || p.Window < p.Duration {
		span = fmt.Sprintf("（以下是这段视频第 %.0f–%.0f 秒（全长 %.0f 秒）按时间顺序均匀抽取的 %d 帧。",
			p.Start, p.Start+p.Window, p.Duration, p.Frames)
	}
	return span + videoGroundingClause + "）"
}

// videoFrames writes the clip to a temp file, probes its duration, and extracts the
// planned stills as JPEG ImageRefs in timeline order.
//
// A temp FILE, not a pipe: an MP4's moov atom may sit at the end of the stream and the
// -ss seek needs to move around the container, neither of which survives stdin. (This
// is why it cannot reuse leaf/asr's transcode, which runs at ingestion while the staged
// LocalPath still exists — a tool only ever has bytes read back through the FileChannel.)
func videoFrames(ctx context.Context, data []byte, start, window float64) ([]contract.ImageRef, framePlan, error) {
	select {
	case videoDecodeSem <- struct{}{}:
		defer func() { <-videoDecodeSem }()
	case <-ctx.Done():
		return nil, framePlan{}, ctx.Err()
	}

	src, err := os.CreateTemp("", "bob-vision-*.video")
	if err != nil {
		return nil, framePlan{}, fmt.Errorf("temp file: %w", err)
	}
	srcPath := src.Name()
	defer os.Remove(srcPath)
	if _, err := src.Write(data); err != nil {
		src.Close()
		return nil, framePlan{}, fmt.Errorf("temp file: %w", err)
	}
	if err := src.Close(); err != nil {
		return nil, framePlan{}, fmt.Errorf("temp file: %w", err)
	}

	duration, err := probeDuration(ctx, srcPath)
	if err != nil {
		return nil, framePlan{}, err
	}
	plan, err := planFrames(duration, start, window)
	if err != nil {
		return nil, framePlan{}, err
	}

	dir, err := os.MkdirTemp("", "bob-frames-*")
	if err != nil {
		return nil, framePlan{}, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// -ss BEFORE -i is the fast seek (demuxer-level); -frames:v caps the output even if
	// the fps filter rounds one frame long at the tail.
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(plan.Start, 'f', 3, 64),
		"-t", strconv.FormatFloat(plan.Window, 'f', 3, 64),
		"-i", srcPath,
		"-vf", fmt.Sprintf("fps=%s,scale=%d:-2", strconv.FormatFloat(plan.FPS(), 'f', 6, 64), videoFrameWidth),
		"-frames:v", strconv.Itoa(plan.Frames),
		"-q:v", videoFrameQuality,
		"-f", "image2",
		filepath.Join(dir, "f%03d.jpg"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, plan, fmt.Errorf("ffmpeg 抽帧失败：%s", msg)
	}

	names, err := filepath.Glob(filepath.Join(dir, "*.jpg"))
	if err != nil {
		return nil, plan, fmt.Errorf("读取帧失败：%w", err)
	}
	sort.Strings(names) // zero-padded %03d — lexical order IS timeline order
	refs := make([]contract.ImageRef, 0, len(names))
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			return nil, plan, fmt.Errorf("读取帧失败：%w", err)
		}
		if len(b) == 0 {
			continue
		}
		refs = append(refs, contract.ImageRef{Data: b, MIME: "image/jpeg"})
	}
	if len(refs) == 0 {
		// ffmpeg exited 0 but produced nothing. Two distinguishable causes, and saying
		// which one keeps the model from retrying the wrong fix: a window so narrow it
		// spans no frame is the model's own doing and is retryable, a container with no
		// decodable video stream (an audio-only file mislabelled as video) is not.
		if plan.Start != 0 || plan.Window < plan.Duration {
			return nil, plan, fmt.Errorf("第 %.1f–%.1f 秒这一段取不到画面，把 time_window 放宽一点再试",
				plan.Start, plan.Start+plan.Window)
		}
		return nil, plan, fmt.Errorf("这个文件里没有可解码的画面（可能只有音轨）")
	}
	// Record what was ACTUALLY produced: a short tail or a truncated stream can yield
	// fewer frames than planned, and the line handed to the model must not claim more.
	plan.Frames = len(refs)
	return refs, plan, nil
}

// probeDuration reads the container's duration in seconds via ffprobe. Shared by the
// frame sampler and the audio tool — both need "how long is this" before deciding what
// slice of it to work on, and two copies would drift on what counts as unreadable.
func probeDuration(ctx context.Context, path string) (float64, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("读不出视频信息（文件可能损坏或不是视频）")
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || d <= 0 {
		// Streams without a container duration (some webm) land here.
		return 0, fmt.Errorf("视频时长未知（文件可能损坏或不是视频）")
	}
	return d, nil
}
