// Package session is the session core: the single authority on which session an
// inbound event resolves to (scope resolution), the per-sid turn arbiter (one
// turn at a time), and session lifecycle. It is generic over a TurnHandler —
// the arbiter knows WHEN to run a turn, the turn module knows HOW. A session is
// a session even when no turn ever runs; the contract.TurnHandler seam is the
// only thing connecting the two cores.
package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"agentbob/contract"
	"agentbob/leaf/session/store"
)

// Config holds the session core's clamped knobs.
type Config struct {
	MaxBatch int // max events queued behind a busy sid before overflow (>= 1)
	// PlaceAttachments relocates a resolved-scope event's staged attachments into the
	// scope's space (wired in cmd/bob over $BOB_HOME via warrant.PlaceAttachments).
	// nil → attachments keep their staged paths (no space placement).
	PlaceAttachments func(scope string, atts []contract.Attachment) []contract.Attachment
	// Transcribe an event's soundtracks to text at INGESTION (before persistence), so
	// spoken content lives in the message text for every downstream reader — the prompt,
	// stored history, the recall feed (F165). Returns TWO strings by position:
	// `instruction` (the sender's own captionless voice note — it IS the request) and
	// `material` (a captioned clip, a replied-to message's clip, a video soundtrack —
	// framed and sanitised, never allowed to occupy the request slot). Either may be
	// empty. Wired by the session module at Start over the model pool's KindASR backend
	// (leaf/asr). nil / nil pool → no transcription.
	Transcribe func(ctx context.Context, ev contract.MessageEvent) (instruction, material string)
}

func (c Config) clamped() Config {
	if c.MaxBatch < 1 {
		c.MaxBatch = 40
	}
	return c
}

// sidMetaRow tracks a busy sid's scope + turn token; lifecycle matches busy[sid].
type sidMetaRow struct {
	Scope string
	// StartedAt is on the DB-calibrated scale (clock.Now()) like every other
	// timestamp bob shows: the webui prints it beside chat-log rows read from
	// the store, and two scales in one view read as a bob that lost time.
	// Stamped once when the sid goes busy and kept across drain turns, so the
	// age derived from it is the busy CHAIN's, not the current turn's.
	StartedAt time.Time
	turn      uint64
	// mode is the running turn's flow-declared SessionMode, cached at turn start
	// (ModeFor). The busy-queue reads it under m.mu to gate notices (auto → silent);
	// the arbiter reads it to decide whether to wire the nudge fast-lane.
	mode contract.SessionMode
}

// armKey identifies one pending toggle arm: the scope it was issued in AND the
// user who issued it. Per-USER, not per-scope, because a scope is shared: with a
// single slot per scope, one member's /thinking silently replaced another's
// pending /uncensored, and either one's "off" disarmed the other's arm while
// reporting success to the wrong person. Only the issuer's own turn can consume
// an arm anyway (takeArmedPrefer), so anything else sharing the slot was pure
// interference.
type armKey struct {
	scope  string
	userID string
}

