package i18n

import (
	"reflect"
	"testing"
)

func TestDetect_PureHan(t *testing.T) {
	if got := Detect("你好世界"); got != "zh" {
		t.Errorf("pure Han: want zh, got %q", got)
	}
}

func TestDetect_PureAscii(t *testing.T) {
	if got := Detect("hello world"); got != "default" {
		t.Errorf("pure ascii: want default, got %q", got)
	}
}

func TestDetect_Mixed_HanWins(t *testing.T) {
	// 4 Han, 2 latin → han > latin → "zh"
	if got := Detect("你好世界 ok"); got != "zh" {
		t.Errorf("han-majority: want zh, got %q", got)
	}
}

func TestDetect_Mixed_LatinWins(t *testing.T) {
	// 1 Han, "hello world" = 10 latin → latin > han → "default"
	if got := Detect("hello world 你"); got != "default" {
		t.Errorf("latin-majority: want default, got %q", got)
	}
}

// No script signal (empty / digits / punctuation) → "" so the caller (the inbound
// flow's lang cascade) falls through to the override / per-sender memory / default.
func TestDetect_Empty(t *testing.T) {
	if got := Detect(""); got != "" {
		t.Errorf("empty: want \"\" (no signal), got %q", got)
	}
}

func TestDetect_Numbers(t *testing.T) {
	if got := Detect("12345"); got != "" {
		t.Errorf("numbers: want \"\" (no signal), got %q", got)
	}
}

func TestDetect_PunctuationOnly(t *testing.T) {
	if got := Detect("!!!???..."); got != "" {
		t.Errorf("punctuation: want \"\" (no signal), got %q", got)
	}
}

func TestChain_BareLang(t *testing.T) {
	if got := chain("zh"); !reflect.DeepEqual(got, []string{"zh", "default"}) {
		t.Errorf("bare zh: %v", got)
	}
}

func TestChain_Variant(t *testing.T) {
	if got := chain("zh-funky"); !reflect.DeepEqual(got, []string{"zh-funky", "zh", "default"}) {
		t.Errorf("variant: %v", got)
	}
}

func TestChain_DefaultAndEn(t *testing.T) {
	if got := chain("default"); !reflect.DeepEqual(got, []string{"default"}) {
		t.Errorf("default: %v", got)
	}
	if got := chain("en"); !reflect.DeepEqual(got, []string{"default"}) {
		t.Errorf("en alias: %v", got)
	}
	if got := chain(""); !reflect.DeepEqual(got, []string{"default"}) {
		t.Errorf("empty: %v", got)
	}
}
