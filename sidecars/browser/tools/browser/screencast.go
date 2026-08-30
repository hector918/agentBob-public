package browser

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// inputDispatchTimeout bounds one takeover input event. A single CDP input
// dispatch is near-instant; this deadline only fires when chromium is wedged,
// freeing the webui POST handler goroutine instead of letting it block forever.
const inputDispatchTimeout = 5 * time.Second

// Screencast + input: the CDP layer behind the webui human-takeover surface
// (docs/remote-browser-handoff.md P1). These drive a profile's LIVE chromium
// directly under webui admin auth — a DIFFERENT trust path from the model-facing
// browser_cdp tool, so they intentionally do NOT go through cdpAllowMethods
// (that allowlist stays tight for the model; Page.startScreencast /
// Input.dispatch* are never exposed to it). A takeover lets an admin act as the
// logged-in person on every site in the profile — gated by webui auth + the
// fact that a stream only opens on explicit takeover (security §5).

// ScreencastFrame is one frame from a live profile browser. DataB64 is a
// base64-encoded JPEG exactly as CDP delivers it (the webui forwards it as-is).
type ScreencastFrame struct {
	DataB64 string
}

// InputEvent is one mouse/key event from the webui takeover, decoded from the
// input POST. Kind selects the union ("mouse" / "key").
type InputEvent struct {
	Kind string `json:"kind"`

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
	Text    string `json:"text,omitempty"`    // char: the produced text
	Key     string `json:"key,omitempty"`     // down/up: DOM key name ("Enter", "a", …)
}

