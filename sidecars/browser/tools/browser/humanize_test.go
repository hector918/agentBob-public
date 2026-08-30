package browser

// Unit tests for the pure humanize planners (humanize.go). The chromedp
// dispatch helpers need a live chromium and are exercised by the existing
// manual/T3 flows, not here.

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestHumanPathEndpointsAndBounds(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cases := []struct{ from, to point }{
		{point{X: 10, Y: 10}, point{X: 60, Y: 40}},       // short hop
		{point{X: 0, Y: 0}, point{X: 1200, Y: 760}},      // cross-screen
		{point{X: 640, Y: 400}, point{X: 640.5, Y: 401}}, // near-degenerate
	}
	for _, c := range cases {
		steps := humanPath(c.from, c.to, rng)
		if len(steps) < minPathPoints || len(steps) > maxPathPoints {
			t.Fatalf("path %v→%v: %d points, want [%d,%d]", c.from, c.to, len(steps), minPathPoints, maxPathPoints)
		}
		last := steps[len(steps)-1].Pos
		if last != c.to {
			t.Fatalf("path %v→%v: final point %v, want exact target", c.from, c.to, last)
		}
		var total time.Duration
		for _, st := range steps {
			if st.Wait <= 0 {
				t.Fatalf("path %v→%v: non-positive step wait %v", c.from, c.to, st.Wait)
			}
			total += st.Wait
		}
		// pathDuration bounds: base 150-400ms scaled by ±20% jitter →
		// [120ms, 480ms]. Integer division per step can only shave time off.
		if total < 100*time.Millisecond || total > 480*time.Millisecond {
			t.Fatalf("path %v→%v: total duration %v outside [100ms,480ms]", c.from, c.to, total)
		}
	}
}

func TestHumanPathDurationScalesWithDistance(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	sum := func(steps []pathStep) time.Duration {
		var d time.Duration
		for _, st := range steps {
			d += st.Wait
		}
		return d
	}
	// Average over runs so the ±20% jitter can't invert the comparison.
	var short, long time.Duration
	for i := 0; i < 50; i++ {
		short += sum(humanPath(point{X: 0, Y: 0}, point{X: 30, Y: 0}, rng))
		long += sum(humanPath(point{X: 0, Y: 0}, point{X: 1500, Y: 0}, rng))
	}
	if long <= short {
		t.Fatalf("cross-screen avg total (%v) should exceed short-hop avg total (%v)", long/50, short/50)
	}
}

func TestHumanPathRandomness(t *testing.T) {
	from, to := point{X: 0, Y: 0}, point{X: 500, Y: 300}
	a := humanPath(from, to, rand.New(rand.NewSource(3)))
	b := humanPath(from, to, rand.New(rand.NewSource(4)))
	same := len(a) == len(b)
	if same {
		for i := range a {
			if a[i].Pos != b[i].Pos {
				same = false
				break
			}
		}
	}
	if same {
		t.Fatal("two independently seeded paths are identical — no randomness")
	}
}

func TestEaseInOutMonotonic(t *testing.T) {
	prev := easeInOut(0)
	if prev != 0 {
		t.Fatalf("easeInOut(0) = %v, want 0", prev)
	}
	for i := 1; i <= 100; i++ {
		v := easeInOut(float64(i) / 100)
		if v <= prev {
			t.Fatalf("easeInOut not strictly increasing at t=%v: %v <= %v", float64(i)/100, v, prev)
		}
		prev = v
	}
	if math.Abs(prev-1) > 1e-12 {
		t.Fatalf("easeInOut(1) = %v, want 1", prev)
	}
}

func TestClickOffsetWithinCentralRegion(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	box := rect{X: 100, Y: 200, W: 80, H: 30}
	cx, cy := box.X+box.W/2, box.Y+box.H/2
	first := clickOffset(box, rng)
	varied := false
	for i := 0; i < 1000; i++ {
		p := clickOffset(box, rng)
		if math.Abs(p.X-cx) > 0.3*box.W || math.Abs(p.Y-cy) > 0.3*box.H {
			t.Fatalf("offset %v outside central ±30%% region of %v", p, box)
		}
		if p != first {
			varied = true
		}
	}
	if !varied {
		t.Fatal("clickOffset returned the same point 1000 times — no randomness")
	}
}

func TestTypeDelaysBoundsAndPauses(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	const n = 100
	delays := typeDelays(n, rng)
	if len(delays) != n {
		t.Fatalf("got %d delays, want %d", len(delays), n)
	}
	pauses := 0
	for i, d := range delays {
		// base 40-140ms, plus an optional 150-300ms pause → max 440ms.
		if d < 40*time.Millisecond || d > 440*time.Millisecond {
			t.Fatalf("delay[%d] = %v outside [40ms,440ms]", i, d)
		}
		if d > 140*time.Millisecond {
			pauses++
		}
	}
	// A pause lands every 10-20 keys; 100 keys must include several.
	if pauses < 2 {
		t.Fatalf("expected periodic thinking pauses over %d keys, saw %d", n, pauses)
	}
}

// --- ③ Type timeout budget --------------------------------------------------

