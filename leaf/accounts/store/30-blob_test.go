package store

import (
	"testing"
	"time"
)

// TestBumpAndSum: a fresh blob accumulates today's turn, and the lifetime sums
// (grand total + per-kind) read it back exactly.
func TestBumpAndSum(t *testing.T) {
	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	blob, err := BumpTurnUsage("", 1, 1,
		map[string]KindTokens{"llm": {In: 100, Out: 200}, "ocr": {In: 20, Out: 0}},
		map[string]int64{"asr:s": 45}, now)
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	// Second turn same day, llm only, failed (success=0).
	blob, err = BumpTurnUsage(blob, 1, 0, map[string]KindTokens{"llm": {In: 50, Out: 30}}, nil, now)
	if err != nil {
		t.Fatalf("bump 2: %v", err)
	}

	turns, success, in, out := SumHandleUsage(blob)
	if turns != 2 || success != 1 {
		t.Errorf("turns/success = %d/%d, want 2/1", turns, success)
	}
	// grand total tokens = llm(150/230) + ocr(20/0)
	if in != 170 || out != 230 {
		t.Errorf("grand tokens in/out = %d/%d, want 170/230", in, out)
	}
	byKind := SumTokensByKind(blob)
	if byKind["llm"] != (KindTokens{In: 150, Out: 230}) {
		t.Errorf("llm = %+v, want {150 230}", byKind["llm"])
	}
	if byKind["ocr"] != (KindTokens{In: 20, Out: 0}) {
		t.Errorf("ocr = %+v, want {20 0}", byKind["ocr"])
	}
	if svc := SumServiceUsage(blob); svc["asr:s"] != 45 {
		t.Errorf("asr:s = %d, want 45", svc["asr:s"])
	}
}

// TestCompaction: a daily bucket older than 3 months folds into its monthly key
// (lossless) while a recent daily bucket stays daily.
func TestCompaction(t *testing.T) {
	now := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, -5, 0) // 5 months ago → should compact to monthly

	blob, _ := BumpTurnUsage("", 3, 3, map[string]KindTokens{"llm": {In: 10, Out: 20}}, nil, old)
	// A fresh turn today triggers compaction of the old daily bucket.
	blob, _ = BumpTurnUsage(blob, 1, 1, map[string]KindTokens{"llm": {In: 1, Out: 2}}, nil, now)

	m, err := parseUsage(blob)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := m[old.Format("2006-01-02")]; ok {
		t.Errorf("old daily key still present — should have compacted to monthly")
	}
	if _, ok := m[old.Format("2006-01")]; !ok {
		t.Errorf("old monthly key missing — compaction lost the bucket")
	}
	if _, ok := m[now.Format("2006-01-02")]; !ok {
		t.Errorf("today's daily key missing")
	}
	// Lossless: totals survive compaction.
	turns, success, in, out := SumHandleUsage(blob)
	if turns != 4 || success != 4 || in != 11 || out != 22 {
		t.Errorf("post-compaction totals = %d/%d/%d/%d, want 4/4/11/22", turns, success, in, out)
	}
}

// TestMalformedBlob: a garbage blob sums to zero on read, and a bump FAILS OPEN —
// resetting to a fresh valid blob (no error) so the corrupt row self-heals on the next
// write, matching the read seams (rather than latching the write error forever).
func TestMalformedBlob(t *testing.T) {
	if turns, _, _, _ := SumHandleUsage("{not json"); turns != 0 {
		t.Errorf("malformed blob summed non-zero")
	}
	out, err := BumpTurnUsage("{not json", 1, 1, nil, nil, time.Now())
	if err != nil {
		t.Errorf("bump on malformed blob should fail open (no error), got %v", err)
	}
	if out == "{not json" || out == "" {
		t.Errorf("bump on malformed blob should reset to a fresh valid blob, got %q", out)
	}
	// The reset blob is valid and reflects the bumped turn.
	if turns, _, _, _ := SumHandleUsage(out); turns != 1 {
		t.Errorf("reset blob should record the bumped turn, got turns=%d", turns)
	}
}

// TestAddBlobRollup: AccountUsage.AddBlob folds multiple handles' blobs into one
// rollup.
func TestAddBlobRollup(t *testing.T) {
	now := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)
	h1, _ := BumpTurnUsage("", 2, 2, map[string]KindTokens{"llm": {In: 100, Out: 100}}, nil, now)
	h2, _ := BumpTurnUsage("", 1, 0, map[string]KindTokens{"llm": {In: 10, Out: 5}, "ocr": {In: 3, Out: 0}}, nil, now)

	var u AccountUsage
	u.AddBlob(h1)
	u.AddBlob(h2)
	if u.Turns != 3 || u.Success != 2 {
		t.Errorf("rollup turns/success = %d/%d, want 3/2", u.Turns, u.Success)
	}
	if u.TokensByKind["llm"] != (KindTokens{In: 110, Out: 105}) {
		t.Errorf("rollup llm = %+v, want {110 105}", u.TokensByKind["llm"])
	}
	if u.TokensByKind["ocr"] != (KindTokens{In: 3, Out: 0}) {
		t.Errorf("rollup ocr = %+v, want {3 0}", u.TokensByKind["ocr"])
	}
}