// StartScreencast streams the profile's live browser as JPEG frames to cb and
// returns an idempotent stop func plus an `ended` channel that closes when
// the stream is over for ANY reason — stop called, or the instance torn down
// under it (CloseInstance via /new purge, LRU eviction, custodian release,
// pool shutdown all cancel the session's root ctx; a watcher goroutine
// propagates that as a stop). Consumers (the webui / browserd SSE handlers)
// select on `ended` to end the human's stream cleanly instead of heartbeating
// a frozen last frame forever. For the duration the instance is exempt
// from idle reclaim / LRU eviction (a human is driving it): the pool-level
// takeoverActive count covers BOTH keyings (it is the legacy scope-keyed
// instance's only protection — no custodian checkout exists to pin), and a
// profile checkout is additionally Pinned at the custodian (refcounted there,
// so concurrent streams don't strip each other). Errors if no live chromium
// instance exists for profileKey (the profile must have been navigated first).
//
// Frame delivery uses a dispatcher → channel → drainer hop: the chromedp event
// dispatcher must NOT call chromedp.Run (it would deadlock waiting on itself),
// so the listener only non-blocking-sends the frame onto a channel; a separate
// drainer goroutine invokes cb AND acks the frame (the ack is required — without
// it chromium stops sending). Frames are dropped when the drainer lags (the
// stream is best-effort, like the console ring).
func (p *Pool) StartScreencast(profileKey string, quality string, cb func(ScreencastFrame)) (stop func(), ended <-chan struct{}, effTier string, err error) {
	p.mu.Lock()
	sess, ok := p.sessions[profileKey]
	if !ok {
		p.mu.Unlock()
		return nil, nil, "", fmt.Errorf("browser screencast: no live session for %q (navigate first)", profileKey)
	}
	if p.takeoverActive == nil {
		p.takeoverActive = map[string]int{}
	}
	// Admission cap (保大盘): bound concurrent takeover streams so a burst of
	// handoffs can't melt the box. In-flight streams are NEVER degraded to make
	// room — a clean refusal is the graceful failure, surfaced upstream as a 503.
	// Counted BEFORE the increment, so the (cap+1)th connect is the one refused.
	total := 0
	for _, n := range p.takeoverActive {
		total += n
	}
	if max := p.cfg.MaxTakeoverStreamsEff(); total >= max {
		p.mu.Unlock()
		return nil, nil, "", fmt.Errorf("browser screencast: takeover capacity reached (%d/%d) — try again shortly", total, max)
	}
	p.takeoverActive[profileKey]++
	// A copy that gets taken over has been (or is being) human-driven → on close, write
	// its login back to the master even without an explicit 保存登陆 (fallback). Monotonic.
	sess.controlled = true
	p.mu.Unlock()
	if p.custodian != nil {
		p.custodian.Pin(profileKey, true)
	}
	// release undoes the liveness bookkeeping above. Called exactly once, by
	// stop (which also serves the start-failure path).
	release := func() {
		p.mu.Lock()
		if n := p.takeoverActive[profileKey]; n > 1 {
			p.takeoverActive[profileKey] = n - 1
		} else {
			delete(p.takeoverActive, profileKey)
		}
		p.mu.Unlock()
		if p.custodian != nil {
			p.custodian.Pin(profileKey, false)
		}
	}

	done := make(chan struct{})
	frames := make(chan *page.EventScreencastFrame, 8)

	// curCtx is the tab ctx the stream is currently bound to; it MOVES as the
	// supervisor below follows active-tab switches. Guarded by smu.
	var smu sync.Mutex
	var curCtx context.Context
	// effTier (named return) is the tier actually applied at the latest bind, guarded
	// by smu (startStream writes it). We snapshot it once after the initial bind below.
	currentCtx := func() context.Context { smu.Lock(); defer smu.Unlock(); return curCtx }

	// Frame intake. Registered on the SESSION (addCastSink / the per-ctx
	// listener ensureCastListener attaches) rather than as a per-takeover
	// ListenTarget: chromedp listeners cannot be removed, so per-takeover
	// listeners on the long-lived root ctx would accumulate one closure plus
	// its captured frames buffer per takeover (EventSource auto-reconnects
	// included) until the instance closes. The sink forwards only frames for
	// the ctx the stream is currently on, so frames from a tab we've since
	// left (or from another concurrent stream's tab) are dropped.
	removeSink := sess.addCastSink(func(bctx context.Context, f *page.EventScreencastFrame) {
		if currentCtx() != bctx { // we've moved to another tab
			return
		}
		select {
		case frames <- f:
		default: // drainer lagging — drop (best-effort)
		}
	})

	// startStream foregrounds bctx, ensures the session's frame listener is on
	// it, marks it current, and acquires the screencast there (refcounted —
	// see castAcquire). headless Page.startScreencast only emits frames for
	// the FOREGROUND target, so a multi-tab session (auto-followed Taobao
	// login popup / OAuth window, see tabs.go) needs the active tab activated
	// or it black-screens. curCtx is set BEFORE the acquire (the first frames
	// may arrive the instant the command is acked, and an unforwarded first
	// frame is never acked → stall) but ROLLED BACK on failure: committing a
	// dead binding would make the supervisor's nb == currentCtx() check true
	// forever, silently freezing the stream with no retry. The tab being left
	// is released only AFTER the new one is live, so a failed move keeps the
	// old stream running.
	startStream := func(bctx context.Context, id target.ID) error {
		sess.ensureCastListener(bctx)
		smu.Lock()
		prev := curCtx
		curCtx = bctx
		smu.Unlock()
		activateTarget(bctx, id)
		// Resolve the screencast params from the requested tier AND the live load:
		// effective tier = min(requested, backend ceiling), so a busy box auto-degrades
		// new/re-bound streams (read fresh each bind so a tab-follow uses current load).
		p.mu.Lock()
		active := 0
		for _, n := range p.takeoverActive {
			active += n
		}
		p.mu.Unlock()
		effName, q, nth, maxW, maxH := p.cfg.ResolveCastTier(quality, active)
		if err := sess.castAcquire(bctx,
			int64(q), int64(nth), int64(maxW), int64(maxH),
		); err != nil {
			smu.Lock()
			if curCtx == bctx {
				curCtx = prev
			}
			smu.Unlock()
			return err
		}
		smu.Lock()
		effTier = effName
		smu.Unlock()
		if prev != nil && prev != bctx {
			sess.castRelease(prev)
		}
		return nil
	}

	go func() {
		for {
			select {
			case <-done:
				return
			case f := <-frames:
				cb(ScreencastFrame{DataB64: f.Data})
				// Ack on THIS goroutine (not the dispatcher) so chromium keeps
				// sending. Best-effort: a failed ack just slows the stream.
				if c := currentCtx(); c != nil {
					_ = chromedp.Run(c, page.ScreencastFrameAck(f.SessionID))
				}
			}
		}
	}()

	var once sync.Once
	stop = func() {
		once.Do(func() {
			removeSink()
			if c := currentCtx(); c != nil {
				sess.castRelease(c) // CDP stop only if no other stream is on this tab
			}
			close(done)
			release()
		})
	}

	// Instance-death watcher: every legitimate close path (CloseInstance —
	// /new purge, LRU eviction, custodian release, pool shutdown) cancels
	// the session's root ctx but knows nothing about open takeover streams.
	// Propagate it as a stop so `done` (returned as `ended`) closes and the
	// stream consumer ends the human's SSE instead of heartbeating over a
	// dead frame source. rootCtx is set once at session creation and never
	// swapped (tab switches move BrowserCtx only), so it IS the instance's
	// lifetime.
	go func() {
		select {
		case <-done:
		case <-sess.rootCtx.Done():
			stop()
		}
	}()

	// Initial bind to whatever tab is active now (ctx + id read atomically so the
	// activated tab IS the streamed tab — see==control).
	bctx, activeID := sess.activeCtxAndTargetID()
	if err := startStream(bctx, activeID); err != nil {
		stop() // curCtx rolled back to nil, so this only unwinds the bookkeeping
		return nil, nil, "", fmt.Errorf("browser screencast start: %w", err)
	}
	// Snapshot the effective tier from the initial bind (before the supervisor goroutine
	// can re-bind/overwrite it) — reported once to the webui so the 画质 button shows what
	// is actually running (which may be below the requested tier under load).
	smu.Lock()
	initialTier := effTier
	smu.Unlock()

	// Supervisor: FOLLOW the tab the human is on. Two reasons the active tab
	// moves under a takeover: (1) a click opens a popup/new tab. During takeover
	// the MODEL is idle, so the auto-follow hook actions.go normally calls
	// (reconcileAndMaybeFollow) never runs — the Session's active-tab tracking
	// stays frozen on the old tab even though chromium opened a new one. So the
	// supervisor drives that reconcile itself each tick (a CDP target enumeration;
	// it switchTo's the Session onto a single new popup). (2) the active tab ctx
	// then differs from the stream's — stop the old stream and re-bind on the new
	// tab, which also re-activates it so it doesn't black-screen. Without this the
	// human had to refresh the page to see the tab their click opened. ~400ms lag
	// is invisible; the enumeration is cheap and the takeover is bounded.
	go func() {
		t := time.NewTicker(400 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				sess.reconcileAndMaybeFollow() // pick up a human-opened popup (model is idle)
				nb, nid := sess.activeCtxAndTargetID()
				if nb == nil || nb == currentCtx() {
					continue
				}
				// The old tab is castRelease'd inside startStream once the new
				// one is live; on failure the old stream keeps running and the
				// next tick retries.
				if e := startStream(nb, nid); e != nil {
					slog.Warn("browser screencast: follow tab switch failed", "err", e)
				}
			}
		}
	}()

	return stop, done, initialTier, nil
}

