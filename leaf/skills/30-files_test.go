package skills

import "testing"

// TestReservedSkillFileVariants: isReservedSkillFile must exclude not just the exact
// reserved meta files but their crash/editor VARIANTS — a crash-orphaned INSIGHT.md.tmp
// (put writes .tmp then renames) and editor artifacts of RUBRIC.md (RUBRIC.md~,
// RUBRIC.md.bak, vim's swap .RUBRIC.md.swp) — else they ship into the model workspace and
// leak the rubric-gate criteria. A legit file (NOTES.md) still ships.
func TestReservedSkillFileVariants(t *testing.T) {
	reserved := []string{
		"SKILL.md", "RUBRIC.md", "INSIGHT.md", // exact
		"INSIGHT.md.tmp", // crash-orphaned atomic-write temp
		"RUBRIC.md~",     // emacs/joe backup
		"RUBRIC.md.bak",  // generic backup
		".RUBRIC.md.swp", // vim swap (leading dot + reserved name)
	}
	for _, r := range reserved {
		if !isReservedSkillFile(r) {
			t.Errorf("%q must be reserved (never shipped into the workspace)", r)
		}
	}
	shippable := []string{"NOTES.md", "run.py", "template.txt", "SKILL.mdx"}
	for _, s := range shippable {
		if isReservedSkillFile(s) {
			t.Errorf("%q is a legit file, must not be reserved", s)
		}
	}
}
