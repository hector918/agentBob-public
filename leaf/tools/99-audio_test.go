package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentbob/contract"
)

// The audio tool takes anything with a soundtrack — video included. Refusing video would
// leave "这段视频里的人说了什么" with no home, which is the seam requests fall through.
func TestAudioCandidate(t *testing.T) {
	for _, a := range []contract.Attachment{
		{Kind: "voice"}, {Kind: "audio"}, {Kind: "document", MIME: "audio/ogg"},
		{Kind: "video"}, {Kind: "animation"}, {Kind: "document", MIME: "video/mp4"},
	} {
		if !audioCandidate(a) {
			t.Errorf("audioCandidate(%s/%s) = false, want true", a.Kind, a.MIME)
		}
	}
	for _, a := range []contract.Attachment{
		{Kind: "image"}, {Kind: "document", MIME: "application/pdf"},
	} {
		if audioCandidate(a) {
			t.Errorf("audioCandidate(%s/%s) = true, want false", a.Kind, a.MIME)
		}
	}
}

// The header always frames the words, and the footer is what makes the model the
// segmenter: it must name the remaining range and a usable time_start, or a long
// recording silently ends at the ten-minute mark.
func TestAudioPlanHeaderFooter(t *testing.T) {
	whole := audioPlan{Duration: 42, Start: 0, Window: 42}
	if h := whole.header(); !strings.Contains(h, "0:42") || strings.Contains(h, "段，") {
		t.Errorf("whole-clip header should state length without a range: %q", h)
	}
	if f := whole.footer(); f != "" {
		t.Errorf("nothing left over, want empty footer, got %q", f)
	}

	part := audioPlan{Duration: 3600, Start: 0, Window: audioMaxWindow}
	h, f := part.header(), part.footer()
	if !strings.Contains(h, "0:00") || !strings.Contains(h, "5:00") || !strings.Contains(h, "60:00") {
		t.Errorf("sliced header must state slice AND whole: %q", h)
	}
	if !strings.Contains(f, "time_start=300") {
		t.Errorf("footer must hand back a usable continuation: %q", f)
	}
	if !strings.Contains(f, "5:00") || !strings.Contains(f, "60:00") {
		t.Errorf("footer must name the remaining range: %q", f)
	}

	// Last slice: ends at the duration → no footer, so the model stops rather than
	// looping on a range that no longer exists.
	last := audioPlan{Duration: 3600, Start: 3300, Window: 300}
	if f := last.footer(); f != "" {
		t.Errorf("final slice must not invite another call, got %q", f)
	}

	// A container with no readable duration still transcribes; the header just must not
	// claim a length nobody read, and the footer must not guess what is left.
	unk := audioPlan{Start: 0, Window: audioMaxWindow, UnknownLength: true}
	if h := unk.header(); strings.Contains(h, ":") && !strings.HasPrefix(h, "[转写]") {
		t.Errorf("unknown-length header must not state times: %q", h)
	}
	if f := unk.footer(); f != "" {
		t.Errorf("unknown-length footer must stay silent, got %q", f)
	}
	if h := (audioPlan{Start: 120, Window: 60, UnknownLength: true}).header(); !strings.Contains(h, "2:00") {
		t.Errorf("an explicit start is known even when the total is not: %q", h)
	}
}

func TestHumanSeconds(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{{0, "0:00"}, {5, "0:05"}, {65, "1:05"}, {600, "10:00"}, {3599.6, "60:00"}} {
		if got := humanSeconds(c.in); got != c.want {
			t.Errorf("humanSeconds(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// End to end through real ffmpeg: a synthesised tone in, a canonical 16 kHz mono FLAC
// slice out, with the window honoured and the ten-minute cap applied.
func TestAudioSegment(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tone.wav")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=20", "-ar", "44100", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot synthesise test audio: %v %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	clip, plan, err := audioSegment(context.Background(), data, 0, 0)
	if err != nil {
		t.Fatalf("audioSegment: %v", err)
	}
	if len(clip) == 0 {
		t.Fatal("empty clip")
	}
	if plan.Duration < 19 || plan.Duration > 21 {
		t.Errorf("duration = %v, want ~20", plan.Duration)
	}
	if plan.Window < 19 {
		t.Errorf("default window should cover the whole clip, got %v", plan.Window)
	}
	if string(clip[:4]) != "fLaC" {
		t.Errorf("backend expects FLAC, got % x", clip[:4])
	}

	sub, subPlan, err := audioSegment(context.Background(), data, 5, 6)
	if err != nil {
		t.Fatalf("audioSegment(window): %v", err)
	}
	if subPlan.Start != 5 || subPlan.Window != 6 {
		t.Errorf("window = [%v,+%v), want [5,+6)", subPlan.Start, subPlan.Window)
	}
	if len(sub) >= len(clip) {
		t.Errorf("a 6s slice should be smaller than the whole 20s clip (%d vs %d)", len(sub), len(clip))
	}

	if _, _, err := audioSegment(context.Background(), data, 99, 0); err == nil {
		t.Error("time_start past the end must be an error, not an empty transcript")
	}
	if _, _, err := audioSegment(context.Background(), []byte("not audio"), 0, 0); err == nil {
		t.Error("undecodable bytes must error")
	}
}
