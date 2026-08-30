package i18n

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestCatalog(entries map[string]Entry, overrides map[string]string) *Catalog {
	if overrides == nil {
		overrides = map[string]string{}
	}
	return &Catalog{entries: entries, overrides: overrides}
}

func TestT_DefaultLookup(t *testing.T) {
	c := newTestCatalog(map[string]Entry{
		"slash.hi": {"default": "Hello", "zh": "你好"},
	}, nil)
	if got := c.T("slash.hi", "default"); got != "Hello" {
		t.Errorf("default: %q", got)
	}
	if got := c.T("slash.hi", "zh"); got != "你好" {
		t.Errorf("zh: %q", got)
	}
}

func TestT_Fallback_VariantToBaseToDefault(t *testing.T) {
	c := newTestCatalog(map[string]Entry{
		"k": {"default": "D", "zh": "Z"},
	}, nil)
	// zh-funky absent → fall back to zh
	if got := c.T("k", "zh-funky"); got != "Z" {
		t.Errorf("zh-funky→zh: %q", got)
	}
}

func TestT_Fallback_BareToDefault(t *testing.T) {
	c := newTestCatalog(map[string]Entry{
		"k": {"default": "D"},
	}, nil)
	if got := c.T("k", "zh"); got != "D" {
		t.Errorf("zh→default: %q", got)
	}
}

func TestT_MissingKey_ReturnsKey(t *testing.T) {
	c := newTestCatalog(map[string]Entry{}, nil)
	if got := c.T("slash.bogus", "zh"); got != "slash.bogus" {
		t.Errorf("missing key: %q", got)
	}
}

func TestT_NoDefault_ReturnsKey(t *testing.T) {
	c := newTestCatalog(map[string]Entry{
		"k": {"zh": "Z"}, // no default
	}, nil)
	// "default" lang chain is just ["default"] — no fallback to zh, so
	// resolution misses and the key surfaces.
	if got := c.T("k", "default"); got != "k" {
		t.Errorf("no default: %q", got)
	}
}

func TestT_SprintfArgs(t *testing.T) {
	c := newTestCatalog(map[string]Entry{
		"k": {"default": "hi %s, %d msgs", "zh": "你好 %s，共 %d 条"},
	}, nil)
	if got := c.T("k", "default", "alice", 3); got != "hi alice, 3 msgs" {
		t.Errorf("default args: %q", got)
	}
	if got := c.T("k", "zh", "alice", 3); got != "你好 alice，共 3 条" {
		t.Errorf("zh args: %q", got)
	}
}

// TestOverride pins the admin per-chat variant pin (overrides.yaml). The full
// language RESOLUTION (override → Detect → per-sender memory → default) now lives in
// the inbound flow; i18n only exposes detection (see detect_test.go) + this override
// lookup. The old per-chat sticky MEMORY was deleted (it moved per-sender to accounts).
func TestOverride(t *testing.T) {
	c := newTestCatalog(map[string]Entry{}, map[string]string{"telegram:42": "zh-funky"})
	if got := c.override("telegram", "42"); got != "zh-funky" {
		t.Errorf("override pin = %q, want zh-funky", got)
	}
	// No pin → "" so the flow's cascade falls through to Detect / the per-sender seed.
	if got := c.override("telegram", "nopin"); got != "" {
		t.Errorf("no pin = %q, want empty", got)
	}
}

func TestLoad_OverlayWinsPerKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "i18n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Embedded catalog has _README + nothing else (skeleton state). Overlay
	// adds one key + overrides a hypothetical existing one. We exercise the
	// per-key merge by writing an overlay with both a fresh key and a
	// per-lang override.
	overlay := `{
  "demo.greeting": {"default": "Hi from overlay", "zh": "覆盖了"},
  "demo.only_zh": {"default": "fresh", "zh": "新增"}
}`
	if err := os.WriteFile(filepath.Join(dir, "i18n", "strings.json"), []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.T("demo.greeting", "default"); got != "Hi from overlay" {
		t.Errorf("overlay greeting default: %q", got)
	}
	if got := c.T("demo.greeting", "zh"); got != "覆盖了" {
		t.Errorf("overlay greeting zh: %q", got)
	}
	if got := c.T("demo.only_zh", "zh"); got != "新增" {
		t.Errorf("overlay only_zh zh: %q", got)
	}
}

