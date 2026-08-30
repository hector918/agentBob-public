// Package browser drives a headless Chromium per IM session via chromedp.
//
// Architecture (4 files, by layer):
//   - pool.go     — Pool + Session lifecycle: chromedp launch, idle TTL,
//     shutdown cleanup, console event ring buffer, JS dialog
//     handler. Engine-specific (chromedp); the only place
//     that imports github.com/chromedp/cdproto.
//   - ref_map.go  — accessibility-tree / interactive-element discovery and
//     the @e1, @e2 ref-ID protocol Hermes uses for click /
//     type. Pure JS injection on top of chromedp.
//   - actions.go  — every browser operation (Navigate, Click, Type, …) as a
//     plain Go function taking (ctx, *Session, params). Tool
//     surface is built on top of this. Engine-swap-friendly:
//     to move to playwright-go or remote CDP, this is the
//     single file to rewrite.
//   - tools.go    — 11 ToolSpec constructors. Each tool is a thin shim
//     that unmarshals args, calls the actions.go function,
//     and marshals the result. Hermes-named so skills /
//     system prompts written for Hermes work as-is.
//
// Native security model (mirrors §5.5 of docs/tools-design.md): chromedp
// spawns Chromium as a subprocess running as bob's UID. In native mode it
// has the host's network and filesystem access; container is the supported
// safe deployment.
//
// v1 scope:
//   - One tab per session_scope (multi-tab / page_id is a future slice; the
//     parameter has been dropped from the tool schemas — audit C4).
//   - Stealth flags applied by default.
//   - User-data-dir under per-session sandbox; cleaned on session close.
package browser

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentbob/sidecars/browser/bobhome"
	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/core"
	"agentbob/sidecars/browser/tools/file"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Session is one chromedp instance for one IM session_scope. Held by Pool.
// Public so actions.go can carry a *Session into every action func without
// re-acquiring through the Pool — actions.go is engine-agnostic, but
// Session exposes the engine handles it needs (browserCtx).
//
// State layers (important for the persistence story):
//   - on-disk in dataDir (cookies / localStorage / Cache) — survives
//     CloseInstance, idle sweep, eviction, bob restart. Only PurgeSession
//     deletes it (called from /new and /close-session).
//   - on-disk last_nav.txt next to dataDir — last successful navigate URL,
//     used to auto-resume the page when the instance is re-spawned after
//     idle close / restart. Cleared by PurgeSession too.
//   - in-memory chromedp browser instance — gone the moment CloseInstance
//     is called; re-spawned lazily on the next Acquire (data dir + cookies
//     restored, lastNavURL re-played if present).
type Session struct {
	mu          sync.Mutex
	allocCancel context.CancelFunc // cancels chromedp ExecAllocator
	BrowserCtx  context.Context    // ACTIVE tab context (exported for actions.go); starts == rootCtx, swapped on a tab switch
	browserCxl  context.CancelFunc
	dataDir     string // user-data-dir on disk; PRESERVED across CloseInstance — see Pool.PurgeSession
	// ephemeralCopy: this session is a read-only per-session worker COPY (seeded from a
	// member's master vault dir). Its dataDir is a disposable copy → CloseInstance
	// RemoveAll's it (a worker never writes the login back). false for the legacy +
	// login/profile paths (their dirs are preserved).
	ephemeralCopy bool
	// masterDir: for a COPY, the member master dir it was seeded from — the WRITE-BACK
	// target. SaveLogin (保存登陆) captures live cookies → the master sidecar without closing;
	// the controlled-close path writes the on-disk storage (dataDir → masterDir) back when the
	// copy naturally closes. "" for non-copy sessions.
	masterDir string
	// controlled: a human took over this copy via the takeover (so a controlled-close is a
	// fallback write-back). Set by the takeover screencast start; read on CloseInstance.
	controlled bool
	// loginSaved: SaveLogin (保存登陆) captured this copy's cookies to the master sidecar. Once
	// set, the controlled-close fallback no longer RE-captures cookies — the worker may have
	// kept browsing (even logged out) after the human saved, and the explicit save wins.
	loginSaved  bool
	lastUsed    time.Time
	console     *consoleRing // ring buffer of console events for browser_console
	dialogReply *dialogReply // pre-set reply for the next JS dialog (nil → default accept)

	// Multi-tab / popup handling. A page click or window.open
	// can spawn a new browser target (tab / popup); chromedp binds to one
	// target at a time, so without tracking the model goes blind to popups
	// (Taobao login, OAuth windows, target=_blank). tabs.go owns the methods.
	// All these fields are guarded by mu. The turn loop is sequential per
	// Session (no concurrent actions on one Session), so a tab swap only ever
	// happens between actions — but mu still guards every read/write so a
	// field is never observed half-written.
	//
	//   rootCtx     — the ORIGINAL browserCtx (tab 1 + the Browser handle).
	//                 Used for Targets() enumeration and as the parent when
	//                 binding child contexts to other targets. NEVER swapped.
	//   rootTarget  — tab 1's target id; captured right after the launch Run.
	//   activeTarget— the target BrowserCtx currently points at; == rootTarget
	//                 when on tab 1.
	//   tabBindings — per-tab child ctx + its cancel, keyed by target id. A
	//                 binding is created the FIRST time switchTo binds a
	//                 non-root tab and KEPT thereafter, so tabs stay open while
	//                 the model switches between them. switching no longer
	//                 cancels the tab it leaves — and that matters because in
	//                 chromedp, cancelling a WithTargetID child ctx
	//                 DetachFromTarget+CloseTarget's it (chromedp.go), so the
	//                 old single shared cancel silently closed a tab on every
	//                 switch. closeTab cancels the binding (chromedp closes the
	//                 target); reconcile cancels bindings whose target vanished.
	//                 The root tab has NO binding (rootCtx is owned by browserCxl).
	//   tabs        — stably-ordered list of live page target ids. The model
	//                 addresses tabs by 1-based index into this slice.
	rootCtx      context.Context
	rootTarget   target.ID
	activeTarget target.ID
	tabBindings  map[target.ID]tabBinding
	tabs         []target.ID

	// bindMu serialises switchTo's bind-new-tab section (tabs.go), which
	// spans an UNLOCKED CDP round-trip (NewContext+Run+attach) before the
	// map write — mu alone can't make that check-then-bind atomic, and the
	// screencast follow supervisor really does race human/model tab
	// switches for the same popup. Always acquired before mu, never inside.
	bindMu sync.Mutex

	// Screencast fan-out (screencast.go). chromedp listeners cannot be
	// removed, so the frame listener for a tab ctx must be attached at most
	// ONCE per ctx for the Session's lifetime — attaching per takeover would
	// stack one closure (plus its captured frame buffer) on the long-lived
	// rootCtx every time a takeover opens (EventSource auto-reconnects
	// included). castBound is that dedup set; castSinks holds the intakes of
	// the currently-open takeover streams keyed by a registration seq;
	// castUsers refcounts the page-global Page.startScreencast per tab ctx
	// (castAcquire/castRelease) so concurrent streams don't stop each other's
	// frames. Entries are dropped with the tab (dropCastState).
	// Guarded by castMu, NOT mu: the listener fires on the chromedp
	// dispatcher goroutine and must not contend with tab-swap sections.
	castMu    sync.Mutex
	castBound map[context.Context]bool
	castSinks map[int]castSink
	castUsers map[context.Context]int
	castSeq   int

	// targetsFn lists the browser's live targets. Defaults to a thin wrapper
	// over chromedp.Targets(rootCtx); overridable in tests so the tab
	// bookkeeping (reconcile add/remove, tabList mapping) is exercisable
	// without launching a real chromium.
	targetsFn func() ([]*target.Info, error)

	// TD6/L1 fix — SPA loop guard.
	// clickFailsSinceSnapshot counts consecutive `selector not found`
	// errors from browser_click since the last successful snapshot. A
	// snapshot resets the counter; click success resets it; click failure
	// bumps it. Three consecutive fails → Click refuses with "page
	// unstable" instead of letting the model spin into another
	// snapshot-click cycle (each spin = an LLM round-trip). Read/written
	// under mu.
	clickFailsSinceSnapshot int

	// humanize mirrors cfg.HumanizeEff() at spawn time: whether Click/Type
	// simulate human input (humanize.go) instead of instant CDP dispatch.
	// Immutable after construction — read without mu.
	humanize bool

	// cursorX/cursorY — the virtual mouse position in viewport coordinates,
	// advanced by each completed humanized click so the next trajectory
	// starts where the last one ended (a real pointer never teleports).
	// Zero value = never placed; cursorStart randomizes it on first use.
	// Guarded by mu.
	cursorX, cursorY float64
}

