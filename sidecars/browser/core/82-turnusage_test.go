package core

import (
	"context"
	"sync"
	"testing"
)

// byKindSums sums In/Out across all kinds of the ByKind snapshot — the
// aggregation a reader would do over the per-kind breakdown.
func byKindSums(u *TurnUsage) (in, out int64) {
	for _, k := range u.ByKind() {
		in += k.In
		out += k.Out
	}
	return in, out
}

func TestTurnUsage_AddAccumulates(t *testing.T) {
	u := &TurnUsage{}
	u.Add(KindLLM, 100, 50)
	u.Add(KindLLM, 20, 10)
	if in, out := byKindSums(u); in != 120 || out != 60 {
		t.Fatalf("ByKind sums = (%d, %d), want (120, 60)", in, out)
	}
}

// TestTurnUsage_PerKind: ByKind() breaks the accumulation out per kind.
func TestTurnUsage_PerKind(t *testing.T) {
	u := &TurnUsage{}
	u.Add(KindLLM, 100, 50)
	u.Add(KindOCR, 30, 5)
	u.Add(KindLLM, 20, 10) // same kind accumulates
	if in, out := byKindSums(u); in != 150 || out != 65 {
		t.Fatalf("ByKind sums = (%d, %d), want (150, 65)", in, out)
	}
	bk := u.ByKind()
	if bk[KindLLM] != (KindTokens{In: 120, Out: 60}) {
		t.Errorf("ByKind[llm] = %+v, want {120, 60}", bk[KindLLM])
	}
	if bk[KindOCR] != (KindTokens{In: 30, Out: 5}) {
		t.Errorf("ByKind[ocr] = %+v, want {30, 5}", bk[KindOCR])
	}
	// Returned map is a snapshot the caller owns: mutating it must not affect u.
	bk[KindLLM] = KindTokens{}
	if u.ByKind()[KindLLM] != (KindTokens{In: 120, Out: 60}) {
		t.Error("ByKind returned a live map — caller mutation leaked into TurnUsage")
	}
}

func TestTurnUsage_NilSafe(t *testing.T) {
	var u *TurnUsage
	u.Add(KindLLM, 5, 5) // must not panic
	if bk := u.ByKind(); bk != nil {
		t.Fatalf("nil ByKind = %v, want nil", bk)
	}
	// TurnUsageFrom on a bare ctx → nil → Add is a no-op (the model-layer path
	// when no turn accumulator is set, e.g. a side-LLM call).
	TurnUsageFrom(context.Background()).Add(KindLLM, 1, 1)
}

func TestTurnUsage_CtxRoundTrip(t *testing.T) {
	u := &TurnUsage{}
	ctx := WithTurnUsage(context.Background(), u)
	got := TurnUsageFrom(ctx)
	if got != u {
		t.Fatal("TurnUsageFrom did not return the stashed accumulator")
	}
	got.Add(KindLLM, 7, 3)
	if in, out := byKindSums(u); in != 7 || out != 3 {
		t.Fatalf("via-ctx Add not reflected: (%d, %d)", in, out)
	}
}

func TestTurnUsage_Concurrent(t *testing.T) {
	u := &TurnUsage{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); u.Add(KindLLM, 2, 1) }()
	}
	wg.Wait()
	if in, out := byKindSums(u); in != 100 || out != 50 {
		t.Fatalf("concurrent ByKind sums = (%d, %d), want (100, 50)", in, out)
	}
}
