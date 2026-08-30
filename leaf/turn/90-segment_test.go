package turn

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agentbob/contract"
	"agentbob/leaf/model"
)

// segPool counts compression calls and returns a canned summary.
type segPool struct {
	fakePool
	calls  int
	failAt int    // fail the Nth call (1-based); 0 = never
	reply  string // canned summary; "" → "S"
}

func (p *segPool) Chat(context.Context, contract.ModelRequest, []contract.Message) (contract.ChatResponse, error) {
	p.calls++
	if p.failAt > 0 && p.calls == p.failAt {
		return contract.ChatResponse{}, context.DeadlineExceeded
	}
	if p.reply == "" {
		return contract.ChatResponse{Content: "S"}, nil
	}
	return contract.ChatResponse{Content: p.reply}, nil
}

// TestCompressPass_SmallSingleShot: under the segment cap → exactly one call.
func TestCompressPass_SmallSingleShot(t *testing.T) {
	pool := &segPool{}
	c := &core{pool: pool}
	out := c.compressPass(context.Background(), "user: 短历史\n")
	if out != "S" || pool.calls != 1 {
		t.Fatalf("out=%q calls=%d, want single-shot", out, pool.calls)
	}
}

// TestCompressPass_CutAndMerge: an oversized text is cut by the sliding window,
// each segment compressed serially, then ONE merge call over the join —
// N segments = N+1 calls (你定的两步：切段压完，全部一并再压).
func TestCompressPass_CutAndMerge(t *testing.T) {
	oldCap := compactSegTokens
	compactSegTokens = 50
	defer func() { compactSegTokens = oldCap }()

	text := strings.Repeat("这一轮做了很多事情。\n", 30) // est ~300 tokens
	pool := &segPool{}
	c := &core{pool: pool}
	out := c.compressPass(context.Background(), text)
	if out != "S" {
		t.Fatalf("out=%q", out)
	}
	if pool.calls < 3 { // ≥2 segment calls + 1 merge
		t.Fatalf("calls=%d, want cut(≥2)+merge(1)", pool.calls)
	}
}

// TestCompressPass_FailureAbandons: any failed call abandons the whole pass
// (recoverable) — no partial summary ever ships.
func TestCompressPass_FailureAbandons(t *testing.T) {
	oldCap := compactSegTokens
	compactSegTokens = 50
	defer func() { compactSegTokens = oldCap }()
	pool := &segPool{failAt: 2}
	c := &core{pool: pool}
	if out := c.compressPass(context.Background(), strings.Repeat("这一轮做了很多事情。\n", 30)); out != "" {
		t.Fatalf("out=%q, want abandon", out)
	}
}

// TestCompressPass_OversizedJoinReturnsAsIs: when the joined segment summaries
// are STILL over the window, the pass returns the join without a merge call —
// it lands as the summary row (the work marker) and the next loop-top's pass
// continues from there. No internal loop, no recursion.
func TestCompressPass_OversizedJoinReturnsAsIs(t *testing.T) {
	oldCap := compactSegTokens
	compactSegTokens = 50
	defer func() { compactSegTokens = oldCap }()
	// Each "summary" is itself ~60 est tokens → any join of ≥2 exceeds the cap.
	pool := &segPool{reply: strings.Repeat("要点。", 60)}
	c := &core{pool: pool}
	out := c.compressPass(context.Background(), strings.Repeat("这一轮做了很多事情。\n", 30))
	if out == "" {
		t.Fatal("oversized join must be RETURNED (work marker), not abandoned")
	}
	segs := pool.calls // every call was a segment call — no merge ran
	if strings.Count(out, strings.Repeat("要点。", 60)) != segs {
		t.Fatalf("out must be the plain join of %d segment summaries", segs)
	}
}

// TestCutBySlidingWindow: cuts land on logical boundaries near the cap when one
// exists in the final tenth of the window, hard cut otherwise; every piece is
// valid UTF-8 and the pieces concatenate back to the original.
func TestCutBySlidingWindow(t *testing.T) {
	// Newline-rich CJK text: cuts must land right after a newline. The cap is
	// sized so the 10% lookback zone (capBytes/10) spans at least one full line
	// (25 bytes) — a boundary is then always in reach.
	text := strings.Repeat("第一句话说完了。\n", 40)
	segs := cutBySlidingWindow(text, 320, 100)
	if len(segs) < 2 {
		t.Fatalf("segs=%d, want a real cut", len(segs))
	}
	for i, s := range segs[:len(segs)-1] {
		if !strings.HasSuffix(s, "\n") && !strings.HasSuffix(s, "。") {
			t.Fatalf("seg %d does not end on a logical boundary: ...%q", i, s[len(s)-12:])
		}
	}
	// Boundary-free blob (pure CJK, no separators): hard cut, rune-safe, lossless.
	blob := strings.Repeat("库", 400)
	segs = cutBySlidingWindow(blob, 400, 50)
	joined := ""
	for _, p := range segs {
		if !utf8.ValidString(p) {
			t.Fatal("hard cut split a rune")
		}
		joined += p
	}
	if joined != blob {
		t.Fatal("cut lost or reordered content")
	}
}