// castSink is one takeover stream's frame intake: it receives every screencast
// frame the session's per-ctx listeners observe, tagged with the ctx it arrived
// on so the stream can drop frames from tabs it isn't currently bound to.
type castSink func(bctx context.Context, f *page.EventScreencastFrame)

// addCastSink registers a takeover stream's frame intake on the session and
// returns its remove func (idempotence is the caller's once.Do). Concurrent
// takeovers on one session each hold their own sink.
func (s *Session) addCastSink(fn castSink) (remove func()) {
	s.castMu.Lock()
	defer s.castMu.Unlock()
	if s.castSinks == nil {
		s.castSinks = map[int]castSink{}
	}
	s.castSeq++
	id := s.castSeq
	s.castSinks[id] = fn
	return func() {
		s.castMu.Lock()
		delete(s.castSinks, id)
		s.castMu.Unlock()
	}
}

// ensureCastListener attaches the screencast frame forwarder to bctx at most
// once for the Session's lifetime (castBound). chromedp listeners cannot be
// removed, so this Session-level dedup — not a per-takeover one — is what
// bounds the listener count on the long-lived root ctx (and on child-tab ctxs,
// which persist across switches via tabBindings) no matter how many takeovers
// open and close. The forwarder itself holds no per-takeover state: it fans
// each frame out to the currently registered sinks.
func (s *Session) ensureCastListener(bctx context.Context) {
	s.castMu.Lock()
	if s.castBound == nil {
		s.castBound = map[context.Context]bool{}
	}
	if s.castBound[bctx] {
		s.castMu.Unlock()
		return
	}
	s.castBound[bctx] = true
	s.castMu.Unlock()
	chromedp.ListenTarget(bctx, func(ev interface{}) {
		f, isFrame := ev.(*page.EventScreencastFrame)
		if !isFrame {
			return
		}
		s.castMu.Lock()
		sinks := make([]castSink, 0, len(s.castSinks))
		for _, fn := range s.castSinks {
			sinks = append(sinks, fn)
		}
		s.castMu.Unlock()
		for _, fn := range sinks {
			fn(bctx, f)
		}
	})
}