// Manager is the session core.
type Manager struct {
	store     store.Store
	handler   contract.TurnHandler
	srcByName func(string) contract.Source // resolve a source by name (the bus's lookup)
	config    Config

	// accounts lazily resolves the optional accounts authority so /whoami can show
	// the sender's bound account. nil func (or a nil return) → the account line is
	// omitted. Set by the module at Start; resolved on first /whoami.
	accounts func() contract.Accounts

	// groupRouter lazily resolves the optional agora group-router (virtual-group member
	// routing). nil func / nil return → groups route 1:1 (no member sub-scopes). Set by
	// the module at Start; resolved per ResolveSession (agora may start after session).
	groupRouter func() contract.GroupRouter

	// turnRef lazily resolves the turn core so /session can read its in-memory
	// context gauge (ContextGauge soft seam — last real prompt size + last
	// sizing budget, zero store reads). Lazy: the turn module starts after
	// session (it Requires the MessageStore session provides). nil func / nil
	// return / a turn without the seam → the context line is omitted.
	turnRef func() contract.Turn

	// restore is the cold-rehydrate hook ResolveSession calls when a reply points
	// at a sid that is no longer live in bob_sessions: it revives a was-alive
	// archived session (returns true) or reports it gone / not-revivable (false →
	// the caller opens a fresh session). nil (no archiver wired) → never revives.
	restore func(ctx context.Context, sid string) bool

	// sfActive dedups concurrent ResolveActiveSid calls for one scope so two
	// dispatch goroutines can't both fall through to OpenSession and create twin
	// sids. Keyed by scope.
	sfActive singleflight.Group

	// sfRestore dedups concurrent cold-revive of one archived sid: two replies to
	// the same archived bot message must not both run Archiver.Restore and double-
	// insert the transcript. Keyed by sid; the second caller joins the first's
	// result and finds the now-live session. Mirrors sfActive.
	sfRestore singleflight.Group

	// sfAlbum dedups concurrent fresh-session opens for one media-group album
	// (keyed scope+"\x00"+MediaGroupID): the album's sibling events each resolve
	// OpenNew on the group @=new path and must converge on ONE session (openNewSid).
	// albumSids (below, under mu) remembers the winner for late siblings.
	sfAlbum singleflight.Group

	// Per-sid arbiter state. recordPending always fires BEFORE busy[sid]=true so
	// a crash window can never lose a message.
	mu      sync.Mutex
	busy    map[string]bool                    // sid → a turn is in flight
	pending map[string][]contract.MessageEvent // sid → events queued during busy
	nudged  map[string][]string                // sid → msg ids pulled forward via nudge this turn (no WAL row; cleared with the served batch on success)
	dropped map[string]bool                    // sid → its pending batch overflowed; next turn announces
	sidMeta map[string]sidMetaRow              // sid → scope + turn token; lifecycle matches busy
	closing map[string]bool                    // sid → /close-session in flight: abandon queued work for this sid
	cancels map[string]context.CancelFunc      // sid → cancel the in-flight turn's ctx (the /close-session lever)
	// armedPrefer maps (scope, issuer) → the soft model tags that toggle armed and
	// no turn has consumed yet (/uncensored, /thinking). The value is a DELTA — the
	// tags to ADD — merged into the session's own stamp at consumption, never a
	// replacement for it (30-arbiter.go).
	//
	// Scope-keyed rather than sid-keyed on purpose: in a group, @=new means the arm
	// must land on whichever session the issuer's NEXT message opens or joins.
	// User-keyed because a stranger's turn in the same scope must skip — not consume
	// — the arm, so their session is never stamped by someone else's toggle.
	armedPrefer map[armKey][]string
	// tagMu serializes the READ-MODIFY-WRITE of a session's prefer_model_tags across
	// its two writers (toggleSessionTag removing one tag, the arbiter merging an arm
	// in). Per-tag independence means both now compute a new set from an old one, so
	// without this two toggles issued together — or a toggle racing a turn start —
	// lose one of the two edits. Deliberately NOT taken on the ordinary turn path:
	// the arbiter grabs it only when it actually has an arm to merge, which is rare,
	// so turn starts do not serialize on it.
	tagMu sync.Mutex
	// albumSids maps a media-group album (scope+"\x00"+MediaGroupID) to the ONE
	// fresh session its siblings converge on (openNewSid). TTL-bounded
	// (albumSidTTL, aligned with telegram's flushedStateTTL) and pruned
	// opportunistically on write — a handful of entries at most.
	albumSids map[string]albumSidEntry
	turnSeq   uint64         // monotonic per-turn token, incremented under mu when busy is set
	handleWg  sync.WaitGroup // tracks in-flight turn / drain goroutines
}

// NewManager builds a session manager over the store, turn handler, and a source
// lookup (the bus's SourceByName) for its pre-turn I/O — reactions, transient
// notices, reply targeting. handler may be a stub (logs, never blocks) until the
// turn module lands. The session holds no source objects: it resolves the live
// source by name through srcByName when it needs to.
func NewManager(st store.Store, handler contract.TurnHandler, srcByName func(string) contract.Source, cfg Config) *Manager {
	return &Manager{
		store:       st,
		handler:     handler,
		srcByName:   srcByName,
		config:      cfg.clamped(),
		busy:        map[string]bool{},
		pending:     map[string][]contract.MessageEvent{},
		nudged:      map[string][]string{},
		dropped:     map[string]bool{},
		sidMeta:     map[string]sidMetaRow{},
		closing:     map[string]bool{},
		cancels:     map[string]context.CancelFunc{},
		armedPrefer: map[armKey][]string{},
		albumSids:   map[string]albumSidEntry{},
	}
}

