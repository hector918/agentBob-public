package email

import (
	"fmt"
	"strings"
	"testing"
)

// These cover the email package's most regression-prone pure logic — header-
// injection defense, reply subject/threading construction, and reply-all
// self-loop exclusion — which had no tests (#447). Behaviour is unchanged; this
// pins it.

// TestStripCRLF: CR/LF must be flattened to spaces so a header value can't smuggle
// a second header (Bcc injection). No bare CR/LF survives.
func TestStripCRLF(t *testing.T) {
	cases := map[string]string{
		"plain":             "plain",
		"a\r\nb":            "a  b",
		"  trim me  ":       "trim me",
		"x\rcarriage":       "x carriage",
		"y\nnewline":        "y newline",
		"head\r\nBcc: evil": "head  Bcc: evil",
	}
	for in, want := range cases {
		got := stripCRLF(in)
		if got != want {
			t.Errorf("stripCRLF(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("stripCRLF(%q) left a CR/LF: %q", in, got)
		}
	}
}

// TestSanitiseHeaderValue: never emits CR/LF (injection defense), falls back to
// "(no subject)" when empty, Q-encodes non-ASCII, passes ASCII through.
func TestSanitiseHeaderValue(t *testing.T) {
	if got := sanitiseHeaderValue(""); got != "(no subject)" {
		t.Errorf("empty → %q, want (no subject)", got)
	}
	if got := sanitiseHeaderValue("hello world"); got != "hello world" {
		t.Errorf("ascii → %q, want passthrough", got)
	}
	inject := sanitiseHeaderValue("subject\r\nBcc: attacker@evil.com")
	if strings.ContainsAny(inject, "\r\n") {
		t.Errorf("injection not neutralised: %q still has CR/LF", inject)
	}
	if cn := sanitiseHeaderValue("报告"); !strings.HasPrefix(cn, "=?UTF-8?") {
		t.Errorf("non-ASCII should be RFC2047 Q-encoded, got %q", cn)
	}
}

// TestBuildSubject: a single "Re: " is prepended; stacked Re/Fwd/回复/转发 prefixes
// are all peeled first; empty → "Re: (no subject)".
func TestBuildSubject(t *testing.T) {
	cases := map[string]string{
		"hi":                "Re: hi",
		"Re: hi":            "Re: hi",
		"RE: hi":            "Re: hi",
		"Re: Fwd: 回复： deep": "Re: deep",
		"转发: 转发: x":         "Re: x",
		"":                  "Re: (no subject)",
		"   ":               "Re: (no subject)",
	}
	for in, want := range cases {
		if got := buildSubject(in); got != want {
			t.Errorf("buildSubject(%q) = %q, want %q", in, got, want)
		}
	}
	// Over-long subject is truncated with an ellipsis, still single-line.
	long := buildSubject(strings.Repeat("x", subjectSoftCap+100))
	if !strings.HasSuffix(long, "…") || len(long) > subjectSoftCap+8 {
		t.Errorf("long subject not capped: len=%d suffix=%q", len(long), long[len(long)-3:])
	}
}

// TestBuildReferences: appends the parent Message-ID; once over the soft cap it
// keeps the thread root + the most recent tail (RFC 5256 guidance).
func TestBuildReferences(t *testing.T) {
	if got := buildReferences("", "msg1"); got != "<msg1>" {
		t.Errorf("first reply → %q, want <msg1>", got)
	}
	if got := buildReferences("<a> <b>", "c"); got != "<a> <b> <c>" {
		t.Errorf("append → %q, want <a> <b> <c>", got)
	}
	// 20 long ids + a new one → over cap → root + last referencesTailKeep + new.
	var ids []string
	for i := 0; i < 20; i++ {
		ids = append(ids, fmt.Sprintf("<ref-%030d@example.com>", i))
	}
	got := buildReferences(strings.Join(ids, " "), "newest")
	parts := strings.Fields(got)
	if len(parts) != referencesTailKeep+1 {
		t.Fatalf("trimmed refs len=%d, want %d (root + tail)", len(parts), referencesTailKeep+1)
	}
	if parts[0] != ids[0] {
		t.Errorf("trimmed refs dropped the thread root: first=%q want %q", parts[0], ids[0])
	}
	if parts[len(parts)-1] != "<newest>" {
		t.Errorf("trimmed refs dropped the newest id: last=%q", parts[len(parts)-1])
	}
}

// TestReplyAllCc: bob's own identities (self addr, alias, primary To, owned
// domains) are excluded from the reply-all Cc — mandatory self-loop prevention —
// and the rest are deduped.
func TestReplyAllCc(t *testing.T) {
	cc := replyAllCc(
		"bob@mybox.com",   // selfAddr
		"agent@mybox.com", // alias
		"alice@y.com",     // primaryTo
		[]string{"mybox.com"},
		[]string{"alice@y.com", "carol@z.com", "CAROL@z.com"}, // To (dup carol)
		[]string{"bob@mybox.com", "agent@mybox.com", "dave@w.com"},
	)
	joined := strings.Join(cc, ",")
	for _, banned := range []string{"bob@mybox.com", "agent@mybox.com", "alice@y.com"} {
		if strings.Contains(joined, banned) {
			t.Errorf("reply-all Cc must exclude own/primary %q, got %v", banned, cc)
		}
	}
	if !strings.Contains(joined, "carol@z.com") || !strings.Contains(joined, "dave@w.com") {
		t.Errorf("reply-all Cc dropped a legitimate recipient: %v", cc)
	}
	if len(cc) != 2 {
		t.Errorf("reply-all Cc not deduped: %v (want carol + dave)", cc)
	}
}

// TestCapsCarriesRequiredModelTags pins the model-tag wiring: a mailbox's
// required_model_tags config rides Caps so the flow can map it to
// ModelRequest.Requires (a hard model constraint for email turns).
func TestCapsCarriesRequiredModelTags(t *testing.T) {
	s := &Source{cfg: EmailConfig{RequiredModelTags: []string{"email", "smart"}}}
	got := s.Caps().RequiredModelTags
	if len(got) != 2 || got[0] != "email" || got[1] != "smart" {
		t.Fatalf("Caps().RequiredModelTags = %v, want [email smart]", got)
	}
	// Empty config → no constraint.
	if rt := (&Source{}).Caps().RequiredModelTags; len(rt) != 0 {
		t.Fatalf("empty config should yield no required tags, got %v", rt)
	}
}
