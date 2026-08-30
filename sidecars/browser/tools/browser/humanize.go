package browser

// Human-input simulation for Click / Type (actions.go). Instant CDP dispatch
// at an element's exact center with zero mousemove history is a textbook
// automation signature; this layer dispatches a curved, jitter-timed
// mouse-move trajectory into a randomized in-element landing point, and a
// per-keystroke typing cadence. Gated by Session.humanize
// (cfg.tools.browser.humanize, default on).
//
// Layout: the geometry/timing planners (humanPath, clickOffset, typeDelays,
// planHumanType, easeInOut, clampPoint, intersectRect, largestRect) and the
// Evaluate expression builders are pure functions — randomness via an
// injected *rand.Rand — so they unit-test deterministically; only the
// dispatch helpers below touch chromedp.
// math/rand (not crypto) on purpose — this is behavioural mimicry, not
// security material.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

type point struct {
	X, Y float64
}

type rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// pathStep is one mouseMoved dispatch: move to Pos, then wait Wait before
// the next step (the wait is what spreads the trajectory over wall time).
type pathStep struct {
	Pos  point
	Wait time.Duration
}

const (
	minPathPoints = 15
	maxPathPoints = 30
	// humanKeyMinDelay is the floor of typeDelays' per-key gap. Shared with
	// planHumanType, whose cheap pre-screen (n×floor vs budget) must never
	// under-estimate what typeDelays can produce.
	humanKeyMinDelay = 40 * time.Millisecond
)

// newRand returns a per-call PRNG seeded off the (thread-safe) global
// source. The planners take *rand.Rand so tests can pass a fixed seed.
func newRand() *rand.Rand {
	return rand.New(rand.NewSource(rand.Int63()))
}

// easeInOut is the smoothstep curve 3t²−2t³ — strictly increasing on [0,1]
// with zero slope at both ends, so the cursor accelerates out of the start
// and decelerates into the target like a hand does.
func easeInOut(t float64) float64 {
	return t * t * (3 - 2*t)
}

// pathDuration scales the total move time with distance — ~150ms for a short
// hop, saturating at ~400ms for a cross-screen move — then applies ±20%
// jitter so equal-length moves never take identical time.
func pathDuration(dist float64, rng *rand.Rand) time.Duration {
	frac := dist / 1500
	if frac > 1 {
		frac = 1
	}
	baseMs := 150 + 250*frac
	jitter := 0.8 + 0.4*rng.Float64()
	return time.Duration(baseMs * jitter * float64(time.Millisecond))
}

// bezierControls picks 1-2 control points near the from→to segment: each sits
// at a random fraction along the line, pushed perpendicularly off it by
// 10-30% of the distance. That bows the trajectory into the slight arc real
// mouse moves have instead of a ruler-straight line.
func bezierControls(from, to point, rng *rand.Rand) []point {
	n := 1 + rng.Intn(2)
	dx, dy := to.X-from.X, to.Y-from.Y
	dist := math.Hypot(dx, dy)
	ctrls := make([]point, n)
	for i := range ctrls {
		u := 0.25 + 0.5*rng.Float64()
		off := (0.1 + 0.2*rng.Float64()) * dist
		if rng.Intn(2) == 0 {
			off = -off
		}
		// Perpendicular unit vector (−dy, dx)/dist; degenerate zero-length
		// moves keep the control on the point itself.
		var px, py float64
		if dist > 0 {
			px, py = -dy/dist, dx/dist
		}
		ctrls[i] = point{
			X: from.X + dx*u + px*off,
			Y: from.Y + dy*u + py*off,
		}
	}
	return ctrls
}

