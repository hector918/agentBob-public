package agora

import "testing"

// TestPrunableCap: clean-on-save (and the "已不可用" annotation) may only prune caps
// that liveCaps actually inventories — tool/skill. credential:use:* and any other
// kind have no catalog, so they must be PRESERVED (prunableCap=false), else the
// matrix editor silently drops a legitimate grant on save.
func TestPrunableCap(t *testing.T) {
	prunable := []string{"tool:use:read_file", "skill:use:arxiv"}
	preserve := []string{
		"credential:use:browser-profile", // the bug this guards
		"channel:use:something",          // any future kind
		"weird", "",
	}
	for _, c := range prunable {
		if !prunableCap(c) {
			t.Errorf("prunableCap(%q) = false, want true (tool/skill is inventoried)", c)
		}
	}
	for _, c := range preserve {
		if prunableCap(c) {
			t.Errorf("prunableCap(%q) = true, want false (no live inventory → must be preserved)", c)
		}
	}
}