// overflow400Pool rejects any compression input over limitBytes with the real
// wrapped context-overflow sentinel — the serving-mouth 400 summarizeSegment
// halves on.
type overflow400Pool struct {
	fakePool
	limitBytes int
	calls      int
}

func (p *overflow400Pool) Chat(_ context.Context, _ contract.ModelRequest, msgs []contract.Message) (contract.ChatResponse, error) {
	p.calls++
	if len(msgs[len(msgs)-1].Content) > p.limitBytes {
		return contract.ChatResponse{}, fmt.Errorf("model pool: %w", model.ErrContextExceeded)
	}
	return contract.ChatResponse{Content: "S"}, nil
}

// TestSummarizeSegment_HalvesOnReal400: a context-overflow reject from the
// serving model — an identified REAL error, never a prediction — licenses one
// halving; a mis-measured cut (estimator undercount, density skew) therefore
// converges instead of 400ing the identical text every loop-top forever.
// Non-overflow failures do NOT split (transient → abandon, retry next loop-top).
func TestSummarizeSegment_HalvesOnReal400(t *testing.T) {
	pool := &overflow400Pool{limitBytes: 300}
	c := &core{pool: pool}
	out := c.summarizeSegment(context.Background(), compactSummarySystem, strings.Repeat("这句话很长。\n", 50)) // ~950B > limit
	if out == "" {
		t.Fatal("real-400 halving must converge to a summary, not abandon")
	}
	if pool.calls < 5 { // 1 overflow + ≥2 second-level (≥1 more overflow) + leaves
		t.Fatalf("calls=%d, want the halving cascade", pool.calls)
	}
	// A transient (non-400) failure must NOT trigger splitting.
	tr := &segPool{failAt: 1}
	c2 := &core{pool: tr}
	if out := c2.summarizeSegment(context.Background(), compactSummarySystem, "短"); out != "" || tr.calls != 1 {
		t.Fatalf("transient failure must abandon without splitting: out=%q calls=%d", out, tr.calls)
	}
}

// windowPool exposes WindowFor like the real MultiPool (optional interface).
type windowPool struct {
	segPool
	window int
}

func (p *windowPool) WindowFor(contract.ModelRequest) int { return p.window }

// TestCompressSegCap: window-driven sizing (declared window × 0.8 − overhead);
// no window → the compactSegTokens fallback; tiny window → the 2000 floor.
func TestCompressSegCap(t *testing.T) {
	c := &core{pool: &windowPool{window: 32768}}
	if got := c.compressSegCap(); got != 24214 {
		t.Fatalf("cap=%d, want window-derived %d", got, 24214)
	}
	// Fallback path: plain pool (no WindowFor).
	c2 := &core{pool: &segPool{}}
	if got := c2.compressSegCap(); got != compactSegTokens {
		t.Fatalf("fallback cap=%d, want compactSegTokens", got)
	}
	// Floor: absurd small window.
	c3 := &core{pool: &windowPool{window: 3000}}
	if got := c3.compressSegCap(); got != 2000 {
		t.Fatalf("floored cap=%d, want 2000", got)
	}
}

// countPool exposes CountTokens like the real MultiPool (optional tokenCounter
// seam): 1 token per rune, so tests reason in rune counts.
type countPool struct {
	segPool
	countCalls int
	broken     bool // true → CountTokens fails (tokenizer outage)
}

func (p *countPool) CountTokens(_ context.Context, _ contract.ModelRequest, text string) (int, bool) {
	p.countCalls++
	if p.broken {
		return 0, false
	}
	return utf8.RuneCountInString(text), true
}

// TestWeigh_DelegateAndBreaker: exact readings come from the pool tokenizer
// (caching is the POOL's job — leaf/model CountTokens keys by serving model);
// a failed tokenize trips the turn-side breaker so subsequent weighs skip the
// pool entirely until the cooldown lapses, then the tokenizer is retried.
func TestWeigh_DelegateAndBreaker(t *testing.T) {
	pool := &countPool{}
	c := &core{pool: pool}
	req := contract.ModelRequest{Requires: []string{"smart"}}
	if got := c.weigh(context.Background(), req, "库库库库"); got != 4 {
		t.Fatalf("weigh=%d, want tokenizer 4", got)
	}
	// Outage: estimator fallback (CJK = 1/rune here too) + breaker latches.
	pool.broken = true
	if got := c.weigh(context.Background(), req, "新文本"); got != 3 {
		t.Fatalf("fallback weigh=%d, want estimator 3", got)
	}
	before := pool.countCalls
	if got := c.weigh(context.Background(), req, "另一段"); got != 3 || pool.countCalls != before {
		t.Fatalf("latched weigh=%d calls=%+d, want estimator with ZERO pool calls while the breaker is open", got, pool.countCalls-before)
	}
	// Cooldown lapse → tokenizer retried.
	pool.broken = false
	c.weighMu.Lock()
	c.weighDownUntil = time.Time{}
	c.weighMu.Unlock()
	if got := c.weigh(context.Background(), req, "另一段"); got != 3 || pool.countCalls != before+1 {
		t.Fatalf("recovery weigh=%d calls=%+d, want tokenizer retry after cooldown", got, pool.countCalls-before)
	}
}