// bezierAt evaluates the Bezier defined by from + ctrls + to at parameter t
// via de Casteljau (works for any control count).
func bezierAt(from point, ctrls []point, to point, t float64) point {
	pts := make([]point, 0, len(ctrls)+2)
	pts = append(pts, from)
	pts = append(pts, ctrls...)
	pts = append(pts, to)
	for len(pts) > 1 {
		next := make([]point, len(pts)-1)
		for i := range next {
			next[i] = point{
				X: pts[i].X + (pts[i+1].X-pts[i].X)*t,
				Y: pts[i].Y + (pts[i+1].Y-pts[i].Y)*t,
			}
		}
		pts = next
	}
	return pts[0]
}

// humanPath plans a curved mouse trajectory from→to: a quadratic/cubic Bezier
// sampled at 15-30 ease-in-out points, each step carrying an equal share of a
// distance-scaled total duration (see pathDuration). The final step lands on
// `to` exactly (float lerp at t=1 can be off by ulps). Pure — all randomness
// comes from rng.
func humanPath(from, to point, rng *rand.Rand) []pathStep {
	n := minPathPoints + rng.Intn(maxPathPoints-minPathPoints+1)
	ctrls := bezierControls(from, to, rng)
	total := pathDuration(math.Hypot(to.X-from.X, to.Y-from.Y), rng)
	per := total / time.Duration(n)
	steps := make([]pathStep, n)
	for i := 0; i < n; i++ {
		t := easeInOut(float64(i+1) / float64(n))
		steps[i] = pathStep{Pos: bezierAt(from, ctrls, to, t), Wait: per}
	}
	steps[n-1].Pos = to
	return steps
}

// clickOffset picks the landing point inside box: uniform within the central
// ±30% region around the center — never the exact center (vanishingly
// unlikely under a continuous distribution), never the edges. Pure.
func clickOffset(box rect, rng *rand.Rand) point {
	return point{
		X: box.X + box.W/2 + (rng.Float64()*2-1)*0.3*box.W,
		Y: box.Y + box.H/2 + (rng.Float64()*2-1)*0.3*box.H,
	}
}

// clampPoint clamps p into r (inclusive edges). Pure.
func clampPoint(p point, r rect) point {
	return point{
		X: math.Min(math.Max(p.X, r.X), r.X+r.W),
		Y: math.Min(math.Max(p.Y, r.Y), r.Y+r.H),
	}
}

// intersectRect returns a∩b; ok=false when the overlap has no area (a click
// can't land there). Pure.
func intersectRect(a, b rect) (rect, bool) {
	x1 := math.Max(a.X, b.X)
	y1 := math.Max(a.Y, b.Y)
	x2 := math.Min(a.X+a.W, b.X+b.W)
	y2 := math.Min(a.Y+a.H, b.Y+b.H)
	if x2 <= x1 || y2 <= y1 {
		return rect{}, false
	}
	return rect{X: x1, Y: y1, W: x2 - x1, H: y2 - y1}, true
}

// largestRect picks the rect with the largest area, skipping zero-area
// entries; ok=false when none has area. Used on getClientRects() output: a
// line-wrapped inline link reports one rect per line fragment, and its
// bounding-box union includes the dead gap between the fragments — the
// biggest fragment is always real clickable surface. Pure.
func largestRect(rects []rect) (rect, bool) {
	var best rect
	found := false
	for _, r := range rects {
		if r.W <= 0 || r.H <= 0 {
			continue
		}
		if !found || r.W*r.H > best.W*best.H {
			best, found = r, true
		}
	}
	return best, found
}

// typeDelays plans the inter-keystroke gaps for n keys: 40-140ms base jitter,
// plus a 150-300ms thinking pause every 10-20 keys. Pure.
func typeDelays(n int, rng *rand.Rand) []time.Duration {
	out := make([]time.Duration, n)
	untilPause := 10 + rng.Intn(11)
	for i := range out {
		d := humanKeyMinDelay + time.Duration(rng.Intn(101))*time.Millisecond
		untilPause--
		if untilPause <= 0 {
			d += time.Duration(150+rng.Intn(151)) * time.Millisecond
			untilPause = 10 + rng.Intn(11)
		}
		out[i] = d
	}
	return out
}

