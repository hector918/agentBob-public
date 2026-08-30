package prompt

import (
	"strings"
	"testing"

	"agentbob/contract"
)

func TestStripControl_C1AndUnicodeSeparators(t *testing.T) {
	// C1 controls (incl U+0085 NEL) and the U+2028/U+2029 line/paragraph
	// separators must be stripped — they break the one-line prompt contract.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"NEL U+0085", "a\u0085b", "ab"},
		{"C1 U+0090", "a\u0090b", "ab"},
		{"line sep U+2028", "a\u2028b", "ab"},
		{"para sep U+2029", "a\u2029b", "ab"},
		{"tab", "a\tb", "ab"},
		{"vertical tab / form feed", "a\x0b\x0cb", "ab"},
		{"esc DEL", "a\x1b\x7fb", "ab"},
		{"plain unicode kept", "café — 名前 ✓", "café — 名前 ✓"},
	}
	for _, c := range cases {
		if got := StripControl(c.in); got != c.want {
			t.Errorf("%s: StripControl(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestSanitizeSpeaker_StripsControlAndReTrims(t *testing.T) {
	// Control chars that used to survive the speaker prefix must go, and the
	// 48-rune cut must not leave trailing whitespace.
	if got := SanitizeSpeaker("a\tb\x0b\x0c\x1b\x7fc"); got != "abc" {
		t.Errorf("SanitizeSpeaker control strip = %q, want %q", got, "abc")
	}
	if got := SanitizeSpeaker("[fake]: name\n"); strings.ContainsAny(got, "[]\n") {
		t.Errorf("SanitizeSpeaker left spoof chars: %q", got)
	}
	// A name whose 48th rune boundary lands on a space must be re-trimmed.
	in := strings.Repeat("x", 47) + " tail"
	got := SanitizeSpeaker(in)
	if len([]rune(got)) > 48 {
		t.Fatalf("SanitizeSpeaker did not cap length: %d runes", len([]rune(got)))
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("SanitizeSpeaker left trailing whitespace after cut: %q", got)
	}
}

func TestQuotedMediaNote_SurvivesTheQuoteIntact(t *testing.T) {
	// The note's whole job is to reach the model saying the picture is out of reach.
	// SanitizeQuoted strips [ ] " — a bracketed note would arrive as a bare word and
	// read as the parent's text, which is what sent a turn hunting through the inbox
	// on. The long cases matter most: a filename is attacker-chosen, and a
	// note whose tail clause is cut measured WORSE than no note at all.
	kinds := []string{
		"一张图片", "一段视频", "一个文件：report.pdf",
		"一个文件：" + strings.Repeat("a", 150) + ".pdf",
		"一个文件：" + strings.Repeat("名", 200) + ".pdf",
	}
	for _, kind := range kinds {
		note := QuotedMediaNote(kind, false)
		if strings.ContainsAny(note, `[]"`) {
			t.Errorf("QuotedMediaNote(%.20q) = %q, contains chars SanitizeQuoted strips", kind, note)
		}
		if got := SanitizeQuoted(note, QuotedMax); got != note {
			t.Errorf("QuotedMediaNote(%.20q) does not survive the quote cap: %q -> %q", kind, note, got)
		}
		if !strings.HasSuffix(note, "，不是本轮附件）") {
			t.Errorf("QuotedMediaNote(%.20q) = %q, lost the out-of-reach clause", kind, note)
		}
	}
	// Reachable media must NOT claim to be out of reach — telegram ingests a
	// user-authored parent's media into this turn's attachment list.
	if got := QuotedMediaNote("一张图片", true); got != "（一张图片）" {
		t.Errorf("QuotedMediaNote(inAttachments) = %q, want %q", got, "（一张图片）")
	}
	if got := QuotedMediaNote("", false); got != "" {
		t.Errorf(`QuotedMediaNote("") = %q, want empty (no media, no note)`, got)
	}
}

func TestReplyLine_MediaNoteReachesTheModelWhole(t *testing.T) {
	ev := contract.MessageEvent{ReplyToUser: "you", ReplyToText: QuotedMediaNote("一张图片", false)}
	want := `[In reply to you: "（一张图片，不是本轮附件）"]`
	if got := ReplyLine(ev); got != want {
		t.Errorf("ReplyLine = %q, want %q", got, want)
	}
	// A long parent caption must not eat the clause. Sources put the note FIRST so the
	// QuotedMax cut lands in the prose; this pins that contract from the render side.
	for _, n := range []int{100, 110, 114, 119, 400} {
		note := QuotedMediaNote("一张图片", false)
		ev := contract.MessageEvent{ReplyToUser: "Alice", ReplyToText: note + " " + strings.Repeat("字", n)}
		if got := ReplyLine(ev); !strings.Contains(got, "不是本轮附件") {
			t.Errorf("caption=%d runes: clause truncated away: %q", n, got)
		}
	}
}
