package model

import "time"

// Soft prompt-cache affinity (hector). The pool remembers, per
// AffinityKey (the turn stamps its session id), which entry last served it.
// Within the TTL that record acts BOTH ways in pickReady's tie-break: the key's
// own requests prefer their remembered entry (its KV cache is warm there), and
// everyone else's requests are steered AWAY from it (a stranger landing on that
// backend could evict the cache slot). Both directions sit BELOW the in-flight
// load comparison, so a genuinely busier backend is never held — "about to be
// full" beats the soft hold, per the design discussion.

const (
	// affinityTTL is how long after its last successful serve a key's record
	// keeps both effects (sliding — refreshed on every serve). It models "the
	// backend's KV cache is probably still warm and worth protecting"; past it
	// the record is dropped entirely (stale stickiness is worthless and a stale
	// hold would repel traffic from a backend whose cache is long gone).
	affinityTTL = 20 * time.Minute

	// affinityMaxKeys caps the ledger (hygiene backstop; expired records are
	// already pruned lazily on keyed reads). At the cap the ledger is wiped
	// whole — losing soft hints that re-book on the next serve, never
	// correctness.
	affinityMaxKeys = 1024
)

// affinityRec is one ledger row: the entry that last served a key, and when.
type affinityRec struct {
	entry    string
	lastUsed time.Time
}

// affinityFor resolves the pick-time view for one request: the requesting
// key's own remembered entry ("" when none / expired / no key), and how many
// OTHER live keys currently sit on each entry (nil when none — a nil map reads
// as 0 everywhere, so callers index it unconditionally). Expired records are
// pruned in passing.
//
// A KEYLESS request gets NO view at all — neither stickiness nor the foreign
// steering. Keyless callers are a conversation's side-LLM calls (salvage,
// compression, judge) whose prompts share no token prefix with any ledgered
// conversation: there is no cache win to steer toward, and counting the
// requesting session's own record as "foreign" would actively repel e.g. its
// salvage call off the very entry its history was sized for (a smaller tied
// peer then 400s, and salvage has no retry net — review). Neutral
// (load → Name) is the honest rank for them; it also makes keyless picks free
// (no lock, no scan).
func (p *MultiPool) affinityFor(key string) (own string, reserved map[string]int) {
	if key == "" {
		return "", nil
	}
	p.affinityMu.Lock()
	defer p.affinityMu.Unlock()
	if len(p.affinity) == 0 {
		return "", nil
	}
	// time.Now, NOT clock.Now: this is operational in-memory state (like the
	// liveness window / deadUntil), not a persisted timestamp — the pool's
	// documented time-source split (10-multipool.go recordError). It also keeps
	// the monotonic reading, so a wall-clock step can't stretch the TTL.
	cutoff := time.Now().Add(-affinityTTL)
	for k, rec := range p.affinity {
		if rec.lastUsed.Before(cutoff) {
			delete(p.affinity, k)
			continue
		}
		if k == key {
			own = rec.entry
			continue
		}
		if reserved == nil {
			reserved = make(map[string]int)
		}
		reserved[rec.entry]++
	}
	return own, reserved
}

// recordAffinity books a successful serve: key → entry, stamped now (sliding
// TTL). No-op for a keyless request. At the cap the whole ledger is wiped
// (tokCache's hygiene idiom, 42-count-tokens.go): affinity is a soft hint that
// re-books on the next serve, so losing it all momentarily costs nothing —
// the TTL prune in affinityFor makes the cap near-unreachable anyway.
func (p *MultiPool) recordAffinity(key, entry string) {
	if key == "" {
		return
	}
	p.affinityMu.Lock()
	defer p.affinityMu.Unlock()
	if len(p.affinity) >= affinityMaxKeys {
		p.affinity = nil
	}
	if p.affinity == nil {
		p.affinity = make(map[string]affinityRec)
	}
	p.affinity[key] = affinityRec{entry: entry, lastUsed: time.Now()}
}
