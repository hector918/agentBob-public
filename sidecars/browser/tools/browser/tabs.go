package browser

// Multi-tab / popup manager. bob is single-tab: every action in
// actions.go runs against Session.BrowserCtx (the active tab). When a click or
// window.open spawns a new browser target (tab / popup — Taobao login, OAuth,
// target=_blank), chromedp stays bound to the original target and the model
// goes blind. This file tracks live page targets, auto-follows a lone popup,
// and backs the browser_tab tool so the model can list / switch / close tabs.
//
// All methods are on *Session and mutate the mu-guarded tab fields
// (BrowserCtx, tabs, activeTarget, tabBindings). The turn loop is sequential
// per Session so swaps only happen between actions, but mu still guards every
// field access. Targets() is reached via s.targetsFn (overridable in tests).

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// RunTab executes one browser_tab action (list / switch / close) against sess
// and returns the JSON-friendly payload the tool envelope carries. Shared by
// the in-process tool shim (tools.go) and the browserd /tab handler so the
// two surfaces cannot drift. Reconciles first so a popup the model hasn't
// acted on yet is visible / addressable in this call. Caller must have
// acquired sess this turn (the Pool refreshes lastUsed there).
func RunTab(sess *Session, action string, index int) (map[string]any, error) {
	sess.reconcileTabs()

	switch strings.ToLower(strings.TrimSpace(action)) {
	case "", "list":
		return map[string]any{"tabs": sess.tabList()}, nil
	case "switch":
		id, err := sess.tabIDForIndex(index)
		if err != nil {
			return nil, err
		}
		if err := sess.switchTo(id); err != nil {
			return nil, err
		}
		return map[string]any{"switched": index, "tabs": sess.tabList()}, nil
	case "close":
		id, err := sess.tabIDForIndex(index)
		if err != nil {
			return nil, err
		}
		if err := sess.closeTab(id); err != nil {
			return nil, err
		}
		return map[string]any{"closed": index, "tabs": sess.tabList()}, nil
	default:
		return nil, fmt.Errorf("browser_tab: action 必须是 list / switch / close")
	}
}