// dialogReply is a one-shot pre-set response for the next JS dialog the
// page shows. Auto-cleared after one use; primed by browser_dialog tool.
type dialogReply struct {
	accept     bool
	promptText string
}

// Pool manages per-session_scope Session instances. Browser instances are
// lazy: the first browser_* call for a session creates one, subsequent
// calls reuse it, and the periodic sweep closes ones idle longer than
// cfg.IdleTTLSeconds.
//
// lifeCtx + lifeCancel (T2 fix) — parent context for every
// chromedp ExecAllocator the Pool spawns. Previously the allocator
// used context.Background() so cancelling a session's ctx mid-launch
// could leave chromium running until the next idle sweep. Now Pool
// owns a lifecycle ctx; all chromium subprocesses are descendants and
// die when Pool.Close() runs.
//
// spawnLocks (T3 fix) — per-key serialisation of "first Acquire spawns
// chromium" so two concurrent Acquires for the same session don't
// both launch chromium on the same --user-data-dir (which corrupts
// the profile silently — two chromiums fighting over the same SQLite).
type Pool struct {
	cfg            config.BrowserConfig
	fsCfg          config.FilesystemConfig
	namespaceByBot bool

	lifeCtx    context.Context
	lifeCancel context.CancelFunc

	mu       sync.Mutex
	sessions map[string]*Session

	// takeoverActive counts the live takeover screencast streams per pool
	// key (StartScreencast +1 / its stop -1; guarded by mu, lazily
	// allocated). A key with a live stream is exempt from SweepIdle and LRU
	// eviction: takeover input does not refresh lastUsed, so a human-driven
	// instance otherwise looks idle. The custodian Pin covers only PROFILE
	// checkouts; legacy scope-keyed instances have no checkout, and this
	// pool-side count is their only mid-takeover protection.
	takeoverActive map[string]int

	spawnMu    sync.Mutex
	spawnLocks map[string]*sync.Mutex // per-key gate around chromium spawn

	// masterLocks serialise, per MEMBER master dir, a copy seed (reads the master) against
	// a write-back (atomically replaces it) — so a worker never seeds from a half-replaced
	// master. The per-member transaction from docs/wip-browser-profile-identity.md.
	masterMu    sync.Mutex
	masterLocks map[string]*sync.Mutex

	writebacks []WritebackDebug // recent write-back history (test hook; guarded by mu, capped)

	// Per-person browser-profile layer (docs/remote-browser-handoff.md).
	// gate authorizes credential:use:browser:<id>; custodian owns the
	// exclusive per-profile checkout lifecycle. Both nil-safe: a nil gate (no
	// permissions plugin) means AcquireFor never takes the profile path, so the
	// browser behaves exactly as the legacy per-scope ephemeral one (reducible
	// leaf). custodian is always created (cheap idle goroutine) so AcquireFor
	// can check out on the profile path without a nil guard.
	gate      core.Gate
	custodian *ProfileCustodian

	// holds is the per-scope human control-hold registry (takeover
	// yield+resume — hold.go). AcquireFor yields while the turn's scope is
	// held so the model never fights the human over the page. nil-safe
	// (ControlHolds methods tolerate a nil receiver), set via
	// SetControlHolds at startup.
	holds *ControlHolds
}

// profileCheckoutIdleTTL is how long a per-person browser-profile checkout may
// go untouched before the custodian reclaims it (closes the instance, frees the
// exclusive hold). 5 min: long enough to span a model's think time / inter-turn
// gaps, short enough that a shared profile frees quickly between tasks. It is
// the UNIVERSAL release backstop — agora sessions are chat-scoped and rarely
// finalize, so this is effectively their primary release. Distinct from
// cfg.IdleTTLSeconds (the legacy chromium-instance idle close, 30 min).
const profileCheckoutIdleTTL = 5 * time.Minute

// NewPool constructs an empty Pool. Browser instances are lazy.
func NewPool(cfg config.BrowserConfig, fsCfg config.FilesystemConfig, namespaceByBot bool) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		cfg:            cfg,
		fsCfg:          fsCfg,
		namespaceByBot: namespaceByBot,
		lifeCtx:        ctx,
		lifeCancel:     cancel,
		sessions:       map[string]*Session{},
		spawnLocks:     map[string]*sync.Mutex{},
	}
	// The custodian's closer drives the SAME instance teardown as everything
	// else (Pool.CloseInstance) so a reclaim/release closes chromium + frees the
	// pool slot identically to an idle sweep.
	p.custodian = NewProfileCustodian(p.CloseInstance, profileCheckoutIdleTTL, nil)
	// Worker COPY dirs are ephemeral and owned by CloseInstance, but a crash / SIGKILL
	// can leave orphans behind. Wipe the whole copies root at startup — there are no live
	// copies yet, and nothing persistent lives here (the master is the vault dir).
	if copies := bobhome.Path("browser-copies"); copies != "" {
		if err := os.RemoveAll(copies); err != nil {
			slog.Warn("browser: failed to clear orphaned copy dirs at startup", "path", copies, "err", err)
		}
	}
	// Recover any master whose write-back was interrupted mid-swap, then clear staging.
	ReconcileWritebacks(ProfileVaultRoot())
	return p
}

// SetProfileGate wires the authorization gate for per-person browser profiles.
// Until set (or set to nil), AcquireFor always takes the legacy per-scope path,
// so a deployment without the permissions plugin keeps today's behaviour.
func (p *Pool) SetProfileGate(g core.Gate) { p.gate = g }

// SetControlHolds wires the human control-hold registry (hold.go). Until set,
// AcquireFor never yields (nil ControlHolds reports nothing held).
func (p *Pool) SetControlHolds(h *ControlHolds) { p.holds = h }

// Custodian exposes the profile-checkout custodian for the lifecycle wiring
// (EndSession → ReleaseScope, the reclaim ticker). nil-safe callers only.
func (p *Pool) Custodian() *ProfileCustodian { return p.custodian }

// spawnLockFor returns (and lazily creates) the per-key spawn mutex.
// Held during chromedp ExecAllocator → chromedp.Run startup; released
// once the Session is in the sessions map (so the next Acquire sees
// the existing session and bypasses the spawn path entirely).
func (p *Pool) spawnLockFor(key string) *sync.Mutex {
	p.spawnMu.Lock()
	defer p.spawnMu.Unlock()
	m, ok := p.spawnLocks[key]
	if !ok {
		m = &sync.Mutex{}
		p.spawnLocks[key] = m
	}
	return m
}

// masterLockFor returns the per-master-dir mutex (the per-member write-back transaction).
func (p *Pool) masterLockFor(dir string) *sync.Mutex {
	p.masterMu.Lock()
	defer p.masterMu.Unlock()
	if p.masterLocks == nil {
		p.masterLocks = map[string]*sync.Mutex{}
	}
	m, ok := p.masterLocks[dir]
	if !ok {
		m = &sync.Mutex{}
		p.masterLocks[dir] = m
	}
	return m
}

