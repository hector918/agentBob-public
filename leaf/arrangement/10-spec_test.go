package arrangement

import "testing"

// TestParseSpecValid: a well-formed spec parses with ordering helpers working.
func TestParseSpecValid(t *testing.T) {
	sp, err := parseSpec(`{"buckets":[
		{"role":"taobao","content":"读取商品信息"},
		{"role":"reviewer","content":"按要求验证"},
		{"role":"wordpress","content":"写成文章发布"}
	]}`)
	if err != nil {
		t.Fatalf("valid spec errored: %v", err)
	}
	if sp.entryRole() != "taobao" {
		t.Errorf("entryRole=%q want taobao", sp.entryRole())
	}
	if got := sp.nextRole("taobao"); got != "reviewer" {
		t.Errorf("nextRole(taobao)=%q want reviewer", got)
	}
	if got := sp.prevRole("reviewer"); got != "taobao" {
		t.Errorf("prevRole(reviewer)=%q want taobao", got)
	}
	if got := sp.nextRole("wordpress"); got != "" {
		t.Errorf("nextRole(terminal)=%q want empty", got)
	}
	if got := sp.prevRole("taobao"); got != "" {
		t.Errorf("prevRole(entry)=%q want empty", got)
	}
	if got := sp.contentFor("reviewer"); got != "按要求验证" {
		t.Errorf("contentFor(reviewer)=%q", got)
	}
}

// TestParseSpecValidation: each malformed spec is rejected.
func TestParseSpecValidation(t *testing.T) {
	cases := map[string]string{
		"empty":          ``,
		"not json":       `not json`,
		"no buckets":     `{"buckets":[]}`,
		"bucket no role": `{"buckets":[{"content":"x"}]}`,
		"dup role":       `{"buckets":[{"role":"a"},{"role":"a"}]}`,
	}
	for name, raw := range cases {
		if _, err := parseSpec(raw); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

// TestClampTick bounds the interval.
func TestClampTick(t *testing.T) {
	if clampTick(minTick/2) != minTick {
		t.Error("below-min not clamped")
	}
	if clampTick(maxTick*2) != maxTick {
		t.Error("above-max not clamped")
	}
}
