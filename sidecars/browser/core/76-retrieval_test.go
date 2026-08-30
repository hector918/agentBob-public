package core

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRetrievalIngestContent pins the §4.2 ingest filter that gates what leaves
// bob for the external retrieval leaf: role filtering (drop system/control),
// credential scrubbing (keys must never go out), and long-blob truncation.
func TestRetrievalIngestContent(t *testing.T) {
	// system/control rows are dropped entirely.
	if _, ok := RetrievalIngestContent("system", "compaction summary"); ok {
		t.Errorf("system role should be dropped from the retrieval feed")
	}
	if _, ok := RetrievalIngestContent("audit", "x"); ok {
		t.Errorf("unknown control role should be dropped from the retrieval feed")
	}

	// user/assistant/tool are kept.
	for _, role := range []string{"user", "assistant", "tool"} {
		if _, ok := RetrievalIngestContent(role, "hi"); !ok {
			t.Errorf("role %q should be kept in the retrieval feed", role)
		}
	}

	// Credentials are scrubbed before they can leave the process.
	secret := "AWS_SECRET_ACCESS_KEY=AKIAIOSFODNN7EXAMPLEAKIAIOSFODNN7"
	got, ok := RetrievalIngestContent("user", "here is my key "+secret)
	if !ok {
		t.Fatal("user message dropped unexpectedly")
	}
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLEAKIAIOSFODNN7") {
		t.Errorf("credential leaked into retrieval feed: %q", got)
	}

	// Over-cap content is truncated.
	long := strings.Repeat("z", RetrievalContentCap+500)
	got, ok = RetrievalIngestContent("tool", long)
	if !ok {
		t.Fatal("tool message dropped unexpectedly")
	}
	if len(got) != RetrievalContentCap {
		t.Errorf("over-cap content len = %d, want %d (truncated)", len(got), RetrievalContentCap)
	}

	// Truncation must not slice a multi-byte rune: an invalid UTF-8 tail
	// would make pg reject the whole enqueue tx, wedging the session's
	// outbox cursor. 3-byte CJK runes guarantee the cap falls mid-rune
	// (8000 % 3 != 0).
	cjk := strings.Repeat("记", RetrievalContentCap/3+200)
	got, ok = RetrievalIngestContent("tool", cjk)
	if !ok {
		t.Fatal("tool message dropped unexpectedly")
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncated CJK content is invalid UTF-8")
	}
	if len(got) > RetrievalContentCap || len(got) < RetrievalContentCap-utf8.UTFMax {
		t.Errorf("truncated CJK content len = %d, want within [%d,%d]",
			len(got), RetrievalContentCap-utf8.UTFMax, RetrievalContentCap)
	}
}