// SaveLogin (保存登陆) captures the LIVE worker COPY's login at scope into the member master's
// cookie sidecar WITHOUT closing it — the human keeps the browser and the worker resumes on
// the SAME live, logged-in instance (no re-seed → no lost session cookies). It reads the full
// cookie set (session cookies included, which never reach disk) straight from chromium's
// memory via CDP, so it needs no graceful close. The on-disk storage (localStorage / IndexedDB
// / Service Worker) is written back later by the controlled-close path when the copy naturally
// closes. No-op when there is no live copy / master.
func (p *Pool) SaveLogin(scope string) error {
	key := profileCopyKey(scope)
	p.mu.Lock()
	s, ok := p.sessions[key]
	p.mu.Unlock()
	if !ok || s.masterDir == "" {
		slog.Info("browser save-login: no live copy / master for scope — noop", "scope", scope)
		return nil
	}
	// Capture under the per-member master lock so this serialises against a concurrent seed's
	// dir copy / a controlled-close write-back on the same master. (The sidecar reader,
	// injectLoginCookies, is NOT under this lock — its torn-read safety comes from the atomic
	// temp+rename in captureLoginCookies, not the lock.)
	mu := p.masterLockFor(s.masterDir)
	n, err := func() (int, error) { mu.Lock(); defer mu.Unlock(); return captureLoginCookies(s.rootCtx, s.masterDir) }()
	if err != nil {
		slog.Error("browser save-login: capture FAILED", "scope", scope, "master", s.masterDir, "err", err)
		return err
	}
	// Mark saved so the controlled-close fallback won't overwrite this sidecar with the worker's
	// later (possibly logged-out) state — the explicit 保存登陆 wins.
	s.mu.Lock()
	s.loginSaved = true
	s.mu.Unlock()
	slog.Info("browser save-login: captured live login to master sidecar (no close)", "scope", scope, "master", s.masterDir, "cookies", n)
	p.recordWriteback(scope, s.masterDir, int64(n))
	return nil
}

// --- test hooks (docs/wip-browser-profile-identity.md) -----------------------

// CopyDebug / WritebackDebug / CopiesDebug back GET /debug/copies so a remote test can
// verify the seed → takeover → 保存登陆 → master flow without shell access to the box.
type CopyDebug struct {
	Key        string `json:"key"`
	MasterDir  string `json:"master_dir"`
	Controlled bool   `json:"controlled"`
}
type WritebackDebug struct {
	Scope  string `json:"scope"`
	Master string `json:"master"`
	Bytes  int64  `json:"bytes"`
	UnixMs int64  `json:"unix_ms"`
}
type CopiesDebug struct {
	Copies     []CopyDebug      `json:"copies"`
	Writebacks []WritebackDebug `json:"writebacks"` // most-recent last, capped
}

const maxWritebackHistory = 32

func (p *Pool) recordWriteback(scope, master string, n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writebacks = append(p.writebacks, WritebackDebug{Scope: scope, Master: master, Bytes: n, UnixMs: time.Now().UnixMilli()})
	if len(p.writebacks) > maxWritebackHistory {
		p.writebacks = p.writebacks[len(p.writebacks)-maxWritebackHistory:]
	}
}

// DebugCopies snapshots the live worker copies + the recent write-back history.
func (p *Pool) DebugCopies() CopiesDebug {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := CopiesDebug{Writebacks: append([]WritebackDebug(nil), p.writebacks...)}
	for key, s := range p.sessions {
		if s.ephemeralCopy {
			out.Copies = append(out.Copies, CopyDebug{Key: key, MasterDir: s.masterDir, Controlled: s.controlled})
		}
	}
	return out
}

// Cfg returns the BrowserConfig the Pool was built with. Used by tools.go
// to thread per-call timeouts without keeping a separate config copy on
// each tool struct.
func (p *Pool) Cfg() config.BrowserConfig { return p.cfg }

// Acquire returns the per-session_scope Session, lazy-creating it on first
// use. The lastUsed timestamp is refreshed so the idle sweep treats this
// session as active.
func (p *Pool) Acquire(actx core.AgentCtx) (*Session, error) {
	key := actx.SessionScope
	if key == "" {
		key = "default"
	}
	return p.acquireKeyed(key, p.dataDirFor(key), false, "", false) // legacy ephemeral dir
}

// AcquireFor is the gated entry the browser tools use. It picks between the
// per-person browser-profile path and the legacy per-scope path:
//
//   - profile path (gated, persistent, exclusive) — taken only when the turn
//     has a bound account AND a gate is wired AND that account's profile
//     credential is granted (credential:use:browser:<id>). The profile is
//     checked out exclusively for the turn's scope (head-to-tail; a different
//     scope wanting it fail-fasts), keyed by ProfileName so it can't collide
//     with a chat-scope key in the pool map, and its chromium runs on the vault
//     profile dir.
//   - legacy path otherwise (no account / no gate / no grant) — today's
//     per-scope ephemeral browser, unchanged. This keeps the feature opt-in and
//     the whole subsystem a reducible leaf.
//
// Re-checking out the same profile from the same scope on each action is the
// per-action re-gate (immediate revocation: revoke the grant and the next
// action drops back to a fresh legacy browser) AND the activity touch (the
// custodian refreshes lastTouched on every successful checkout).
func (p *Pool) AcquireFor(ctx context.Context, actx core.AgentCtx) (*Session, error) {
	// Human control-hold gate (hold.go): while a human drives this scope's
	// browser via the takeover overlay, every model-side action yields with
	// the fixed envelope instead of competing for the page. Checked BEFORE
	// any routing/spawn so a held scope costs nothing and — deliberately —
	// does not refresh the custodian's idle clock. The human path
	// (StartScreencastForScope / DispatchInputForScope) never passes here.
	if p.holds.Held(actx.SessionScope) {
		return nil, ErrHeldByHuman
	}
	useProfile, key, dir, err := p.profileRoute(ctx, actx)
	if err != nil {
		return nil, err
	}
	if !useProfile {
		return p.Acquire(actx)
	}
	scope := actx.SessionScope
	if !p.custodian.Checkout(key, scope) {
		return nil, fmt.Errorf("browser busy: another conversation is currently using this profile — retry shortly")
	}
	sess, err := p.acquireKeyed(key, dir, true, "", false) // persistent profile vault dir
	if err != nil {
		// Spawn failed after a successful checkout — free the hold now instead of
		// leaving the profile "busy" until idle reclaim.
		p.custodian.Release(key, scope)
		return nil, err
	}
	return sess, nil
}

// profileRoute decides whether this acquire should use a gated per-person
// profile (returning its pool key + vault dataDir) or fall back to the legacy
// per-scope path (useProfile=false). Side-effect on the granted path only:
// ProfileDir lazily creates the 0o700 vault dir — which is fine because the
// gate has already authorized this identity's profile credential.
func (p *Pool) profileRoute(ctx context.Context, actx core.AgentCtx) (useProfile bool, key, dir string, err error) {
	// The gate + identity pick lives in ProfileIdentity (shared with the
	// browserd thin client): the browser login belongs to the IDENTITY THE
	// TURN ACTS AS — agora Member first (server-resolved, not spoofable),
	// the personal AccountID otherwise. "" → no grant / no identity / no
	// gate → legacy ephemeral (opt-in). docs/remote-browser-handoff.md.
	id := ProfileIdentity(ctx, p.gate, actx)
	if id == "" {
		return false, "", "", nil
	}
	d, derr := ProfileDir(id)
	if derr != nil {
		return false, "", "", derr
	}
	return true, ProfileName(id), d, nil
}

// AcquireRouted is the UNGATED routing entry the browserd control plane uses
// (docs/browserd.md P1). The permission gate stays bob-side: bob resolves the
// acting identity via ProfileIdentity and ships it as profileID; browserd is a
// trusted private-network executor and only routes. Empty profileID takes the
// legacy per-scope path (today's ephemeral browser, key = scope).
//
// mode (browser profile identity Phase 2, docs/wip-browser-profile-identity.md):
//   - "" / in-place: the custodian-exclusive vault dir, keyed by ProfileName — the
//     SINGLE writer of the master login. The LOGIN session a human takes over (and the
//     legacy gated path) use this; the takeover targets it (keyed by ProfileName/scope).
//   - "copy": a read-only per-session WORKER copy — its own pool key + its own dir,
//     seeded fresh from the master vault dir, NO custodian checkout (so concurrent
//     same-principal turns never collide on the one exclusive instance), discarded on
//     close. The worker never writes the login back.
func (p *Pool) AcquireRouted(scope, profileID, mode string) (*Session, error) {
	if profileID == "" {
		key := scope
		if key == "" {
			key = "default"
		}
		return p.acquireKeyed(key, p.dataDirFor(key), false, "", false) // legacy ephemeral dir
	}
	master, err := ProfileDir(profileID)
	if err != nil {
		return nil, err
	}
	if mode == "copy" {
		// Per-session worker copy: distinct key (no collision with the ProfileName login
		// key) + own dir seeded from the master, no custodian checkout, discarded on close.
		return p.acquireKeyed(profileCopyKey(scope), profileCopyDir(scope), false, master, true)
	}
	key := ProfileName(profileID)
	if !p.custodian.Checkout(key, scope) {
		return nil, fmt.Errorf("browser busy: another conversation is currently using this profile — retry shortly")
	}
	sess, err := p.acquireKeyed(key, master, true, "", false) // persistent profile vault dir
	if err != nil {
		// Spawn failed after a successful checkout — free the hold now instead
		// of leaving the profile "busy" until idle reclaim.
		p.custodian.Release(key, scope)
		return nil, err
	}
	return sess, nil
}

