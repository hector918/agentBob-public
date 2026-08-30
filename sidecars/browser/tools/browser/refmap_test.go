package browser

import (
	"strings"
	"testing"
)

// TestTruncHref: short hrefs pass through; long tracking URLs are capped to a
// recognizable prefix (the model clicks by @e ref, not the URL).
func TestTruncHref(t *testing.T) {
	short := "//www.taobao.com/"
	if got := truncHref(short); got != short {
		t.Errorf("short href must pass through; got %q", got)
	}
	long := "https://click.simba.taobao.com/cc_im?p=%BF%DA%BA%EC&s=1989391411&k=2361&" + strings.Repeat("x", 300)
	got := truncHref(long)
	if len([]rune(got)) > maxHrefChars+1 { // +1 for the … rune
		t.Errorf("long href not capped: %d runes", len([]rune(got)))
	}
	if !strings.HasPrefix(got, "https://click.simba.taobao.com/cc_im") {
		t.Errorf("cap must keep the recognizable prefix; got %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated href should be marked with …; got %q", got)
	}
}

// TestRenderRefList_PrunesHref: the rendered snapshot line keeps the @e ref +
// label but the long href is pruned.
func TestRenderRefList_PrunesHref(t *testing.T) {
	items := []interactive{
		{Ref: "@e1", Tag: "a", Text: "口红", Href: "https://click.simba.taobao.com/cc_im?p=" + strings.Repeat("y", 400)},
	}
	out := renderRefList(items)
	if !strings.Contains(out, "[@e1]") || !strings.Contains(out, `"口红"`) {
		t.Errorf("ref + label must survive; got %q", out)
	}
	if strings.Contains(out, strings.Repeat("y", 100)) {
		t.Error("long href tail must be pruned out of the snapshot")
	}
}
