package browser

// All browser operations as plain Go functions taking (ctx, *Session, params)
// and returning typed results. tools.go wraps each one in a ToolSpec; this
// file is the single place to look for what each tool actually does.
//
// Engine-agnostic intent: these functions only assume the Session exposes a
// chromedp browser context. To swap to playwright-go, replace pool.go +
// rewrite the bodies here against the new engine; tools.go and ref_map.go
// stay nearly intact.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"agentbob/sidecars/browser/tools/textcap"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// refPattern — R5A-11. interactiveTagJS only ever assigns
// refs of the form `@e<digits>`; refusing anything else at the Go layer
// stops a model-supplied attribute-selector injection (e.g. ref=`a"]
// [onclick]`) at parse time, before fmt.Sprintf %q escapes it into the
// CSS selector. Defense-in-depth — %q already quotes for CSS-attribute
// context, but parsing rules between CSS-attribute and HTML-attribute
// strings differ in fiddly ways across chromium versions.
var refPattern = regexp.MustCompile(`^@e\d+$`)

// Locator is "how to find an element". Hermes-style refs (e.g. "@e5") win
// when both are set. Either one alone works.
type Locator struct {
	Ref      string
	Selector string
}

// resolve produces a CSS selector chromedp can use. For a ref it's
// `[data-bob-ref="@e5"]` (the attribute set by interactiveTagJS). For a
// raw selector it's the selector unchanged. Errors when both are empty.
func (l Locator) resolve() (string, error) {
	if l.Ref != "" {
		ref := strings.TrimSpace(l.Ref)
		// R5A-11: validate ref shape before substitution. Bobs's JS
		// only ever emits `@e<digits>`; anything else is either an
		// LLM hallucination or a deliberate injection attempt.
		if !refPattern.MatchString(ref) {
			return "", fmt.Errorf("invalid ref %q: must match @e<digits> (refs are emitted by browser_snapshot)", ref)
		}
		// Quote-escape for the attribute selector.
		return fmt.Sprintf(`[data-bob-ref=%q]`, ref), nil
	}
	if l.Selector != "" {
		return l.Selector, nil
	}
	return "", fmt.Errorf("locator requires ref or selector")
}

// activeCtx returns the current active-tab browser context under mu. A tab
// switch (tabs.go switchTo) swaps Session.BrowserCtx between actions; reading
// it under mu means an action never observes a torn pointer even though the
// turn loop is sequential per Session. All chromedp work derives from this.
func (s *Session) activeCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.BrowserCtx
}

// activeTargetID returns the current active-tab target id under mu — the same
// tab activeCtx() points at. The takeover screencast uses it to ActivateTarget
// (bring the tab to chromium's foreground) so headless emits screencast frames
// for it; see screencast.go StartScreencast.
func (s *Session) activeTargetID() target.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeTarget
}

// activeCtxAndTargetID returns the active-tab ctx AND its target id read under a
// SINGLE mu hold, so the two can't straddle a concurrent switchTo: reading them
// separately (activeCtx() then activeTargetID()) lets a tab swap land between,
// and StartScreencast would then activate one tab while binding the stream to
// another (see==control breaks → black frame). One lock keeps the pair coherent.
func (s *Session) activeCtxAndTargetID() (context.Context, target.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.BrowserCtx, s.activeTarget
}

// withTimeout derives a per-action timeout context off the long-lived
// Session.BrowserCtx so chromedp's internal Browser/Target value (set
// via chromedp.NewContext) stays attached — derive from any other
// parent and chromedp errors with "invalid context" the moment a call
// hits the wire.
//
// T4 fix: the agent ctx is also wired in so /close-session
// (and other turn-level cancels) preempt an in-flight chromedp call
// instead of having to wait the full per-call timeout. We do this by
// spawning a tiny watcher goroutine that calls the BrowserCtx-derived
// cancel when the agent ctx fires. CancelFunc returned to the caller
// is composite — closes the doneCh first so the watcher exits, then
// runs the underlying cancel.
func (s *Session) withTimeout(agentCtx context.Context, secs int) (context.Context, context.CancelFunc) {
	if secs <= 0 {
		secs = 30
	}
	bctx, bcancel := context.WithTimeout(s.activeCtx(), time.Duration(secs)*time.Second)
	if agentCtx == nil {
		return bctx, bcancel
	}
	doneCh := make(chan struct{})
	go func() {
		select {
		case <-agentCtx.Done():
			bcancel()
		case <-doneCh:
		}
	}()
	// Idempotent: context.CancelFunc's contract allows being called more than
	// once. Navigate's auto-follow path cancels this explicitly AND the
	// function's deferred cancel runs again on return; without sync.Once the
	// second close(doneCh) panics ("close of closed channel").
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(doneCh)
			bcancel()
		})
	}
	return bctx, cancel
}

