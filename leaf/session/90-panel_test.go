package session

import (
	"testing"
	"time"

	"agentbob/heartwood/clock"
)

func TestBusyLongestText(t *testing.T) {
	if got := busyLongestText(nil); got != "—" {
		t.Errorf("busyLongestText(nothing running) = %q, want %q", got, "—")
	}
	// Unsorted input: the stat is the OLDEST start, wherever it sits in the slice
	// — it must not inherit whatever order the running table happens to want.
	// Stamped with clock.Now() because that is what the helper measures against;
	// time.Now() would agree only while the calibration offset is zero.
	now := clock.Now()
	rows := []InFlightRow{
		{Sid: "b", StartedAt: now.Add(-30 * time.Second)},
		{Sid: "a", StartedAt: now.Add(-5 * time.Minute)},
		{Sid: "c", StartedAt: now.Add(-1 * time.Minute)},
	}
	if got := busyLongestText(rows); got != "5m0s" {
		t.Errorf("busyLongestText(unsorted) = %q, want %q", got, "5m0s")
	}
}

// A start stamp in the future must not render as a negative age. clock.Now() is
// .UTC()'d and so carries no monotonic reading, which leaves both the stat and
// the running table's elapsed column exposed to a backwards calibration resync.
func TestAgeTextClampsFutureStart(t *testing.T) {
	now := clock.Now()
	if got := ageText(now, now.Add(3*time.Second)); got != "0s" {
		t.Errorf("ageText(start in the future) = %q, want %q", got, "0s")
	}
	if got := ageText(now, now.Add(-90*time.Second)); got != "1m30s" {
		t.Errorf("ageText(90s ago) = %q, want %q", got, "1m30s")
	}
}
