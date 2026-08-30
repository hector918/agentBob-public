package browser

import (
	"log/slog"
	"sync"
	"time"
)

// ProfileCustodian is the single-goroutine actor that owns the per-profile
// exclusive-checkout lifecycle (docs/remote-browser-handoff.md). It sits ON TOP
// of Pool: the custodian decides WHO currently holds a profile and WHEN to
// reclaim it; Pool still owns the chromium instances and does the actual
// spawn/close. Every piece of checkout state lives on the run() goroutine, so
// the gnarly concurrency this feature introduces — cross-scope checkout race,
// release-vs-cancel, idle reclaim, handoff pin — is processed serially and
// needs no locks.
//
// Keying. A *profile key* is the per-person identity the browser profile
// belongs to (AccountID, or a SourceHandle fallback) — NOT the chat scope. The
// *owner* of a checkout is a session_scope: one scope holds a profile
// head-to-tail across all its turns (the "one browser, used start to finish,
// like a human" model); a different scope asking for the same profile
// fail-fasts. A single scope may legitimately own several distinct profiles
// (different people triggering browser work in one group chat).
//
// Release doors (both automatic, no human needed):
//   - ReleaseScope — the owning scope's last session ended (EndSession: a human
//     /close-session OR an agora finalize both route through it). Frees every
//     profile that scope owned.
//   - Reclaim — a checkout untouched for idleTTL (default 5 min) is freed. The
//     universal backstop; in agora, where sessions are chat-scoped and rarely
//     finalize, this is effectively the primary release.
//
// Releasing a checkout closes the chromium instance (cookies persist on disk in
// the profile's vault dir — a release is NOT a logout). A handoff-pinned profile
// (human actively logging in via the takeover UI) is exempt from Reclaim.
type ProfileCustodian struct {
	reqs     chan any
	quit     chan struct{}
	quitOnce sync.Once
	closer   func(profileKey string) // closes the chromium instance for a profile (Pool.CloseInstance)
	idleTTL  time.Duration
	now      func() time.Time // injectable clock (real in prod, fake in tests)
}

// checkout is the per-profile state. Present in the map ⇒ checked out; absent ⇒
// free. Only the run() goroutine reads/writes these.
type checkout struct {
	owner       string // session_scope holding the profile (never "" while present)
	lastTouched time.Time
	pins        int // live handoffs — exempt from idle Reclaim while > 0. A COUNT, not a bool: two concurrent takeover streams on one profile each pin/unpin, and the first to stop must not strip the other's exemption.
}

// CheckoutView is a read-only copy of one checkout, for Snapshot (tests/debug).
type CheckoutView struct {
	Profile     string
	Owner       string
	LastTouched time.Time
	Pinned      bool
}

// --- request messages (one type per operation; run() type-switches) ---

type coCheckout struct {
	profile, scope string
	reply          chan bool // true = granted (free or already this scope's); false = busy (another scope)
}
type coRelease struct {
	scope string
	reply chan int // number of profiles freed
}
type coPin struct {
	profile string
	on      bool
	reply   chan bool // true = profile was checked out (pin applied); false = unknown profile
}
type coReclaim struct{ reply chan int } // number of profiles reclaimed
type coSnapshot struct{ reply chan []CheckoutView }
type coReleaseProfile struct {
	profile, scope string
	reply          chan bool // true = freed (was owned by scope)
}

// NewProfileCustodian starts the actor. closer closes a profile's chromium
// instance (pass Pool.CloseInstance); idleTTL is the untouched-reclaim window
// (<=0 → 5 min). now defaults to time.Now when nil.
func NewProfileCustodian(closer func(profileKey string), idleTTL time.Duration, now func() time.Time) *ProfileCustodian {
	if idleTTL <= 0 {
		idleTTL = 5 * time.Minute
	}
	if now == nil {
		now = time.Now
	}
	c := &ProfileCustodian{
		reqs:    make(chan any),
		quit:    make(chan struct{}),
		closer:  closer,
		idleTTL: idleTTL,
		now:     now,
	}
	go c.run()
	return c
}

// Close stops the actor goroutine. Idempotent (guarded by quitOnce), so the
// owning Pool's CloseAll can call it freely on a possibly-repeated shutdown.
func (c *ProfileCustodian) Close() { c.quitOnce.Do(func() { close(c.quit) }) }

// safeClose invokes the injected closer with a recover. The closer is untrusted
// from the actor's standpoint (it's a callback — in prod Pool.CloseInstance, but
// the custodian can't vouch for it), and it runs ON the run() goroutine. Without
// the recover a single panic would kill run(), hang the in-flight Release/Reclaim
// caller's reply, AND wedge every future caller forever on the now-readerless
// reqs channel — a silent total outage of the profile-checkout subsystem. We
// swallow + log instead; a leaked chromium instance is far less bad than that.
func (c *ProfileCustodian) safeClose(profileKey string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("browser custodian: closer panicked (instance may leak)", "profile", profileKey, "panic", r)
		}
	}()
	c.closer(profileKey)
}

// reclaimCheckInterval is how often the internal ticker sweeps for idle
// checkouts. Independent of idleTTL (the age threshold) — a profile is reclaimed
// within [idleTTL, idleTTL+reclaimCheckInterval] of its last touch. Self-driven
// so the custodian "takes care of itself" with no external sweep wiring.
const reclaimCheckInterval = time.Minute