// planHumanType decides whether text fits the per-keystroke path inside the
// action timeout and returns the planned gaps if so. Budget = ~50% of the
// effective timeout (timeoutSecs<=0 mirrors withTimeout's 30s default), so a
// humanized fill can never eat the whole action window and leave a half-typed
// field behind — over-budget texts (long pastes, tight timeouts) keep the
// instant SendKeys path. The n×minimum pre-screen rejects huge pastes without
// allocating their delay slice. Pure.
func planHumanType(text string, timeoutSecs int, rng *rand.Rand) ([]time.Duration, bool) {
	if timeoutSecs <= 0 {
		timeoutSecs = 30
	}
	budget := time.Duration(timeoutSecs) * time.Second / 2
	n := utf8.RuneCountInString(text)
	if time.Duration(n)*humanKeyMinDelay > budget {
		return nil, false
	}
	delays := typeDelays(n, rng)
	var total time.Duration
	for _, d := range delays {
		total += d
	}
	if total > budget {
		return nil, false
	}
	return delays, true
}

// jitterDur returns a uniform duration in [loMs, hiMs] milliseconds.
func jitterDur(loMs, hiMs int, rng *rand.Rand) time.Duration {
	return time.Duration(loMs+rng.Intn(hiMs-loMs+1)) * time.Millisecond
}

// sleepCtx waits d, or returns the ctx error the moment the action context
// is cancelled — the per-action timeout / turn cancel must preempt a
// trajectory mid-flight, not wait it out.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// --- Evaluate expression builders (pure, escaping-testable) ----------------

// jsQuote returns sel as a JS string literal (JSON string syntax is a strict
// subset of JS), so attribute selectors with quotes/backslashes can't break
// out of the expression.
func jsQuote(sel string) string {
	quoted, _ := json.Marshal(sel)
	return string(quoted)
}

// measureExpr builds the single measurement round-trip for humanClick: the
// target's client rects (one per line fragment for wrapped inline elements;
// getBoundingClientRect fallback when the list is empty) plus the viewport
// size, all in viewport coordinates.
func measureExpr(sel string) string {
	return fmt.Sprintf(`(()=>{const el=document.querySelector(%s);`+
		`if(!el)return{found:false};`+
		`let rs=[...el.getClientRects()].map(r=>({x:r.x,y:r.y,w:r.width,h:r.height}));`+
		`if(!rs.length){const r=el.getBoundingClientRect();rs=[{x:r.x,y:r.y,w:r.width,h:r.height}];}`+
		`return{found:true,rects:rs,vw:window.innerWidth,vh:window.innerHeight};})()`,
		jsQuote(sel))
}

// hitTestExpr builds humanClick's pre-press TOCTOU guard: does the element
// under the planned landing point still belong to sel? elementFromPoint also
// subsumes the old path's NodeVisible gate — a visibility:hidden /
// covered-by-overlay element is never the hit. hit.contains(el) covers
// sel being an inner node of the thing painted on top (e.g. clicking a span
// whose button parent owns the pixels — backwards case: el inside hit is
// el===hit's ancestor chain, so the symmetric containment check keeps both
// label-in-button and button-with-icon-children clickable).
func hitTestExpr(sel string, p point) string {
	return fmt.Sprintf(`(()=>{const el=document.querySelector(%s);if(!el)return false;`+
		`const hit=document.elementFromPoint(%g,%g);`+
		`return !!hit&&(el===hit||el.contains(hit)||hit.contains(el));})()`,
		jsQuote(sel), p.X, p.Y)
}

// fileInputProbeExpr builds Type's file-input bypass probe: native file
// inputs must keep the chromedp.SendKeys path — SendKeys special-cases
// INPUT[type=file] into DOM.setFileInputFiles (actually attaching the file),
// while dispatched keystrokes at a file input are a silent no-op that would
// report success without uploading anything. A keystroke cadence is
// meaningless on a native file chooser anyway.
func fileInputProbeExpr(sel string) string {
	return fmt.Sprintf(`(()=>{const el=document.querySelector(%s);`+
		`return !!el&&el.matches('input[type=file]');})()`, jsQuote(sel))
}

