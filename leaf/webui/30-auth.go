package webui

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"agentbob/contract"
)

// Token / session lifetimes — see docs/webui-trunk.md §5.2 / docs/webui.md 规则 8.
const (
	tokenTTL   = 5 * time.Minute // one-time unlock token
	sessionTTL = 24 * time.Hour  // management / capability session cookie

	// maxCoverages bounds the live coverage population (pending tokens mirror +
	// sessions) so a flood of MintScoped calls (one per scope) cannot grow Auth
	// unbounded. The admin "*" coverage is always allowed (it re-mints in place);
	// only a NEW scoped coverage is refused past the cap. Far above any real
	// single-deploy count.
	maxCoverages = 128
)

// AdminCoverage is the coverage of the global management session — the role that
// may reach every write/admin endpoint. A capability session instead carries a
// single scope as its coverage and may ONLY take over that one scope's live
// browser (reserved for takeover; see docs/webui-trunk.md §5.4).
const AdminCoverage = "*"

// webuiUnlockKind is the claim-token facility kind for webui one-time unlock tokens
// (both the admin "*" management token and a scoped takeover-capability token — the
// coverage payload distinguishes them). HTTP channel: webui owns the post-flow
// (session creation + coverage gating), the facility owns only the token lifecycle.
const webuiUnlockKind = "webui-unlock"

// Auth holds at most one pending unlock token + one session PER COVERAGE. A
// Mint(coverage) is a hard reset of THAT coverage only. `coverage` is OPAQUE to
// Auth — it never interprets the string, only compares it, which keeps this
// package free of any agora / browser knowledge.
//
//   - coverage "*"       → the admin management session.
//   - coverage "<scope>" → a capability session that may take over only that
//     scope's live browser; it reaches NO admin/write endpoint.
//
// The one-time unlock tokens live in the shared claim-token facility (kind
// webuiUnlockKind), not in Auth; pendingByCoverage mirrors coverage→its current
// token so a re-mint can VOID the prior token (the per-coverage hard reset). The
// durable sessions (cookies) — the post-flow — stay here.
type Auth struct {
	mu sync.Mutex

	tokens            contract.ClaimTokens // pending unlock tokens (Mint/Verify/Consume)
	pendingByCoverage map[string]string    // coverage → its current pending token (mirror, for the re-mint void)
	sessions          map[string]session   // live sessions, keyed by cookie value
}

type session struct {
	coverage string
	expires  time.Time
}

// NewAuth builds an empty Auth over the claim-token facility — no pending tokens,
// no sessions.
func NewAuth(tokens contract.ClaimTokens) *Auth {
	return &Auth{tokens: tokens, pendingByCoverage: map[string]string{}, sessions: map[string]session{}}
}

// Mint mints an ADMIN (coverage "*") management token. Used by the /webui slash.
func (a *Auth) Mint() string { return a.MintScoped(AdminCoverage) }

// MintTakeover implements contract.TakeoverMinter: a token bound to one sid's browser
// for the escalate_to_coo tool. It IS MintScoped (the takeover axis is the sid) — same
// one-time / per-coverage-reset semantics; "" when the coverage cap refuses it.
func (a *Auth) MintTakeover(sid string) string { return a.MintScoped(sid) }

var _ contract.TakeoverMinter = (*Auth)(nil)

// MintScoped performs a per-coverage hard reset: it voids any pending token and
// kills any live session for `coverage`, then issues a fresh one-time token
// bound to that coverage. Returns "" (minting nothing) only when a NEW scoped
// coverage would exceed the cap.
func (a *Auth) MintScoped(coverage string) string {
	if coverage == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()

	had := a.dropCoverageLocked(coverage)
	if coverage != AdminCoverage && !had && len(a.pendingByCoverage)+len(a.sessions) >= maxCoverages {
		slog.Warn("webui: scoped token refused — coverage cap reached")
		return ""
	}

	tok := a.tokens.Mint(webuiUnlockKind, coverage, tokenTTL)
	a.pendingByCoverage[coverage] = tok
	if coverage == AdminCoverage {
		slog.Info("webui: management token minted — prior admin session and token voided")
	} else {
		slog.Info("webui: scoped takeover token minted", "coverage", coverage)
	}
	return tok
}

