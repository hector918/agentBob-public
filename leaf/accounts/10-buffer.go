package accounts

import (
	"context"
	"log/slog"
	"sync"

	"agentbob/leaf/accounts/store"
)

// handleUsageBuffer is the in-memory accumulator for per-handle turn usage.
// Instead of a per-turn read-modify-write of the store's usage_json blob, the
// flow folds each turn's cost into a mutex-guarded map keyed by (source,
// platformUID) and a periodic Flush drains the whole map into store.AddTurnUsage
// — one store write per distinct handle per flush window, regardless of how many
// turns that handle ran.
//
// Best-effort: a crash loses the un-flushed window's usage. Add never
// blocks/fails a turn; a per-handle store error during Flush is logged WARN and
// the entry is re-staged so the next flush retries it (usage is not dropped while
// the process lives). Memory: a successful Flush drains the map to zero, so it
// normally holds only the handles active since the last flush. CAVEAT: this bound
// holds only WHILE flushes succeed — a prolonged store outage re-stages every entry
// each cycle with no cap, so the map grows with the number of distinct handles seen
// during the outage. Acceptable as best-effort today; add a cap/eviction if an
// unbounded-during-outage growth ever matters (owner call, not yet wired).
type handleUsageBuffer struct {
	mu      sync.Mutex
	pending map[handleKey]*pendingUsage
	store   store.Store // nil → Flush is a silent no-op

	// flushMu serializes Flush bodies. Snapshot-and-clear makes the MAP race-safe
	// but NOT the per-handle store read-modify-write; Flush is reachable from the
	// housekeeping tick AND shutdown, so without this two flushes could
	// SELECT→bump→UPSERT the same row and lose a delta. Held across the whole
	// Flush; Add never takes it, so a turn is never blocked by a flush.
	flushMu sync.Mutex
}

type handleKey struct {
	source string
	uid    string
}

// pendingUsage is one handle's accumulated, not-yet-flushed usage. Mirrors the
// AddTurnUsage argument shape so Flush forwards it verbatim.
type pendingUsage struct {
	turns   int64
	success int64
	tokens  map[string]store.KindTokens // per-kind (llm/ocr/translate) → {In,Out}
	native  map[string]int64            // per "kind:unit" (e.g. "asr:s") → amount
}

// newHandleUsageBuffer builds an empty buffer over s. A nil store makes Flush a
// no-op (test / pre-wire paths).
func newHandleUsageBuffer(s store.Store) *handleUsageBuffer {
	return &handleUsageBuffer{pending: map[handleKey]*pendingUsage{}, store: s}
}

// Add folds one turn's usage into the (source, uid) entry. Concurrency-safe:
// turns run concurrently across sessions, all folding into the shared map under
// the mutex. nil-safe and a no-op for an empty handle.
func (b *handleUsageBuffer) Add(source, uid string, turns, success int64, tokens map[string]store.KindTokens, native map[string]int64) {
	if b == nil || source == "" || uid == "" {
		return
	}
	k := handleKey{source: source, uid: uid}
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.pending[k]
	if p == nil {
		p = &pendingUsage{tokens: map[string]store.KindTokens{}, native: map[string]int64{}}
		b.pending[k] = p
	}
	p.turns += turns
	p.success += success
	for kind, t := range tokens {
		cur := p.tokens[kind]
		cur.In += t.In
		cur.Out += t.Out
		p.tokens[kind] = cur
	}
	for nk, n := range native {
		p.native[nk] += n
	}
}

// Flush drains every accumulated entry into the store via AddTurnUsage and clears
// the map. A per-handle failure is logged WARN and the entry re-staged for the
// next flush. No-op when no store is wired or nothing is pending. Best-effort:
// never returns an error.
func (b *handleUsageBuffer) Flush(ctx context.Context) {
	if b == nil {
		return
	}
	b.flushMu.Lock()
	defer b.flushMu.Unlock()
	b.mu.Lock()
	st := b.store
	if st == nil || len(b.pending) == 0 {
		b.mu.Unlock()
		return
	}
	drained := b.pending
	b.pending = map[handleKey]*pendingUsage{}
	b.mu.Unlock()

	for k, p := range drained {
		if err := st.AddTurnUsage(ctx, k.source, k.uid, p.turns, p.success, p.tokens, p.native); err != nil {
			slog.Warn("accounts: handle usage flush failed — re-staging for next flush",
				"source", k.source, "uid", k.uid, "err", err)
			b.restage(k, p)
		}
	}
}

// restage merges a failed-flush entry back into the live map so the next flush
// retries it. A concurrent Add may have started a fresh entry for the same
// handle, so merge rather than overwrite.
func (b *handleUsageBuffer) restage(k handleKey, p *pendingUsage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.pending[k]
	if cur == nil {
		b.pending[k] = p
		return
	}
	cur.turns += p.turns
	cur.success += p.success
	for kind, t := range p.tokens {
		c := cur.tokens[kind]
		c.In += t.In
		c.Out += t.Out
		cur.tokens[kind] = c
	}
	for nk, n := range p.native {
		cur.native[nk] += n
	}
}
