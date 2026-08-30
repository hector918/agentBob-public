package contract

import "context"

// BrowserTakeover is the live remote-browser handoff seam: it relays browserd's
// takeover face (screencast + input) for a given browser, keyed by the COPY SCOPE —
// the worker copy's scope (browserd's takeover resolves it to browsercopy_<scope>). The
// `sid` parameters below are that scope, not a session id. Provided by leaf/tools (the browserremote client
// implements it) ONLY when a browserd backend is configured; consumed by leaf/webui's
// takeover endpoints. Absent → the webui exposes no takeover.
//
// Auth is bob's job: the webui has already run Authorize(cookie, key) before calling
// Screencast/Input. The browserremote client reaches browserd's takeover face with the
// control-plane api key (server-to-server, never exposed to the browser); browserd knows
// nothing about user business (docs/browserd.md §4 / docs/wip-browser-profile-identity.md).
type BrowserTakeover interface {
	// Screencast opens sid's live browser stream; frame receives each JPEG as base64
	// until done is closed (stream end / browserd restart / stop). stop severs the
	// upstream connection (idempotent).
	// quality is the requested bandwidth tier ("high"|"med"|"low"; "" → med) — a
	// REQUEST the backend clamps down under load; pass "" for the default.
	Screencast(sid, quality string, frame func(jpegB64 string)) (stop func(), done <-chan struct{}, err error)
	// Input forwards one human input event to sid's live browser.
	Input(sid string, ev BrowserInput) error
	// SaveLogin (保存登陆) captures the live copy at the scope — the login the human just
	// established, cookies including session cookies — into its member master so future
	// copies seed from it. The copy is NOT closed: it stays running and the worker resumes
	// on the same live instance (no re-seed, so nothing is lost). On-disk storage is written
	// back later by the controlled-close path.
	SaveLogin(ctx context.Context, scope string) error
	// LiveForSid reports whether the key has a live, take-over-able browser right now (the
	// no-live guard). The key is the COPY's scope. Conservative: any uncertainty → false.
	LiveForSid(ctx context.Context, sid string) (bool, error)
	// LiveSids lists the sids that have a live browser (the admin takeover picker).
	LiveSids(ctx context.Context) ([]string, error)
}

// TakeoverMinter mints a one-time, coverage-locked webui capability token for a
// copy's live browser (the `sid` params are the copy SCOPE) — the seam the
// escalate_to_coo tool uses so bob can hand off to a human when it hits a login wall /
// captcha it can't clear. Provided by leaf/webui (the auth authority); consumed by
// leaf/tools. Returns "" when minting is refused (e.g. the coverage cap is hit).
type TakeoverMinter interface {
	// MintTakeover issues a fresh token for sid (the copy scope), hard-resetting the
	// coverage so the newest token supersedes any prior one. escalate_to_coo re-mints on
	// every (re-)escalation: minting never touches the live browser copy, so a human just
	// pastes the latest token and lands back on the same browser — no loss.
	MintTakeover(sid string) (token string)
}

// BrowserControlHold is the human-control hold registry: while a human drives the
// takeover in CONTROL mode, the frontend asserts a heartbeat-leased hold on the sid,
// and bob's browser tool yields for that sid instead of fighting the human over one
// page. Provided by leaf/tools (the gate point — bob is browserd's only client, so an
// advisory hold is fully effective); the frontend's control toggle drives it through
// leaf/webui's POST /api/browser/hold. The lease self-expires (no renewal within the
// TTL → released), so a dead tab can never wedge bob forever.
type BrowserControlHold interface {
	// Set asserts / heartbeat-renews the hold on key (the control-mode 10s beat). key
	// is the worker COPY's scope (one copy per worker), NOT a session id.
	Set(key string)
	// Clear releases the hold; wasLive reports whether a live hold was actually
	// released (an EXPLICIT hand-back), which the caller uses to decide whether to
	// resume. resumeSid is the SESSION that handed this browser off (recorded by
	// escalate_to_coo via NoteResume) so the caller wakes the right worker — the hold
	// is keyed by the copy scope, but the turn to resume is a sid. "" when no handoff was
	// noted (e.g. a proactive /takeover). Clearing an expired / never-held key → false.
	//
	// The resume note is CONSUMED only on a live release; a lapsed/never-held Clear
	// PRESERVES it so a retried hand-back (or TTL expiry) can still find it. A caller
	// that must consume the note regardless of liveness (the 保存登陆 save path) uses
	// the ConsumeResume extension seam on the concrete impl instead.
	Clear(key string) (wasLive bool, resumeSid string)
	// NoteResume records that session sid handed key's browser off to a human, so a
	// later Clear(key) can name the worker to resume. escalate_to_coo is in-turn (it
	// knows the sid); the webui hand-back only knows the copy scope. Bounded: one note
	// per key, overwritten on the next handoff, expired on a TTL.
	NoteResume(key, sid string)
	// Held reports whether key is currently human-held (live, unexpired) — the gate
	// bob's browser tool checks before each action.
	Held(key string) bool
}

// BrowserInput is one human input event relayed to the live browser. Field tags
// match browserd's takeover input wire verbatim, so it marshals straight through.
type BrowserInput struct {
	Kind string `json:"kind"` // "mouse" | "key"
	// mouse
	MouseType  string  `json:"mouseType,omitempty"` // move | press | release | wheel
	X          float64 `json:"x,omitempty"`
	Y          float64 `json:"y,omitempty"`
	Button     string  `json:"button,omitempty"` // left | right | middle | none
	ClickCount int64   `json:"clickCount,omitempty"`
	DeltaX     float64 `json:"deltaX,omitempty"` // wheel
	DeltaY     float64 `json:"deltaY,omitempty"`
	// key
	KeyType string `json:"keyType,omitempty"` // down | up | char
	Text    string `json:"text,omitempty"`    // char: the produced text (the dock-typed path)
	Key     string `json:"key,omitempty"`     // down/up: DOM key name
}