func TestLoad_OverridesYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "i18n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ov := `chat_variants:
  "telegram:42": zh-funky
  "webui:admin": default
`
	if err := os.WriteFile(filepath.Join(dir, "i18n", "overrides.yaml"), []byte(ov), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.override("telegram", "42"); got != "zh-funky" {
		t.Errorf("override telegram:42: %q", got)
	}
	if got := c.override("webui", "admin"); got != "default" {
		t.Errorf("override webui:admin: %q", got)
	}
}

func TestLoad_MissingFilesOK(t *testing.T) {
	dir := t.TempDir()
	// No bobHome/i18n/ at all → embedded-only catalog, no error.
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("missing overlay should be ok: %v", err)
	}
	if c == nil {
		t.Fatal("nil catalog")
	}
}

func TestLoad_ParseErrorSurfaces(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "i18n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "i18n", "strings.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected parse error")
	}
}

// TestLoad_BrokenOverlayStillLoadsOverrides: a typo in the OPTIONAL strings.json
// overlay must cost only that overlay — the overrides.yaml per-chat variant pins
// still load. Regression guard for the early-return that used to bail out of Load
// before the overrides.yaml section ran, silently dropping every admin pin.
func TestLoad_BrokenOverlayStillLoadsOverrides(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "i18n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Broken overlay (not JSON) …
	if err := os.WriteFile(filepath.Join(dir, "i18n", "strings.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// … but a perfectly valid overrides.yaml alongside it.
	ov := `chat_variants:
  "telegram:42": zh-funky
`
	if err := os.WriteFile(filepath.Join(dir, "i18n", "overrides.yaml"), []byte(ov), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err == nil {
		t.Fatal("expected overlay parse error to surface")
	}
	if c == nil {
		t.Fatal("catalog must still be assembled despite the broken overlay")
	}
	// The pin from overrides.yaml must have loaded despite the overlay typo.
	if got := c.override("telegram", "42"); got != "zh-funky" {
		t.Errorf("override pin dropped by broken overlay: got %q, want zh-funky", got)
	}
}

// TestT_LiteralPercentNoArgs: a string with a literal % and NO args passes
// through verbatim (no Sprintf at all).
func TestT_LiteralPercentNoArgs(t *testing.T) {
	c := newTestCatalog(map[string]Entry{"k": {"default": "50% off deals"}}, nil)
	if got := c.T("k", "default"); got != "50% off deals" {
		t.Errorf("literal %% no-args: %q, want \"50%% off deals\"", got)
	}
}

// TestT_BadFormatFallsBackToRaw: when a string is NOT a valid format for the
// args (literal % treated as a stray verb, or arg-count mismatch), fmt embeds
// %!-markers — T must fall back to the raw catalog string, not leak the garbage.
func TestT_BadFormatFallsBackToRaw(t *testing.T) {
	// literal % + an arg → "50% off" is not a valid format string for one arg.
	c := newTestCatalog(map[string]Entry{"k": {"default": "50% off"}}, nil)
	if got := c.T("k", "default", "extra"); got != "50% off" {
		t.Errorf("bad-format fallback: %q, want raw \"50%% off\" (no %%! markers)", got)
	}
	// verb present but extra args → also falls back rather than leaking %!(EXTRA).
	c2 := newTestCatalog(map[string]Entry{"k": {"default": "hi %s"}}, nil)
	if got := c2.T("k", "default", "a", "b"); got != "hi %s" {
		t.Errorf("extra-args fallback: %q, want raw \"hi %%s\"", got)
	}
}

// TestT_ArgWithMarkerFormatsFine: an arg that itself contains "%!" (user- or
// provider-controlled data) must NOT trip the fmt-error sniff — formatting
// succeeded, so the expanded output stands rather than the raw template.
func TestT_ArgWithMarkerFormatsFine(t *testing.T) {
	c := newTestCatalog(map[string]Entry{"k": {"default": "bound %s"}}, nil)
	if got := c.T("k", "default", "weird%!name"); got != "bound weird%!name" {
		t.Errorf("marker-bearing arg: %q, want \"bound weird%%!name\"", got)
	}
}