// profileCopyKey / profileCopyDir address a per-session worker copy. The key prefix is
// distinct from ProfileName ("browser_") and legacy scope keys so a copy never collides
// in the sessions / spawn-lock maps; the dir lives under its own root (browser-copies),
// separate from the vault and the legacy sandbox, so the existing sweepers leave it alone
// and CloseInstance owns its lifetime.
func profileCopyKey(scope string) string { return "browsercopy_" + file.SanitizeKey(scope) }
func profileCopyDir(scope string) string {
	return bobhome.Path("browser-copies", file.SanitizeKey(scope))
}

// acquireKeyed is the engine core shared by the legacy per-scope path (Acquire,
// key=session_scope, dataDir=sandbox subtree) and the per-identity
// browser-profile path (custodian, key=identity, dataDir=the profile's vault
// dir). The two keying schemes use distinct key strings so they never collide
// in the sessions map / spawn-lock map. Everything below — fast-path reuse,
// per-key spawn serialisation, chromium launch, console/dialog wiring,
// auto-resume, LRU eviction — is identical regardless of which path called in.
// seedFrom (copy mode): a master profile dir to seed a FRESH per-session copy from;
// "" = no copy (dataDir used as-is). ephemeral: discard the dataDir on close (a worker
// copy, vs a preserved login/legacy dir).
func (p *Pool) acquireKeyed(key, dataDir string, persistent bool, seedFrom string, ephemeral bool) (*Session, error) {
	p.mu.Lock()
	if s, ok := p.sessions[key]; ok {
		s.mu.Lock()
		s.lastUsed = time.Now()
		s.mu.Unlock()
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()

	// T3 fix: per-key serialisation around chromium spawn.
	// Without this, two concurrent first-Acquire calls for the same
	// key both launched chromium on the same --user-data-dir,
	// corrupting the profile (two processes fighting over the same
	// SQLite databases). The post-spawn map check still runs as a
	// safety net but the spawn lock makes the race impossible.
	//
	// The per-key entry is NEVER deleted from spawnLocks. An earlier
	// revision deleted it after every Acquire (to bound map growth),
	// but that re-opened the very race the lock guards: on a launch
	// failure (chromedp.Run err → no sessions entry written) the defer
	// would delete the key while a sibling Acquire was either blocked
	// on this same mutex or about to spawnLockFor it. The blocked
	// sibling resumes holding the now-orphaned mutex while a third
	// Acquire creates a fresh mutex for the re-inserted key, and both
	// enter the spawn path concurrently on the same deterministic
	// dataDir. Keeping one persistent mutex per key makes the lock a
	// true per-key critical section. Map growth is bounded by the
	// SanitizeKey'd key set (the few-tens-of-IM-groups case bob
	// targets); each entry is a bare sync.Mutex (~8 bytes).
	spawnLock := p.spawnLockFor(key)
	spawnLock.Lock()
	defer spawnLock.Unlock()

	// Re-check under spawn lock: if a sibling Acquire spawned while we
	// were waiting, use that session and skip the launch.
	p.mu.Lock()
	if s, ok := p.sessions[key]; ok {
		s.mu.Lock()
		s.lastUsed = time.Now()
		s.mu.Unlock()
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()

	// Copy mode: seed a FRESH per-session copy from the master (seedFrom) under the
	// spawn lock, so the worker runs on its OWN dir and concurrent same-principal turns
	// never share one. RemoveAll first so a copy a crashed prior session left behind
	// can't poison the seed. Runs only on a real spawn — a reused session returned at the
	// fast-path / re-check above never reaches here, so a turn's later actions reuse the
	// one copy (no per-action re-copy).
	if seedFrom != "" {
		// Under the per-member master lock: a concurrent write-back atomically replaces the
		// master, and this seed must read a whole one (not a half-replaced dir mid-walk).
		mu := p.masterLockFor(seedFrom)
		mu.Lock()
		rerr := os.RemoveAll(dataDir)
		var cerr error
		if rerr == nil {
			cerr = copyProfileDir(seedFrom, dataDir)
		}
		mu.Unlock()
		if rerr != nil {
			return nil, fmt.Errorf("browser copy: clear stale copy %s: %w", dataDir, rerr)
		}
		if cerr != nil {
			return nil, fmt.Errorf("browser copy: seed from master: %w", cerr)
		}
		slog.Info("browser copy: seeded from master", "key", key, "master", seedFrom)
	}

	// dataDir is supplied by the caller: Acquire derives the legacy scope→
	// sandbox path via dataDirFor; the profile path passes the vault dir. The
	// derivation that used to live here now lives in dataDirFor (its single
	// source of truth), so Acquire and PurgeSession/HasSession agree.
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("browser: create data dir %s: %w", dataDir, err)
	}

	// A persistent profile that was left by a chromium which exited uncleanly
	// (crash, OOM, or a bob restart that SIGKILLs the subprocess) keeps a stale
	// SingletonLock/Socket/Cookie in its user-data-dir; the next launch then
	// fails with "profile appears to be in use" and chromium won't start. The
	// custodian holds this profile's checkout EXCLUSIVELY (no other bob chromium
	// is on this dir), so clearing those stale lock files here is safe and lets
	// a restarted bob re-open a preserved login instead of dead-locking on it.
	// Only the persistent (profile) path needs this; the legacy ephemeral dir is
	// wiped on launch failure anyway.
	if persistent {
		clearStaleProfileLocks(dataDir)
	}

	// chromedp ExecAllocator. DefaultExecAllocatorOptions is an array;
	// slice it before append.
	def := chromedp.DefaultExecAllocatorOptions[:]
	opts := append([]chromedp.ExecAllocatorOption(nil), def...)
	winW, winH := p.cfg.WindowSizeEff()
	opts = append(opts,
		chromedp.UserDataDir(dataDir),
		chromedp.NoSandbox,              // alpine chromium needs this; safe in our deploy
		chromedp.WindowSize(winW, winH), // else headless defaults to a small 800×600 viewport
	)
	if !p.cfg.HeadlessEff() {
		opts = append(opts, chromedp.Flag("headless", false))
	}
	if p.cfg.ExecPath != "" {
		opts = append(opts, chromedp.ExecPath(p.cfg.ExecPath))
	}
	if p.cfg.StealthFlagsEff() {
		opts = append(opts,
			chromedp.Flag("disable-blink-features", "AutomationControlled"),
			chromedp.Flag("disable-features", "IsolateOrigins,site-per-process"),
			chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
		)
	}

	// T2 fix: parent is p.lifeCtx, not context.Background.
	// That way Pool.Close cancels every chromium subprocess and the
	// caller (Hub.Close) doesn't have to enumerate sessions explicitly.
	allocCtx, allocCancel := chromedp.NewExecAllocator(p.lifeCtx, opts...)
	browserCtx, browserCxl := chromedp.NewContext(allocCtx)

	// Force the browser to start so missing-chromium / wrong-path fails
	// immediately (loud error) instead of inside the first navigate.
	if err := chromedp.Run(browserCtx); err != nil {
		browserCxl()
		allocCancel()
		// NEVER wipe a persistent profile dir on launch failure: it is a
		// credential-grade asset (the user's saved login). The original cleanup
		// assumed dataDir was always an ephemeral sandbox — true only for the
		// legacy path. Wiping a profile here destroyed the login on any transient
		// launch failure (e.g. a stale lock from an unclean prior exit). Only the
		// ephemeral legacy dir is safe to remove.
		if !persistent {
			if rerr := os.RemoveAll(dataDir); rerr != nil {
				slog.Warn("browser: dataDir cleanup failed on launch failure (may accumulate)", "path", dataDir, "err", rerr)
			}
		}
		return nil, fmt.Errorf("browser: launch chromium: %w (set tools.browser.exec_path or apt/apk install chromium)", err)
	}

	ring := newConsoleRing(256, browserCtx.Done())
	attachConsoleListener(browserCtx, ring)

	// Capture tab 1's target id now that the launch Run has attached the
	// Browser/Target to browserCtx. This is the root tab the multi-tab
	// manager (tabs.go) anchors on — rootCtx is never swapped.
	rootTarget := chromedp.FromContext(browserCtx).Target.TargetID

	s := &Session{
		allocCancel:   allocCancel,
		BrowserCtx:    browserCtx,
		browserCxl:    browserCxl,
		dataDir:       dataDir,
		lastUsed:      time.Now(),
		console:       ring,
		rootCtx:       browserCtx,
		rootTarget:    rootTarget,
		activeTarget:  rootTarget,
		tabBindings:   map[target.ID]tabBinding{},
		tabs:          []target.ID{rootTarget},
		humanize:      p.cfg.HumanizeEff(),
		ephemeralCopy: ephemeral,
		masterDir:     seedFrom, // copy mode: the member master to write back to ("" otherwise)
	}
	s.targetsFn = func() ([]*target.Info, error) { return chromedp.Targets(s.rootCtx) }
	attachDialogHandler(browserCtx, s)

	// Copy mode: restore the master's saved login (cookies incl session cookies, which the
	// on-disk seed cannot carry) into the fresh copy BEFORE the auto-resume navigate, so the
	// resumed page loads logged in. Best-effort — a never-saved master has no sidecar (no-op).
	if seedFrom != "" {
		if n, ierr := injectLoginCookies(browserCtx, seedFrom); ierr != nil {
			slog.Warn("browser copy: inject login cookies failed", "key", key, "err", ierr)
		} else if n > 0 {
			slog.Info("browser copy: injected login cookies from master sidecar", "key", key, "cookies", n)
		}
	}

	// Auto-resume: if a previous turn navigated somewhere on this session,
	// restore that page so subsequent browser_snapshot/click/etc see the
	// page they expect (chromedp would come up on about:blank otherwise).
	// Best-effort — if the resume nav fails, log and continue; the next
	// browser_navigate the model issues will establish fresh state.
	//
	// Concurrency: this runs BEFORE the session is published into
	// p.sessions (we still hold spawnLock, so no concurrent Acquire can
	// reach this key yet — the fast path at the top of Acquire only hits a
	// session already in the map). Doing the resume navigate here, rather
	// than after the map insert, guarantees no other goroutine can issue a
	// concurrent chromedp.Run on this browserCtx while the resume nav is in
	// flight (chromedp/CDP is not safe for overlapping commands on one
	// target). No p.mu / s.mu is held across the navigate, so there's no
	// deadlock — spawnLock alone provides the exclusion.
	if lastURL := readLastNavURL(dataDir); lastURL != "" {
		pageTO := time.Duration(p.cfg.PageTimeoutSecondsEff()) * time.Second
		nctx, ncancel := context.WithTimeout(browserCtx, pageTO)
		if err := chromedp.Run(nctx, chromedp.Navigate(lastURL)); err != nil {
			slog.Warn("browser session: auto-resume navigate failed", "session", key, "url", lastURL, "err", err)
		} else {
			slog.Info("browser session: auto-resumed last nav URL", "session", key, "url", lastURL)
		}
		ncancel()
	}

	// Eviction exemption set, computed BEFORE taking p.mu: pinnedProfileKeys
	// round-trips the custodian actor, whose closer (CloseInstance) takes
	// p.mu — holding p.mu across it could deadlock.
	pinned := p.pinnedProfileKeys()
	p.mu.Lock()
	if existing, ok := p.sessions[key]; ok {
		// Race: another goroutine created the session first. Use whoever
		// got there first; cancel our chromium contexts. We do NOT delete
		// dataDir here — it's the deterministic shared path computed from
		// file.SanitizeKey(key), so the winner is using the very same dir.
		// Removing it would corrupt the winning session.
		p.mu.Unlock()
		browserCxl()
		allocCancel()
		existing.mu.Lock()
		existing.lastUsed = time.Now()
		existing.mu.Unlock()
		return existing, nil
	}
	// Audit B1: enforce a global cap so a burst of active IM groups
	// can't OOM the host (each chromium ~200 MB). Evict the LRU
	// session before adding the new one. Eviction uses CloseInstance
	// (preserves cookies); the evicted session can re-spawn warm next
	// time it's accessed. When every candidate is mid-takeover the cap is
	// exceeded temporarily (evictKey "") — never close a browser under a
	// human's hands.
	cap := p.cfg.MaxConcurrentSessionsEff()
	var evictKey string
	if len(p.sessions) >= cap {
		evictKey = p.lruKeyLocked(pinned)
	}
	p.sessions[key] = s
	p.mu.Unlock()
	if evictKey != "" {
		slog.Info("browser pool full — evicting LRU session (data dir preserved)", "evicted", evictKey, "cap", cap)
		p.CloseInstance(evictKey)
	}
	slog.Info("browser session opened", "session", key, "data_dir", dataDir)
	return s, nil
}

// lruKeyLocked finds the least-recently-used EVICTABLE session key. Caller
// must hold p.mu and pass the pinned-profile set (pinnedProfileKeys, taken
// before locking). Pinned or takeover-active keys are never returned: a
// takeover freezes lastUsed, so the human's instance would otherwise always
// be the LRU first choice — same invariant as SweepIdle. Returns "" when
// there is nothing evictable; the caller then temporarily exceeds the cap.
func (p *Pool) lruKeyLocked(pinned map[string]bool) string {
	var oldestKey string
	var oldestTime time.Time
	for k, s := range p.sessions {
		if pinned[k] || p.takeoverActive[k] > 0 {
			continue
		}
		s.mu.Lock()
		t := s.lastUsed
		s.mu.Unlock()
		if oldestKey == "" || t.Before(oldestTime) {
			oldestKey = k
			oldestTime = t
		}
	}
	return oldestKey
}

// pinnedProfileKeys returns the pool keys (custodian profile keys) of
// checkouts currently handoff-pinned. Must be called WITHOUT p.mu held:
// Snapshot round-trips the custodian actor, whose closer (Pool.CloseInstance)
// takes p.mu — holding p.mu across the call can deadlock.
func (p *Pool) pinnedProfileKeys() map[string]bool {
	pinned := map[string]bool{}
	if p.custodian != nil {
		for _, cv := range p.custodian.Snapshot() {
			if cv.Pinned {
				pinned[cv.Profile] = true
			}
		}
	}
	return pinned
}

// CloseInstance closes the per-session chromium instance but PRESERVES
// the on-disk data dir (cookies / localStorage / last_nav.txt). The
// session can re-spawn warm on the next Acquire — Acquire reads the
// preserved last_nav.txt and auto-navigates back to the previous URL,
// and chromium picks up the cookies from the dataDir → "as if it
// never closed" except for in-memory page DOM state.
//
// Idempotent. Called from idle sweep, pool eviction, and CloseAll
// (bob shutdown). For "user explicitly wants a clean slate" use
// PurgeSession instead.
func (p *Pool) CloseInstance(sessionScope string) {
	p.mu.Lock()
	s, ok := p.sessions[sessionScope]
	var controlled, loginSaved bool // captured under p.mu (StartScreencast / SaveLogin set them too)
	if ok {
		delete(p.sessions, sessionScope)
		controlled = s.controlled
		s.mu.Lock()
		loginSaved = s.loginSaved
		s.mu.Unlock()
	}
	p.mu.Unlock()
	if !ok {
		return
	}
	// Cancel every live child-tab ctx (popups / OAuth windows the session
	// opened) before tearing down the root ctx / allocator, so their chromedp
	// event-pump goroutines exit. Root has no entry (rootCtx is browserCxl's).
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.tabBindings))
	for id, b := range s.tabBindings {
		cancels = append(cancels, b.cancel)
		delete(s.tabBindings, id)
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	// A controlled worker COPY (a human took it over + logged in) saves its login on close even
	// if 保存登陆 was never pressed (e.g. the takeover SSE idle-TTL'd out): capture the live
	// cookies — session cookies included — to the master sidecar FIRST, then GRACEFUL-close so
	// chromium FLUSHES on-disk storage (localStorage/IndexedDB/SW) before the disk write-back.
	// chromedp.Cancel does the CDP Browser.close that flushes; the RAW browserCxl does NOT
	// (verified on the box: an empty Cookies DB after a raw close even after a real login).
	// Bounded so a wedged chromium can't hang the close — fall back to allocCancel (kill).
	// loginSaved → the human already 保存'd: DON'T re-capture (the worker may have browsed on /
	// logged out since) — the explicit save wins. Still graceful-close + disk write-back.
	controlledCopy := s.ephemeralCopy && controlled && s.masterDir != ""
	if controlledCopy {
		if loginSaved {
			slog.Info("browser writeback: controlled-close keeping explicitly-saved login (no re-capture)", "session", sessionScope)
		} else {
			mu := p.masterLockFor(s.masterDir)
			if n, cerr := func() (int, error) { mu.Lock(); defer mu.Unlock(); return captureLoginCookies(s.rootCtx, s.masterDir) }(); cerr != nil {
				slog.Warn("browser writeback: controlled-close cookie capture failed", "session", sessionScope, "err", cerr)
			} else {
				slog.Info("browser writeback: controlled-close captured login cookies", "session", sessionScope, "cookies", n)
				p.recordWriteback(sessionScope, s.masterDir, int64(n))
			}
		}
		cctx, ccancel := context.WithTimeout(s.rootCtx, 15*time.Second)
		if err := chromedp.Cancel(cctx); err != nil {
			slog.Warn("browser writeback: controlled-close graceful close error/timeout (continuing)", "session", sessionScope, "err", err)
		}
		ccancel()
	} else {
		s.browserCxl()
	}
	s.allocCancel()
	// A worker COPY's dir is disposable — discard it now that chromium has exited. Its login
	// lives in the master (cookies in the sidecar, the rest written back just below); legacy /
	// in-place login dirs are PRESERVED (ephemeralCopy=false).
	if s.ephemeralCopy && s.dataDir != "" {
		// Disk channel of the controlled-close fallback: the cookies already went to the sidecar
		// above (incl session cookies); this carries the on-disk storage chromium just flushed
		// (localStorage/IndexedDB/Service Worker) back to the master.
		if controlledCopy {
			mu := p.masterLockFor(s.masterDir)
			mu.Lock()
			n, werr := writeProfileBack(s.dataDir, s.masterDir)
			mu.Unlock()
			if werr != nil {
				// KEEP the copy dir on a failed write-back (BP-2): removing it here would
				// destroy the flushed login storage the master never received — silent data
				// loss with no recovery. Kept, it survives until the next seed for this scope
				// RemoveAll's it (or a restart sweep), leaving a manual-recovery window for
				// the transient cause (disk full).
				slog.Error("browser writeback: controlled-close disk write-back FAILED — keeping copy dir for recovery", "session", sessionScope, "master", s.masterDir, "copy", s.dataDir, "err", werr)
				return
			}
			slog.Info("browser writeback: controlled-close saved disk storage to master", "session", sessionScope, "master", s.masterDir, "bytes", n)
		}
		if err := os.RemoveAll(s.dataDir); err != nil {
			slog.Warn("browser session: copy dir cleanup failed (may accumulate)", "session", sessionScope, "path", s.dataDir, "err", err)
		}
		slog.Info("browser session: copy instance closed (copy dir discarded)", "session", sessionScope)
		return
	}
	slog.Info("browser session: instance closed (data dir preserved)", "session", sessionScope)
}

