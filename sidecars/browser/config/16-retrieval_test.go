package config

import "testing"

// TestRetrievalConfiguredEff guards the single live-feed predicate shared by
// the write side (enqueue sink), drainer, recall tool and /recall slash. If
// the write side gated on EnabledEff alone while the readers also require a
// base_url, an enabled-but-base_url-empty config grows bob_retrieval_outbox
// unbounded with no reader. ConfiguredEff must be true ONLY when both hold.
func TestRetrievalConfiguredEff(t *testing.T) {
	tr := true
	fa := false
	cases := []struct {
		name    string
		enabled *bool
		baseURL string
		want    bool
	}{
		{"enabled+base_url", &tr, "http://leaf:8000", true},
		{"enabled+no base_url", &tr, "", false}, // the outbox-growth trap
		{"disabled+base_url", &fa, "http://leaf:8000", false},
		{"nil enabled+base_url", nil, "http://leaf:8000", false},
		{"nil enabled+no base_url", nil, "", false},
	}
	for _, c := range cases {
		r := RetrievalConfig{Enabled: c.enabled, BaseURL: c.baseURL}
		if got := r.ConfiguredEff(); got != c.want {
			t.Errorf("%s: ConfiguredEff() = %v, want %v", c.name, got, c.want)
		}
	}
}
