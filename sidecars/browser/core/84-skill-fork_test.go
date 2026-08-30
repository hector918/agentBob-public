package core

import "testing"

func strptr(s string) *string { return &s }

func TestSkillManifest_RubricResolution(t *testing.T) {
	for _, tc := range []struct {
		name string
		man  *SkillManifest
		want RubricState
		body string
	}{
		{"nil-manifest", nil, RubricInherit, ""},
		{"nil-rubric", &SkillManifest{}, RubricInherit, ""},
		{"empty-removed", &SkillManifest{Rubric: strptr("")}, RubricRemoved, ""},
		{"replaced", &SkillManifest{Rubric: strptr("尺子")}, RubricReplaced, "尺子"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, body := tc.man.RubricResolution()
			if st != tc.want || body != tc.body {
				t.Errorf("= (%v,%q), want (%v,%q)", st, body, tc.want, tc.body)
			}
		})
	}
}

func TestSkillManifest_IsRemoved(t *testing.T) {
	m := &SkillManifest{
		Scripts: map[string]string{"scripts/keep.py": "x"},
		Removed: []string{"scripts/gone.py", "scripts/keep.py"},
	}
	if !m.IsRemoved("scripts/gone.py") {
		t.Error("gone.py should be removed")
	}
	// re-added (in Scripts) beats removed
	if m.IsRemoved("scripts/keep.py") {
		t.Error("keep.py is re-added in Scripts → not removed")
	}
	if m.IsRemoved("scripts/other.py") {
		t.Error("other.py not in Removed")
	}
	if (*SkillManifest)(nil).IsRemoved("x") {
		t.Error("nil manifest removes nothing")
	}
}
