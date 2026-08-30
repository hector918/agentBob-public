package warrant

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestPanelApply_RejectsSilentWipe locks the editor's guard: a typo'd top-level
// key or a cleared editor must be REJECTED at validate (not parsed into an empty
// matrix that then reconciles deny-all live and reports success). The on-disk file
// must be left untouched on every rejection.
func TestPanelApply_RejectsSilentWipe(t *testing.T) {
	const good = "principals:\n  - user\ngrants:\n  tool:use:now:\n    user: \"on\"\n"
	reject := []struct {
		name string
		body string
	}{
		{"typo'd grants key", "principals:\n  - user\ngrant:\n  tool:use:now:\n    user: \"on\"\n"},
		{"typo'd principals key", "principls:\n  - user\ngrants:\n  tool:use:now:\n    user: \"on\"\n"},
		{"empty body", ""},
		{"whitespace only", "   \n"},
		{"unrelated keys only", "foo: bar\n"},
	}
	for _, c := range reject {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			seedPermsFile(t, home, good) // a real prior policy on disk
			m := &Manager{home: home, catalog: newFakeCat("now")}
			m.policy.Store(emptyPolicy())

			res := m.panel().Apply(context.Background(), "permissions.yaml", c.body)
			if res.Success {
				t.Fatalf("expected rejection, got Success (Phase %q)", res.Phase)
			}
			if res.Phase != "validate" {
				t.Errorf("Phase = %q, want validate", res.Phase)
			}
			// the prior file must be intact — a rejected save never touches disk
			got, err := os.ReadFile(policyPath(home))
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if string(got) != good {
				t.Errorf("on-disk file was modified by a rejected save:\n%s", got)
			}
		})
	}
}

// TestPanelApply_PersistsRawBody locks fix 6: a valid save writes the admin's exact
// text (comments + ordering preserved), not a re-serialized canonical form, and the
// grant takes effect live.
func TestPanelApply_PersistsRawBody(t *testing.T) {
	home := t.TempDir()
	m := &Manager{home: home, catalog: newFakeCat("now")}
	m.policy.Store(emptyPolicy())

	body := "# my hand-written note\nprincipals:\n  - user\ngrants:\n  tool:use:now:\n    user: \"on\"\n"
	res := m.panel().Apply(context.Background(), "permissions.yaml", body)
	if !res.Success {
		t.Fatalf("valid save rejected: Phase %q, Error %q", res.Phase, res.Error)
	}
	got, err := os.ReadFile(policyPath(home))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(got), "# my hand-written note") {
		t.Errorf("comment not preserved (body was re-serialized):\n%s", got)
	}
	if ok, _ := m.Check(context.Background(), "user", "tool", "now"); !ok {
		t.Error("saved grant must be live after Apply")
	}
}
