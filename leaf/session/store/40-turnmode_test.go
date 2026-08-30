package store_test

import (
	"context"
	"testing"

	"agentbob/contract"
)

// TestTurnMode_BirthStampAndImmutability (dev-pg): the scope default (/looping)
// is birth-stamped onto a session at creation and stays immutable — flipping the
// default afterwards affects only FUTURE sessions (docs/turn-driver-split.md).
func TestTurnMode_BirthStampAndImmutability(t *testing.T) {
	s, done := liveStore(t)
	defer done()
	ctx := context.Background()
	scope := uniqueScope("turnmode")

	// No prefs row → default regular, and a session born now stamps regular.
	if mode, err := s.GetScopeTurnMode(ctx, scope); err != nil || mode != contract.TurnModeRegular {
		t.Fatalf("virgin scope mode=%q err=%v, want regular/nil", mode, err)
	}
	sidA, _, err := s.EnsureSession(ctx, scope, "test", "u1", "")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := s.SessionByID(ctx, sidA); err != nil || info.TurnMode != contract.TurnModeRegular {
		t.Fatalf("session A stamp=%q err=%v, want regular", info.TurnMode, err)
	}

	// Flip the default to looping: the LIVE session keeps its birth stamp…
	if err := s.SetScopeTurnMode(ctx, scope, contract.TurnModeLooping); err != nil {
		t.Fatal(err)
	}
	if info, _ := s.SessionByID(ctx, sidA); info.TurnMode != contract.TurnModeRegular {
		t.Fatalf("session A stamp changed to %q after default flip — the stamp must be immutable", info.TurnMode)
	}
	// …and a NEW generation is born looping.
	sidB, err := s.NewGeneration(ctx, scope, "test", "u1", "")
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := s.SessionByID(ctx, sidB); info.TurnMode != contract.TurnModeLooping {
		t.Fatalf("session B stamp=%q, want looping (born after the flip)", info.TurnMode)
	}

	// SetScopeTurnMode on a scope that already has a sink-prefs row must not
	// clobber trace/stream (upsert touches turn_mode only).
	if err := s.SetScopePrefs(ctx, scope, contract.SinkPrefs{Trace: false, Stream: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScopeTurnMode(ctx, scope, contract.TurnModeRegular); err != nil {
		t.Fatal(err)
	}
	if prefs, _ := s.GetScopePrefs(ctx, scope); prefs.Trace || !prefs.Stream {
		t.Fatalf("sink prefs clobbered by SetScopeTurnMode: %+v", prefs)
	}
	if mode, _ := s.GetScopeTurnMode(ctx, scope); mode != contract.TurnModeRegular {
		t.Fatalf("scope default=%q, want regular after the second flip", mode)
	}
}