// castAcquire refcounts the page-global screencast on bctx: only the FIRST
// stream on a tab issues Page.startScreencast, and only the LAST castRelease
// stops it. Page.{start,stop}Screencast are page-global, so an unconditional
// stop from one of two concurrent takeover streams on the same tab would
// freeze the survivor's frames — its supervisor only re-binds when the active
// ctx CHANGES, so it would never recover. browserd enforces ONE takeover stream
// per scope (it evicts a prior viewer and waits for its release before this
// acquire), so a 画质 switch / reconnect comes in as the FIRST stream and applies
// its tier cleanly — no in-place reconfigure of a shared cast is needed.
// maxW/maxH (px, 0 = no cap) downscale frames SERVER-SIDE before JPEG encoding
// (chromium preserves aspect within the box, never upscales) — the resolution lever
// that decouples the takeover stream from the model-facing window size.
func (s *Session) castAcquire(bctx context.Context, quality, everyNth, maxW, maxH int64) error {
	s.castMu.Lock()
	if s.castUsers == nil {
		s.castUsers = map[context.Context]int{}
	}
	s.castUsers[bctx]++
	first := s.castUsers[bctx] == 1
	s.castMu.Unlock()
	if !first {
		return nil // already casting on this tab
	}
	cast := page.StartScreencast().
		WithFormat(page.ScreencastFormatJpeg).
		WithQuality(quality).
		WithEveryNthFrame(everyNth)
	if maxW > 0 {
		cast = cast.WithMaxWidth(maxW)
	}
	if maxH > 0 {
		cast = cast.WithMaxHeight(maxH)
	}
	err := chromedp.Run(bctx, cast)
	if err != nil {
		s.castMu.Lock()
		if s.castUsers[bctx]--; s.castUsers[bctx] <= 0 {
			delete(s.castUsers, bctx)
		}
		s.castMu.Unlock()
	}
	return err
}

// castRelease undoes one castAcquire; the CDP stop fires only when the last
// user leaves the tab. Best-effort: stopping on a dead ctx just errors.
func (s *Session) castRelease(bctx context.Context) {
	s.castMu.Lock()
	last := false
	if n, ok := s.castUsers[bctx]; ok {
		if n--; n <= 0 {
			delete(s.castUsers, bctx)
			last = true
		} else {
			s.castUsers[bctx] = n
		}
	}
	s.castMu.Unlock()
	if last {
		_ = chromedp.Run(bctx, page.StopScreencast())
	}
}

// dropCastState forgets a closed tab's ctx in the screencast maps. The
// chromedp listener itself dies with the cancelled ctx; without this,
// castBound/castUsers would keep one entry (pinning the dead ctx chain) per
// tab ever screencast for the life of the instance.
func (s *Session) dropCastState(bctx context.Context) {
	s.castMu.Lock()
	delete(s.castBound, bctx)
	delete(s.castUsers, bctx)
	s.castMu.Unlock()
}

// activateTarget brings id to chromium's foreground via Target.activateTarget
// so headless Page.startScreencast emits frames for it (background targets are
// silent). bctx must be a chromedp context carrying the Browser handle (the
// active-tab ctx is one). Best-effort: a failed activate is warned and ignored
// — the worst case is a black screencast frame, never a crash. A blank id (no
// active target captured) is skipped silently.
func activateTarget(bctx context.Context, id target.ID) {
	if id == "" {
		return
	}
	if err := chromedp.Run(bctx, chromedp.ActionFunc(func(c context.Context) error {
		return target.ActivateTarget(id).Do(c)
	})); err != nil {
		slog.Warn("browser screencast: activate target failed (may black-screen)", "target", id, "err", err)
	}
}