// SetAccounts wires the optional accounts lookup /whoami uses to show the sender's
// bound account. Lazy (the accounts module may Start after session); a nil func or
// nil return omits the account line. Called by the module at Start.
func (m *Manager) SetAccounts(f func() contract.Accounts) { m.accounts = f }

// SetGroupRouter wires the optional agora group-router the resolver uses for
// virtual-group member routing. Lazy (agora may Start after session); nil keeps the
// 1:1 group routing. Called by the module at Start.
func (m *Manager) SetGroupRouter(f func() contract.GroupRouter) { m.groupRouter = f }

// groupRoute resolves the group router if wired, else nil.
func (m *Manager) groupRoute() contract.GroupRouter {
	if m.groupRouter == nil {
		return nil
	}
	return m.groupRouter()
}

// SetRestore wires the cold-rehydrate hook (the module passes the Archiver's
// Restore). nil keeps the no-archiver behaviour: a reply to a non-live sid never
// revives and falls through to a fresh session.
func (m *Manager) SetRestore(f func(ctx context.Context, sid string) bool) { m.restore = f }

// Wait blocks until all in-flight turn/drain goroutines finish (shutdown drain).
func (m *Manager) Wait() { m.handleWg.Wait() }

// ResumeSession implements contract.SessionResume: wake sid to continue its task,
// applying the flow-declared resume policy. Today only an AUTO session is woken (an
// agora worker has no human present to re-trigger it); a manual/none session is left
// for the user's next message (no-op). It resolves the sid's scope and emits an
// internal MessageEvent — processed as a real inbound event (scope → sid → a fresh
// turn) — so the note rides as ordinary turn input, no bespoke resume hook needed.
func (m *Manager) ResumeSession(ctx context.Context, sid, note string) {
	if sid == "" || note == "" {
		return
	}
	mode, err := m.store.GetSessionMode(ctx, sid)
	if err != nil {
		// A missing row returns nil-err SessionModeNone, so this is a real store
		// failure — an auto worker's resume silently dropped here would wedge it
		// with zero trace, so at least leave one.
		slog.Warn("session: resume mode read failed", "sid", sid, "err", err)
		return
	}
	if mode != contract.SessionModeAuto {
		return // policy: only auto-mode sessions auto-resume; others wait for the user
	}
	info, err := m.store.SessionByID(ctx, sid)
	if err != nil {
		return // session gone — nothing to wake
	}
	// F88: resume ONLY if this is still the scope's ACTIVE session. The member scope is
	// shared (one active); a force_new task opened during the escalation window supersedes
	// this worker, so waking the scope now would land the resume in the WRONG (new)
	// session. Drop it instead — the superseded task was abandoned by whatever force-newed
	// the member. Logged so a genuinely-lost hand-back is visible (the browser copy stays
	// live, keyed by scope, so the login work isn't destroyed).
	if active, ok, cerr := m.store.CurrentSession(ctx, info.Key); cerr == nil && ok && active != sid {
		slog.Warn("session: resume dropped — session superseded (force-new during escalation)", "sid", sid, "active_sid", active, "scope", info.Key)
		return
	}
	src := m.srcByName(contract.SourceNameInternal)
	if src == nil {
		return
	}
	emitter, ok := src.(contract.MessageEmitter)
	if !ok {
		return
	}
	// Retry a busy bridge: an auto worker has no human to re-trigger it, so a dropped
	// resume would wedge it forever. ErrBridgeBusy is transient (buffer full); a few
	// short retries clear it. Any other error / a cancelled ctx → give up (logged).
	ev := contract.MessageEvent{ChatID: info.Key, Text: note}
	for attempt := 0; attempt < 4; attempt++ {
		err := emitter.Emit(ev)
		if err == nil {
			return
		}
		if !errors.Is(err, contract.ErrBridgeBusy) {
			slog.Warn("session: resume emit failed", "sid", sid, "err", err)
			return
		}
		select {
		case <-ctx.Done():
			slog.Warn("session: resume emit abandoned (ctx done)", "sid", sid)
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	slog.Warn("session: resume emit gave up — bridge stayed busy", "sid", sid)
}

var _ contract.SessionManager = (*Manager)(nil)
var _ contract.SessionResume = (*Manager)(nil)