// --- Navigate ------------------------------------------------------------

// NavigateResult mirrors Hermes: the snapshot is returned alongside the
// page metadata so a follow-up Snapshot call isn't needed for the simple
// "navigate then click" case.
type NavigateResult struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Ready    bool   `json:"ready"`
	Snapshot string `json:"snapshot"` // compact accessibility-tree text with @e refs
}

func Navigate(ctx context.Context, sess *Session, url string, timeoutSecs int) (*NavigateResult, error) {
	target := strings.TrimSpace(url)
	if target == "" {
		return nil, fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return nil, fmt.Errorf("url must start with http:// or https://")
	}
	tctx, cancel := sess.withTimeout(ctx, timeoutSecs)
	// Closure (not `defer cancel()`): the auto-follow path below reassigns
	// `cancel`, and a bare `defer cancel()` would have captured the ORIGINAL
	// function value at defer time, leaking the followed-tab context until
	// return. Wrapping in a closure makes the deferred call observe the
	// current `cancel`.
	defer func() { cancel() }()

	var title string
	if err := chromedp.Run(tctx,
		chromedp.Navigate(target),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Title(&title),
	); err != nil {
		return nil, fmt.Errorf("navigate %s: %w", target, err)
	}

	// Auto-follow a lone popup the navigation opened (rare for navigate, but a
	// landing page can window.open). If it followed, re-derive the snapshot
	// context off the now-active tab so the snapshot reflects the followed tab.
	if sess.reconcileAndMaybeFollow() {
		// Cancel the old context now (idempotent — the deferred closure will
		// no-op on it via sync.Once) and re-derive off the followed tab. The
		// deferred closure observes this reassigned `cancel` and frees the new
		// context on return.
		cancel()
		tctx, cancel = sess.withTimeout(ctx, timeoutSecs)
	}

	// Compact snapshot built into the navigate response — Hermes does the
	// same. Saves the model one tool round when it just wants to read /
	// click on what it landed on.
	items, _ := captureRefList(tctx, "")
	snap := renderRefList(items)
	if len(snap) > browserTreeMaxChars {
		snap, _, _ = textcap.Apply(snap, browserTreeMaxChars)
	}
	// Prepend the multi-tab header when >1 tab is live (zero noise when 1).
	snap = sess.tabHeader() + snap
	return &NavigateResult{URL: target, Title: title, Ready: true, Snapshot: snap}, nil
}

// --- Snapshot ------------------------------------------------------------

// SnapshotOpts maps the Hermes browser_snapshot params:
//   - Format = "tree" (default, accessibility-tree text with @e refs),
//     "html" (full page HTML), "text" (innerText of selector or body),
//     "image" (PNG to sandbox).
//   - Selector scopes tree/html/text to a subtree.
//   - Full=true on tree format includes everything (not just interactive).
type SnapshotOpts struct {
	Format   string // tree | html | text | image
	Selector string
	Full     bool
}

type SnapshotResult struct {
	Format         string `json:"format"`
	Selector       string `json:"selector,omitempty"`
	Content        string `json:"content,omitempty"`
	ScreenshotPath string `json:"path,omitempty"`
	// Image carries the raw PNG when format=image runs in bytes mode
	// (screenshotDir == "" — the browserd split): the CALLER owns the
	// disk write so the file lands on the machine whose sandbox the
	// model's relative path resolves against. json []byte = base64 on
	// the wire. Never forwarded to the model.
	Image     []byte `json:"image_b64,omitempty"`
	ByteCount int    `json:"byte_count"`
	Truncated bool   `json:"truncated"`
}

// Screenshot captures the current page as PNG bytes (no disk side-effect),
// for tools that need to pipe the image into another component (e.g.
// browser_vision sending it to a vision-capable model). Mirrors the
// chromedp call used by Snapshot(format=image) but skips the file write.
func Screenshot(ctx context.Context, sess *Session, timeoutSecs int) ([]byte, error) {
	tctx, cancel := sess.withTimeout(ctx, timeoutSecs)
	defer cancel()
	var buf []byte
	if err := chromedp.Run(tctx, chromedp.CaptureScreenshot(&buf)); err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	return buf, nil
}