// reclaimSweep frees every un-pinned checkout older than idleTTL, closing its
// instance. MUST be called only on the run() goroutine (it mutates state) — both
// the ticker and the coReclaim request do. Returns the count reclaimed.
func (c *ProfileCustodian) reclaimSweep(state map[string]*checkout) int {
	cutoff := c.now().Add(-c.idleTTL)
	n := 0
	for profile, cur := range state {
		if cur.pins > 0 {
			continue // handoff in progress — never reclaim
		}
		if cur.lastTouched.Before(cutoff) {
			delete(state, profile)
			c.safeClose(profile)
			n++
		}
	}
	return n
}

func (c *ProfileCustodian) run() {
	state := map[string]*checkout{}
	ticker := time.NewTicker(reclaimCheckInterval)
	defer ticker.Stop()
	for {
		var raw any
		select {
		case <-c.quit:
			return
		case <-ticker.C:
			c.reclaimSweep(state) // periodic idle backstop (real-time; frozen in tests)
			continue
		case raw = <-c.reqs:
		}
		switch r := raw.(type) {
		case coCheckout:
			cur, ok := state[r.profile]
			switch {
			case !ok:
				state[r.profile] = &checkout{owner: r.scope, lastTouched: c.now()}
				r.reply <- true
			case cur.owner == r.scope:
				cur.lastTouched = c.now()
				r.reply <- true
			default:
				r.reply <- false // owned by another scope — fail-fast
			}
		case coRelease:
			freed := 0
			for profile, cur := range state {
				if cur.owner == r.scope {
					delete(state, profile)
					c.safeClose(profile)
					freed++
				}
			}
			r.reply <- freed
		case coPin:
			if cur, ok := state[r.profile]; ok {
				if r.on {
					cur.pins++
				} else if cur.pins > 0 {
					cur.pins-- // floor at 0: an unmatched unpin (e.g. after a release+re-checkout) must not underflow
				}
				r.reply <- true
			} else {
				r.reply <- false
			}
		case coReclaim:
			r.reply <- c.reclaimSweep(state)
		case coReleaseProfile:
			if cur, ok := state[r.profile]; ok && cur.owner == r.scope {
				delete(state, r.profile)
				c.safeClose(r.profile)
				r.reply <- true
			} else {
				r.reply <- false
			}
		case coSnapshot:
			out := make([]CheckoutView, 0, len(state))
			for profile, cur := range state {
				out = append(out, CheckoutView{Profile: profile, Owner: cur.owner, LastTouched: cur.lastTouched, Pinned: cur.pins > 0})
			}
			r.reply <- out
		}
	}
}

// post sends req to the actor, returning false if the custodian has been
// Close()d (so a call racing shutdown can't block forever on the now-readerless
// reqs channel — an in-flight request during drain must not wedge handleWg).
func (c *ProfileCustodian) post(req any) bool {
	select {
	case c.reqs <- req:
		return true
	case <-c.quit:
		return false
	}
}

// Checkout requests exclusive use of profile for scope. Returns true when the
// scope now holds it (it was free, or already this scope's); false when another
// scope holds it (caller fail-fasts with "browser busy") or the custodian is
// shutting down. A successful checkout also refreshes the touch timestamp.
func (c *ProfileCustodian) Checkout(profile, scope string) bool {
	reply := make(chan bool, 1)
	if !c.post(coCheckout{profile: profile, scope: scope, reply: reply}) {
		return false
	}
	return <-reply
}

// ReleaseScope frees every profile owned by scope and closes their chromium
// instances. Called from the EndSession path when the scope goes dormant
// (/close-session for a human, agora finalize for automation). Returns how many
// profiles were freed. Blocks until the releases (incl. instance close) are
// done so the caller can sequence teardown.
func (c *ProfileCustodian) ReleaseScope(scope string) int {
	reply := make(chan int, 1)
	if !c.post(coRelease{scope: scope, reply: reply}) {
		return 0
	}
	return <-reply
}

// Release frees a SINGLE profile iff it's owned by scope, closing its instance.
// The spawn-failure cleanup path: a scope that just checked a profile out but
// then failed to launch chromium frees it immediately rather than leaving it
// "busy" until idle reclaim. Returns whether it was freed. (ReleaseScope is the
// bulk variant for session-end.)
func (c *ProfileCustodian) Release(profile, scope string) bool {
	reply := make(chan bool, 1)
	if !c.post(coReleaseProfile{profile: profile, scope: scope, reply: reply}) {
		return false
	}
	return <-reply
}

// Pin adds (on=true) or drops (on=false) one handoff-in-progress hold on a
// profile; the profile is exempt from idle Reclaim while it has any hold. A
// REFCOUNT, not a flag: every screencast stream pins on start and unpins on
// stop, so with two concurrent streams the first to stop must not strip the
// other's exemption. Returns false if the profile isn't checked out (the
// LEGACY scope-keyed path — its takeover exemption is Pool.takeoverActive)
// or on shutdown.
func (c *ProfileCustodian) Pin(profile string, on bool) bool {
	reply := make(chan bool, 1)
	if !c.post(coPin{profile: profile, on: on, reply: reply}) {
		return false
	}
	return <-reply
}

// Reclaim frees (and closes) every un-pinned checkout untouched for longer than
// idleTTL. Returns the count reclaimed. A background ticker calls this in
// production; tests call it after advancing the injected clock.
func (c *ProfileCustodian) Reclaim() int {
	reply := make(chan int, 1)
	if !c.post(coReclaim{reply: reply}) {
		return 0
	}
	return <-reply
}

// Snapshot returns a copy of the current checkouts (tests / debug).
func (c *ProfileCustodian) Snapshot() []CheckoutView {
	reply := make(chan []CheckoutView, 1)
	if !c.post(coSnapshot{reply: reply}) {
		return nil
	}
	return <-reply
}
