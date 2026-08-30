package providers

import "testing"

// TestHasResidualToolCallMarkup pins the sidecar-markup detector: the
// `<|tool_call>` / `<tool_call|>` token family trips it; ordinary prose and
// the RECOVERABLE `<tool_call>` form do not (the latter is handled by
// ExtractInlineToolCalls, not failed over).
func TestHasResidualToolCallMarkup(t *testing.T) {
	malformed := []string{
		`<|tool_call>call:todo{todos:[{content:<|"|>x<|"|>}]}<tool_call|>`,
		`prefix <|tool_call>call:deliver_file{path:<|"|>/a.md<|"|>}<tool_call|>`,
		`dangling open <|tool_call>call:foo{`,
	}
	for _, s := range malformed {
		if !HasResidualToolCallMarkup(s) {
			t.Errorf("expected residual markup detected in %q", s)
		}
	}
	clean := []string{
		"just normal prose",
		"a < b and c > d; let's talk about the tool_call concept",
		`<tool_call>{"name":"x","arguments":{}}</tool_call>`, // recoverable form — not residual
	}
	for _, s := range clean {
		if HasResidualToolCallMarkup(s) {
			t.Errorf("false positive on %q", s)
		}
	}
}

// TestHasResidualPlainToolCallMarkup pins the ordinary `<tool_call>` leak
// detector used after recovery fails: a truncated-mid-body block (no close
// token, so toolCallBlockRe never matched) and a complete-but-unparseable
// body both leave the `<tool_call>` open token in the content, which must
// trip the failover. Recovered content (markup stripped) and plain prose
// mentioning the concept must NOT trip it.
func TestHasResidualPlainToolCallMarkup(t *testing.T) {
	// Truncated: extraction returns the original content untouched.
	truncated := `<tool_call>{"name":"f","argu`
	cleaned, calls := ExtractInlineToolCalls(truncated)
	if len(calls) != 0 {
		t.Fatalf("truncated block must not recover a call; got %d", len(calls))
	}
	if !HasResidualPlainToolCallMarkup(cleaned) {
		t.Errorf("truncated `<tool_call>` should be flagged residual; got %q", cleaned)
	}

	// Complete block, body neither JSON nor XML function form. The caller only
	// swaps in `cleaned` when an inline call was recovered; with zero calls it
	// keeps the ORIGINAL content, which still carries the `<tool_call>` token.
	garbage := `<tool_call>not json or xml</tool_call>`
	_, calls = ExtractInlineToolCalls(garbage)
	if len(calls) != 0 {
		t.Fatalf("unparseable body must not recover a call; got %d", len(calls))
	}
	if !HasResidualPlainToolCallMarkup(garbage) {
		t.Errorf("unrecovered complete block (content kept as-is) should be flagged; got %q", garbage)
	}

	// Recovered content has the markup stripped — must NOT trip. Plain prose
	// mentioning the concept lacks the `<tool_call>` token, so it's safe too.
	recoveredContent, recovered := ExtractInlineToolCalls(`<tool_call>{"name":"x","arguments":{}}</tool_call>`)
	if len(recovered) != 1 {
		t.Fatalf("well-formed JSON block should recover 1 call; got %d", len(recovered))
	}
	if HasResidualPlainToolCallMarkup(recoveredContent) {
		t.Errorf("recovered content should be clean; got residual %q", recoveredContent)
	}
	clean := []string{
		"just normal prose",
		"let's discuss the tool_call concept abstractly",
	}
	for _, s := range clean {
		if HasResidualPlainToolCallMarkup(s) {
			t.Errorf("false positive on %q", s)
		}
	}
}

// TestExtractInlineToolCalls_SidecarLeftResidual proves the recovery step
// does NOT structure the malformed sidecar form (its pseudo-JSON is
// unparseable), leaving it as residual markup for the caller's failover.
func TestExtractInlineToolCalls_SidecarLeftResidual(t *testing.T) {
	content := `<|tool_call>call:todo{todos:[{content:<|"|>x<|"|>}]}<tool_call|>`
	cleaned, calls := ExtractInlineToolCalls(content)
	if len(calls) != 0 {
		t.Fatalf("sidecar form must NOT be recovered as a tool_call; got %d", len(calls))
	}
	if !HasResidualToolCallMarkup(cleaned) {
		t.Errorf("sidecar markup should remain residual for the failover check; got %q", cleaned)
	}
}

// TestStripResidualToolCallMarkup pins the strip used when a real tool_call
// coexists with leftover sidecar junk: complete blocks and a dangling open
// are removed, surrounding prose survives.
func TestStripResidualToolCallMarkup(t *testing.T) {
	cases := []struct{ in, want string }{
		{`done <|tool_call>call:todo{x:<|"|>y<|"|>}<tool_call|>`, "done"},
		{`<|tool_call>call:a{}<tool_call|> middle <|tool_call>call:b{}<tool_call|>`, "middle"},
		{`tail truncated <|tool_call>call:foo{`, "tail truncated"},
		{"no markup here", "no markup here"},
	}
	for _, c := range cases {
		if got := StripResidualToolCallMarkup(c.in); got != c.want {
			t.Errorf("StripResidualToolCallMarkup(%q) = %q, want %q", c.in, got, c.want)
		}
		if HasResidualToolCallMarkup(StripResidualToolCallMarkup(c.in)) {
			t.Errorf("strip left residual markup for %q", c.in)
		}
	}
}