func Snapshot(ctx context.Context, sess *Session, screenshotDir string, opts SnapshotOpts, timeoutSecs int) (*SnapshotResult, error) {
	if opts.Format == "" {
		opts.Format = "tree"
	}

	tctx, cancel := sess.withTimeout(ctx, timeoutSecs)
	defer cancel()

	switch opts.Format {
	case "tree":
		items, err := captureRefList(tctx, opts.Selector)
		if err != nil {
			return nil, fmt.Errorf("snapshot tree: %w", err)
		}
		// TD6/L1 reset: a SUCCESSFUL tree snapshot installs fresh @e refs, so
		// the previous click-fail counter is no longer meaningful. A failed
		// (timed-out/errored) snapshot installs nothing — leave the counter so
		// a model can't reset the SPA-loop guard by spamming broken snapshots.
		// The non-tree formats don't emit refs at all, so they never reset it.
		sess.mu.Lock()
		sess.clickFailsSinceSnapshot = 0
		sess.mu.Unlock()
		text := renderRefList(items)
		capped, truncated, total := textcap.Apply(text, browserTreeMaxChars)
		// Pick up tabs/popups that appeared since the last action — a popup can
		// open a beat after the click that triggered it (so the click's own
		// reconcile missed it) or via a JS timer with no click at all. Reconcile
		// here (no auto-switch — a read shouldn't change tabs) so every page read
		// surfaces them. Then prepend the header when >1 tab is live; zero noise
		// in the single-tab common case (tabHeader returns "").
		sess.reconcileTabs()
		capped = sess.tabHeader() + capped
		return &SnapshotResult{
			Format:    "tree",
			Selector:  opts.Selector,
			Content:   capped,
			ByteCount: total,
			Truncated: truncated,
		}, nil
	case "text":
		target := opts.Selector
		if target == "" {
			target = "body"
		}
		var text string
		if err := chromedp.Run(tctx, chromedp.Text(target, &text, chromedp.ByQuery, chromedp.NodeVisible)); err != nil {
			return nil, fmt.Errorf("snapshot text (%s): %w", target, err)
		}
		capped, truncated, total := textcap.Apply(strings.TrimSpace(text), textcap.DefaultMaxChars)
		return &SnapshotResult{Format: "text", Selector: opts.Selector, Content: capped, ByteCount: total, Truncated: truncated}, nil
	case "html":
		var html string
		sel := opts.Selector
		if sel == "" {
			sel = "html"
		}
		if err := chromedp.Run(tctx, chromedp.OuterHTML(sel, &html, chromedp.ByQuery)); err != nil {
			return nil, fmt.Errorf("snapshot html (%s): %w", sel, err)
		}
		capped, truncated, total := textcap.Apply(html, textcap.DefaultMaxChars)
		return &SnapshotResult{Format: "html", Selector: opts.Selector, Content: capped, ByteCount: total, Truncated: truncated}, nil
	case "image":
		var buf []byte
		if err := chromedp.Run(tctx, chromedp.CaptureScreenshot(&buf)); err != nil {
			return nil, fmt.Errorf("screenshot: %w", err)
		}
		// Bytes mode (browserd split): no disk side-effect here — the PNG
		// travels back to the caller (bob), which writes it into ITS OWN
		// sandbox. The old behaviour (browserd writing into a path bob
		// sent) was a lie on multi-host deploys: the write landed on
		// browserd's filesystem, the tool reported ok:true, and bob's
		// deliver_file/read_file could never see the file.
		if screenshotDir == "" {
			return &SnapshotResult{Format: "image", Image: buf, ByteCount: len(buf)}, nil
		}
		// In-process mode: screenshotDir is computed by the caller via
		// file.SessionSandbox (honouring SandboxRoot + namespace_by_bot) so
		// the write dir equals read_file's resolve dir — the relative path
		// we return below ("screenshots/snap-*.png") then resolves back to
		// this exact file. See browserSnapshot.Run in tools.go.
		dir := filepath.Join(screenshotDir, "screenshots")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
		path := filepath.Join(dir, fmt.Sprintf("snap-%d.png", time.Now().UnixNano()))
		if err := os.WriteFile(path, buf, 0o600); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		// Return a path relative to the per-session sandbox (the model's
		// working dir) — read_file resolves relative paths against it.
		// Avoids disclosing the absolute backend layout.
		relPath := filepath.Join("screenshots", filepath.Base(path))
		return &SnapshotResult{Format: "image", ScreenshotPath: relPath, ByteCount: len(buf)}, nil
	default:
		return nil, fmt.Errorf("format must be tree, html, text, or image")
	}
}

