package compose

import (
	"strings"
	"testing"

	"agentbob/contract"
)

// TestDumpUserText: voice/audio is transcribed at INGESTION now (F165), so a stored
// turn's transcript is already folded into ev.Text — the /prompt dump just renders it
// like any text, with no special audio note. The dump matches the real turn's input.
func TestDumpUserText(t *testing.T) {
	// A voice note whose transcript was folded into ev.Text at ingestion — the dump shows
	// that text (plus the audio file's attachment line).
	voice := []contract.MessageEvent{{
		Text:        "[语音]\n你好",
		Attachments: []contract.Attachment{{Kind: "voice", FileName: "note.ogg", Path: "inbox/note.ogg"}},
	}}
	if got := DumpUserText(voice); !strings.Contains(got, "[语音]") || !strings.Contains(got, "你好") {
		t.Fatalf("dump must render the folded voice text, got %q", got)
	}
	// plain text passes through.
	if out := DumpUserText([]contract.MessageEvent{{Text: "just text"}}); !strings.Contains(out, "just text") {
		t.Fatalf("plain text dump = %q, want the text", out)
	}
}

// TestRenderDump_Skeleton: the shared skeleton renders system prompt + user input +
// extras + tools/skills in order; nil tools/skills and empty extras are omitted cleanly.
func TestRenderDump_Skeleton(t *testing.T) {
	out := RenderDump("SYS", "USER", nil, nil, "")
	if !strings.Contains(out, "# 系统提示") || !strings.Contains(out, "SYS") ||
		!strings.Contains(out, "# 本轮用户输入") || !strings.Contains(out, "USER") {
		t.Fatalf("skeleton missing core sections: %q", out)
	}
	if strings.Contains(out, "# 工具") || strings.Contains(out, "# 技能") {
		t.Fatalf("nil tools/skills must omit their sections: %q", out)
	}

	withExtras := RenderDump("SYS", "USER", nil, nil, "# agora 身份\n\n- 主体：member:alice")
	if !strings.Contains(withExtras, "# agora 身份") || !strings.Contains(withExtras, "member:alice") {
		t.Fatalf("extras must be inserted: %q", withExtras)
	}
}
