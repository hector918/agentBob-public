// Tests for the passive liveness breaker's consecutive-fail rule: a backend
// that fails every call must be marked dead even on a low-traffic deployment
// where errors land minutes apart and never fill livenessWindow (the
// time-window rule needs >= livenessFailThreshold fails inside 60s).
//
// White-box (package model) — recordError / recordSuccess / isEligible are
// unexported.
package model

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConsecutiveFailsMarkDead(t *testing.T) {
	p := newTestPool()
	defer p.Close()
	row := entry("x", 0, &fakeChatter{})
	boom := errors.New("backend down")

	// One failure — below consecutiveFailThreshold, still eligible.
	p.recordError(context.Background(), row, boom)
	if !row.IsEligible() {
		t.Fatal("a single failure must not mark the entry dead")
	}
	// consecutiveFailThreshold failures in a row — marked dead, with no
	// dependency on how far apart the errors landed (the time window is
	// irrelevant here — only two errors total, never >= livenessFailThreshold).
	p.recordError(context.Background(), row, boom)
	if row.IsEligible() {
		t.Fatalf("%d consecutive failures must mark the entry dead", consecutiveFailThreshold)
	}
}

// TestHeartbeatReArmsStillDeadEntry pins the fix for a down backend
// silently showing "live": a finite deadUntil that lapses mid-outage
// must not flip the entry usable again. heartbeatTick re-arms deadUntil
// on a failed Ping so a still-unreachable entry stays "cooling".
func TestHeartbeatReArmsStillDeadEntry(t *testing.T) {
	down := &fakeChatter{pingErr: errors.New("backend unreachable")}
	row := entry("down", 0, down)
	p := newTestPool(row)
	defer p.Close()

	// Marked dead, cooldown still ahead — heartbeatTick will Ping it.
	row.deadUntil = time.Now().Add(time.Second)

	if !p.heartbeat.tick() {
		t.Fatal("heartbeatTick must report the entry still dead after a failed Ping")
	}
	if left := time.Until(row.deadUntil); left < time.Minute {
		t.Fatalf("a failed heartbeat Ping must re-arm deadUntil; only %v left", left)
	}
}

// TestRecordErrorCapturesLastErr pins the webui diagnostic: a counted error is
// retained as the entry's last error (the "why" behind cooling/tentative), an
// IGNORED error (caller-cancelled, non-transport) must not overwrite it, and a
// still-LIVE entry does NOT surface the stale text (only non-live entries do).
func TestRecordErrorCapturesLastErr(t *testing.T) {
	p := newTestPool()
	defer p.Close()
	row := entry("x", 0, &fakeChatter{})

	p.recordError(context.Background(), row, errors.New("dial tcp: connection refused"))
	if row.lastErr != "dial tcp: connection refused" {
		t.Fatalf("lastErr not captured: %q", row.lastErr)
	}
	// One error doesn't cool the entry — it's still live, so the diagnostic is hidden.
	if got := row.snapshotInfo(time.Now()).LastError; got != "" {
		t.Fatalf("a live entry must not surface lastErr, got %q", got)
	}
	// context.Canceled is filtered (not the entry's fault) — it must not clobber
	// the retained diagnostic.
	p.recordError(context.Background(), row, context.Canceled)
	if row.lastErr != "dial tcp: connection refused" {
		t.Fatalf("ignored error overwrote lastErr: %q", row.lastErr)
	}
}

// TestSnapshotCoolingDeadUntil: a cooling entry surfaces a future DeadUntilUnix
// (for the "cooling — Nm left" readout) and the most-recent error text.
func TestSnapshotCoolingDeadUntil(t *testing.T) {
	p := newTestPool()
	defer p.Close()
	row := entry("x", 0, &fakeChatter{})
	p.recordError(context.Background(), row, errors.New("boom1"))
	p.recordError(context.Background(), row, errors.New("boom2")) // 2nd consecutive → dead/cooling
	info := row.snapshotInfo(time.Now())
	if info.State != "cooling" {
		t.Fatalf("expected cooling, got %q", info.State)
	}
	if info.DeadUntilUnix <= time.Now().Unix() {
		t.Fatalf("cooling entry must expose a future DeadUntilUnix, got %d", info.DeadUntilUnix)
	}
	if info.LastError != "boom2" {
		t.Fatalf("LastError should be the most recent error, got %q", info.LastError)
	}
}