// captureRefList runs interactiveTagJS in the page and decodes the
// returned descriptor list. rootSel="" means whole document.
func captureRefList(ctx context.Context, rootSel string) ([]interactive, error) {
	root := "document"
	if rootSel != "" {
		// JSON-encode the selector for safe embedding in JS.
		quoted, _ := json.Marshal(rootSel)
		root = fmt.Sprintf("(document.querySelector(%s) || document)", string(quoted))
	}
	expr := fmt.Sprintf(interactiveTagJS, root)
	var items []interactive
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &items)); err != nil {
		return nil, err
	}
	return items, nil
}

// --- Click ---------------------------------------------------------------

// spaLoopThreshold is the consecutive snapshot→click-fail count after
// which Click refuses (TD6/L1 fix). Three is generous —
// real SPA mid-render usually settles by the second snapshot.
const spaLoopThreshold = 3

func Click(ctx context.Context, sess *Session, loc Locator, timeoutSecs int) error {
	sel, err := loc.resolve()
	if err != nil {
		return err
	}

	// TD6/L1 guard: if the model already burned through
	// `spaLoopThreshold` snapshot-click-fail cycles since the last
	// fresh snapshot, refuse this click before spending another LLM
	// round-trip. The page is unstable enough that ref-based
	// targeting won't reliably work.
	sess.mu.Lock()
	fails := sess.clickFailsSinceSnapshot
	sess.mu.Unlock()
	if fails >= spaLoopThreshold {
		return fmt.Errorf("click refused: %d consecutive failed clicks since last browser_snapshot — "+
			"the page is likely re-rendering on every interaction (SPA loop / WebSocket stream / "+
			"hostile auto-refresh). Stop using @e refs on this URL; try a different approach "+
			"(scrapling / read_file via copy-paste / different page).", fails)
	}

	tctx, cancel := sess.withTimeout(ctx, timeoutSecs)
	defer cancel()
	// Humanized path (humanize.go): trajectory + offset press/release. Any
	// failure BEFORE the press (element unmeasurable, move dispatch error) is
	// side-effect-free → committed=false and we fall through to the plain
	// chromedp.Click below, behaving exactly as with humanize off. Once the
	// press is attempted, committed=true: no second click path may run (double
	// click), so its error feeds the same TD6 failure handling as a plain one.
	var committed bool
	var runErr error
	if sess.humanize {
		committed, runErr = sess.humanClick(tctx, sel)
	}
	if !committed {
		runErr = chromedp.Run(tctx, chromedp.Click(sel, chromedp.ByQuery, chromedp.NodeVisible))
	}
	if runErr != nil {
		// TD6 B: SPA hint. Most click failures on @e-style refs are
		// "ref attribute gone because page re-rendered", not "the
		// element never existed". Tell the model both possibilities
		// so it picks the right next action.
		sess.mu.Lock()
		sess.clickFailsSinceSnapshot++
		count := sess.clickFailsSinceSnapshot
		sess.mu.Unlock()
		return fmt.Errorf("click %q failed: %w · if this is an SPA the page may have re-rendered "+
			"(call browser_snapshot for fresh @e refs); consecutive-fail #%d", sel, runErr, count)
	}
	// Success — clear the loop guard counter.
	sess.mu.Lock()
	sess.clickFailsSinceSnapshot = 0
	sess.mu.Unlock()
	// Auto-follow a lone popup the click opened (window.open / target=_blank).
	// Same-tab navigation (most clicks) opens 0 new targets → cheap no-op. The
	// next snapshot the model gets reflects the followed tab.
	sess.reconcileAndMaybeFollow()
	return nil
}

// --- Type ----------------------------------------------------------------

