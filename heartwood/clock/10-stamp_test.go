package clock

import (
	"testing"
	"time"
)

func TestStamp(t *testing.T) {
	// Year-bearing: last August's timestamp must not read like this August's.
	at := time.Date(2025, 8, 6, 14, 30, 5, 0, time.Local)
	if got := Stamp(at); got != "2025-08-06 14:30" {
		t.Errorf("Stamp = %q, want %q", got, "2025-08-06 14:30")
	}
	// "never" is a real answer — a config that has not reloaded, a source that
	// has not run — not a year-1 date.
	if got := Stamp(time.Time{}); got != "—" {
		t.Errorf("Stamp(zero) = %q, want %q", got, "—")
	}
	// The instant is not re-scaled, only re-zoned: a UTC input renders as the
	// same moment in the process's zone.
	utc := at.UTC()
	if got := Stamp(utc); got != Stamp(at) {
		t.Errorf("Stamp(UTC) = %q, Stamp(local) = %q — the same instant must render identically", got, Stamp(at))
	}
}

func TestTimeOfDay(t *testing.T) {
	at := time.Date(2025, 8, 6, 14, 30, 5, 0, time.Local)
	// Seconds survive: this spelling exists for rows sitting next to an elapsed
	// column counting in seconds, where Stamp's minute truncation would show a
	// start up to a minute earlier than it was.
	if got := TimeOfDay(at); got != "14:30:05" {
		t.Errorf("TimeOfDay = %q, want %q", got, "14:30:05")
	}
	if got := TimeOfDay(time.Time{}); got != "—" {
		t.Errorf("TimeOfDay(zero) = %q, want %q", got, "—")
	}
}
