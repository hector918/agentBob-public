package policy

import (
	"testing"

	"agentbob/sidecars/browser/config"
)

// TestCompileCmdPatternTrailingSpaceWordBoundary verifies the documented
// contract: a trailing space in a pattern forces a word boundary, so "rm "
// matches the command `rm` (followed by a space, e.g. an argument) but NOT
// `rmdir`. Previously compileCmdPattern's strings.TrimSpace ate the trailing
// space, making the pattern over-match `rmdir`.
func TestCompileCmdPatternTrailingSpaceWordBoundary(t *testing.T) {
	re := compileCmdPattern("rm ")
	if re == nil {
		t.Fatal("compileCmdPattern(\"rm \") returned nil")
	}

	cases := []struct {
		text string
		want bool
	}{
		{"rm foo", true},        // `rm` with an argument — the space matches
		{"rm ", true},           // bare `rm ` (trailing space present)
		{"please rm foo", true}, // mid-line occurrence still matches
		{"rmdir foo", false},    // word boundary: must NOT match `rmdir`
		{"rmtmp", false},        // no trailing space at all
		{"rm", false},           // bare `rm` with no following space
	}
	for _, c := range cases {
		got := re.FindStringIndex(c.text) != nil
		if got != c.want {
			t.Errorf("pattern %q against %q: got match=%v, want %v (regex=%q)",
				"rm ", c.text, got, c.want, re.String())
		}
	}
}

// TestCompileCmdPatternNoTrailingSpacePrefix verifies that a pattern WITHOUT
// a trailing space keeps its prefix/substring behaviour (matches `rmdir`),
// i.e. the trailing-space semantics only kick in when the space is present.
func TestCompileCmdPatternNoTrailingSpacePrefix(t *testing.T) {
	re := compileCmdPattern("rm")
	if re == nil {
		t.Fatal("compileCmdPattern(\"rm\") returned nil")
	}
	for _, text := range []string{"rm foo", "rmdir foo", "rm"} {
		if re.FindStringIndex(text) == nil {
			t.Errorf("pattern %q should match %q (prefix behaviour) but did not", "rm", text)
		}
	}
}

// TestCompileCmdPatternLeadingWhitespaceTrimmed verifies leading YAML-
// indentation whitespace is still trimmed (the doc allows normalising it),
// while a trailing space remains load-bearing.
func TestCompileCmdPatternLeadingWhitespaceTrimmed(t *testing.T) {
	re := compileCmdPattern("  rm ")
	if re == nil {
		t.Fatal("compileCmdPattern(\"  rm \") returned nil")
	}
	if re.FindStringIndex("rm foo") == nil {
		t.Errorf("leading whitespace should be trimmed; %q should match %q", "  rm ", "rm foo")
	}
	if re.FindStringIndex("rmdir foo") != nil {
		t.Errorf("trailing space should survive leading-trim; %q should NOT match %q", "  rm ", "rmdir foo")
	}
}

// TestCompileCmdPatternWhitespaceOnly verifies a whitespace-only / empty
// pattern compiles to nil (skipped) rather than to a regex that matches
// everything.
func TestCompileCmdPatternWhitespaceOnly(t *testing.T) {
	for _, pat := range []string{"", "   ", "\t"} {
		if re := compileCmdPattern(pat); re != nil {
			t.Errorf("compileCmdPattern(%q) should be nil, got %q", pat, re.String())
		}
	}
}

// TestCompileCmdPatternWordBoundaryEscape verifies that the \b two-char
// sequence compiles to a regex word-boundary anchor rather than being
// QuoteMeta'd into a literal backslash. Regression guard: before this,
// `\bsu\b` matched only the literal text "\bsu\b", so the default sudo/su
// bans never fired on real commands.
func TestCompileCmdPatternWordBoundaryEscape(t *testing.T) {
	re := compileCmdPattern(`\bsu\b`)
	if re == nil {
		t.Fatal("compileCmdPattern(`\\bsu\\b`) returned nil")
	}
	cases := []struct {
		text string
		want bool
	}{
		{"su", true},
		{"su - root", true},
		{"/usr/bin/su root", true},
		{"echo hi && su", true},
		{`\bsu\b`, false}, // the old broken behaviour: literal backslashes
		{"import subprocess as sp", false},
		{"pip install BeautifulSoup", false},
		{"gh issue list", false},
		{"echo pseudonym", false},
	}
	for _, c := range cases {
		got := re.FindStringIndex(c.text) != nil
		if got != c.want {
			t.Errorf("pattern `\\bsu\\b` against %q: got match=%v, want %v (regex=%q)",
				c.text, got, c.want, re.String())
		}
	}
}

// TestDefaultBannedSudoSu pins the load-bearing invariant that the built-in
// sudo/su bans actually refuse real invocations through the full
// NewFromConfig → CheckCommand path (defaults compile through
// compileCmdPattern like operator patterns do), and that the word-boundary
// anchors keep "subprocess"-style text from false-positiving.
func TestDefaultBannedSudoSu(t *testing.T) {
	p := NewFromConfig(config.PolicyConfig{})
	refused := []string{
		"sudo apt install foo",
		"sudo rm -rf /", // banned beats the rm -rf approval pattern
		"su",
		"su - root",
	}
	for _, cmd := range refused {
		if r := p.CheckCommand(cmd); r.Decision != DecisionRefuse {
			t.Errorf("CheckCommand(%q) = %v, want DecisionRefuse", cmd, r.Decision)
		}
	}
	allowed := []string{
		"python -c 'import subprocess as sp'",
		"pip install BeautifulSoup",
		"gh issue list",
	}
	for _, cmd := range allowed {
		if r := p.CheckCommand(cmd); r.Decision == DecisionRefuse {
			t.Errorf("CheckCommand(%q) refused (%s), want non-refuse", cmd, r.Reason)
		}
	}
}