func Type(ctx context.Context, sess *Session, loc Locator, text string, replace bool, timeoutSecs int) error {
	sel, err := loc.resolve()
	if err != nil {
		return err
	}
	tctx, cancel := sess.withTimeout(ctx, timeoutSecs)
	defer cancel()

	tasks := chromedp.Tasks{chromedp.Focus(sel, chromedp.ByQuery)}
	if replace {
		// T5 fix: select-all modifier differs by OS.
		// Linux / Windows = Ctrl+A (KeyModifiers(2)), macOS = Cmd+A
		// (KeyModifiers(8)). chromium honours the host OS convention,
		// so a hardcoded Ctrl meant `replace=true` silently appended
		// instead of replacing on macOS native dev. Bob's primary
		// deploy is linux docker so the prior bug rarely fired in
		// prod, but it broke local mac development.
		tasks = append(tasks,
			chromedp.KeyEvent("a", chromedp.KeyModifiers(selectAllModifier())),
			chromedp.KeyEvent("\b"),
		)
	}
	// Humanized path (humanize.go): same focus/replace tasks, then the text
	// rune-by-rune with a typing cadence instead of SendKeys' instant fill.
	// Two gates, both falling open to SendKeys: planHumanType rejects texts
	// whose keystroke gaps would eat >~half the action timeout (long pastes
	// must not leave a half-typed field), and native file inputs must keep
	// SendKeys — it special-cases INPUT[type=file] into DOM.setFileInputFiles
	// (actually attaching the file), whereas keystrokes at a file input are a
	// silent no-op "success". A probe error also keeps SendKeys.
	if sess.humanize {
		if delays, ok := planHumanType(text, timeoutSecs, newRand()); ok {
			if isFile, ferr := isFileInput(tctx, sel); ferr == nil && !isFile {
				if err := chromedp.Run(tctx, tasks); err != nil {
					return fmt.Errorf("type into %q: %w", sel, err)
				}
				if err := dispatchHumanKeys(tctx, text, delays); err != nil {
					return fmt.Errorf("type into %q: %w", sel, err)
				}
				return nil
			}
		}
	}
	tasks = append(tasks, chromedp.SendKeys(sel, text, chromedp.ByQuery))
	if err := chromedp.Run(tctx, tasks); err != nil {
		return fmt.Errorf("type into %q: %w", sel, err)
	}
	return nil
}

// selectAllModifier returns the platform-correct modifier mask for
// "select all" in browser text fields. CDP modifier bits: 1=Alt,
// 2=Ctrl, 4=Meta-old, 8=Meta(Cmd). chromium routes the modifier to
// the page using the OS keymap, so the value must match the host OS
// the user expects.
func selectAllModifier() input.Modifier {
	if runtime.GOOS == "darwin" {
		return input.Modifier(8) // Cmd
	}
	return input.Modifier(2) // Ctrl
}

// --- Press ---------------------------------------------------------------

// pressKeyMap normalises common human key names to the chromedp / CDP key
// codes accepted by chromedp.KeyEvent. Most CDP names work verbatim;
// these are the ones users actually type.
var pressKeyMap = map[string]string{
	"enter":     "\r",
	"return":    "\r",
	"tab":       "\t",
	"escape":    "\x1b",
	"esc":       "\x1b",
	"backspace": "\b",
	"space":     " ",
	// Navigation keys map to chromedp's kb PUA runes (single rune each), NOT
	// ANSI terminal escapes: chromedp.KeyEvent iterates the string rune-by-rune
	// and kb.Encode()s each, so "\x1b[A" would dispatch Escape+'['+'A' instead
	// of one ArrowUp. kb.* are the runes kb.Encode recognises as these keys.
	"arrowup":    kb.ArrowUp,
	"arrowdown":  kb.ArrowDown,
	"arrowleft":  kb.ArrowLeft,
	"arrowright": kb.ArrowRight,
	"pageup":     kb.PageUp,
	"pagedown":   kb.PageDown,
	"home":       kb.Home,
	"end":        kb.End,
}

