package email

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// buildMIME assembles a multipart/mixed test message. Each part is
// (headers, body) already CRLF-free — the helper adds the wire framing.
func buildMIME(parts ...[2]string) string {
	const boundary = "TESTBOUNDARY42"
	var b strings.Builder
	b.WriteString("From: alice@example.com\r\n")
	b.WriteString("To: bob@example.com\r\n")
	b.WriteString("Subject: test\r\n")
	b.WriteString("Message-ID: <m1@example.com>\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=" + boundary + "\r\n")
	b.WriteString("\r\n")
	for _, p := range parts {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString(p[0])
		b.WriteString("\r\n")
		b.WriteString(p[1])
		b.WriteString("\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

// A disp=inline NON-text part (pasted screenshot — Gmail / Apple Mail default)
// must land in Attachments, NOT be concatenated into TextBody as raw binary
// (F20). go-message classifies every disp=inline part as InlineHeader
// regardless of content type, so the type gate lives in parseMIME.
func TestParseMIME_InlineNonTextIsAttachment(t *testing.T) {
	raw := buildMIME(
		[2]string{"Content-Type: text/plain; charset=UTF-8\r\n", "hello body"},
		[2]string{
			"Content-Type: image/png\r\nContent-Disposition: inline\r\nContent-Transfer-Encoding: base64\r\n",
			"iVBORw0KGgoAAAANSUhEUg==", // binary-ish payload; content is irrelevant
		},
		[2]string{
			"Content-Type: application/pdf\r\nContent-Disposition: inline; filename=\"chart.pdf\"\r\n",
			"%PDF-1.4 fake",
		},
	)
	pe, err := parseMIME(strings.NewReader(raw), nil, "", 1<<20)
	if err != nil {
		t.Fatalf("parseMIME: %v", err)
	}
	if pe.TextBody != "hello body" {
		t.Fatalf("TextBody must carry ONLY the text part, got %q", pe.TextBody)
	}
	if len(pe.Attachments) != 2 {
		t.Fatalf("inline image + inline pdf must both be attachments, got %+v", pe.Attachments)
	}
	img := pe.Attachments[0]
	if img.Kind != "image" || img.MIME != "image/png" {
		t.Errorf("inline image misclassified: %+v", img)
	}
	if img.FileName != "inline.png" {
		t.Errorf("filename-less inline image should default by MIME ext, got %q", img.FileName)
	}
	pdf := pe.Attachments[1]
	if pdf.FileName != "chart.pdf" || pdf.Kind != "document" {
		t.Errorf("inline pdf should keep its disposition filename: %+v", pdf)
	}
}

// The TOTAL text-body cap must hold at append time (F27): a part landing on a
// partly-filled body is trimmed to the remaining budget instead of being
// concatenated whole (which let the body reach ~2× the cap).
func TestParseMIME_TotalTextCapEnforcedOnAppend(t *testing.T) {
	first := strings.Repeat("a", 512<<10)           // half the budget
	second := strings.Repeat("b", maxTextPartBytes) // a full per-part-cap part
	raw := buildMIME(
		[2]string{"Content-Type: text/plain\r\n", first},
		[2]string{"Content-Type: text/plain\r\n", second},
	)
	pe, err := parseMIME(strings.NewReader(raw), nil, "", 1<<20)
	if err != nil {
		t.Fatalf("parseMIME: %v", err)
	}
	if len(pe.TextBody) > maxTextBodyBytes {
		t.Fatalf("TextBody %d bytes exceeds the total cap %d", len(pe.TextBody), maxTextBodyBytes)
	}
	if !strings.HasPrefix(pe.TextBody, first) {
		t.Fatal("first part must survive intact — only the overflowing tail is dropped")
	}
	if !strings.Contains(pe.TextBody, "\nb") {
		t.Fatal("second part should still contribute up to the remaining budget")
	}
}

// An HTML-only message (no text/plain sibling) must NOT yield an empty TextBody
// — it is degraded to plain text so the mail carries its intent into a turn
// instead of being silently dropped at submit (F21).
func TestParseMIME_HTMLOnlyFallback(t *testing.T) {
	raw := buildMIME(
		[2]string{
			"Content-Type: text/html; charset=UTF-8\r\n",
			"<html><body><p>Hello&nbsp;<b>Bob</b></p><p>Need the report by <i>Friday</i>.</p><script>var x=1;</script></body></html>",
		},
	)
	pe, err := parseMIME(strings.NewReader(raw), nil, "", 1<<20)
	if err != nil {
		t.Fatalf("parseMIME: %v", err)
	}
	if strings.TrimSpace(pe.TextBody) == "" {
		t.Fatal("HTML-only body must fall back to stripped text, got empty")
	}
	if !strings.Contains(pe.TextBody, "Hello") || !strings.Contains(pe.TextBody, "Bob") ||
		!strings.Contains(pe.TextBody, "Friday") {
		t.Fatalf("stripped text must preserve the prose, got %q", pe.TextBody)
	}
	if strings.Contains(pe.TextBody, "<") || strings.Contains(pe.TextBody, "var x") {
		t.Fatalf("stripped text must drop tags + script bodies, got %q", pe.TextBody)
	}
}

// A message carrying BOTH text/plain and text/html keeps the plain part — the
// html fallback only fires when text/plain is absent.
func TestParseMIME_PlainWinsOverHTML(t *testing.T) {
	raw := buildMIME(
		[2]string{"Content-Type: text/plain; charset=UTF-8\r\n", "the plain body"},
		[2]string{"Content-Type: text/html; charset=UTF-8\r\n", "<p>the html body</p>"},
	)
	pe, err := parseMIME(strings.NewReader(raw), nil, "", 1<<20)
	if err != nil {
		t.Fatalf("parseMIME: %v", err)
	}
	if pe.TextBody != "the plain body" {
		t.Fatalf("text/plain must win over html fallback, got %q", pe.TextBody)
	}
}

func TestStripHTMLToText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<p>one</p><p>two</p>", "one\ntwo"},
		{"a<br>b", "a\nb"},
		{"<style>.x{color:red}</style>hi", "hi"},
		{"tom &amp; jerry &lt;3", "tom & jerry <3"},
		{"  <div>  spaced   text  </div>  ", "spaced text"},
	}
	for _, c := range cases {
		if got := stripHTMLToText(c.in); got != c.want {
			t.Errorf("stripHTMLToText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Error grading (F23): a permanent content defect is recognised through wrap
// layers; a bare transport error is not (→ left UNSEEN + retried).
func TestPermanentErrGrading(t *testing.T) {
	transient := fmt.Errorf("screen-fetch: %w", errors.New("connection reset"))
	if isPermanent(transient) {
		t.Error("a bare transport error must be transient")
	}
	perm := permanent(fmt.Errorf("parseMIME: %w", errors.New("bad boundary")))
	if !isPermanent(perm) {
		t.Error("a wrapped permanent error must be recognised")
	}
	// isPermanent sees through an outer wrap of a permanent error.
	if !isPermanent(fmt.Errorf("poll: %w", perm)) {
		t.Error("isPermanent must unwrap outer layers")
	}
}
