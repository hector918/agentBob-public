package tools

import (
	"strings"
	"testing"
)

func TestScrubToText(t *testing.T) {
	// raw WebP-ish binary mixed with text → binary dropped, real text kept.
	in := "前文 " + string([]byte{0x52, 0x49, 0x46, 0x46, 0xff, 0xfe, 0x00, 0x01, 0x57, 0x45, 0x42, 0x50}) + " 后文\n第二行"
	got := scrubToText(in)
	if got == in {
		t.Fatal("expected binary stripped, got unchanged")
	}
	for _, want := range []string{"前文", "后文", "第二行"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost text %q: %q", want, got)
		}
	}
	if scrubToText("a\x00b\nc") != "ab\nc" {
		t.Errorf("ctrl scrub: %q", scrubToText("a\x00b\nc"))
	}
	if scrubToText("héllo 世界 😀") != "héllo 世界 😀" {
		t.Errorf("unicode mangled: %q", scrubToText("héllo 世界 😀"))
	}
}