// liveKeyForScope maps a session_scope to the pool key of the live browser
// instance that scope is currently driving (docs/remote-browser-handoff.md
// P1b). The takeover handle is the scope; the instance key is internal:
//
//   - profile path: the scope OWNS a custodian checkout whose Profile is the
//     pool key (instances are keyed by ProfileName, not by scope).
//   - legacy path: the instance is keyed by the scope directly.
//
// Falls back to the scope itself (legacy key) when no checkout is owned — if no
// instance exists under either key, the StartScreencast / DispatchInput call
// reports "no live session", which is the correct "nothing to take over" answer.
//
// A single scope CAN own several profiles at once (custodian.go: different
// people driving browser work in one group chat). The takeover handle is only
// the scope, so we must pick deterministically: the MOST-RECENTLY-TOUCHED
// checkout — the browser the scope last drove, i.e. the one most likely stuck
// at the wall the human is here to clear. (DM scopes — the /takeover path — own
// a single profile, so this only matters for the admin picker over group scopes.)
func (p *Pool) liveKeyForScope(scope string) string {
	// A worker COPY is keyed browsercopy_<scope> with NO custodian checkout — check it
	// first so the takeover targets the worker's own copy (the human logs in on it, then
	// 保存登陆 writes it back to the master).
	copyKey := profileCopyKey(scope)
	p.mu.Lock()
	_, hasCopy := p.sessions[copyKey]
	p.mu.Unlock()
	if hasCopy {
		return copyKey
	}
	if p.custodian != nil {
		best, found := "", false
		var bestTouched time.Time
		for _, cv := range p.custodian.Snapshot() {
			if cv.Owner == scope && (!found || cv.LastTouched.After(bestTouched)) {
				best, bestTouched, found = cv.Profile, cv.LastTouched, true
			}
		}
		if found {
			return best
		}
	}
	return scope
}

// StartScreencastForScope is the scope-keyed entry the webui takeover uses: it
// resolves scope→live instance and screencasts it. See liveKeyForScope.
func (p *Pool) StartScreencastForScope(scope string, quality string, cb func(ScreencastFrame)) (stop func(), ended <-chan struct{}, effTier string, err error) {
	return p.StartScreencast(p.liveKeyForScope(scope), quality, cb)
}

// LiveForScope reports whether scope currently has a LIVE chromium instance a
// human could take over — the /takeover "is there a browser to take over?"
// guard (docs/remote-browser-handoff.md P1b). It must agree EXACTLY with what
// StartScreencastForScope would do: resolve scope→pool key via liveKeyForScope,
// then check the in-memory sessions map (the same lookup StartScreencast makes
// before opening a stream). On-disk HasSession is deliberately NOT used here —
// a scope that navigated then idle-closed leaves disk state but no live
// chromium, and a takeover token minted against it would land on a black
// screen. Only an instance that can actually screencast counts as live.
func (p *Pool) LiveForScope(scope string) bool {
	key := p.liveKeyForScope(scope)
	p.mu.Lock()
	_, ok := p.sessions[key]
	p.mu.Unlock()
	return ok
}

// DispatchInputForScope is the scope-keyed input entry the webui takeover uses.
func (p *Pool) DispatchInputForScope(scope string, ev InputEvent) error {
	return p.DispatchInput(p.liveKeyForScope(scope), ev)
}

// RunTabForScope is the scope-keyed tab entry the webui takeover uses so a human
// holding control can list / switch / close tabs in the browser they're driving
// (the overlay tab switcher). Resolves scope→live instance the same way the
// screencast/input paths do, then runs the shared browser_tab logic. A switch
// updates Session.activeTarget, which the screencast follow-tab supervisor picks
// up on its next tick and re-binds the stream to.
func (p *Pool) RunTabForScope(scope, action string, index int) (map[string]any, error) {
	key := p.liveKeyForScope(scope)
	p.mu.Lock()
	sess, ok := p.sessions[key]
	p.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("browser tab: no live session for %q", scope)
	}
	return RunTab(sess, action, index)
}

