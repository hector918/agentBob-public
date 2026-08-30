package model

import (
	"context"
	"hash/fnv"
	"log/slog"

	"agentbob/contract"
	"agentbob/leaf/model/providers"
)

// CountTokens returns the exact token count of text as measured by the
// tokenizer of the entry a Chat with req would currently route to (pick's
// choice — same kind/tag/priority order, no size filter since text IS the
// thing being sized). ok=false when nothing is pickable or the picked
// backend has no tokenize path / the call failed — the caller (the turn's
// 称重 path) falls back to its estimator.
//
// Readings are cached HERE, keyed by (entry name, model, content hash) —
// the pool is the one place that knows WHICH tokenizer measured a text, so
// the cache can never serve a count across tokenizers: a pinned/preferred
// pick, a tag-fallback hop (compress→smart) and a /model reload that swaps
// an entry's backing model all land on their own cache lines (a swapped
// model changes the key, so stale counts die with the swap).
//
// This is the pool half of the weigh-don't-guess sizing model: window truth
// comes from models.yaml (buildEntry), token truth comes from the serving
// tokenizer, and the turn only does arithmetic on the two.
func (p *MultiPool) CountTokens(ctx context.Context, req contract.ModelRequest, text string) (int, bool) {
	// STATIC scale choice (pickStatic): the first tokenize-CAPABLE entry in
	// the request's configured preference order. Static on purpose — a live
	// pick's health/load ranking would flap the scale between siblings and
	// re-key the whole cache (mass re-tokenize), and its pinned branch ignores
	// excludes (an incapable pinned entry would loop). Capability is filtered
	// INSIDE the ranking — "no tokenize endpoint" is a structural fact, not an
	// outage (实弹: a priority-9 openrouter entry won the pick and
	// tripped the caller's breaker while two llama-cpp scales sat idle). nil →
	// nothing capable is configured; the caller falls back to its estimator.
	row := p.pickStatic(req, func(r *entryRow) bool { return providers.CanTokenize(r.info.Provider) })
	if row == nil {
		return 0, false
	}
	key := tokenizeKey(row.info.Name, row.info.Model, text)
	p.tokCacheMu.Lock()
	n, hit := p.tokCache[key]
	p.tokCacheMu.Unlock()
	if hit {
		return n, true
	}
	n, terr := providers.CountTokens(ctx, row.info.Provider, row.baseURL, row.apiKey, row.info.Model, text)
	if terr != nil {
		// Failures are never cached — a transient outage must not freeze
		// estimator-era gaps. Name the backend loudly: the caller's breaker
		// warn is anonymous, and "which tokenizer failed" is the whole
		// diagnosis (: a slow llama-swap proxy tripped the breaker
		// and nothing in the logs said who).
		slog.Warn("model pool: tokenize failed — caller falls back to its estimator",
			"entry", row.info.Name, "provider", row.info.Provider, "text_bytes", len(text), "err", terr)
		return 0, false
	}
	p.tokCacheMu.Lock()
	if p.tokCache == nil {
		p.tokCache = make(map[uint64]int)
	}
	if len(p.tokCache) >= tokenizeCacheCap {
		p.tokCache = make(map[uint64]int) // hygiene bound — texts re-weigh lazily
	}
	p.tokCache[key] = n
	p.tokCacheMu.Unlock()
	return n, true
}

// tokenizeCacheCap bounds the process-lifetime tokenize cache. A HYGIENE
// bound, not a correctness one: on overflow the map is cleared and callers
// re-weigh lazily (history rows are immutable, so each costs one round trip).
const tokenizeCacheCap = 65536

// tokenizeKey hashes (entry, model, text) with FNV-64a — content-addressed
// per tokenizer, so identical texts share one line per serving model and a
// model swap naturally invalidates.
func tokenizeKey(entry, model, text string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(entry))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return h.Sum64()
}
