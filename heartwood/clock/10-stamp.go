package clock

import "time"

// StampLayout is bob's ONE spelling for a timestamp shown to a human — webui
// panel cells, table columns, glance details. Year-bearing on purpose: a
// year-less "08-06 14:00" is indistinguishable from the same date a year ago,
// and a panel is exactly where a stale artefact (a config not reloaded since
// last summer, a learn source that never ran again) has to be legible as stale.
const StampLayout = "2006-01-02 15:04"

// Stamp renders an instant in StampLayout, in the process's local zone.
//
// Formatting only — it never re-scales the instant. The SOURCE decides the
// scale: anything that is persisted or ordered against DB rows must come from
// Now() / UnixEpoch() (see the package doc), while a process-local deadline —
// a cooldown countdown, a turn's elapsed time — is measured with the host
// clock, which a calibration resync cannot jump out from under.
//
// Local, not UTC: the container's TZ is the one knob that then moves every
// panel's clock at once, rather than each panel deciding for itself.
//
// The zero time renders as an em dash: "never" is a real answer for a config
// that has not reloaded or a source that has not run, and a 1-year-1 date
// would read as a bug.
func Stamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format(StampLayout)
}

// TimeOfDayLayout is the spelling for an instant that is necessarily RECENT —
// a row in a live-only table, where every entry started moments ago and the
// date would be the same noise on every line. Seconds are the part that
// carries information there, and Stamp's minute truncation would put a start
// time up to a minute earlier than it was, right beside an elapsed column
// counting in seconds.
const TimeOfDayLayout = "15:04:05"

// TimeOfDay renders an instant in TimeOfDayLayout, same zone and zero-value
// rules as Stamp.
//
// Two spellings, not two conventions: the choice is about what the reader is
// looking at, not about taste. Anything that could be old — a config mtime, a
// last-run watermark, a stored message — takes Stamp and must carry its date.
// Only a surface that CANNOT show an old instant may take this one. Adding a
// third spelling is how the drift this package exists to end starts again.
func TimeOfDay(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format(TimeOfDayLayout)
}
