package browser

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/target"
)

// newTabTestSession builds a Session wired only with the fields the tab
// bookkeeping touches, plus an injectable targetsFn. No chromium is launched —
// these tests exercise the PURE add/remove/index/active logic. switchTo /
// closeTab are NOT exercised here (they call chromedp.Run against a real
// browser); reconcileTabs and tabList are pure given targetsFn + the fields.
func newTabTestSession(root target.ID, list *[]*target.Info) *Session {
	s := &Session{
		rootTarget:   root,
		activeTarget: root,
		tabs:         []target.ID{root},
	}
	s.targetsFn = func() ([]*target.Info, error) { return *list, nil }
	return s
}

func pageInfo(id, title, url string) *target.Info {
	return &target.Info{TargetID: target.ID(id), Type: "page", Title: title, URL: url}
}

// TestReconcileTabs_AddRemove pins the core bookkeeping: a new page-target is
// appended (and counted), a closed one is dropped, existing order is stable,
// and non-"page" targets are ignored.
func TestReconcileTabs_AddRemove(t *testing.T) {
	list := []*target.Info{pageInfo("t1", "home", "https://a")}
	s := newTabTestSession("t1", &list)

	// No change → 0 new, tabs unchanged.
	if n := s.reconcileTabs(); n != 0 {
		t.Fatalf("steady state: new=%d, want 0", n)
	}
	if len(s.tabs) != 1 || s.tabs[0] != "t1" {
		t.Fatalf("tabs = %v, want [t1]", s.tabs)
	}

	// A popup appears (+ an iframe that must be ignored).
	list = append(list,
		pageInfo("t2", "popup", "https://b"),
		&target.Info{TargetID: "frame1", Type: "iframe"},
	)
	if n := s.reconcileTabs(); n != 1 {
		t.Fatalf("popup appeared: new=%d, want 1 (iframe must not count)", n)
	}
	if len(s.tabs) != 2 || s.tabs[0] != "t1" || s.tabs[1] != "t2" {
		t.Fatalf("tabs = %v, want [t1 t2] (stable order, new at end)", s.tabs)
	}

	// Two more open at once.
	list = append(list, pageInfo("t3", "c", "https://c"), pageInfo("t4", "d", "https://d"))
	if n := s.reconcileTabs(); n != 2 {
		t.Fatalf("two opened: new=%d, want 2", n)
	}

	// The middle tab closes → dropped, order of survivors preserved.
	list = []*target.Info{pageInfo("t1", "home", "https://a"), pageInfo("t3", "c", "https://c"), pageInfo("t4", "d", "https://d")}
	if n := s.reconcileTabs(); n != 0 {
		t.Fatalf("close only: new=%d, want 0", n)
	}
	want := []target.ID{"t1", "t3", "t4"}
	if len(s.tabs) != 3 || s.tabs[0] != want[0] || s.tabs[1] != want[1] || s.tabs[2] != want[2] {
		t.Fatalf("tabs = %v, want %v (t2 dropped, order stable)", s.tabs, want)
	}
}

// TestReconcileTabs_ErrorIsNoOp: a targetsFn error must leave the list intact
// (fail-open so a transient enumeration hiccup can't break the common case).
func TestReconcileTabs_ErrorIsNoOp(t *testing.T) {
	s := &Session{rootTarget: "t1", activeTarget: "t1", tabs: []target.ID{"t1", "t2"}}
	s.targetsFn = func() ([]*target.Info, error) { return nil, assertErr{} }
	if n := s.reconcileTabs(); n != 0 {
		t.Fatalf("error path: new=%d, want 0", n)
	}
	if len(s.tabs) != 2 {
		t.Fatalf("error path mutated tabs to %v; must be a no-op", s.tabs)
	}
}

type assertErr struct{}

func (assertErr) Error() string { return "boom" }

// TestTabList_IndexActiveMapping pins 1-based indexing, the Active flag, and
// title/url pull-through from the fresh Targets() lookup.
func TestTabList_IndexActiveMapping(t *testing.T) {
	list := []*target.Info{
		pageInfo("t1", "首页", "https://taobao.com"),
		pageInfo("t2", "登录", "https://login.taobao.com"),
	}
	s := newTabTestSession("t1", &list)
	s.tabs = []target.ID{"t1", "t2"}
	s.activeTarget = "t2"

	rows := s.tabList()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Index != 1 || rows[0].Title != "首页" || rows[0].Active {
		t.Fatalf("row0 = %+v, want index1 title=首页 inactive", rows[0])
	}
	if rows[1].Index != 2 || rows[1].Title != "登录" || !rows[1].Active {
		t.Fatalf("row1 = %+v, want index2 title=登录 active", rows[1])
	}
	if rows[1].URL != "https://login.taobao.com" {
		t.Fatalf("row1 url = %q, want full login url", rows[1].URL)
	}
}

