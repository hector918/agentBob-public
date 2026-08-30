package browser

// Per-scope human control-hold registry (takeover yield+resume,
// docs/remote-browser-handoff.md). While a human drives the takeover overlay
// in "control" mode the frontend asserts a hold on the session_scope; every
// model-side browser_* action for that scope then yields with a fixed D1
// error envelope instead of fighting the human over one page. The hold is a
// heartbeat lease: the frontend must re-assert it periodically (Set), and a
// hold not renewed within the TTL silently expires — so no matter how the
// takeover stream dies (tab closed, network drop, bob restart), the model is never blocked forever.
//
// An EXPLICIT release (Clear on a live hold — the human clicked back to
// view mode) additionally parks a one-shot resume note for the scope, which
// the next agent turn surfaces through the prompt layer ("the human is done,
// continue the task"). A TTL expiry parks nothing: the human may just have
// lost connectivity, and the next browser action simply proceeds.
//
// Zero-growth: one map entry per actively-held scope, deleted on Clear /
// expiry; resume notes are one entry per scope, deleted on Take / expiry.
// Expired entries are also purged opportunistically on every call.

import (
	"errors"
	"sync"
	"time"
)

// HoldEnvelopeMsg is the model-visible yield message a browser_* action
// returns while the scope is human-held. Shared by the in-process pool gate
// (AcquireFor) and the browserd thin-client gate (Client.actionPost) so both
// modes speak the exact same envelope.
const HoldEnvelopeMsg = "人正在通过接管控制此浏览器,本轮暂停浏览器操作;请优雅收尾本轮,等人完成后会收到继续提示。"

// holdResumeNote is the model-visible nudge surfaced on the scope's next
// turn after the human EXPLICITLY handed control back (Clear on a live
// hold). Deterministic text — it rides the prompt module's nudge layer,
// which requires cache-stable strings.
const holdResumeNote = "人已完成手动操作,浏览器已交还;请继续之前的浏览器任务。如之前是委派的子任务在操作浏览器,可重新委派让它继续。"

// ErrHeldByHuman is the sentinel a gated acquire/action returns while the
// scope is human-held. Its Error() text IS the model-visible envelope
// message (tool shells render it via errResult).
var ErrHeldByHuman = errors.New(HoldEnvelopeMsg)

// DefaultHoldTTL is the heartbeat lease window. The frontend re-asserts
// every ~10s; 30s tolerates two missed beats before the hold lapses.
const DefaultHoldTTL = 30 * time.Second

// resumeNoteTTL bounds how long an unconsumed resume note may park. If no
// turn ran in the scope for this long after the hand-back, the task context
// is stale anyway — drop the note instead of keeping the entry forever.
const resumeNoteTTL = 30 * time.Minute

// ControlHolds is the registry. All methods are nil-receiver-safe so
// callers wired without the browser subsystem need no guards.
type ControlHolds struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time

	holds  map[string]time.Time // scope → hold lease expiry (last Set + ttl)
	resume map[string]time.Time // scope → parked resume-note expiry
}

// NewControlHolds builds the registry. ttl <= 0 → DefaultHoldTTL; nil now →
// time.Now (injectable clock for tests).
func NewControlHolds(ttl time.Duration, now func() time.Time) *ControlHolds {
	if ttl <= 0 {
		ttl = DefaultHoldTTL
	}
	if now == nil {
		now = time.Now
	}
	return &ControlHolds{
		ttl:    ttl,
		now:    now,
		holds:  map[string]time.Time{},
		resume: map[string]time.Time{},
	}
}

// purgeLocked drops every expired hold and resume note. Caller holds h.mu.
// Expiry here is SILENT by design: a lapsed hold parks no resume note.
func (h *ControlHolds) purgeLocked(now time.Time) {
	for scope, exp := range h.holds {
		if now.After(exp) {
			delete(h.holds, scope)
		}
	}
	for scope, exp := range h.resume {
		if now.After(exp) {
			delete(h.resume, scope)
		}
	}
}

// Set asserts (or heartbeat-renews) the human control hold on scope.
func (h *ControlHolds) Set(scope string) {
	if h == nil || scope == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.purgeLocked(now)
	h.holds[scope] = now.Add(h.ttl)
}

// Clear releases the hold on scope (the human clicked back to view mode /
// closed the overlay). When the hold was still live, a one-shot resume note
// is parked for the scope's next turn. Clearing an already-expired or
// never-held scope parks nothing (the TTL-expiry no-nudge contract).
func (h *ControlHolds) Clear(scope string) {
	if h == nil || scope == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.purgeLocked(now) // drops a lapsed hold BEFORE the liveness check below
	if _, live := h.holds[scope]; !live {
		return
	}
	delete(h.holds, scope)
	h.resume[scope] = now.Add(resumeNoteTTL)
}

// Held reports whether scope is currently human-held (live, unexpired).
func (h *ControlHolds) Held(scope string) bool {
	if h == nil || scope == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.purgeLocked(h.now())
	_, ok := h.holds[scope]
	return ok
}

// TakeResumeNote consumes the parked resume note for scope, returning the
// model-visible nudge text and whether one was pending. One-shot: a second
// Take returns ("", false). Wired into the agent loop as the turn-start
// prompt hook (agentorch Deps.BrowserResumeNote).
func (h *ControlHolds) TakeResumeNote(scope string) (string, bool) {
	if h == nil || scope == "" {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.purgeLocked(h.now())
	if _, ok := h.resume[scope]; !ok {
		return "", false
	}
	delete(h.resume, scope)
	return holdResumeNote, true
}
