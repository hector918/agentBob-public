package core

import "testing"

// TestTruncateRunesEllipsis_CJKMidCodepoint is the regression guard for
// B3 (audit): clipText in agora query tools claimed
// rune-cap but byte-sliced, cutting Chinese summaries mid-codepoint so
// json.Marshal produced U+FFFD tails the model could not read.
//
// A 3-byte UTF-8 rune ("你") at byte budget 1 must NOT appear in the
// output; the helper caps at runes, not bytes.
func TestTruncateRunesEllipsis_CJKMidCodepoint(t *testing.T) {
	// 6 Chinese runes = 18 bytes in UTF-8. Naive s[:5] would land
	// mid-codepoint at byte 5 (inside the 2nd rune).
	in := "你好世界测试"
	got := TruncateRunesEllipsis(in, 3)
	want := "你好世" + "…"
	if got != want {
		t.Fatalf("TruncateRunesEllipsis(%q, 3) = %q, want %q", in, got, want)
	}
	for _, r := range got {
		if r == '�' {
			t.Fatalf("output contains U+FFFD replacement rune: %q", got)
		}
	}
}

func TestTruncateRunesEllipsis_NoTruncation(t *testing.T) {
	in := "abc"
	if got := TruncateRunesEllipsis(in, 10); got != in {
		t.Fatalf("got %q, want %q", got, in)
	}
}

func TestTruncateRunesEllipsis_ZeroCap(t *testing.T) {
	if got := TruncateRunesEllipsis("anything", 0); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// TestTruncateRunes_CodepointBoundary pins the byte-budget helper:
// must never land mid-rune even when maxBytes falls inside a
// multi-byte sequence.
func TestTruncateRunes_CodepointBoundary(t *testing.T) {
	in := "你好" // 6 bytes
	got := TruncateRunes(in, 4)
	if got != "你" {
		t.Fatalf("TruncateRunes(%q, 4) = %q, want %q", in, got, "你")
	}
	if got := TruncateRunes(in, 6); got != in {
		t.Fatalf("at exact budget got %q, want %q", got, in)
	}
}