// PurgeSession closes the chromium instance AND deletes the on-disk
// data dir + last_nav.txt. Implements core.SessionPurger — wired to
// /close-session so admin's "fresh start" intent fully wipes the LEGACY
// per-scope ephemeral browser (logs out, drops the page). Idempotent.
//
// Per-person browser PROFILES are different: this is also the release door for
// them (a /close-session OR an agora finalize routes through the purger chain
// when the scope goes dormant), so it releases any profile CHECKOUT this scope
// held — closing the instance but PRESERVING the profile's vault dir. Closing a
// conversation must not log the person out of their accounts; only an explicit
// admin credential-delete wipes a profile. The legacy dataDir wipe below is a
// no-op for a scope that only used profiles (their dirs live under the vault,
// not dataDirFor(scope)).
//
// Note: must NOT be called from idle sweep / eviction / shutdown —
// those should use CloseInstance to preserve cookies for resume.
func (p *Pool) PurgeSession(sessionScope string) {
	if sessionScope == "" {
		return
	}
	// Release (close + free) any per-person profile checkouts this scope owned.
	// Keeps the profile vault dirs (a release is not a logout).
	if p.custodian != nil {
		p.custodian.ReleaseScope(sessionScope)
	}
	// Resolve dataDir even if no live instance — the disk state may
	// still exist from a previous boot.
	dataDir := p.dataDirFor(sessionScope)
	p.CloseInstance(sessionScope)
	if err := os.RemoveAll(dataDir); err != nil {
		slog.Warn("browser session: data dir purge failed", "session", sessionScope, "data_dir", dataDir, "err", err)
	} else {
		slog.Info("browser session: purged (instance closed + data dir deleted)", "session", sessionScope, "data_dir", dataDir)
	}
}

