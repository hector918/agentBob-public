package turn

// 称重制 prompt sizing (replaces the est→real calibration ruler):
// every compaction sizing decision reads EXACT token counts from the backend's
// own tokenizer instead of estimating and correcting. The pool owns the cache
// (it alone knows WHICH tokenizer measured a text — leaf/model CountTokens);
// the turn owns the degrade posture: heartwood/prompt EstTokens remains solely
// as the fallback for pools without a tokenize path (remote-only deployments,
// test fakes) or during a tokenizer outage — one fallback, not a parallel
// ruler. A failed tokenize latches a short breaker so a hung endpoint costs
// one timeout per window, not one per text per loop-top.

import (
	"context"
	"log/slog"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/prompt"
)

// tokenCounter is the pool's optional exact-tokenizer seam — a soft assertion
// so test fakes and remote-only pools degrade to the estimator instead of
// breaking.
type tokenCounter interface {
	CountTokens(ctx context.Context, req contract.ModelRequest, text string) (int, bool)
}

// windowSizer is the pool's optional sizing seam: WindowFor returns the
// declared context window of the entry that would serve req right now (the
// window-blind picker's winner — routing never looks at windows, so sizing
// asks "how big is the winner's mouth" and the turn compacts to exactly
// that). 0 / absent seam → the turn skips proactive compaction (native
// long-context posture) and relies on the reactive 400 net.
type windowSizer interface {
	WindowFor(req contract.ModelRequest) int
}

// budgetFor resolves the input budget for a request — the winner's declared
// window, 0 when the pool can't say.
func (c *core) budgetFor(req contract.ModelRequest) int {
	if ws, ok := c.pool.(windowSizer); ok {
		return ws.WindowFor(req)
	}
	return 0
}

// sizingReq is the request every SIZING question (budget, weigh scale, split
// walk) is asked with: spec.ModelReq plus the toolcall preference that
// routeToolRound joins onto every tool round — sizing and serving must rank
// the same winner, or the loop-top budget describes a mouth that never eats
// (review: a [smart]-sized 131K budget while a [smart,toolcall]
// entry served 32K → 400 every tool round). The failure-escalation smart tag
// is per-round state the loop-top can't see; the ambient Prefer already
// carries "smart", so that residual drift window is narrow.
func sizingReq(spec contract.TurnSpec) contract.ModelRequest {
	req := spec.ModelReq
	if spec.Tools != nil {
		req.Prefer = appendPrefer(req.Prefer, "toolcall")
	}
	return req
}

// weighBreakerCooldown is how long the estimator serves after a tokenize
// failure before the tokenizer is probed again. Short: an outage should not
// linger past the backend's recovery, and one probe per window is cheap.
const weighBreakerCooldown = 60 * time.Second

// weigh returns the token count of text on the scale of req's tag class:
// exact (backend tokenizer, pool-cached per serving model) when available,
// estimator otherwise. The known estimator pathology — a 2.6-4x undercount on
// CJK/high-entropy content — therefore exists ONLY while the breaker is open
// or the pool has no tokenize path; the pool's context-400 net (forceCompact)
// backstops whatever the degraded readings let through.
func (c *core) weigh(ctx context.Context, req contract.ModelRequest, text string) int {
	if text == "" {
		return 0
	}
	if tc, ok := c.pool.(tokenCounter); ok && c.weighBreakerClosed() {
		if n, counted := tc.CountTokens(ctx, req, text); counted {
			return n
		}
		c.tripWeighBreaker()
	}
	return prompt.EstTokens(text)
}

// weighBreakerClosed reports whether the tokenize path may be tried.
func (c *core) weighBreakerClosed() bool {
	c.weighMu.Lock()
	defer c.weighMu.Unlock()
	return time.Now().After(c.weighDownUntil)
}

// tripWeighBreaker latches the estimator fallback for weighBreakerCooldown,
// logging ONCE per trip — sizing silently degrading is the one thing this
// path must never do quietly.
func (c *core) tripWeighBreaker() {
	c.weighMu.Lock()
	defer c.weighMu.Unlock()
	if !time.Now().After(c.weighDownUntil) {
		return // already latched by a concurrent weigh
	}
	c.weighDownUntil = time.Now().Add(weighBreakerCooldown)
	slog.Warn("turn: tokenize unavailable — prompt sizing falls back to the estimator",
		"cooldown", weighBreakerCooldown)
}

// weighMsg weighs one history row: content plus each assistant row's tool-call
// arguments — the same per-message accounting as prompt.EstTokensMsg, so the
// trigger, the split walk and the segment packer can never drift apart.
func (c *core) weighMsg(ctx context.Context, req contract.ModelRequest, m contract.Message) int {
	n := c.weigh(ctx, req, m.Content)
	for _, tc := range m.ToolCalls {
		n += c.weigh(ctx, req, tc.Arguments)
	}
	return n
}

// weighMsgs sums weighMsg over msgs.
func (c *core) weighMsgs(ctx context.Context, req contract.ModelRequest, msgs []contract.Message) int {
	total := 0
	for _, m := range msgs {
		total += c.weighMsg(ctx, req, m)
	}
	return total
}

// weighToolSpecs weighs the advertised tool specs (name + description +
// parameter schema) — request-side tokens the history walk can't see. Specs
// are stable across rounds, so after the first loop-top these are all cache
// hits pool-side.
func (c *core) weighToolSpecs(ctx context.Context, req contract.ModelRequest, specs []contract.ToolSpec) int {
	total := 0
	for _, sp := range specs {
		total += c.weigh(ctx, req, sp.Name+"\n"+sp.Description+"\n"+string(sp.Parameters))
	}
	return total
}
