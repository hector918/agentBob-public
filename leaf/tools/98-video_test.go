package tools

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"agentbob/contract"
)

// The default window is the WHOLE clip, never its leading N seconds. This is the case
// that motivated the leg: a 65 s surveillance recording whose event starts around 40 s.
// A leading 30 s window returns only the calm half and the model confidently describes
// an ordinary parking lot — the exact wrong answer the frame budget exists to prevent.
func TestPlanFramesCoversWholeClipByDefault(t *testing.T) {
	p, err := planFrames(65.47, 0, 0)
	if err != nil {
		t.Fatalf("planFrames: %v", err)
	}
	if p.Start != 0 || p.Window != 65.47 {
		t.Errorf("default window = [%v, +%v), want the whole clip [0, +65.47)", p.Start, p.Window)
	}
	if p.Frames != videoMaxFrames {
		t.Errorf("frames = %d, want the %d-frame budget spread across the clip", p.Frames, videoMaxFrames)
	}
	// Spread evenly: the last sample must land near the END, not 30 s in.
	if last := float64(p.Frames-1) / p.FPS(); last < p.Duration*0.9 {
		t.Errorf("last sample at %.1fs of a %.1fs clip — the tail is not covered", last, p.Duration)
	}
}

func TestPlanFrames(t *testing.T) {
	cases := []struct {
		name                    string
		duration, start, window float64
		wantFrames              int
		wantStart, wantWindow   float64
	}{
		// At or below the budget the rate IS one frame per second.
		{"short clip is 1fps", 20, 0, 0, 20, 0, 20},
		{"exactly the budget", 30, 0, 0, 30, 0, 30},
		// Past it the same budget stretches instead of truncating.
		{"long clip stretches", 600, 0, 0, videoMaxFrames, 0, 600},
		// An explicit window is honoured and re-densified to 1fps inside itself.
		{"explicit window", 65, 40, 20, 20, 40, 20},
		// A window past the end is clamped to what is left, not refused.
		{"window overruns end", 65, 50, 40, 15, 50, 15},
		// Sub-second material still yields something to look at.
		{"sub-second clip", 0.4, 0, 0, 1, 0, 0.4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := planFrames(c.duration, c.start, c.window)
			if err != nil {
				t.Fatalf("planFrames: %v", err)
			}
			if p.Frames != c.wantFrames {
				t.Errorf("frames = %d, want %d", p.Frames, c.wantFrames)
			}
			if p.Start != c.wantStart || math.Abs(p.Window-c.wantWindow) > 1e-9 {
				t.Errorf("window = [%v, +%v), want [%v, +%v)", p.Start, p.Window, c.wantStart, c.wantWindow)
			}
			if fps := p.FPS(); fps <= 0 || fps > 1.0000001 {
				t.Errorf("fps = %v, want (0, 1] — never faster than one frame per second", fps)
			}
		})
	}
}

func TestPlanFramesRejects(t *testing.T) {
	for _, c := range []struct {
		name                    string
		duration, start, window float64
	}{
		{"unknown duration", 0, 0, 0},
		{"start past end", 30, 40, 0},
		{"negative start", 30, -1, 0},
		{"negative window", 30, 0, -5},
	} {
		if _, err := planFrames(c.duration, c.start, c.window); err == nil {
			t.Errorf("%s: want error, got none", c.name)
		}
	}
}