// TestTabHeader pins the header: empty for a single tab (zero noise) and the
// "[标签页 N个] … *…(当前)" form with the active marker for multiple tabs.
func TestTabHeader(t *testing.T) {
	single := []*target.Info{pageInfo("t1", "home", "https://a")}
	s := newTabTestSession("t1", &single)
	if h := s.tabHeader(); h != "" {
		t.Fatalf("single-tab header = %q, want empty (zero noise)", h)
	}

	multi := []*target.Info{pageInfo("t1", "首页", "https://a"), pageInfo("t2", "登录", "https://b")}
	s = newTabTestSession("t1", &multi)
	s.tabs = []target.ID{"t1", "t2"}
	s.activeTarget = "t2"
	h := s.tabHeader()
	if h == "" {
		t.Fatal("multi-tab header is empty, want a header line")
	}
	if h[len(h)-1] != '\n' {
		t.Fatalf("header must end in newline, got %q", h)
	}
	if !strings.Contains(h, "[标签页 2个]") || !strings.Contains(h, "2*:登录(当前)") || !strings.Contains(h, "1:首页") {
		t.Fatalf("header = %q, missing expected pieces", h)
	}
}

// TestSwitchTo_DoesNotCloseDepartedTab is the #575 regression: a tab switch
// must NOT cancel the binding of the tab it leaves. In chromedp, cancelling a
// WithTargetID child ctx CloseTarget's the tab, so the old single-activeCancel
// design silently closed a tab on every switch. With per-tab bindings, hopping
// t1→t2→t3→t1 keeps every binding alive (no cancel fires) and re-switching to a
// bound tab re-points BrowserCtx without touching chromedp.
func TestSwitchTo_DoesNotCloseDepartedTab(t *testing.T) {
	rootCtx := context.Background()
	c2, c3 := false, false
	s := &Session{
		rootCtx: rootCtx, rootTarget: "t1", activeTarget: "t1",
		tabs: []target.ID{"t1", "t2", "t3"},
		tabBindings: map[target.ID]tabBinding{
			"t2": {ctx: context.Background(), cancel: func() { c2 = true }},
			"t3": {ctx: context.Background(), cancel: func() { c3 = true }},
		},
	}
	for _, id := range []target.ID{"t2", "t3", "t1"} {
		if err := s.switchTo(id); err != nil {
			t.Fatalf("switchTo(%s): %v", id, err)
		}
		if s.activeTarget != id {
			t.Fatalf("after switchTo(%s) active=%s", id, s.activeTarget)
		}
	}
	if s.BrowserCtx != rootCtx {
		t.Fatal("switch back to root must restore rootCtx")
	}
	if c2 || c3 {
		t.Fatalf("a switch cancelled a binding (c2=%v c3=%v) — departed tabs must stay open", c2, c3)
	}
	if len(s.tabBindings) != 2 {
		t.Fatalf("bindings = %d, want 2 (none dropped by switching)", len(s.tabBindings))
	}
}

// TestCloseTab_CancelsBindingDropsReanchors: closing a bound, active tab cancels
// its binding (chromedp closes the target), drops it from the list + map, and
// re-anchors the active tab on root.
func TestCloseTab_CancelsBindingDropsReanchors(t *testing.T) {
	rootCtx := context.Background()
	cancelled := false
	s := &Session{
		rootCtx: rootCtx, rootTarget: "t1", activeTarget: "t2",
		tabs: []target.ID{"t1", "t2"},
		tabBindings: map[target.ID]tabBinding{
			"t2": {ctx: context.Background(), cancel: func() { cancelled = true }},
		},
	}
	if err := s.closeTab("t2"); err != nil {
		t.Fatalf("closeTab(t2): %v", err)
	}
	if !cancelled {
		t.Fatal("closeTab must cancel the tab's binding (the chromedp close)")
	}
	if _, ok := s.tabBindings["t2"]; ok {
		t.Fatal("binding must be removed")
	}
	if len(s.tabs) != 1 || s.tabs[0] != "t1" {
		t.Fatalf("tabs = %v, want [t1]", s.tabs)
	}
	if s.activeTarget != "t1" || s.BrowserCtx != rootCtx {
		t.Fatalf("active tab must re-anchor on root, got active=%s", s.activeTarget)
	}
}