// SweepIdle closes the chromium instance for any session whose lastUsed
// is older than ttl. Cookies / data dir PRESERVED — re-spawn restores.
// Called from pipeline-startup/60-start-background.go.
//
// Keys mid-takeover are exempt: a human driving the browser through the
// takeover UI does NOT refresh lastUsed (nor does the model while a
// control-hold is up), so a long manual session would otherwise be reaped
// mid-takeover. Two sources, because the two instance keyings protect
// themselves differently: custodian-pinned PROFILE checkouts (Pin already
// means "exempt from idle reclaim" at the custodian — honour it here too)
// and the pool-side takeoverActive count, which is the only cover LEGACY
// scope-keyed instances have (they own no checkout to pin).
func (p *Pool) SweepIdle(ttl time.Duration) int {
	cutoff := time.Now().Add(-ttl)
	pinned := p.pinnedProfileKeys()

	p.mu.Lock()
	stale := make([]string, 0)
	for key, s := range p.sessions {
		if pinned[key] || p.takeoverActive[key] > 0 {
			continue
		}
		s.mu.Lock()
		used := s.lastUsed
		s.mu.Unlock()
		if used.Before(cutoff) {
			stale = append(stale, key)
		}
	}
	p.mu.Unlock()

	for _, key := range stale {
		p.CloseInstance(key)
	}
	return len(stale)
}

// CloseAll closes every chromium instance on bob shutdown. Cookies /
// data dirs PRESERVED so the next bob start can auto-resume sessions.
func (p *Pool) CloseAll() {
	p.mu.Lock()
	keys := make([]string, 0, len(p.sessions))
	for k := range p.sessions {
		keys = append(keys, k)
	}
	p.mu.Unlock()
	for _, k := range keys {
		p.CloseInstance(k)
	}
	// T2 belt-and-suspenders: cancel pool lifecycle context so any
	// chromium that survived per-key CloseInstance (e.g. stuck in
	// shutdown) gets SIGKILL'd via the inherited cancel chain.
	if p.lifeCancel != nil {
		p.lifeCancel()
	}
	// Stop the profile-checkout custodian goroutine (idempotent Close).
	if p.custodian != nil {
		p.custodian.Close()
	}
}

