package config

import "testing"

// TestResolveCastTier_LoadCeilingClamps locks the agreed degrade policy: the
// requested tier is honoured only up to the backend's load ceiling (>=4 active
// streams → low, >=3 → med), effective = min(requested, ceiling). Quality is
// constant across tiers (text legibility); only resolution + frame rate move.
func TestResolveCastTier_LoadCeilingClamps(t *testing.T) {
	var b BrowserConfig // all defaults: quality 50, everyNth 3, maxWidth 1024

	cases := []struct {
		name     string
		tier     string
		active   int
		wantTier string
		wantNth  int
		wantMaxW int
	}{
		{"high, idle → high (native, fast)", "high", 1, "high", 2, 0},
		{"med, idle → med", "med", 1, "med", 3, 1024},
		{"low, idle → low", "low", 1, "low", 5, 768},
		{"unknown tier → med", "bogus", 1, "med", 3, 1024},
		{"empty tier → med", "", 1, "med", 3, 1024},
		{"high but 3 active → clamped to med", "high", 3, "med", 3, 1024},
		{"high but 4 active → clamped to low", "high", 4, "low", 5, 768},
		{"med but 4 active → clamped to low", "med", 4, "low", 5, 768},
		{"low never upgraded by light load", "low", 1, "low", 5, 768},
		// "slow" is manual-only: med resolution (1024), everyNth=1 (capture every frame —
		// the 5s wall-clock gate is the sole limiter; everyNth>1 starves capture on quiet
		// pages), and NOT subject to the load ceiling (honored as-is even at max active streams).
		{"slow at idle → slow (med res)", "slow", 1, "slow", 1, 1024},
		{"slow ignores load ceiling", "slow", 4, "slow", 1, 1024},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			effTier, q, nth, maxW, maxH := b.ResolveCastTier(c.tier, c.active)
			if effTier != c.wantTier {
				t.Errorf("effTier = %q, want %q", effTier, c.wantTier)
			}
			if q != 50 {
				t.Errorf("quality = %d, want constant 50", q)
			}
			if nth != c.wantNth {
				t.Errorf("everyNth = %d, want %d", nth, c.wantNth)
			}
			if maxW != c.wantMaxW {
				t.Errorf("maxW = %d, want %d", maxW, c.wantMaxW)
			}
			if maxH != maxW {
				t.Errorf("maxH = %d, want == maxW (%d)", maxH, maxW)
			}
		})
	}
}

func TestTakeoverDefaults(t *testing.T) {
	var b BrowserConfig
	if got := b.MaxTakeoverStreamsEff(); got != 4 {
		t.Errorf("MaxTakeoverStreamsEff default = %d, want 4", got)
	}
	if got := b.TakeoverMaxWidthEff(); got != 1024 {
		t.Errorf("TakeoverMaxWidthEff default = %d, want 1024", got)
	}
	if got := b.TierMinIntervalMs("slow"); got != 5000 {
		t.Errorf("TierMinIntervalMs(slow) = %d, want 5000", got)
	}
	for _, tr := range []string{"high", "med", "low", ""} {
		if got := b.TierMinIntervalMs(tr); got != 0 {
			t.Errorf("TierMinIntervalMs(%q) = %d, want 0 (no gate)", tr, got)
		}
	}
}
