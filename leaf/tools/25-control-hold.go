package tools

// Human control-hold registry (browser takeover yield, docs/wip-takeover-port.md).
// While a human drives the takeover in CONTROL mode the frontend heartbeats a hold on
// the sid; bob's browser tool then yields for that sid (browser.go's Run gate) instead
// of fighting the human over one page. The gate lives bob-side because bob is
// browserd's only client — an advisory hold is fully effective and browserd stays
// stateless about it.
//
// Heartbeat lease: the frontend re-asserts (Set) every ~10s; a hold not renewed within
// the TTL silently expires — so no matter how the takeover stream dies (tab closed,
// connectivity lost, lease never renewed), bob is never blocked forever. The hold is
// keyed by the worker COPY's SCOPE (one copy per worker), not a session id. The RESUME half: an explicit
// hand-back (Clear of a live hold) reports wasLive AND resumeSid — the worker session
// that handed off, recorded by escalate_to_coo's NoteResume (the hold is scope-keyed
// but the turn to wake is a sid) — and leaf/webui asks leaf/session to wake that worker.

import (
	"sync"
	"time"

	"agentbob/contract"
)

// defaultHoldTTL is the heartbeat lease window: the frontend re-asserts every ~10s, so
// 30s tolerates two missed beats before a hold lapses. Not load-bearing — the lapse
// self-heals (bob's next browser action simply proceeds).
const defaultHoldTTL = 30 * time.Second

// resumeNoteTTL bounds a resume note (copy scope → worker sid). A handoff a human never
// took over goes stale; without a TTL a much-later proactive /takeover of the same
// scope would resume a long-dead session. 30 min covers a human handling a login.
const resumeNoteTTL = 30 * time.Minute

// resumeNote is the worker sid that handed a principal's browser off, plus its expiry.
type resumeNote struct {
	sid     string
	expires time.Time
}

// controlHolds is the per-principal hold registry. Near-zero growth: one entry per held
// key, deleted on Clear / expiry; one resume note per key, overwritten / consumed /
// expired. Purge runs opportunistically on every call — bounded and self-healing.
type controlHolds struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	holds   map[string]time.Time  // key → lease expiry (last Set + ttl)
	resumes map[string]resumeNote // key → worker sid that handed off (+ expiry)
}

func newControlHolds() *controlHolds {
	return &controlHolds{ttl: defaultHoldTTL, now: time.Now, holds: map[string]time.Time{}, resumes: map[string]resumeNote{}}
}

func (h *controlHolds) purgeLocked(now time.Time) {
	for key, exp := range h.holds {
		if now.After(exp) {
			delete(h.holds, key)
		}
	}
	for key, n := range h.resumes {
		if now.After(n.expires) {
			delete(h.resumes, key)
		}
	}
}

// Set asserts (or heartbeat-renews) the human control hold on key (the principal).
func (h *controlHolds) Set(key string) {
	if key == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.purgeLocked(now)
	h.holds[key] = now.Add(h.ttl)
}

// NoteResume records that worker sid handed key's browser off, so a later Clear(key)
// names the worker to wake. Overwrites any prior note for key (the latest handoff wins).
func (h *controlHolds) NoteResume(key, sid string) {
	if key == "" || sid == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.purgeLocked(now)
	h.resumes[key] = resumeNote{sid: sid, expires: now.Add(resumeNoteTTL)}
}

// Clear releases the hold on key. wasLive reports whether a LIVE hold was released (an
// explicit hand-back) vs an already-expired / never-held key — the caller resumes only
// on a live release (a lapse is not a hand-back, so it wakes nothing). resumeSid is the
// worker that handed off, consumed ONLY on a live release: a lapsed / never-held Clear
// returns "" and LEAVES the note in place (for a retried hand-back or its natural TTL
// expiry) — consuming it there would silently destroy the one pointer that can wake the
// worker. The 保存登陆 path, which must wake the worker regardless of hold liveness,
// consumes explicitly via ConsumeResume instead.
func (h *controlHolds) Clear(key string) (wasLive bool, resumeSid string) {
	if key == "" {
		return false, ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.purgeLocked(h.now()) // drop a lapsed hold BEFORE the liveness check
	if _, live := h.holds[key]; !live {
		return false, ""
	}
	delete(h.holds, key)
	if n, ok := h.resumes[key]; ok {
		resumeSid = n.sid
		delete(h.resumes, key)
	}
	return true, resumeSid
}

// ConsumeResume unconditionally takes (and clears) key's resume note, regardless of the
// hold's liveness — the explicit-consume door for 保存登陆: clicking save IS an explicit
// hand-back even when the heartbeat lapsed mid-login, so the caller always gets the
// worker to wake exactly once. Returns "" when no note is pending (or it expired).
func (h *controlHolds) ConsumeResume(key string) (resumeSid string) {
	if key == "" {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.purgeLocked(h.now())
	if n, ok := h.resumes[key]; ok {
		delete(h.resumes, key)
		return n.sid
	}
	return ""
}

// Held reports whether key is currently human-held (live, unexpired).
func (h *controlHolds) Held(key string) bool {
	if key == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.purgeLocked(h.now())
	_, ok := h.holds[key]
	return ok
}

var _ contract.BrowserControlHold = (*controlHolds)(nil)
