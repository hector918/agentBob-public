package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbob/contract"
)

// TestTriggersLenientParse: a triggers field never fails frontmatter parse — list, scalar,
// and CSV-scalar (the natural mistake) all parse, and after cleanTriggers yield the expected
// set; an odd value leaves triggers empty but the skill still parses (not dropped).
func TestTriggersLenientParse(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{"list", "---\nname: x\ntriggers: [超声, ultrasound]\n---\nb\n", []string{"超声", "ultrasound"}},
		{"block", "---\nname: x\ntriggers:\n  - 超声\n  - ultrasound\n---\nb\n", []string{"超声", "ultrasound"}},
		{"csv-scalar", "---\nname: x\ntriggers: \"超声, ultrasound\"\n---\nb\n", []string{"超声", "ultrasound"}},
		{"single", "---\nname: x\ntriggers: ocr\n---\nb\n", []string{"ocr"}},
		{"map-ignored", "---\nname: x\ntriggers: {a: b}\n---\nb\n", nil},
		{"none", "---\nname: x\n---\nb\n", nil},
	}
	for _, c := range cases {
		fm, _, err := parseSkillMD([]byte(c.yaml))
		if err != nil {
			t.Fatalf("%s: parse failed (skill would be dropped): %v", c.name, err)
		}
		if got := cleanTriggers(fm.Triggers); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: triggers = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseSkillMD(t *testing.T) {
	fm, body, err := parseSkillMD([]byte("---\nname: x\ndescription: d\n---\nthe body\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fm.Name != "x" || fm.Description != "d" {
		t.Fatalf("frontmatter = %+v", fm)
	}
	if body != "the body\n" {
		t.Errorf("body = %q, want \"the body\\n\"", body)
	}
	// No frontmatter → error.
	if _, _, err := parseSkillMD([]byte("no frontmatter")); err == nil {
		t.Error("missing frontmatter should error")
	}
	// Unclosed frontmatter → error.
	if _, _, err := parseSkillMD([]byte("---\nname: x\nbody without close")); err == nil {
		t.Error("unclosed frontmatter should error")
	}
}

// TestBuiltinLoads asserts the embedded built-in skill is scanned (Origin internal).
func TestBuiltinLoads(t *testing.T) {
	c := NewCatalog(t.TempDir()) // no external dir
	if err := c.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	sk, ok := c.Get("summarize")
	if !ok {
		t.Fatal("built-in summarize skill should load from embed")
	}
	if sk.Origin != contract.OriginInternal {
		t.Errorf("summarize = %+v, want internal", sk.SkillInfo)
	}
	if body, ok := c.Read("summarize"); !ok || body == "" {
		t.Error("summarize body should be readable and non-empty")
	}
}

// TestExternalOverridesAndChecks covers external loading, external-over-internal,
// the name==dirname check, and RUBRIC.md loading (present / absent / oversized).
func TestExternalOverridesAndChecks(t *testing.T) {
	home := t.TempDir()
	ext := filepath.Join(home, "skills", "external")

	write := func(dir, name, content string) {
		d := filepath.Join(ext, dir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// An external skill that overrides the built-in summarize.
	write("summarize", "SKILL.md", "---\nname: summarize\ndescription: external version\n---\nexternal body\n")
	// A skill carrying a RUBRIC.md → Skill.Rubric is populated.
	write("ultrasound", "SKILL.md", "---\nname: ultrasound\ndescription: 超声报告\n---\n报告模板...\n")
	write("ultrasound", "RUBRIC.md", "## pass\n产出 ⊆ OCR 原文。")
	// A skill whose RUBRIC.md exceeds the cap → dropped (no gate).
	write("bigrubric", "SKILL.md", "---\nname: bigrubric\ndescription: x\n---\nb\n")
	write("bigrubric", "RUBRIC.md", strings.Repeat("x", maxRubricBytes+1))
	// A name-mismatch skill that must be rejected.
	write("evil", "SKILL.md", "---\nname: not-evil\ndescription: spoof\n---\nbody\n")

	c := NewCatalog(home)
	if err := c.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	sk, _ := c.Get("summarize")
	if sk.Origin != contract.OriginExternal {
		t.Error("external summarize should override the built-in")
	}
	if body, _ := c.Read("summarize"); body != "external body" {
		t.Errorf("summarize body = %q, want external", body)
	}
	if sk.Rubric != "" {
		t.Errorf("summarize ships no RUBRIC.md, want empty rubric, got %q", sk.Rubric)
	}

	us, ok := c.Get("ultrasound")
	if !ok || !strings.Contains(us.Rubric, "产出 ⊆ OCR 原文") {
		t.Errorf("ultrasound rubric not loaded: %q", us.Rubric)
	}
	if big, _ := c.Get("bigrubric"); big.Rubric != "" {
		t.Errorf("oversized RUBRIC.md must be dropped, got %d bytes", len(big.Rubric))
	}

	if _, ok := c.Get("not-evil"); ok {
		t.Error("name!=dirname skill must be rejected")
	}
	if _, ok := c.Get("evil"); ok {
		t.Error("name-mismatch skill must not load under its dirname either")
	}
}

// TestAddendumProjection: a learned addendum (what skillTarget.Apply stores)
// rides at the tail of the skill body returned by Read, and survives a Reload.
func TestAddendumProjection(t *testing.T) {
	c := NewCatalog(t.TempDir()) // no external dir → just the embedded builtins
	if err := c.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	base, ok := c.Read("summarize")
	if !ok {
		t.Fatal("builtin skill 'summarize' missing")
	}
	c.setAddendum(contract.OriginInternal, "summarize", "LEARNED: 优先列结论。") // builtin = internal origin
	got, _ := c.Read("summarize")
	if got == base || !contains(got, "LEARNED: 优先列结论。") {
		t.Fatalf("addendum not appended to body:\n%s", got)
	}
	// A reload rescans skills but must NOT drop learned addenda.
	if err := c.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := c.Read("summarize"); !contains(got, "LEARNED: 优先列结论。") {
		t.Error("addendum lost across reload")
	}
}

// TestAddendumKeyedByOrigin: an addendum set against the INTERNAL origin must NOT
// surface on a same-named EXTERNAL skill. The catalog keys live skills by name
// (external overrides internal), so Read resolves the external skill and looks its
// addendum up under the external origin — finding none.
func TestAddendumKeyedByOrigin(t *testing.T) {
	home := t.TempDir()
	// Plant an external skill named "summarize" that overrides the builtin one.
	ext := filepath.Join(home, "skills", "external", "summarize")
	if err := os.MkdirAll(ext, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: summarize\ndescription: external override\n---\nEXTERNAL BODY\n"
	if err := os.WriteFile(filepath.Join(ext, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewCatalog(home)
	if err := c.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	sk, ok := c.Get("summarize")
	if !ok || sk.Origin != contract.OriginExternal {
		t.Fatalf("external 'summarize' should win the name clash, origin=%v ok=%v", sk.Origin, ok)
	}

	// Set a note keyed to the INTERNAL origin (the now-shadowed builtin).
	c.setAddendum(contract.OriginInternal, "summarize", "INTERNAL NOTE — must not leak.")
	got, _ := c.Read("summarize")
	if contains(got, "INTERNAL NOTE") {
		t.Fatalf("internal-origin addendum leaked onto the external skill:\n%s", got)
	}
	// A note keyed to the resolved (external) origin DOES surface.
	c.setAddendum(contract.OriginExternal, "summarize", "EXTERNAL NOTE — applies.")
	got, _ = c.Read("summarize")
	if !contains(got, "EXTERNAL NOTE") {
		t.Fatalf("external-origin addendum should apply to the external skill:\n%s", got)
	}
}

// TestBrokenExternalKeepsAddendum: a present-but-invalid external SKILL.md (mid-edit
// broken frontmatter) is skipped on Reload — but, mirroring the failure-snapshot `seen`
// guard, its learned addendum must survive the round-trip and onPrune must NOT fire (else
// external/<name>/INSIGHT.md is deleted). Fixing the frontmatter and reloading restores
// the skill WITH its memory intact.
func TestBrokenExternalKeepsAddendum(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "skills", "external", "ultrasound")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := filepath.Join(dir, "SKILL.md")
	good := "---\nname: ultrasound\ndescription: 超声报告\n---\nbody\n"
	if err := os.WriteFile(md, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewCatalog(home)
	var pruned []addendaKey
	c.SetAddendumPruner(func(o contract.SkillOrigin, name string) {
		pruned = append(pruned, addendaKey{o, name})
	})
	if err := c.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	c.setAddendum(contract.OriginExternal, "ultrasound", "LEARNED — must survive a broken edit.")

	// Break the frontmatter (no closing ---) → skipped as invalid, but the SKILL.md
	// still exists on disk (seen).
	if err := os.WriteFile(md, []byte("---\nname: ultrasound\nbroken, no close\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); err != nil {
		t.Fatalf("reload after break: %v", err)
	}
	if _, ok := c.Get("ultrasound"); ok {
		t.Fatal("broken-frontmatter skill must be skipped as invalid")
	}
	if c.addendum(contract.OriginExternal, "ultrasound") == "" {
		t.Fatal("a mid-edit-broken external skill must keep its addendum (seen guard)")
	}
	for _, k := range pruned {
		if k.origin == contract.OriginExternal && k.name == "ultrasound" {
			t.Fatal("onPrune fired for a still-present-but-invalid external skill — INSIGHT.md would be deleted")
		}
	}

	// Fix the frontmatter → the skill returns WITH its learned note appended.
	if err := os.WriteFile(md, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); err != nil {
		t.Fatalf("reload after fix: %v", err)
	}
	if body, _ := c.Read("ultrasound"); !contains(body, "LEARNED — must survive a broken edit.") {
		t.Fatalf("restored skill lost its addendum:\n%s", body)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestDegradedFlag: loadBuiltinsOnly marks the catalog as serving a known-incomplete
// set; a later successful Reload clears it (warrant's grant-row reconcile reads this
// via the optional Degraded() type-assert to skip pruning in the degraded window).
func TestDegradedFlag(t *testing.T) {
	c := NewCatalog(t.TempDir()) // no external dir — Reload succeeds on builtins
	if err := c.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c.Degraded() {
		t.Fatal("a successful reload must not leave the catalog degraded")
	}
	c.loadBuiltinsOnly()
	if !c.Degraded() {
		t.Fatal("the builtins-only fallback must mark the catalog degraded")
	}
	if err := c.Reload(); err != nil {
		t.Fatalf("reload after fallback: %v", err)
	}
	if c.Degraded() {
		t.Fatal("a successful reload must clear the degraded mark")
	}
}