// ConsoleSnapshot returns a chronologically ordered snapshot of this
// session's console ring buffer, optionally filtered by level and capped at
// max entries. Lives on *Session (callers hold one from AcquireFor /
// AcquireRouted) rather than as a scope-keyed Pool lookup: the profile path
// keys the pool map by ProfileName, so a scope lookup would miss it.
func (s *Session) ConsoleSnapshot(level string, max int) []ConsoleEntry {
	if s == nil || s.console == nil {
		return []ConsoleEntry{}
	}
	return s.console.snapshot(level, max)
}

// SetDialogReply primes the per-session dialog handler: the next JS dialog
// (alert / confirm / prompt) resolves with the given action + text, then the
// override clears. On *Session for the same reason as ConsoleSnapshot.
func (s *Session) SetDialogReply(accept bool, promptText string) {
	s.mu.Lock()
	s.dialogReply = &dialogReply{accept: accept, promptText: promptText}
	s.mu.Unlock()
}

// --- console event capture ----------------------------------------------

// ConsoleEntry is one captured console message.
type ConsoleEntry struct {
	When  time.Time `json:"when"`
	Level string    `json:"level"` // "log" / "warning" / "error" / "info"
	Text  string    `json:"text"`
	URL   string    `json:"url,omitempty"`
}

// consoleRing is a fixed-size circular buffer of ConsoleEntry.
//
// T6 fix: writes route through a buffered channel + drainer
// goroutine rather than taking r.mu directly on the chromedp dispatcher.
// A page that spams console.log (WebSocket noise, third-party SDK) used
// to back-pressure the dispatcher goroutine, blocking *all* chromedp
// events including the click / type / nav we actually care about. Now
// pushPush() does a non-blocking send: if the drain queue is full
// (1024 entries), the event is dropped — fine, the ring is best-effort.
type consoleRing struct {
	mu    sync.Mutex
	buf   []ConsoleEntry
	next  int
	count int

	// in is the dispatcher-side intake. drainer reads from in and
	// writes into buf under mu. cap chosen so a normal-noise burst
	// fits without drop; pathological pages drop silently.
	in chan ConsoleEntry

	// done signals the drainer to exit. Threaded from the session's
	// browserCtx so the drainer dies with the chromium instance
	// (CloseInstance / idle sweep / eviction / shutdown). Without it
	// the `for range in` drainer leaked one goroutine per session
	// close→respawn and pinned the ring from GC. Mirrors the dialog
	// worker's `<-browserCtx.Done()` exit (see attachDialogHandler).
	//
	// We keep push()'s non-blocking send on `in` (never closed) rather
	// than close(in): a close would race the dispatcher's send and
	// panic. Selecting on done is the panic-free stop path.
	done <-chan struct{}
}

func newConsoleRing(capacity int, done <-chan struct{}) *consoleRing {
	r := &consoleRing{
		buf:  make([]ConsoleEntry, capacity),
		in:   make(chan ConsoleEntry, 1024),
		done: done,
	}
	go r.drain()
	return r
}

// drain reads `in` and writes into buf under mu. The dispatcher's push()
// never takes mu. Exits when `done` fires (browserCtx cancelled on
// CloseInstance) — after which the ring is eligible for GC and no
// goroutine leaks across session close→respawn cycles. A nil done
// (test rings with no lifecycle) keeps the drainer alive for the
// process lifetime, the pre-fix behaviour.
func (r *consoleRing) drain() {
	for {
		select {
		case e := <-r.in:
			r.mu.Lock()
			r.buf[r.next] = e
			r.next = (r.next + 1) % len(r.buf)
			if r.count < len(r.buf) {
				r.count++
			}
			r.mu.Unlock()
		case <-r.done:
			return
		}
	}
}

// push is called from the chromedp dispatcher goroutine. Non-blocking;
// drops the entry if the drain queue is full so the dispatcher never
// stalls. Returns silently — caller is fire-and-forget telemetry.
func (r *consoleRing) push(e ConsoleEntry) {
	select {
	case r.in <- e:
	default:
		// Queue full — drop. This is rare (drainer is fast); a
		// runaway page spammer can pin the queue, but the bug we're
		// guarding against (dispatcher block) was worse.
	}
}

func (r *consoleRing) snapshot(filterLevel string, max int) []ConsoleEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ConsoleEntry, 0, r.count)
	start := (r.next - r.count + len(r.buf)) % len(r.buf)
	for i := 0; i < r.count; i++ {
		e := r.buf[(start+i)%len(r.buf)]
		if filterLevel != "all" && filterLevel != "" && e.Level != filterLevel {
			continue
		}
		out = append(out, e)
	}
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// attachConsoleListener wires CDP runtime events into the per-session
// ring buffer. Called once per session in Pool.Acquire.
func attachConsoleListener(browserCtx context.Context, ring *consoleRing) {
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		switch e := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			ring.push(ConsoleEntry{
				When:  time.Now(),
				Level: normaliseLevel(string(e.Type)),
				Text:  joinArgs(e.Args),
			})
		case *runtime.EventExceptionThrown:
			det := e.ExceptionDetails
			if det == nil {
				return
			}
			text := det.Text
			if det.Exception != nil && det.Exception.Description != "" {
				text = det.Exception.Description
			}
			ring.push(ConsoleEntry{
				When:  time.Now(),
				Level: "error",
				Text:  text,
				URL:   det.URL,
			})
		}
	})
}

func joinArgs(args []*runtime.RemoteObject) string {
	if len(args) == 0 {
		return ""
	}
	var out string
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		switch {
		case a.Description != "":
			out += a.Description
		case a.Value != nil:
			out += string(a.Value)
		default:
			out += a.Type.String()
		}
	}
	return out
}

func normaliseLevel(t string) string {
	switch t {
	case "warning", "warn":
		return "warning"
	case "error", "assert":
		return "error"
	case "info", "log", "debug", "trace":
		return "log"
	default:
		return "log"
	}
}

// dialogJob carries one dialog event from the chromedp dispatcher to
// the per-session worker. The struct is tiny (just the reply intent +
// debug info for logging); we transfer ownership through a buffered
// channel so the dispatcher goroutine never blocks even on a malicious
// page spamming alert().
type dialogJob struct {
	accept     bool
	promptText string
	kind       string // dialog type for log
	msg        string // dialog text for log
}

// attachDialogHandler subscribes to page.EventJavascriptDialogOpening
// and routes each dialog through a per-session buffered channel +
// single worker (TD5 fix). Why this shape:
//
//   - chromium pauses the page until something calls
//     HandleJavaScriptDialog, so dialogs are inherently serial per
//     page. One worker per session handles them in order.
//   - The chromedp dispatcher goroutine MUST NOT block on dialog
//     handling — if it did, every other chromedp event (clicks /
//     navigations / console) would stall. Non-blocking send to a
//     buffered channel keeps the dispatcher free.
//   - If the channel fills (rare; cap 8 covers every benign page),
//     we drop the dialog event. The page hangs until session restart,
//     which is the correct outcome for a hostile page in tight-loop
//     alert(). The prior fire-and-forget `go func()` per event was
//     a goroutine leak vector with no upper bound.
func attachDialogHandler(browserCtx context.Context, s *Session) {
	jobs := make(chan dialogJob, 8)

	// Single worker. Runs for the lifetime of the session — terminates
	// when browserCtx is cancelled (chromedp.Run returns immediately
	// with the cancel error, worker exits cleanly).
	go func() {
		for {
			select {
			case <-browserCtx.Done():
				return
			case job, ok := <-jobs:
				if !ok {
					return
				}
				act := page.HandleJavaScriptDialog(job.accept).WithPromptText(job.promptText)
				if err := chromedp.Run(browserCtx, act); err != nil && s.console != nil {
					s.console.push(ConsoleEntry{
						When:  time.Now(),
						Level: "error",
						Text:  fmt.Sprintf("dialog handler failed: %v (dialog type=%s msg=%q)", err, job.kind, job.msg),
					})
				}
			}
		}
	}()

	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		dialog, ok := ev.(*page.EventJavascriptDialogOpening)
		if !ok {
			return
		}
		s.mu.Lock()
		reply := s.dialogReply
		s.dialogReply = nil
		s.mu.Unlock()

		job := dialogJob{accept: true, kind: string(dialog.Type), msg: dialog.Message}
		if reply != nil {
			job.accept = reply.accept
			job.promptText = reply.promptText
		}
		// Non-blocking send. If full, the page is in an alert-spam loop
		// and we drop the event — the in-flight worker is already
		// handling one, the next will pause the page. WARN-log so admin
		// sees the smoke signal.
		select {
		case jobs <- job:
		default:
			if s.console != nil {
				s.console.push(ConsoleEntry{
					When:  time.Now(),
					Level: "warning",
					Text:  fmt.Sprintf("dialog queue full; dropping (type=%s msg=%q) — page may be alert-spamming", job.kind, job.msg),
				})
			}
		}
	})
}