func TestPlanHumanTypeBudget(t *testing.T) {
	short := strings.Repeat("a", 20)
	long := strings.Repeat("a", 500) // ≥500×40ms = 20s minimum > 15s budget

	delays, ok := planHumanType(short, 30, rand.New(rand.NewSource(7)))
	if !ok {
		t.Fatal("20 runes under a 30s timeout: want humanized path")
	}
	if len(delays) != 20 {
		t.Fatalf("want one delay per rune (20), got %d", len(delays))
	}
	var total time.Duration
	for _, d := range delays {
		total += d
	}
	if total > 15*time.Second {
		t.Fatalf("accepted plan exceeds the 50%% budget: %v > 15s", total)
	}

	if _, ok := planHumanType(long, 30, rand.New(rand.NewSource(8))); ok {
		t.Fatal("500 runes under a 30s timeout: must fall back to SendKeys")
	}
	// A tight configured timeout shrinks the budget below even modest texts:
	// 100 runes ≥ 4s minimum > 2.5s budget.
	if _, ok := planHumanType(strings.Repeat("a", 100), 5, rand.New(rand.NewSource(9))); ok {
		t.Fatal("100 runes under a 5s timeout: must fall back to SendKeys")
	}
	// timeoutSecs<=0 mirrors withTimeout's 30s default rather than a 0 budget.
	if _, ok := planHumanType(short, 0, rand.New(rand.NewSource(10))); !ok {
		t.Fatal("timeoutSecs=0 must budget against the 30s default, not reject everything")
	}
}

func TestPlanHumanTypeCountsRunesNotBytes(t *testing.T) {
	// 200×"测" = 600 bytes but 200 runes; min 8s ≤ 15s budget, and a byte
	// count (600×40ms=24s pre-screen) would wrongly reject it outright.
	text := strings.Repeat("测", 200)
	// Average gap ~96ms ⇒ ~19s expected total for 200 keys, which usually
	// exceeds the 15s budget — only assert the delays length when accepted.
	if delays, ok := planHumanType(text, 60, rand.New(rand.NewSource(11))); !ok {
		t.Fatal("200 CJK runes under a 60s timeout: want humanized path")
	} else if len(delays) != 200 {
		t.Fatalf("want 200 delays (rune count), got %d (byte count is 600)", len(delays))
	}
}

// --- ① file-input bypass probe ----------------------------------------------

func TestFileInputProbeExprEscaping(t *testing.T) {
	expr := fileInputProbeExpr(`input[name="up\load"]`)
	if !strings.Contains(expr, `document.querySelector("input[name=\"up\\load\"]")`) {
		t.Fatalf("selector not JSON-escaped into the expression:\n%s", expr)
	}
	if !strings.Contains(expr, `el.matches('input[type=file]')`) {
		t.Fatalf("expression must test input[type=file]:\n%s", expr)
	}
}

// --- ②⑥⑦ geometry helpers ---------------------------------------------------

func TestClampPoint(t *testing.T) {
	vp := rect{X: 0, Y: 0, W: 1280, H: 800}
	cases := []struct{ in, want point }{
		{point{X: 640, Y: 400}, point{X: 640, Y: 400}},   // inside untouched
		{point{X: -33, Y: 400}, point{X: 0, Y: 400}},     // left
		{point{X: 1500, Y: -9}, point{X: 1280, Y: 0}},    // right+top
		{point{X: 640, Y: 900}, point{X: 640, Y: 800}},   // bottom
		{point{X: 1280, Y: 800}, point{X: 1280, Y: 800}}, // edge stays
	}
	for _, c := range cases {
		if got := clampPoint(c.in, vp); got != c.want {
			t.Fatalf("clampPoint(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIntersectRect(t *testing.T) {
	vp := rect{X: 0, Y: 0, W: 1280, H: 800}
	// Partially off-viewport box → the visible sliver.
	got, ok := intersectRect(rect{X: -50, Y: 750, W: 100, H: 100}, vp)
	if !ok {
		t.Fatal("overlapping rects: want ok")
	}
	if got != (rect{X: 0, Y: 750, W: 50, H: 50}) {
		t.Fatalf("intersection = %+v, want {0 750 50 50}", got)
	}
	// Fully off-viewport → empty.
	if _, ok := intersectRect(rect{X: 1300, Y: 0, W: 40, H: 40}, vp); ok {
		t.Fatal("disjoint rects: want !ok")
	}
	// Touching edge (zero width overlap) → empty: no area to click.
	if _, ok := intersectRect(rect{X: 1280, Y: 0, W: 40, H: 40}, vp); ok {
		t.Fatal("edge-touching rects: want !ok (zero-area overlap)")
	}
}

func TestLargestRect(t *testing.T) {
	// Wrapped-link shape: two line fragments, second one bigger.
	frag1 := rect{X: 600, Y: 100, W: 80, H: 18}
	frag2 := rect{X: 20, Y: 120, W: 300, H: 18}
	got, ok := largestRect([]rect{frag1, frag2})
	if !ok || got != frag2 {
		t.Fatalf("largestRect = %+v ok=%v, want bigger fragment %+v", got, ok, frag2)
	}
	// Zero-area entries are skipped, not chosen.
	got, ok = largestRect([]rect{{X: 1, Y: 1, W: 0, H: 50}, frag1})
	if !ok || got != frag1 {
		t.Fatalf("largestRect with degenerate entry = %+v ok=%v, want %+v", got, ok, frag1)
	}
	// Nothing with area → !ok (caller falls back to plain click).
	if _, ok := largestRect([]rect{{W: 0, H: 0}}); ok {
		t.Fatal("all-degenerate rects: want !ok")
	}
	if _, ok := largestRect(nil); ok {
		t.Fatal("no rects: want !ok")
	}
}

func TestHitTestExprEscapingAndPoint(t *testing.T) {
	expr := hitTestExpr(`a[href="x"]`, point{X: 12.5, Y: 700})
	if !strings.Contains(expr, `document.querySelector("a[href=\"x\"]")`) {
		t.Fatalf("selector not JSON-escaped:\n%s", expr)
	}
	if !strings.Contains(expr, `document.elementFromPoint(12.5,700)`) {
		t.Fatalf("landing point not embedded:\n%s", expr)
	}
}