// HEIC and MP4 both carry "ftyp" at offset 4 — only the brand separates them. The sniff
// ORDER is the guard (detectImageMIME first), so an iPhone photo is never handed to the
// frame extractor. This pins both halves of that contract.
func TestDetectVideoMIMEvsHEIC(t *testing.T) {
	heic := append([]byte{0, 0, 0, 0x18}, []byte("ftypheic")...)
	heic = append(heic, make([]byte, 8)...)
	if got := detectImageMIME(heic); got != "image/heic" {
		t.Fatalf("detectImageMIME(heic) = %q, want image/heic — the ordering guard depends on this", got)
	}
	mp4 := append([]byte{0, 0, 0, 0x18}, []byte("ftypisom")...)
	mp4 = append(mp4, make([]byte, 8)...)
	if got := detectImageMIME(mp4); got != "" {
		t.Errorf("detectImageMIME(mp4) = %q, want \"\" (falls through to the video sniff)", got)
	}
	if got := detectVideoMIME(mp4); got != "video/mp4" {
		t.Errorf("detectVideoMIME(mp4) = %q, want video/mp4", got)
	}
	webm := append([]byte{0x1a, 0x45, 0xdf, 0xa3}, make([]byte, 8)...)
	if got := detectVideoMIME(webm); got != "video/webm" {
		t.Errorf("detectVideoMIME(webm) = %q, want video/webm", got)
	}
	if got := detectVideoMIME([]byte{0x89, 'P', 'N', 'G', 0, 0, 0, 0, 0, 0, 0, 0}); got != "" {
		t.Errorf("detectVideoMIME(png) = %q, want \"\"", got)
	}
	if got := detectVideoMIME([]byte{1, 2, 3}); got != "" {
		t.Errorf("detectVideoMIME(short) = %q, want \"\"", got)
	}
}

// The vision tool's appetite is a UNION; the shared contract predicate stays narrow so
// imagecreate's i2i init image keeps rejecting clips.
func TestVisionCandidateUnion(t *testing.T) {
	img := contract.Attachment{Kind: "image"}
	vid := contract.Attachment{Kind: "video"}
	gif := contract.Attachment{Kind: "animation"}
	doc := contract.Attachment{Kind: "document", MIME: "application/pdf"}
	for _, a := range []contract.Attachment{img, vid, gif} {
		if !visionCandidate(a) {
			t.Errorf("visionCandidate(%s) = false, want true", a.Kind)
		}
	}
	if visionCandidate(doc) {
		t.Error("visionCandidate(pdf) = true, want false")
	}
	for _, a := range []contract.Attachment{vid, gif} {
		if a.IsImageContent() {
			t.Errorf("%s must stay OUT of IsImageContent — imagecreate shares it", a.Kind)
		}
	}
}

// End-to-end through real ffmpeg: a synthesised clip in, ordered JPEG stills out.
func TestVideoFramesEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "src.mp4")
	// 8 seconds of a moving test pattern — long enough to exercise 1fps sampling.
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=8",
		"-pix_fmt", "yuv420p", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot synthesise a test clip: %v %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	frames, plan, err := videoFrames(context.Background(), data, 0, 0)
	if err != nil {
		t.Fatalf("videoFrames: %v", err)
	}
	if len(frames) != plan.Frames {
		t.Errorf("got %d frames, plan says %d — the plan handed to the model must match reality", len(frames), plan.Frames)
	}
	if len(frames) < 7 || len(frames) > 8 {
		t.Errorf("got %d frames from an 8s clip, want ~8 (one per second)", len(frames))
	}
	for i, f := range frames {
		if f.MIME != "image/jpeg" {
			t.Errorf("frame %d MIME = %q, want image/jpeg", i, f.MIME)
		}
		if detectImageMIME(f.Data) != "image/jpeg" {
			t.Errorf("frame %d is not decodable JPEG", i)
		}
	}
	if plan.Describe() == "" {
		t.Error("Describe() empty — the model would read the stills as unrelated pictures")
	}

	// A narrowed window samples that slice, not the head of the clip.
	sub, subPlan, err := videoFrames(context.Background(), data, 5, 3)
	if err != nil {
		t.Fatalf("videoFrames(window): %v", err)
	}
	if subPlan.Start != 5 || subPlan.Window != 3 {
		t.Errorf("window = [%v, +%v), want [5, +3)", subPlan.Start, subPlan.Window)
	}
	if len(sub) != 3 {
		t.Errorf("got %d frames for a 3s window, want 3", len(sub))
	}
}

func TestVideoFramesRejectsNonVideo(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	if _, _, err := videoFrames(context.Background(), []byte("not a video at all"), 0, 0); err == nil {
		t.Error("want an error for undecodable bytes, got none")
	}
}