func Press(ctx context.Context, sess *Session, key string, loc Locator, timeoutSecs int) error {
	keyTrim := strings.TrimSpace(key)
	if keyTrim == "" {
		return fmt.Errorf("key is required")
	}
	if mapped, ok := pressKeyMap[strings.ToLower(keyTrim)]; ok {
		keyTrim = mapped
	}
	tctx, cancel := sess.withTimeout(ctx, timeoutSecs)
	defer cancel()

	tasks := chromedp.Tasks{}
	if loc.Ref != "" || loc.Selector != "" {
		// R5A-6: surface the locator error to the caller
		// instead of silently skipping focus + sending the key to whichever
		// element happens to be focused (could be unrelated, e.g. URL bar).
		sel, err := loc.resolve()
		if err != nil {
			return fmt.Errorf("press: %w", err)
		}
		tasks = append(tasks, chromedp.Focus(sel, chromedp.ByQuery))
	}
	tasks = append(tasks, chromedp.KeyEvent(keyTrim))
	if err := chromedp.Run(tctx, tasks); err != nil {
		return fmt.Errorf("press %q: %w", key, err)
	}
	return nil
}

// --- Scroll --------------------------------------------------------------

type ScrollOpts struct {
	Direction string // up | down | top | bottom
	Pixels    int    // for up/down; default 500
	Selector  string // alternative: scroll this element into view
}

func Scroll(ctx context.Context, sess *Session, opts ScrollOpts, timeoutSecs int) error {
	tctx, cancel := sess.withTimeout(ctx, timeoutSecs)
	defer cancel()

	if sel := strings.TrimSpace(opts.Selector); sel != "" {
		quoted, _ := json.Marshal(sel)
		// Report a missing selector as an error (matching Click/Type) instead of
		// the optional-chaining no-op that silently returned success — the model
		// must learn the target wasn't in view so it can re-snapshot or retry.
		expr := fmt.Sprintf(`(()=>{const el=document.querySelector(%s); if(!el) return false; el.scrollIntoView({behavior:"instant",block:"center"}); return true;})()`, string(quoted))
		var found bool
		if err := chromedp.Run(tctx, chromedp.Evaluate(expr, &found)); err != nil {
			return fmt.Errorf("scroll selector %q: %w", sel, err)
		}
		if !found {
			return fmt.Errorf("scroll selector %q: element not found", sel)
		}
		return nil
	}
	dir := strings.ToLower(strings.TrimSpace(opts.Direction))
	if dir == "" {
		dir = "down"
	}
	pixels := opts.Pixels
	if pixels <= 0 {
		pixels = 500
	}
	var expr string
	switch dir {
	case "down":
		expr = fmt.Sprintf("window.scrollBy(0, %d)", pixels)
	case "up":
		expr = fmt.Sprintf("window.scrollBy(0, -%d)", pixels)
	case "top":
		expr = "window.scrollTo(0, 0)"
	case "bottom":
		expr = "window.scrollTo(0, document.body.scrollHeight)"
	default:
		return fmt.Errorf("direction must be up/down/top/bottom")
	}
	if err := chromedp.Run(tctx, chromedp.Evaluate(expr, nil)); err != nil {
		return fmt.Errorf("scroll %s: %w", dir, err)
	}
	return nil
}

// --- Back ----------------------------------------------------------------

type BackResult struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

func Back(ctx context.Context, sess *Session, timeoutSecs int) (*BackResult, error) {
	tctx, cancel := sess.withTimeout(ctx, timeoutSecs)
	defer cancel()
	var url, title string
	if err := chromedp.Run(tctx,
		chromedp.NavigateBack(),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Location(&url),
		chromedp.Title(&title),
	); err != nil {
		return nil, fmt.Errorf("history back: %w", err)
	}
	return &BackResult{URL: url, Title: title}, nil
}

// --- GetImages -----------------------------------------------------------

