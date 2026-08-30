package browser

import (
	"testing"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/target"
)

// TestInputAction_Mouse pins the InputEvent → CDP mouse command mapping.
func TestInputAction_Mouse(t *testing.T) {
	act, err := inputAction(InputEvent{
		Kind: "mouse", MouseType: "press", X: 10, Y: 20, Button: "left", ClickCount: 2,
	})
	if err != nil {
		t.Fatalf("inputAction mouse: %v", err)
	}
	m, ok := act.(*input.DispatchMouseEventParams)
	if !ok {
		t.Fatalf("mouse action type = %T, want *input.DispatchMouseEventParams", act)
	}
	if m.Type != input.MousePressed || m.X != 10 || m.Y != 20 || m.Button != input.Left || m.ClickCount != 2 {
		t.Fatalf("mouse params = %+v, want pressed/(10,20)/left/clicks=2", m)
	}
	// A press MUST carry the held-button bitmask (Left=1) — newer chromium
	// drops a mousePressed whose buttons mask is 0 (the click never fires).
	if m.Buttons != 1 {
		t.Fatalf("press buttons mask = %d, want 1 (left held)", m.Buttons)
	}

	// Release clears the held mask back to 0 (button no longer down).
	rel, _ := inputAction(InputEvent{Kind: "mouse", MouseType: "release", X: 10, Y: 20, Button: "left", ClickCount: 1})
	rm := rel.(*input.DispatchMouseEventParams)
	if rm.Type != input.MouseReleased || rm.Button != input.Left || rm.Buttons != 0 {
		t.Fatalf("release params = %+v, want released/left/buttons=0", rm)
	}

	// A right-button press carries the right bit (2).
	rp, _ := inputAction(InputEvent{Kind: "mouse", MouseType: "press", X: 1, Y: 1, Button: "right", ClickCount: 1})
	if rpm := rp.(*input.DispatchMouseEventParams); rpm.Buttons != 2 {
		t.Fatalf("right press buttons mask = %d, want 2", rpm.Buttons)
	}

	// Wheel carries deltas and no held button.
	w, _ := inputAction(InputEvent{Kind: "mouse", MouseType: "wheel", X: 1, Y: 2, DeltaX: 3, DeltaY: -4})
	wm := w.(*input.DispatchMouseEventParams)
	if wm.Type != input.MouseWheel || wm.DeltaX != 3 || wm.DeltaY != -4 || wm.Buttons != 0 {
		t.Fatalf("wheel params = %+v, want wheel/dx=3/dy=-4/buttons=0", wm)
	}
}

// TestInputAction_Key pins the key mapping (char carries Text; down/up carry Key).
func TestInputAction_Key(t *testing.T) {
	c, err := inputAction(InputEvent{Kind: "key", KeyType: "char", Text: "a"})
	if err != nil {
		t.Fatalf("inputAction key char: %v", err)
	}
	cp := c.(*input.DispatchKeyEventParams)
	if cp.Type != input.KeyChar || cp.Text != "a" {
		t.Fatalf("key char = %+v, want char/text=a", cp)
	}

	d, _ := inputAction(InputEvent{Kind: "key", KeyType: "down", Key: "Enter"})
	dp := d.(*input.DispatchKeyEventParams)
	if dp.Type != input.KeyDown || dp.Key != "Enter" {
		t.Fatalf("key down = %+v, want down/key=Enter", dp)
	}
}

// TestInputAction_Errors pins strict rejection of unknown kinds/types.
func TestInputAction_Errors(t *testing.T) {
	for _, ev := range []InputEvent{
		{Kind: "nope"},
		{Kind: "mouse", MouseType: "teleport"},
		{Kind: "key", KeyType: "smash"},
	} {
		if _, err := inputAction(ev); err == nil {
			t.Fatalf("inputAction(%+v) should error", ev)
		}
	}
}

// TestActiveTargetID_TracksActiveTab pins that the id StartScreencast activates
// (Session.activeTargetID) is the SAME tab activeCtx() drives, and that it
// follows a tab swap — the see==control invariant the multi-tab black-screen fix
// rests on. No chromium: we mutate the mu-guarded tab fields directly the way
// switchTo does and read the helper back.
func TestActiveTargetID_TracksActiveTab(t *testing.T) {
	s := &Session{rootTarget: "t1", activeTarget: "t1", tabs: []target.ID{"t1", "t2"}}

	if got := s.activeTargetID(); got != "t1" {
		t.Fatalf("activeTargetID = %q, want t1 (initial active)", got)
	}

	// Simulate a tab swap (what switchTo does after attaching the child ctx):
	// activeTarget moves to t2. activeTargetID must follow so the screencast
	// activates the tab the human will be driving.
	s.mu.Lock()
	s.activeTarget = "t2"
	s.mu.Unlock()

	if got := s.activeTargetID(); got != "t2" {
		t.Fatalf("activeTargetID after swap = %q, want t2", got)
	}
}

// TestActivateTarget_BlankIDNoOp pins that activateTarget skips a blank id
// without touching CDP — a Session whose active target was never captured must
// not crash the takeover (it would dereference a nil chromedp context if it ran
// chromedp.Run). A blank id returns immediately, before any Run.
func TestActivateTarget_BlankIDNoOp(t *testing.T) {
	// nil bctx would panic inside chromedp.Run; the blank-id guard must return
	// before reaching it. No panic == pass.
	activateTarget(nil, "")
}

func TestMouseButton(t *testing.T) {
	cases := map[string]input.MouseButton{
		"left": input.Left, "right": input.Right, "middle": input.Middle,
		"none": input.None, "": input.None, "bogus": input.None,
	}
	for in, want := range cases {
		if got := mouseButton(in); got != want {
			t.Fatalf("mouseButton(%q) = %q, want %q", in, got, want)
		}
	}
}