// SubmitToken validates tok. On a match it consumes the token, hard-resets any
// existing session of that coverage, creates a fresh session, and returns its
// cookie with ok=true. Otherwise ok=false and no slot is consumed.
func (a *Auth) SubmitToken(tok string) (cookie string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()
	// Authenticate via the facility (128-bit crypto-random, single-use, 5-min-TTL).
	// An empty token is rejected by Verify below (the facility never mints one), so
	// no separate pre-check is needed.
	kind, pl, present := a.tokens.Verify(tok)
	coverage, isCov := pl.(string)
	if !present || kind != webuiUnlockKind || !isCov || coverage == "" {
		slog.Warn("webui: token submission rejected")
		return "", false
	}
	a.tokens.Consume(tok)                 // single-use
	delete(a.pendingByCoverage, coverage) // mirror cleanup (token is now spent)
	a.dropSessionsForCoverage(coverage)   // per-coverage single session
	cookie = randToken()
	a.sessions[cookie] = session{coverage: coverage, expires: time.Now().Add(sessionTTL)}
	if coverage == AdminCoverage {
		slog.Info("webui: management session unlocked")
	} else {
		slog.Info("webui: scoped takeover session unlocked", "coverage", coverage)
	}
	return cookie, true
}

// Authorize reports whether cookie names a live session whose coverage covers
// `scope`: admin "*" covers every scope; a capability coverage covers only its
// exact scope. The single predicate the takeover endpoints gate on (reserved).
func (a *Auth) Authorize(cookie, scope string) bool {
	if cookie == "" || scope == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.liveSessionLocked(cookie)
	if !ok {
		return false
	}
	return s.coverage == AdminCoverage || s.coverage == scope
}

// ValidAdmin reports whether cookie names a live ADMIN session (coverage "*").
// Every write/management endpoint gates on this.
func (a *Auth) ValidAdmin(cookie string) bool {
	if cookie == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.liveSessionLocked(cookie)
	return ok && s.coverage == AdminCoverage
}

// Coverage returns the coverage of cookie's live session, or "" if none. Used by
// /api/whoami so the frontend picks dashboard ("*") vs bare overlay (scope).
func (a *Auth) Coverage(cookie string) string {
	if cookie == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.liveSessionLocked(cookie)
	if !ok {
		return ""
	}
	return s.coverage
}

// Logout clears the single session named by cookie. No-op when unknown.
func (a *Auth) Logout(cookie string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.sessions[cookie]; ok {
		delete(a.sessions, cookie)
		slog.Info("webui: session ended (logout)")
	}
}

// Sweep evicts expired pending tokens and sessions (hygiene; lookups also evict
// on access).
func (a *Auth) Sweep() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()
}

// --- locked helpers (caller holds a.mu) ---

func (a *Auth) liveSessionLocked(cookie string) (session, bool) {
	s, ok := a.sessions[cookie]
	if !ok {
		return session{}, false
	}
	if time.Now().After(s.expires) {
		delete(a.sessions, cookie)
		return session{}, false
	}
	return s, true
}

func (a *Auth) dropCoverageLocked(coverage string) (had bool) {
	if tok, ok := a.pendingByCoverage[coverage]; ok {
		a.tokens.Consume(tok) // VOID the prior pending token in the facility (re-mint reset)
		delete(a.pendingByCoverage, coverage)
		had = true
	}
	if a.dropSessionsForCoverage(coverage) {
		had = true
	}
	return had
}

func (a *Auth) dropSessionsForCoverage(coverage string) (had bool) {
	for c, s := range a.sessions {
		if s.coverage == coverage {
			delete(a.sessions, c)
			had = true
		}
	}
	return had
}

func (a *Auth) sweepLocked() {
	now := time.Now()
	for c, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, c)
		}
	}
	// Prune mirror entries whose facility token is gone (expired/consumed). Verify is
	// non-consuming and lazily reaps the expired entry in the facility too, so this both
	// checks and cleans — and keeps pendingByCoverage from counting dead tokens at the cap.
	for cov, tok := range a.pendingByCoverage {
		if _, _, ok := a.tokens.Verify(tok); !ok {
			delete(a.pendingByCoverage, cov)
		}
	}
}

// randToken returns a 32-hex-char (16-byte) cryptographically random string.
func randToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("webui: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