// --- chromedp dispatch -----------------------------------------------------

// dispatchMousePath replays a planned trajectory as CDP mouseMoved events
// (buttons mask 0 — nothing held during a move; see screencast.go
// inputAction for why the mask matters on press). One chromedp.Run so the
// whole path shares a single target attach. No sleep after the final move —
// the caller owns the pre-press hover pause.
func dispatchMousePath(ctx context.Context, steps []pathStep) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ictx context.Context) error {
		for i, st := range steps {
			if err := input.DispatchMouseEvent(input.MouseMoved, st.Pos.X, st.Pos.Y).Do(ictx); err != nil {
				return err
			}
			if i == len(steps)-1 {
				break
			}
			if err := sleepCtx(ictx, st.Wait); err != nil {
				return err
			}
		}
		return nil
	}))
}

// dispatchHumanKeys sends text one rune at a time to the focused element
// (caller has already run the Focus/replace tasks) with the caller-planned
// per-key gaps (planHumanType — len(delays) == rune count). No sleep after
// the last key: the field is already complete.
func dispatchHumanKeys(ctx context.Context, text string, delays []time.Duration) error {
	runes := []rune(text)
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ictx context.Context) error {
		for i, r := range runes {
			if err := chromedp.KeyEvent(string(r)).Do(ictx); err != nil {
				return err
			}
			if i == len(runes)-1 {
				break
			}
			if err := sleepCtx(ictx, delays[i]); err != nil {
				return err
			}
		}
		return nil
	}))
}

// isFileInput evaluates fileInputProbeExpr; the caller treats an error as
// "assume the worst" and keeps the SendKeys path (fail-open).
func isFileInput(ctx context.Context, sel string) (bool, error) {
	var is bool
	err := chromedp.Run(ctx, chromedp.Evaluate(fileInputProbeExpr(sel), &is))
	return is, err
}

// elementGeom is measureExpr's payload: the target's client rects plus the
// viewport size (for cursor placement and clamping). Found=false → element
// vanished between resolve and now.
type elementGeom struct {
	Found bool    `json:"found"`
	Rects []rect  `json:"rects"`
	VW    float64 `json:"vw"`
	VH    float64 `json:"vh"`
}

// cursorStart returns the virtual cursor's current viewport position,
// randomizing it within the central viewport on first use (zero value =
// never placed; a real pointer is never parked at the exact origin).
func (s *Session) cursorStart(vw, vh float64, rng *rand.Rand) point {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursorX == 0 && s.cursorY == 0 {
		if vw <= 0 || vh <= 0 {
			vw, vh = 1280, 800
		}
		s.cursorX = vw * (0.2 + 0.6*rng.Float64())
		s.cursorY = vh * (0.2 + 0.6*rng.Float64())
	}
	return point{X: s.cursorX, Y: s.cursorY}
}

// setCursor records where a completed humanized click left the pointer, so
// the next trajectory starts there instead of teleporting.
func (s *Session) setCursor(p point) {
	s.mu.Lock()
	s.cursorX, s.cursorY = p.X, p.Y
	s.mu.Unlock()
}

// recoverPress best-effort releases the left button after a committed press
// whose follow-up failed (press-release sleep preempted by ctx cancel,
// release dispatch error). It runs on a short timeout off
// context.WithoutCancel(tctx) — values (chromedp's target attachment)
// survive, the cancellation that just fired does not — so a turn cancel
// can't strand the page with the button held, which would turn the next
// humanized trajectory into a drag-select. The pointer physically sits at
// `to` either way, so the cursor is recorded there. The caller re-throws its
// original error regardless of how this goes.
func (s *Session) recoverPress(tctx context.Context, to point) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(tctx), 2*time.Second)
	defer cancel()
	_ = chromedp.Run(rctx, input.DispatchMouseEvent(input.MouseReleased, to.X, to.Y).
		WithButton(input.Left).
		WithButtons(0).
		WithClickCount(1))
	s.setCursor(to)
}