// TestCloseTab_RootRefused: the root tab is the durable anchor — closeTab must
// refuse it without mutating anything.
func TestCloseTab_RootRefused(t *testing.T) {
	s := &Session{
		rootCtx: context.Background(), rootTarget: "t1", activeTarget: "t1",
		tabs: []target.ID{"t1", "t2"}, tabBindings: map[target.ID]tabBinding{},
	}
	if err := s.closeTab("t1"); err == nil {
		t.Fatal("closing the root tab must error")
	}
	if len(s.tabs) != 2 {
		t.Fatalf("refused close mutated tabs to %v", s.tabs)
	}
}

// TestReconcileTabs_CancelsDroppedBinding: a tab that vanishes from the live set
// must have its binding cancelled (goroutine cleanup) and removed.
func TestReconcileTabs_CancelsDroppedBinding(t *testing.T) {
	live := []*target.Info{pageInfo("t1", "home", "https://a"), pageInfo("t2", "popup", "https://b")}
	cancelled := false
	s := &Session{
		rootTarget: "t1", activeTarget: "t1",
		tabs: []target.ID{"t1", "t2"},
		tabBindings: map[target.ID]tabBinding{
			"t2": {ctx: context.Background(), cancel: func() { cancelled = true }},
		},
	}
	s.targetsFn = func() ([]*target.Info, error) { return live, nil }

	live = []*target.Info{pageInfo("t1", "home", "https://a")} // t2 closed externally
	s.reconcileTabs()

	if !cancelled {
		t.Fatal("reconcile must cancel a vanished tab's binding")
	}
	if _, ok := s.tabBindings["t2"]; ok {
		t.Fatal("vanished tab's binding must be removed")
	}
	if len(s.tabs) != 1 || s.tabs[0] != "t1" {
		t.Fatalf("tabs = %v, want [t1]", s.tabs)
	}
}

// TestReconcileTabs_ReanchorsVanishedActive: a tab that closes ITSELF while
// active (an OAuth popup window.close()s after login — the standard flow) must
// re-anchor the session on root, mirroring closeTab's wasActive handling.
// Without it BrowserCtx stays pointed at the cancelled child ctx: every later
// action fails with "context canceled", and the screencast supervisor wedges
// (the active ctx never changes again, so it never re-binds the stream).
func TestReconcileTabs_ReanchorsVanishedActive(t *testing.T) {
	rootCtx := context.Background()
	childCtx, childCancel := context.WithCancel(context.Background())
	defer childCancel()
	cancelled := false
	live := []*target.Info{pageInfo("t1", "home", "https://a"), pageInfo("t2", "oauth", "https://b")}
	s := &Session{
		rootCtx: rootCtx, rootTarget: "t1",
		BrowserCtx: childCtx, activeTarget: "t2",
		tabs: []target.ID{"t1", "t2"},
		tabBindings: map[target.ID]tabBinding{
			"t2": {ctx: childCtx, cancel: func() { cancelled = true }},
		},
	}
	s.targetsFn = func() ([]*target.Info, error) { return live, nil }

	live = []*target.Info{pageInfo("t1", "home", "https://a")} // popup closed itself
	s.reconcileTabs()

	if !cancelled {
		t.Fatal("vanished active tab's binding must still be cancelled")
	}
	if s.activeTarget != "t1" || s.BrowserCtx != rootCtx {
		t.Fatalf("session must re-anchor on root after the active tab vanished, got active=%s", s.activeTarget)
	}
	// A vanished INACTIVE tab must not disturb the active anchor.
	live = []*target.Info{pageInfo("t1", "home", "https://a"), pageInfo("t3", "x", "https://c")}
	s.reconcileTabs()
	live = []*target.Info{pageInfo("t1", "home", "https://a")}
	s.activeTarget = "t1"
	s.reconcileTabs()
	if s.activeTarget != "t1" || s.BrowserCtx != rootCtx {
		t.Fatalf("inactive vanish must keep the anchor, got active=%s", s.activeTarget)
	}
}

// TestTabIDForIndex pins index resolution + out-of-range errors.
func TestTabIDForIndex(t *testing.T) {
	s := &Session{tabs: []target.ID{"t1", "t2", "t3"}}
	if id, err := s.tabIDForIndex(2); err != nil || id != "t2" {
		t.Fatalf("index 2 → id=%q err=%v, want t2", id, err)
	}
	if _, err := s.tabIDForIndex(0); err == nil {
		t.Fatal("index 0 must error (1-based)")
	}
	if _, err := s.tabIDForIndex(4); err == nil {
		t.Fatal("index past end must error")
	}
}