// DispatchInput injects one mouse/key event into the profile's live browser.
// Errors if no live session or the event is malformed.
func (p *Pool) DispatchInput(profileKey string, ev InputEvent) error {
	p.mu.Lock()
	sess, ok := p.sessions[profileKey]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("browser input: no live session for %q", profileKey)
	}
	cmd, err := inputAction(ev)
	if err != nil {
		return err
	}
	// activeCtx() (not raw BrowserCtx): a model-side tab switch swaps
	// BrowserCtx concurrently with this takeover read; the Session doc
	// requires every tab-field access under mu.
	//
	// Bound the call: a single input event is near-instant, but takeover
	// often happens while a page is misbehaving. Without a deadline a wedged
	// chromium would block this handler goroutine forever, and high-frequency
	// mousemove POSTs would pile up unbounded until the instance is reclaimed.
	tctx, cancel := context.WithTimeout(sess.activeCtx(), inputDispatchTimeout)
	defer cancel()
	return chromedp.Run(tctx, cmd)
}

// inputAction maps an InputEvent to its CDP command.
func inputAction(ev InputEvent) (chromedp.Action, error) {
	switch ev.Kind {
	case "mouse":
		mt, err := mouseType(ev.MouseType)
		if err != nil {
			return nil, err
		}
		cmd := input.DispatchMouseEvent(mt, ev.X, ev.Y).
			WithButton(mouseButton(ev.Button)).
			// WithButtons is the bitmask of buttons HELD during the event
			// (Left=1/Right=2/Middle=4/None=0) — distinct from WithButton (which
			// button changed). Newer headless chromium IGNORES a mousePressed
			// whose buttons mask is 0: the synthesized click never fires, so the
			// event dispatches cleanly (no CDP error) yet has no effect. Set the
			// pressed button's bit on press; 0 on release/move/wheel (the overlay
			// sends bare moves, so no drag-state to track).
			WithButtons(heldButtons(ev.Button, mt)).
			WithClickCount(ev.ClickCount)
		if mt == input.MouseWheel {
			cmd = cmd.WithDeltaX(ev.DeltaX).WithDeltaY(ev.DeltaY)
		}
		return cmd, nil
	case "key":
		kt, err := keyType(ev.KeyType)
		if err != nil {
			return nil, err
		}
		cmd := input.DispatchKeyEvent(kt)
		if ev.Text != "" {
			cmd = cmd.WithText(ev.Text)
		}
		if ev.Key != "" {
			cmd = cmd.WithKey(ev.Key)
		}
		return cmd, nil
	case "text":
		// Whole-string insertion into the focused element — the IME-capable path
		// the takeover input box uses. Unlike per-keystroke key events, this
		// carries composed CJK/unicode correctly (keydown never sees IME output).
		return input.InsertText(ev.Text), nil
	default:
		return nil, fmt.Errorf("browser input: unknown kind %q", ev.Kind)
	}
}

func mouseType(s string) (input.MouseType, error) {
	switch s {
	case "move":
		return input.MouseMoved, nil
	case "press":
		return input.MousePressed, nil
	case "release":
		return input.MouseReleased, nil
	case "wheel":
		return input.MouseWheel, nil
	default:
		return "", fmt.Errorf("browser input: unknown mouseType %q", s)
	}
}

func mouseButton(s string) input.MouseButton {
	switch s {
	case "left":
		return input.Left
	case "right":
		return input.Right
	case "middle":
		return input.Middle
	default:
		return input.None
	}
}

// heldButtons returns the CDP `buttons` bitmask (Left=1/Right=2/Middle=4) of
// buttons held DURING a mouse event. On press the changed button is now held;
// on release nothing remains held (single-button overlay, no chord), and a bare
// move/wheel carries no held button. Without this, a press dispatches with
// buttons=0 and newer chromium drops it — clicks silently do nothing.
func heldButtons(button string, mt input.MouseType) int64 {
	if mt != input.MousePressed {
		return 0
	}
	switch button {
	case "left":
		return 1
	case "right":
		return 2
	case "middle":
		return 4
	default:
		return 0
	}
}

func keyType(s string) (input.KeyType, error) {
	switch s {
	case "down":
		return input.KeyDown, nil
	case "up":
		return input.KeyUp, nil
	case "char":
		return input.KeyChar, nil
	default:
		return "", fmt.Errorf("browser input: unknown keyType %q", s)
	}
}
