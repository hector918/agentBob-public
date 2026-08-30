package model

import (
	"errors"
	"strings"
	"testing"

	"agentbob/contract"
)

// keyJoin renders a chain of tag-sets as a stable string for comparison.
func keyJoin(chain [][]string) string {
	parts := make([]string, len(chain))
	for i, s := range chain {
		parts[i] = tagSetKey(s)
	}
	return strings.Join(parts, " | ")
}

func TestFallbackChain(t *testing.T) {
	rules := []FallbackRule{
		{Tags: []string{"vision"}, To: []string{"smart"}},
		{Tags: []string{"smart"}, To: []string{"small"}},
	}
	cases := []struct {
		name     string
		requires []string
		rules    []FallbackRule
		want     [][]string
	}{
		{"multi-hop chain", []string{"vision"}, rules,
			[][]string{{"vision"}, {"smart"}, {"small"}}},
		{"swap trigger keep rest", []string{"vision", "zh"}, rules,
			[][]string{{"vision", "zh"}, {"zh", "smart"}, {"zh", "small"}}},
		{"no rule matches", []string{"x"}, rules,
			[][]string{{"x"}}},
		{"no rules at all", []string{"vision"}, nil,
			[][]string{{"vision"}}},
		{"empty requires", nil, rules,
			[][]string{nil}},
		{"cycle terminates", []string{"a"},
			[]FallbackRule{{Tags: []string{"a"}, To: []string{"b"}}, {Tags: []string{"b"}, To: []string{"a"}}},
			[][]string{{"a"}, {"b"}}},
		{"empty To is skipped", []string{"vision"},
			[]FallbackRule{{Tags: []string{"vision"}, To: nil}},
			[][]string{{"vision"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fallbackChain(tc.requires, tc.rules)
			if keyJoin(got) != keyJoin(tc.want) {
				t.Fatalf("chain = %v, want %v", got, tc.want)
			}
		})
	}
}

// fbEntry builds an eligible LLM entry with the given tags.
func fbEntry(name string, tags ...string) *entryRow {
	return &entryRow{info: contract.ModelInfo{
		Name: name, Kind: contract.KindLLM, Model: name + "-model", Tags: tags,
	}}
}

func withFallback(p *MultiPool, rules ...FallbackRule) {
	p.state.Load().fallback = rules
}

// TestPickTagFallback: a request requiring a tag with NO eligible entry falls
// back to the rule's target tag.
func TestPickTagFallback(t *testing.T) {
	smart := fbEntry("s", "smart")
	p := newTestPool(smart)
	defer p.Close()
	withFallback(p, FallbackRule{Tags: []string{"vision"}, To: []string{"smart"}})

	got, err := p.pick(contract.ModelRequest{Requires: []string{"vision"}}, nil)
	if err != nil {
		t.Fatalf("expected tag-fallback to find the smart entry, got err: %v", err)
	}
	if got != smart {
		t.Fatalf("fell back to %v, want the smart entry", got.info.Name)
	}
}

// TestPickTagFallbackPrimaryWins: when an eligible primary-tag entry exists, the
// fallback is NOT used even though a rule is configured.
func TestPickTagFallbackPrimaryWins(t *testing.T) {
	vision := fbEntry("v", "vision")
	smart := fbEntry("s", "smart")
	p := newTestPool(vision, smart)
	defer p.Close()
	withFallback(p, FallbackRule{Tags: []string{"vision"}, To: []string{"smart"}})

	got, err := p.pick(contract.ModelRequest{Requires: []string{"vision"}}, nil)
	if err != nil || got != vision {
		t.Fatalf("primary vision entry should win; got %v err %v", got, err)
	}
}

// TestPickTagFallbackOnPausedPrimary: a paused (ineligible) primary triggers
// fallback — "no AVAILABLE model" is dynamic, not just tag-absent.
func TestPickTagFallbackOnPausedPrimary(t *testing.T) {
	vision := fbEntry("v", "vision")
	vision.paused = true // ineligible right now
	smart := fbEntry("s", "smart")
	p := newTestPool(vision, smart)
	defer p.Close()
	withFallback(p, FallbackRule{Tags: []string{"vision"}, To: []string{"smart"}})

	got, err := p.pick(contract.ModelRequest{Requires: []string{"vision"}}, nil)
	if err != nil || got != smart {
		t.Fatalf("paused primary should fall back to smart; got %v err %v", got, err)
	}
}

// TestPickNoFallbackConfigured: without a rule, an unsatisfiable required tag
// still errors with ErrNoModelAvailable (no behavior change).
func TestPickNoFallbackConfigured(t *testing.T) {
	smart := fbEntry("s", "smart")
	p := newTestPool(smart)
	defer p.Close()

	_, err := p.pick(contract.ModelRequest{Requires: []string{"vision"}}, nil)
	if !errors.Is(err, ErrNoModelAvailable) {
		t.Fatalf("want ErrNoModelAvailable, got %v", err)
	}
}