// TabInfo is one row of the tab list the model sees (1-based Index).
type TabInfo struct {
	Index  int    `json:"index"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// livePageTargets returns the current page-type targets keyed by id (for
// title/url lookups) plus the enumeration-ordered id list — Targets()
// order, which reconcileTabs needs so newly-appeared tabs append in a
// deterministic order (map-range order would shuffle the model's 1-based
// indices between identical calls). Filters to Type=="page" (drops
// iframes, workers, and the devtools target). A freshly-opened popup may
// briefly be about:blank — it is still Type=="page", so it is kept; only
// the devtools page (which chromedp never reports as a plain "page"
// target) is excluded by the type filter.
func (s *Session) livePageTargets() (map[target.ID]*target.Info, []target.ID, error) {
	infos, err := s.targetsFn()
	if err != nil {
		return nil, nil, err
	}
	out := make(map[target.ID]*target.Info, len(infos))
	order := make([]target.ID, 0, len(infos))
	for _, ti := range infos {
		if ti == nil || ti.Type != "page" {
			continue
		}
		out[ti.TargetID] = ti
		order = append(order, ti.TargetID)
	}
	return out, order, nil
}

// reconcileTabs syncs s.tabs with the browser's live page targets and returns
// how many NEW targets were appended. Removes ids that have closed; appends
// ids not already tracked (counting them). Order is kept stable — existing
// entries never move, new ones land at the end — because the model addresses
// tabs by 1-based index and a reorder would silently retarget a switch/close.
// On a Targets() error it leaves s.tabs untouched and returns 0 (fail-open:
// the common same-tab case must not be broken by a transient enumeration
// hiccup).
func (s *Session) reconcileTabs() (newCount int) {
	live, order, err := s.livePageTargets()
	if err != nil {
		return 0
	}
	s.mu.Lock()

	// Drop closed ids, preserving order. A tab that vanished from the live set
	// (page closed itself, parent navigated away) has its child binding
	// cancelled so the chromedp event-pump goroutine exits — but we COLLECT the
	// cancels here and run them AFTER releasing mu: cancel is chromedp's
	// cancelWait, which blocks on DetachFromTarget+CloseTarget (up to ~1s), so
	// holding mu across it would stall concurrent takeover input handlers that
	// contend on s.mu (screencast supervisor + DispatchInput).
	var stale []tabBinding
	kept := s.tabs[:0:0]
	activeGone := false
	for _, id := range s.tabs {
		if _, ok := live[id]; ok {
			kept = append(kept, id)
			continue
		}
		if id == s.activeTarget {
			activeGone = true
		}
		if b, ok := s.tabBindings[id]; ok {
			stale = append(stale, b)
			delete(s.tabBindings, id)
		}
	}
	// If the ACTIVE tab vanished (a popup that window.close()s itself after an
	// OAuth login is the standard case), re-anchor on root — the same semantics
	// closeTab applies for an explicit close. Without this, BrowserCtx stays
	// pointed at the now-cancelled child ctx and every later action fails with
	// "context canceled" (and the screencast supervisor wedges: the active ctx
	// never changes again, so it never re-binds).
	if activeGone {
		s.BrowserCtx = s.rootCtx
		s.activeTarget = s.rootTarget
	}
	// Append present-but-untracked ids (new tabs / popups) in enumeration
	// order — deterministic, unlike ranging over the live map.
	have := make(map[target.ID]bool, len(kept))
	for _, id := range kept {
		have[id] = true
	}
	for _, id := range order {
		if !have[id] {
			kept = append(kept, id)
			have[id] = true
			newCount++
		}
	}
	s.tabs = kept
	s.mu.Unlock()

	for _, b := range stale {
		b.cancel()
		s.dropCastState(b.ctx) // forget the dead ctx in the screencast maps
	}
	return newCount
}

// reconcileAndMaybeFollow is the auto-follow hook actions.go calls after a
// successful Click / Navigate. It reconciles the tab list and, IFF exactly one
// new target appeared AND it isn't already the active tab, switches to it (the
// single-popup ergonomic case). >1 new → leave them for the model to pick via
// browser_tab. 0 new (the overwhelmingly common same-tab navigation) → cheap
// no-op. Returns whether it followed. Best-effort: a switch error is swallowed
// (the next snapshot still reflects whatever tab is active).
func (s *Session) reconcileAndMaybeFollow() (followed bool) {
	newCount := s.reconcileTabs()
	if newCount != 1 {
		return false
	}
	// The single new tab is the last one appended (reconcileTabs appends).
	s.mu.Lock()
	newID := s.tabs[len(s.tabs)-1]
	active := s.activeTarget
	s.mu.Unlock()
	if newID == active {
		return false
	}
	if err := s.switchTo(newID); err != nil {
		return false
	}
	return true
}

// tabBinding is a non-root tab's live child context + its cancel. Cancelling
// it (via closeTab / reconcile / CloseInstance) DetachFromTarget+CloseTarget's
// the tab in chromedp — so a binding is kept alive for the whole life of the
// tab and only cancelled when the tab is actually meant to close.
type tabBinding struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// switchTo points the active tab (BrowserCtx) at id WITHOUT closing the tab it
// leaves. Switching to the root tab restores rootCtx. Switching to a non-root
// tab reuses that tab's existing child binding if it has one, else binds a
// fresh child context (shared Browser), attaches it via a Run, and remembers
// it in tabBindings so a later switch back finds it alive. Crucially it never
// cancels the previous tab's binding — in chromedp that cancel would close the
// departed tab (chromedp.go cancel path → CloseTarget). All mutations under mu.
func (s *Session) switchTo(id target.ID) error {
	s.mu.Lock()
	root := s.rootTarget
	rootCtx := s.rootCtx
	if id == root {
		s.BrowserCtx = rootCtx
		s.activeTarget = id
		s.mu.Unlock()
		return nil
	}
	if b, ok := s.tabBindings[id]; ok {
		// Already bound (and still live) — just re-point. Its dialog/console
		// handlers were attached at bind time and live as long as the binding.
		s.BrowserCtx = b.ctx
		s.activeTarget = id
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	// Bind section, serialised by bindMu. Two callers race to bind the same
	// unbound id for real: the screencast follow supervisor ticks
	// reconcileAndMaybeFollow concurrently with a human tab switch
	// (RunTabForScope) or a model-side browser_tab. Without the lock the
	// loser's map write below overwrites the winner's binding and the
	// winner's cancel is lost forever — and a compensating cancel is NOT an
	// option, it would CloseTarget the tab (see tabBinding). Re-checking
	// under mu after taking bindMu makes the loser reuse the winner's
	// binding instead of creating a second ctx.
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	s.mu.Lock()
	if b, ok := s.tabBindings[id]; ok {
		s.BrowserCtx = b.ctx
		s.activeTarget = id
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	child, cancel := chromedp.NewContext(rootCtx, chromedp.WithTargetID(id))
	if err := chromedp.Run(child); err != nil {
		cancel()
		return fmt.Errorf("switch to tab %s: %w", id, err)
	}
	// chromedp.ListenTarget is per-target: the dialog handler + console listener
	// attached at launch live on rootCtx only. Without attaching here, a child
	// tab (auto-followed popup, OAuth window) that fires alert()/confirm() would
	// have no HandleJavaScriptDialog consumer — CDP pauses the page and every
	// later action on this tab times out — and its console events are lost. The
	// dialog worker exits when this binding is cancelled (closeTab / teardown);
	// the console ring is shared, so it just keeps appending.
	attachDialogHandler(child, s)
	if s.console != nil {
		attachConsoleListener(child, s.console)
	}
	s.mu.Lock()
	s.tabBindings[id] = tabBinding{ctx: child, cancel: cancel}
	s.BrowserCtx = child
	s.activeTarget = id
	s.mu.Unlock()
	return nil
}

// tabList returns one TabInfo per tracked tab (1-based Index), pulling the
// current Title/URL from a fresh Targets() lookup. Title/URL are truncated for
// display. A tab whose info is momentarily missing still appears (empty
// title/url) so indices stay aligned with s.tabs.
func (s *Session) tabList() []TabInfo {
	live, _, _ := s.livePageTargets() // nil map on error → empty title/url, indices intact

	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TabInfo, 0, len(s.tabs))
	for i, id := range s.tabs {
		var title, url string
		if info := live[id]; info != nil {
			title = info.Title
			url = info.URL
		}
		out = append(out, TabInfo{
			Index:  i + 1,
			Title:  truncRunes(title, 40),
			URL:    truncRunes(url, 80),
			Active: id == s.activeTarget,
		})
	}
	return out
}

// tabIDForIndex maps a model-supplied 1-based tab index to its target id under
// mu, erroring on an out-of-range index (the model may quote a stale list).
func (s *Session) tabIDForIndex(index int) (target.ID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 1 || index > len(s.tabs) {
		return "", fmt.Errorf("无效标签序号 %d（当前共 %d 个标签，用 1-%d）", index, len(s.tabs), len(s.tabs))
	}
	return s.tabs[index-1], nil
}

// closeTab closes one non-root tab. A tab with a live child binding is closed
// by cancelling that binding (chromedp DetachFromTarget+CloseTarget's it AND
// stops its event-pump goroutine — the idiomatic close); a tab the model only
// listed but never switched to has no binding, so it's closed with an explicit
// CloseTarget over the root connection. The closed id is dropped from s.tabs
// deterministically (not via a Targets() re-poll, which wouldn't yet reflect
// the async close), and if it was the active tab we re-anchor on root. The
// root tab is the durable anchor and cannot be closed.
func (s *Session) closeTab(id target.ID) error {
	s.mu.Lock()
	rootCtx := s.rootCtx
	root := s.rootTarget
	if id == root {
		s.mu.Unlock()
		return fmt.Errorf("不能关闭主标签页（标签 1）")
	}
	wasActive := id == s.activeTarget
	binding, hasBinding := s.tabBindings[id]
	s.mu.Unlock()

	if hasBinding {
		binding.cancel() // chromedp detaches + closes the target; stops its goroutine
		s.dropCastState(binding.ctx)
	} else if err := chromedp.Run(rootCtx, chromedp.ActionFunc(func(c context.Context) error {
		return target.CloseTarget(id).Do(c)
	})); err != nil {
		return fmt.Errorf("close tab %s: %w", id, err)
	}

	// Drop the closed id + its binding deterministically; re-anchor on root if
	// it was active (root stays in s.tabs — it can't be closed above).
	s.mu.Lock()
	delete(s.tabBindings, id)
	kept := s.tabs[:0:0]
	for _, t := range s.tabs {
		if t != id {
			kept = append(kept, t)
		}
	}
	s.tabs = kept
	s.mu.Unlock()

	if !wasActive {
		return nil
	}
	return s.switchTo(root)
}

// tabHeader is the one-line header prepended to a tree snapshot when more than
// one tab is live, e.g. "[标签页 2个] 1:淘宝首页  2*:登录(当前)". The active
// tab is marked with "*" and "(当前)". Returns "" when there is only one tab,
// so the single-tab common case carries ZERO extra noise.
func (s *Session) tabHeader() string {
	// Cheap gate first: the single-tab common case must not pay a Targets() CDP
	// round-trip on every snapshot. len(s.tabs) is known without touching CDP.
	s.mu.Lock()
	n := len(s.tabs)
	s.mu.Unlock()
	if n <= 1 {
		return ""
	}
	tabs := s.tabList()
	if len(tabs) <= 1 {
		return ""
	}
	header := fmt.Sprintf("[标签页 %d个]", len(tabs))
	for _, t := range tabs {
		label := t.Title
		if label == "" {
			label = t.URL
		}
		if t.Active {
			header += fmt.Sprintf(" %d*:%s(当前)", t.Index, label)
		} else {
			header += fmt.Sprintf(" %d:%s", t.Index, label)
		}
	}
	return header + "\n"
}

// truncRunes caps s to at most n runes (rune-safe, no mid-rune cut), appending
// "…" when it had to cut. For tab-list display only.
func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	cnt := 0
	for i := range s {
		if cnt == n {
			return s[:i] + "…"
		}
		cnt++
	}
	return s
}
