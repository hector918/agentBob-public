package core

// SessionPurger is implemented by subsystems that hold per-session
// persistent state on disk OR in-memory across multiple turns of the
// same session, and that should be reset to a clean slate when the
// session is explicitly reset (`/new`, `/reset`, `/close-session`).
//
// Examples in v1: the browser pool's chromedp data dir (cookies / login
// state / lastNavURL). Future: per-session attachment caches if any
// long-lived state lands.
//
// Idempotent: calling on an unknown sessionScope is a no-op. Implementers
// must NOT delete on idle close, model restart, or pool eviction —
// those preserve on-disk state so the session can resume. PurgeSession
// is the explicit "user said reset" lever.
type SessionPurger interface {
	PurgeSession(sessionScope string)
}
