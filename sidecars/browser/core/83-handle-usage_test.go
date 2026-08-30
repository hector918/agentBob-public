package core_test

import (
	"encoding/json"
	"testing"
	"time"

	"agentbob/sidecars/browser/core"
)

// tok is a one-kind tokens map shorthand for the tests.
func tok(kind string, in, out int64) map[string]core.KindTokens {
	return map[string]core.KindTokens{kind: {In: in, Out: out}}
}

// TestTurnUsage_Tiers: a bump older than 3 months downsamples to a month key on
// the next bump; older than 3 years to a year key. Per-kind tokens are preserved
// (lossless) across the fold, both in the grand total and per-kind.
func TestTurnUsage_Tiers(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

	blob, err := core.BumpTurnUsage("", 1, 1, tok("llm", 10, 5), nil, now.AddDate(0, -4, 0))
	if err != nil {
		t.Fatal(err)
	}
	blob, err = core.BumpTurnUsage(blob, 2, 1, tok("llm", 20, 8), nil, now)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["2026-02-06"]; ok {
		t.Errorf("4-month-old daily key should have folded into a month key: %v", keysOf(m))
	}
	if _, ok := m["2026-02"]; !ok {
		t.Errorf("expected a 2026-02 month key after folding; got %v", keysOf(m))
	}
	turns, success, in, out := core.SumHandleUsage(blob)
	if turns != 3 || success != 2 || in != 30 || out != 13 {
		t.Fatalf("sum=(%d %d %d %d), want (3 2 30 13) — lossless across the fold", turns, success, in, out)
	}
	if bk := core.SumTokensByKind(blob); bk["llm"] != (core.KindTokens{In: 30, Out: 13}) {
		t.Fatalf("SumTokensByKind[llm]=%+v, want {30,13}", bk["llm"])
	}

	// 4 years ago → folds to a YEAR key.
	old, _ := core.BumpTurnUsage("", 1, 1, tok("llm", 1, 1), nil, now.AddDate(-4, 0, 0))
	old, _ = core.BumpTurnUsage(old, 1, 1, tok("llm", 1, 1), nil, now)
	ym := map[string]json.RawMessage{}
	_ = json.Unmarshal([]byte(old), &ym)
	if _, ok := ym["2022"]; !ok {
		t.Errorf("4-year-old bucket should have folded into year key 2022; got %v", keysOf(ym))
	}
}

// TestTurnUsage_PerKind: distinct kinds accumulate separately; the grand total
// is their sum; tokens and native units are DISJOINT (neither pollutes the
// other's sum).
func TestTurnUsage_PerKind(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	blob, _ := core.BumpTurnUsage("", 1, 1, map[string]core.KindTokens{
		"llm": {In: 100, Out: 50}, "ocr": {In: 30, Out: 5},
	}, map[string]int64{"asr:s": 12}, now)
	blob, _ = core.BumpTurnUsage(blob, 1, 1, tok("llm", 20, 10), map[string]int64{"asr:s": 8}, now)

	bk := core.SumTokensByKind(blob)
	if bk["llm"] != (core.KindTokens{In: 120, Out: 60}) {
		t.Errorf("llm=%+v, want {120,60}", bk["llm"])
	}
	if bk["ocr"] != (core.KindTokens{In: 30, Out: 5}) {
		t.Errorf("ocr=%+v, want {30,5}", bk["ocr"])
	}
	// Grand total = sum of all kinds.
	if _, _, in, out := core.SumHandleUsage(blob); in != 150 || out != 65 {
		t.Fatalf("grand total=(%d,%d), want (150,65)", in, out)
	}
	// Native disjoint: asr seconds accumulate in V, NOT in the token totals.
	if svc := core.SumServiceUsage(blob); svc["asr:s"] != 20 {
		t.Fatalf("asr:s=%d, want 20", svc["asr:s"])
	}
	if _, _, in, _ := core.SumHandleUsage(blob); in != 150 {
		t.Fatalf("native usage leaked into token total: in=%d, want 150", in)
	}
	// A native-only bump leaves token sums untouched and vice versa.
	nb, _ := core.BumpTurnUsage("", 0, 0, nil, map[string]int64{"tts:c": 200}, now)
	if _, _, in, out := core.SumHandleUsage(nb); in|out != 0 {
		t.Fatalf("native-only blob has nonzero token totals (%d,%d)", in, out)
	}
	if core.SumServiceUsage(nb)["tts:c"] != 200 {
		t.Fatal("tts:c not recorded")
	}
}

// TestTurnUsage_LegacyBlob: a blob written by the pre-per-kind code (aggregate
// i/o, no k section) still sums into the grand total; a mixed old+new blob sums
// both.
func TestTurnUsage_LegacyBlob(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	legacy := `{"2026-06-06":{"t":1,"s":1,"i":40,"o":20}}`
	if turns, _, in, out := core.SumHandleUsage(legacy); turns != 1 || in != 40 || out != 20 {
		t.Fatalf("legacy blob sum=(%d _ %d %d), want turns=1 in=40 out=20", turns, in, out)
	}
	// New per-kind bump on top of the legacy blob → grand total adds both.
	mixed, _ := core.BumpTurnUsage(legacy, 1, 1, tok("ocr", 10, 5), nil, now)
	if _, _, in, out := core.SumHandleUsage(mixed); in != 50 || out != 25 {
		t.Fatalf("mixed legacy+per-kind total=(%d,%d), want (50,25)", in, out)
	}
	// SumTokensByKind only reports the kinded part (legacy i/o has no kind).
	if bk := core.SumTokensByKind(mixed); bk["ocr"] != (core.KindTokens{In: 10, Out: 5}) {
		t.Fatalf("SumTokensByKind[ocr]=%+v, want {10,5}", bk["ocr"])
	}
}

// TestTurnUsage_LosslessAndBounded: 40 years of weekly bumps stays bounded while
// the per-kind sum equals the number of bumps exactly.
func TestTurnUsage_LosslessAndBounded(t *testing.T) {
	base := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	blob := ""
	var n int64
	for w := 0; w < 40*52; w++ {
		b, err := core.BumpTurnUsage(blob, 1, 1, tok("llm", 10, 5), map[string]int64{"asr:s": 2}, base.AddDate(0, 0, w*7))
		if err != nil {
			t.Fatalf("bump %d: %v", w, err)
		}
		blob = b
		n++
	}
	turns, success, in, out := core.SumHandleUsage(blob)
	if turns != n || success != n || in != n*10 || out != n*5 {
		t.Fatalf("sum=(%d %d %d %d), want turns=%d (lossless over 40y)", turns, success, in, out, n)
	}
	if core.SumServiceUsage(blob)["asr:s"] != n*2 {
		t.Fatalf("asr:s=%d, want %d (lossless)", core.SumServiceUsage(blob)["asr:s"], n*2)
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatal(err)
	}
	if len(m) > 200 {
		t.Fatalf("key count = %d after 40y of weekly bumps — downsampling not bounding it", len(m))
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
