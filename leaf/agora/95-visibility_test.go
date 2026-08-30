package agora

import (
	"testing"

	"agentbob/leaf/agora/store"
)

// TestParseVisibility pins the denylist editor's parse: comments/blanks ignored,
// names trimmed, an all-empty body → nil (clears the filter).
func TestParseVisibility(t *testing.T) {
	// comments + whitespace + a real hide list
	vis, err := parseVisibility("# pick what to hide\nhidden_tools:\n  - terminal\n  -   web_search  \nhidden_skills: []\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if vis == nil || len(vis.HiddenTools) != 2 || vis.HiddenTools[0] != "terminal" || vis.HiddenTools[1] != "web_search" {
		t.Fatalf("hidden_tools = %+v, want [terminal web_search] (trimmed)", vis)
	}
	if len(vis.HiddenSkills) != 0 {
		t.Fatalf("hidden_skills should be empty, got %+v", vis.HiddenSkills)
	}

	// empty / comments-only → nil (clear)
	for _, body := range []string{"", "# nothing\nhidden_tools: []\nhidden_skills: []\n", "hidden_tools:\n  - \"\"\n"} {
		v, err := parseVisibility(body)
		if err != nil {
			t.Fatalf("parse(%q): %v", body, err)
		}
		if v != nil {
			t.Fatalf("empty body %q → %+v, want nil (clear)", body, v)
		}
	}

	// malformed yaml → error (validate phase rejects)
	if _, err := parseVisibility("hidden_tools: [unclosed\n"); err == nil {
		t.Fatal("malformed yaml should error")
	}
}

// TestRenderVisibilityRoundTrip: render then parse recovers the hidden lists.
func TestRenderVisibilityRoundTrip(t *testing.T) {
	body := renderVisibility(
		&store.InboxVisibility{HiddenTools: []string{"read_file"}, HiddenSkills: []string{"research"}},
		[]string{"read_file", "terminal"}, []string{"research", "summarize"})
	vis, err := parseVisibility(body)
	if err != nil || vis == nil {
		t.Fatalf("round-trip parse: vis=%+v err=%v", vis, err)
	}
	if len(vis.HiddenTools) != 1 || vis.HiddenTools[0] != "read_file" ||
		len(vis.HiddenSkills) != 1 || vis.HiddenSkills[0] != "research" {
		t.Fatalf("round-trip lost data: %+v", vis)
	}
}