// dataDirFor returns the deterministic on-disk path for a sessionScope's
// chromedp data dir. Mirrors the computation in Acquire so PurgeSession
// can locate the dir even when no live instance exists (disk state may
// have outlived the previous bob process).
func (p *Pool) dataDirFor(sessionScope string) string {
	if sessionScope == "" {
		sessionScope = "default"
	}
	root := p.cfg.UserDataDirRoot
	if root == "" {
		return filepath.Join(bobhome.Path("sandbox"), file.ScopeRelDir(sessionScope), "chromedp")
	}
	return filepath.Join(root, file.ScopeRelDir(sessionScope))
}

// byScopeRoot is the parent dir under which legacy per-scope chromedp data
// dirs live (<root>/by_sessions_scope/<sanitized-scope>[/chromedp]). Mirrors
// dataDirFor's root choice. Profile vault dirs live under a DIFFERENT root
// (ProfileVaultRoot, in credentials/) and are intentionally NOT swept here.
func (p *Pool) byScopeRoot() string {
	root := p.cfg.UserDataDirRoot
	if root == "" {
		root = bobhome.Path("sandbox")
	}
	return filepath.Join(root, "by_sessions_scope")
}

// SweepDataDirs removes legacy per-scope chromedp data dirs whose mtime is
// older than ttl AND which have no live session. It is the directory-level
// reclaimer the in-process mode gets for free from bob's sandbox sweep
// (pipeline-startup sweepSandbox) but which the browserd process must run
// itself — CloseInstance/SweepIdle preserve data dirs by contract, so without
// this they grow without bound on the browserd volume. Returns the count
// removed. Profile vault dirs are untouched (different root). Best-effort:
// per-dir failures log and continue.
func (p *Pool) SweepDataDirs(ttl time.Duration) int {
	root := p.byScopeRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("browser data-dir sweep: read failed", "root", root, "err", err)
		}
		return 0
	}
	// Live scope dirs to spare: a session keyed by scope maps onto this root;
	// profile sessions map onto the vault root and won't appear here.
	live := map[string]bool{}
	p.mu.Lock()
	for key := range p.sessions {
		live[file.SanitizeKey(key)] = true
	}
	p.mu.Unlock()

	cutoff := time.Now().Add(-ttl)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || live[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		full := filepath.Join(root, e.Name())
		if err := os.RemoveAll(full); err != nil {
			slog.Warn("browser data-dir sweep: remove failed", "path", full, "err", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		slog.Info("browser data-dir sweep", "root", root, "removed", removed)
	}
	return removed
}

// sessionSandbox returns the per-session sandbox dir exactly as the
// filesystem tools (read_file / search_files) resolve it — via
// file.SessionSandbox, which honours cfg.SandboxRoot and inserts the
// bots/<botID>/ segment when namespaceByBot is on. browser_snapshot
// (format=image) writes screenshots under this dir so the relative path
// it returns to the model ("screenshots/snap-*.png") resolves back to
// the same file when read_file opens it. This is the read-side mirror of
// dataDirFor's write-side layout (dataDir intentionally stays under
// UserDataDirRoot since chromium's profile is never handed to the model).
func (p *Pool) sessionSandbox(actx core.AgentCtx) string {
	return file.SessionSandbox(p.fsCfg, p.namespaceByBot, actx)
}

// HasSession reports whether browser state exists on disk for sessionScope.
// Disk-based so it survives idle close + bob restart — the tool-list
// filter (SpecForSession) uses this to decide whether to advertise the
// interactive browser_* tools (snapshot/click/type/etc), which are only
// useful after a navigate has happened.
//
// browser_navigate itself is always advertised regardless — it's the
// entry point that produces the disk state in the first place.
func (p *Pool) HasSession(sessionScope string) bool {
	if sessionScope == "" {
		return false
	}
	info, err := os.Stat(p.dataDirFor(sessionScope))
	return err == nil && info.IsDir()
}

// lastNavMarker is the filename of the auto-resume URL marker. Lives
// INSIDE the dataDir so:
//   - PurgeSession's RemoveAll(dataDir) wipes it for free
//   - one marker per session (collision-proof when UserDataDirRoot is
//     configured to a custom path; the dataDir parent would otherwise
//     be shared across sessions)
//
// The dot prefix keeps it sorted out of the way; chromium doesn't
// manage random dotfiles in its UserDataDir.
const lastNavMarker = ".bob_last_nav.txt"

// scopeUsedMarker is a zero-content file under dataDirFor(scope) that records
// "this scope has used the browser". It exists purely so HasSession (the
// tool-list gate) can answer from disk after idle close / restart, decoupled
// from where the auto-resume last-nav marker lives.
const scopeUsedMarker = ".bob_browser_used"

// RecordNav persists the just-navigated URL for auto-resume AND records the
// scope-level "used" signal for the tool-list gate. Two destinations because
// the two consumers diverge under the profile path:
//
//   - auto-resume (acquireKeyed → readLastNavURL) reads from the SESSION's
//     real dataDir, which is the vault dir for a profile session. So the
//     last-nav marker MUST live in sess.dataDir, not dataDirFor(scope).
//   - HasSession gates on dataDirFor(scope) (the scope is the stable axis the
//     tool list keys on, independent of which profile a turn acts as). So the
//     used-marker lives under dataDirFor(scope).
//
// For the legacy path sess.dataDir == dataDirFor(scope) and both land in the
// same directory, exactly as before. Best-effort: write failures log and
// degrade gracefully (auto-resume / gate just don't fire).
func (p *Pool) RecordNav(sess *Session, scope, url string) {
	if sess != nil {
		sess.setLastNavURL(url)
	}
	p.markScopeUsed(scope)
}

// setLastNavURL writes the auto-resume marker into this session's own
// dataDir, so acquireKeyed's readLastNavURL(dataDir) finds it on re-spawn
// regardless of whether dataDir is the legacy scope dir or the profile vault.
func (s *Session) setLastNavURL(url string) {
	if s == nil || s.dataDir == "" || url == "" {
		return
	}
	path := filepath.Join(s.dataDir, lastNavMarker)
	if err := os.WriteFile(path, []byte(url), 0o644); err != nil {
		slog.Warn("browser session: last_nav.txt write failed", "data_dir", s.dataDir, "err", err)
	}
}

// markScopeUsed records the scope-level "browser used" signal for HasSession.
func (p *Pool) markScopeUsed(scope string) {
	if scope == "" {
		return
	}
	dir := p.dataDirFor(scope)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("browser session: scope used-marker mkdir failed", "scope", scope, "dir", dir, "err", err)
		return
	}
	path := filepath.Join(dir, scopeUsedMarker)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		slog.Warn("browser session: scope used-marker write failed", "scope", scope, "path", path, "err", err)
	}
}

// readLastNavURL is the symmetric reader for Session.setLastNavURL — used by
// acquireKeyed to drive auto-resume on a re-spawned chromedp instance.
// Returns "" when the marker is missing or unreadable; callers treat
// that as "no resume needed".
//
// Takes dataDir directly (rather than sessionScope) because it's called
// from Acquire after the dataDir is already resolved.
func readLastNavURL(dataDir string) string {
	body, err := os.ReadFile(filepath.Join(dataDir, lastNavMarker))
	if err != nil {
		return ""
	}
	url := strings.TrimSpace(string(body))
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return ""
	}
	return url
}

// Compile-time check: *Pool satisfies core.SessionPurger so Hub can
// hold it as that interface (decouples /new handler from this package).
var _ core.SessionPurger = (*Pool)(nil)