func TestRecordSuccessResetsConsecutiveFails(t *testing.T) {
	p := newTestPool()
	defer p.Close()
	row := entry("x", 0, &fakeChatter{})
	boom := errors.New("backend down")

	// fail, succeed, fail — the success resets the consecutive counter, so
	// the trailing failure is the first of a new run, not the second of a
	// pair; the entry stays eligible.
	p.recordError(context.Background(), row, boom)
	p.recordSuccess(row)
	p.recordError(context.Background(), row, boom)
	if !row.IsEligible() {
		t.Fatal("a success between failures must reset the consecutive-fail counter")
	}
}

// TestBurnedTentativeRecoveryBacksOffProbe pins the fix for the
// dead↔tentative flap. Shape of the real outage: nginx answers /models with a
// 502, which Ping deliberately counts as reachable (providers.Ping's B2 note),
// so every heartbeat tick re-admitted the entry, the next real call re-deaded
// it, and the exponential cooldown never elapsed — consecutiveDeadEvents
// climbed to 8 while not one of the nominal cooldowns held, each cycle burning
// a live request on a backend that could not serve.
func TestBurnedTentativeRecoveryBacksOffProbe(t *testing.T) {
	up := &fakeChatter{} // Ping always succeeds; completions are what fail
	row := entry("flaky", 0, up)
	p := newTestPool(row)
	defer p.Close()

	row.deadUntil = time.Now().Add(time.Minute) // dead, cooldown ahead

	// Tick 1: a first outage still recovers fast — nextProbeAt is zero, so the
	// entry is probed and tentatively re-admitted. That behaviour is preserved.
	p.heartbeat.tick()
	if !row.tentative {
		t.Fatal("a successful Ping on a fresh outage must tentatively re-admit")
	}

	// The tentative recovery proves false: a real call fails.
	p.recordError(context.Background(), row, errors.New("502 Bad Gateway"))
	if row.tentative {
		t.Fatal("a real failure while tentative must re-dead the entry")
	}
	if !row.nextProbeAt.After(time.Now()) {
		t.Fatalf("a burned tentative recovery must arm the probe backoff, got %v", row.nextProbeAt)
	}

	// Tick 2 is the regression: before the fix this re-admitted the entry
	// again (and every tick after), so the cooldown was never served.
	if !p.heartbeat.tick() {
		t.Fatal("a backed-off dead entry must keep the heartbeat loop alive")
	}
	if row.tentative {
		t.Fatal("probe backoff must stop the heartbeat re-admitting a proven-broken entry")
	}

	// Once the backoff elapses the entry is probed again — the loop recovers,
	// it does not strand a backend that really did come back.
	row.mu.Lock()
	row.nextProbeAt = time.Now().Add(-time.Second)
	row.mu.Unlock()
	p.heartbeat.tick()
	if !row.tentative {
		t.Fatal("after the probe backoff elapses the entry must be probed again")
	}
}

// TestRecordSuccessClearsProbeBackoff pins that a real call succeeding wipes
// the probe debt — otherwise a recovered entry would carry a stale
// nextProbeAt into its NEXT outage and skip that outage's first probe.
func TestRecordSuccessClearsProbeBackoff(t *testing.T) {
	row := entry("x", 0, &fakeChatter{})
	p := newTestPool(row)
	defer p.Close()

	row.mu.Lock()
	row.nextProbeAt = time.Now().Add(time.Hour)
	row.mu.Unlock()

	p.recordSuccess(row)

	row.mu.Lock()
	defer row.mu.Unlock()
	if !row.nextProbeAt.IsZero() {
		t.Fatalf("a successful call must clear the probe backoff, got %v", row.nextProbeAt)
	}
}