// humanClick performs the humanized click on sel: scroll into view → measure
// (client rects + viewport, one Evaluate) → trajectory → hover → hit-test
// (one Evaluate) → press → release.
//
// committed=false means NOTHING with a side effect was dispatched (measure
// failed, element off-viewport, move dispatch failed, landing point no longer
// hits the element — mouse moves carry no click semantics) and the caller
// MUST fall back to the plain chromedp.Click path, preserving today's
// behaviour exactly. Once the press is attempted, committed=true and the
// caller must NOT retry through any path — a chromedp.Click after an
// ambiguous press would double-click. err is only meaningful when committed.
func (s *Session) humanClick(tctx context.Context, sel string) (committed bool, err error) {
	var geom elementGeom
	if err := chromedp.Run(tctx,
		chromedp.ScrollIntoView(sel, chromedp.ByQuery),
		chromedp.Evaluate(measureExpr(sel), &geom),
	); err != nil || !geom.Found {
		return false, nil
	}
	// Largest line fragment, not the union box: a wrapped inline link's union
	// includes the dead gap between its two line fragments.
	box, ok := largestRect(geom.Rects)
	if !ok {
		return false, nil
	}
	vp := rect{X: 0, Y: 0, W: geom.VW, H: geom.VH}
	// Landing must sit inside box∩viewport — coordinates outside the viewport
	// are undispatchable as a click; no overlap → not humanly clickable.
	landZone, ok := intersectRect(box, vp)
	if !ok {
		return false, nil
	}

	rng := newRand()
	from := clampPoint(s.cursorStart(geom.VW, geom.VH, rng), vp)
	to := clampPoint(clickOffset(box, rng), landZone)
	steps := humanPath(from, to, rng)
	for i := range steps {
		steps[i].Pos = clampPoint(steps[i].Pos, vp) // Bezier bow can leave the viewport
	}
	if err := dispatchMousePath(tctx, steps); err != nil {
		return false, nil
	}
	if err := sleepCtx(tctx, jitterDur(30, 80, rng)); err != nil {
		return false, nil
	}

	// TOCTOU guard: the trajectory+hover burned 150-560ms — an SPA re-render
	// or layout shift can move the element off `to`, and DispatchMouseEvent
	// is coordinate-level (never errors on a wrong target), which would both
	// click the wrong thing AND reset the TD6 SPA-loop counter on a "success".
	// elementFromPoint also restores the old path's NodeVisible gate: a
	// hidden/covered element is never the hit. Probe failure or miss →
	// fall back to chromedp.Click, whose error semantics are the old ones.
	var hit bool
	if err := chromedp.Run(tctx, chromedp.Evaluate(hitTestExpr(sel, to), &hit)); err != nil || !hit {
		return false, nil
	}

	// Point of no return: from here a retry risks a double click, so every
	// error surfaces through the normal click-failure path instead.
	press := input.DispatchMouseEvent(input.MousePressed, to.X, to.Y).
		WithButton(input.Left).
		WithButtons(1). // pressed-button bit must be set or headless ignores the press
		WithClickCount(1)
	if err := chromedp.Run(tctx, press); err != nil {
		return true, err
	}
	if err := sleepCtx(tctx, jitterDur(40, 120, rng)); err != nil {
		s.recoverPress(tctx, to)
		return true, err
	}
	release := input.DispatchMouseEvent(input.MouseReleased, to.X, to.Y).
		WithButton(input.Left).
		WithButtons(0).
		WithClickCount(1)
	if err := chromedp.Run(tctx, release); err != nil {
		s.recoverPress(tctx, to)
		return true, err
	}
	s.setCursor(to)
	return true, nil
}
