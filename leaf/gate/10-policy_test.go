package gate

import "testing"

// TestIDListContainsEmailCase pins the @-carve-out (F25): the email source
// canonicalizes From to lowercase while operators type mixed-case addresses into
// the yaml, so email entries must match case-insensitively (allowlist would fail
// closed, denylist would fail OPEN) — while platform-native ids (Telegram numeric,
// Slack U…) keep the exact case-sensitive match.
func TestIDListContainsEmailCase(t *testing.T) {
	l := IDList{"John.Doe@Example.com", "12345", "U0AB"}

	// mixed-case yaml entry vs the source's lowercased id.
	if !l.Contains("john.doe@example.com") {
		t.Error("mixed-case email entry must match the lowercased inbound address")
	}
	// reverse direction: lowercase entry vs a mixed-case id.
	if !(IDList{"john.doe@example.com"}).Contains("John.Doe@Example.COM") {
		t.Error("lowercase email entry must match a mixed-case address")
	}
	// platform ids stay exact — case-folding a Slack-style id would be wrong.
	if l.Contains("u0ab") {
		t.Error("non-@ platform ids must stay case-sensitive")
	}
	if !l.Contains("12345") {
		t.Error("exact platform-id match broken")
	}
	// the carve-out is a fold, not a substring: different addresses stay distinct.
	if l.Contains("jane.doe@example.com") {
		t.Error("different email must not match")
	}

	// the same rule guards the two-tier policy checks (denylist fails open otherwise).
	p := Policy{Denylist: IDList{"Blocked@Example.com"}}
	if !p.Denied("c1", "blocked@example.com") {
		t.Error("mixed-case denylist entry must still deny the lowercased sender")
	}
}