type ImageInfo struct {
	Src    string `json:"src"`
	Alt    string `json:"alt"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

const listImagesJS = `
(() => {
  const root = %s;
  const out = [];
  for (const img of root.querySelectorAll('img')) {
    const w = img.naturalWidth, h = img.naturalHeight;
    if (w <= 1 && h <= 1) continue;
    out.push({
      src: img.currentSrc || img.src || '',
      alt: img.alt || '',
      width: w, height: h,
    });
  }
  return out;
})()
`

func GetImages(ctx context.Context, sess *Session, selector string, max int, timeoutSecs int) ([]ImageInfo, error) {
	if max <= 0 {
		max = 50
	}
	root := "document"
	if selector != "" {
		quoted, _ := json.Marshal(selector)
		root = fmt.Sprintf("(document.querySelector(%s) || document)", string(quoted))
	}
	expr := fmt.Sprintf(listImagesJS, root)
	tctx, cancel := sess.withTimeout(ctx, timeoutSecs)
	defer cancel()
	var imgs []ImageInfo
	if err := chromedp.Run(tctx, chromedp.Evaluate(expr, &imgs)); err != nil {
		return nil, fmt.Errorf("get_images: %w", err)
	}
	if len(imgs) > max {
		imgs = imgs[:max]
	}
	return imgs, nil
}

// --- Console -------------------------------------------------------------

// (Console capture lives on the Pool/Session — no action func here;
// tools.go calls Session.ConsoleSnapshot directly.)

// --- CDP -----------------------------------------------------------------

type CDPResult struct {
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
}

// rawMessage adapts json.RawMessage to easyjson.Marshaler so cdp.Execute
// passes it through verbatim. Without this we'd have to know each method's
// typed param struct.
type rawMessage []byte

func (r rawMessage) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("{}"), nil
	}
	return []byte(r), nil
}
func (r *rawMessage) UnmarshalJSON(b []byte) error {
	*r = append((*r)[0:0], b...)
	return nil
}

// cdpAllowMethods are the ONLY CDP commands browser_cdp may invoke,
// even at admin-approval tier (R5A-1 fix — flipped from
// the previous deny-list which was unsound because CDP's surface is
// vast and new methods land in every chromium release). Rationale:
//   - Anything not on this list — most critically Page.navigate (can
//     land on file:///data/.bob/credentials/*), Runtime.evaluate (arbitrary
//     JS in page context), Fetch.* / Network.setRequestInterception
//     (response fakery), Page.handleJavaScriptDialog (bypass our dialog
//     handler), Target.* (open new pages out-of-pool) — is rejected.
//   - The whitelist is the small intersection of "read-only / observability"
//     plus a couple of layout helpers (Emulation.setDeviceMetricsOverride,
//     Emulation.setUserAgentOverride) that admin uses to debug responsive
//     pages. Adding to this list requires an explicit security review.
//   - Network.getAllCookies is deliberately EXCLUDED: it returns
//     every cookie for every origin in the profile — i.e. the session tokens
//     of every site the admin ever logged into via the browser — which is far
//     broader than any single task needs and a one-call cross-site
//     account-takeover exfil vector. Network.getCookies (current page only)
//     stays: it's scoped to the page the agent is already (approval-gated)
//     operating on. Do NOT re-add getAllCookies without a security review.
//   - Lowercase compare so case spelling can't sneak past (CDP itself is
//     case-sensitive, but we'd rather catch a typo as "denied" than have
//     it forward to chromium with surprising semantics).
var cdpAllowMethods = map[string]bool{
	"page.capturescreenshot":             true,
	"page.capturesnapshot":               true,
	"page.printtopdf":                    true,
	"dom.getdocument":                    true,
	"dom.queryselector":                  true,
	"dom.queryselectorall":               true,
	"dom.getouterhtml":                   true,
	"network.getcookies":                 true,
	"runtime.getproperties":              true,
	"emulation.setdevicemetricsoverride": true,
	"emulation.setuseragentoverride":     true,
}

func CDPExec(ctx context.Context, sess *Session, method string, params json.RawMessage, timeoutSecs int) (*CDPResult, error) {
	method = strings.TrimSpace(method)
	if method == "" {
		return nil, fmt.Errorf("method is required")
	}
	if !cdpAllowMethods[strings.ToLower(method)] {
		return nil, fmt.Errorf("CDP method %q is not permitted; "+
			"use a higher-level browser_* tool instead", method)
	}
	if len(params) == 0 {
		params = []byte("{}")
	}
	tctx, cancel := sess.withTimeout(ctx, timeoutSecs)
	defer cancel()

	// cdp.Execute pulls the Executor from a context value that chromedp
	// only attaches *inside* a chromedp.Run action — calling cdp.Execute
	// directly on the chromedp browser context produces "invalid context"
	// because the target Executor isn't yet on the ctx. Wrap in
	// ActionFunc so chromedp injects the target before we run.
	var resp json.RawMessage
	if err := chromedp.Run(tctx, chromedp.ActionFunc(func(ictx context.Context) error {
		return cdp.Execute(ictx, method, rawMessage(params), &resp)
	})); err != nil {
		return nil, fmt.Errorf("CDP %s: %w", method, err)
	}
	return &CDPResult{Method: method, Result: resp}, nil
}
