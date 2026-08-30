package model

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"agentbob/contract"
)

// FallbackRule is the model package's view of one tag-fallback rule (the config
// layer's config.FallbackRule, translated at the pipeline-startup seam — the
// model package imports no config, mirroring Entry). When a request requires
// ALL of Tags but no eligible entry has them, the pool retries requiring To
// instead, preserving the request's other required tags. See fallbackChain.
type FallbackRule struct {
	Tags []string
	To   []string
}

// maxFallbackHops caps how many tag-fallback rules a single pick may chain
// through (rule A→B→C). A backstop in addition to the cycle guard — a
// misconfigured ruleset can't make a pick walk an unbounded chain.
const maxFallbackHops = 8

// noModelConfiguredErr is the canonical internal error string the
// "no models configured" surfaces with. It is matched by i18n.
// ClassifyError ("no model configured" substring) so the user-facing
// reply renders in the active language. Keep the wording stable —
// changing it breaks that substring match.
const noModelConfiguredErr = "no model configured — add an entry to $BOB_HOME/models.yaml (run `bob model add`, or `bob model presets` to see provider defaults)"

// pick chooses an entry for req. PinnedEntry takes precedence; otherwise we
// filter by kind AND Requires AND eligibility (skips paused / dead+cooling),
// then rank by Prefer hits (desc) → Priority (desc) → in-flight load (asc) →
// cache affinity (own remembered entry first, then away from entries other
// live AffinityKeys occupy — 31-affinity.go) → Name (asc).
//
// pick is WINDOW-BLIND by design (hector): eligibility is tags +
// enabled + health, ranking is Prefer → priority → load. Context capacity is
// NOT a routing concern — the turn sizes its prompt to the chosen entry's
// declared window (WindowFor), and an overflow 400 comes straight back as
// ErrContextExceeded for the turn to compact and retry on the same winner.
//
// `tried` names entries already attempted (and failed) for THIS request —
// they are skipped so Chat / ChatStreamWatch can fail over to the next-best
// entry. Empty / nil on the first pick. PinnedEntry that points at a
// dead/paused entry returns an error rather than silently picking another
// one — the caller asked for it specifically.
func (p *MultiPool) pick(req contract.ModelRequest, tried map[string]bool) (*entryRow, error) {
	// Throttled models.yaml mtime check — kicks off an async Reload
	// when the file changed. This request still uses the current state
	// below.
	p.maybeReload()
	st := p.state.Load()
	if st == nil || len(st.entries) == 0 {
		// The empty-boot pool (models.yaml missing/empty/broken): surface the
		// canonical configured-nothing message — i18n classifies it — rather
		// than a generic no-model-available failover error.
		return nil, errors.New(noModelConfiguredErr)
	}
	if req.PinnedEntry != "" {
		row, ok := st.byName[req.PinnedEntry]
		if !ok {
			return nil, fmt.Errorf("no such pool entry: %s: %w", req.PinnedEntry, ErrNoModelAvailable)
		}
		// A disabled entry (priority < minEnabledPriority) is never routable —
		// IsEligible only tests pause/cooldown, so without this a disabled entry
		// would still be pinnable, contradicting the pool docs (B9). Kind
		// bypass stays as pin semantics.
		if row.info.Priority < minEnabledPriority {
			return nil, fmt.Errorf("pinned pool entry %q is disabled: %w", req.PinnedEntry, ErrNoModelAvailable)
		}
		if !row.IsEligible() {
			return nil, fmt.Errorf("pinned pool entry %q is not currently available (paused or in cooldown): %w", req.PinnedEntry, ErrNoModelAvailable)
		}
		return row, nil
	}
	wantKind := req.Kind
	if wantKind == "" {
		wantKind = contract.KindLLM
	}
	// Try the primary required-tag set, then each tag-fallback hop. The chain
	// is [primary, fb1, fb2, …] — a hop is only reached when no eligible entry
	// satisfied the previous set (graceful degradation when no model is
	// available, NOT a preference). Failover composes naturally: as entries
	// land in `tried`, the primary set eventually yields nothing and the next
	// chain link is consulted.
	// Resolve the affinity view ONCE per pick (not per hop): the requester's own
	// remembered entry, and how many other live conversations sit on each entry.
	own, reserved := p.affinityFor(req.AffinityKey)
	chain := fallbackChain(req.Requires, st.fallback)
	for hop, requires := range chain {
		row := pickReady(st, wantKind, requires, req.Prefer, own, reserved, tried)
		if row == nil {
			continue
		}
		if hop > 0 && len(tried) == 0 {
			// Log once per request that started on a fallback (tried empty =
			// first pick). Mid-failover re-picks (tried non-empty) stay quiet.
			slog.Info("model pool: tag-fallback engaged",
				"kind", wantKind, "required", req.Requires, "served_as", requires, "entry", row.info.Name)
		}
		return row, nil
	}
	if len(req.Requires) == 0 {
		return nil, fmt.Errorf("no live %s pool entry (none configured, paused, or in cooldown): %w", wantKind, ErrNoModelAvailable)
	}
	return nil, fmt.Errorf("no live %s pool entry has all required tags %v (tag-fallback exhausted): %w", wantKind, req.Requires, ErrNoModelAvailable)
}

// pickReady returns the best eligible entry of wantKind whose tags satisfy
// `requires`, skipping disabled / already-tried / ineligible entries (window-
// blind — capacity is the turn's sizing concern, not routing's); ranked by
// Prefer-hits (desc) → Priority (desc) → in-flight load (asc, spread work across
// equal-priority peers) → own affinity (soft cache-locality: the requester's
// remembered backend, breaks only an exact in-flight tie) → foreign occupancy
// (asc: steer toward entries fewer other live conversations sit on, so a
// stranger doesn't evict their warm cache; nil map = all zero) → Name (asc,
// final deterministic fallback). nil when nothing qualifies.
func pickReady(st *poolState, wantKind string, requires, prefer []string, own string, reserved map[string]int, tried map[string]bool) *entryRow {
	var ready []*entryRow
	for _, row := range st.entries {
		if row.info.Kind != wantKind {
			continue // wrong kind — a request only matches its own kind
		}
		if tried[row.info.Name] {
			continue // already attempted + failed for this request
		}
		if row.info.Priority < minEnabledPriority {
			continue // disabled entry — never routed to
		}
		if !hasAll(row.info.Tags, requires) {
			continue
		}
		if row.IsEligible() {
			ready = append(ready, row)
		}
	}
	if len(ready) == 0 {
		return nil
	}
	if len(ready) == 1 {
		return ready[0]
	}
	// Snapshot each candidate's live in-flight count under its OWN e.mu (one row lock
	// at a time, released immediately — never held across the sort, never fused with
	// IsEligible above which itself locks + mutates deadUntil). inFlight is e.mu-guarded
	// and, unlike the row.info.* fields the comparator reads lock-free, must be captured
	// under lock. Sorting on the captured value keeps the comparator lock-free.
	type cand struct {
		row      *entryRow
		inFlight int
	}
	cands := make([]cand, len(ready))
	for i, row := range ready {
		row.mu.Lock()
		ifl := row.inFlight
		row.mu.Unlock()
		cands[i] = cand{row: row, inFlight: ifl}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		ai, aj := countMatches(cands[i].row.info.Tags, prefer), countMatches(cands[j].row.info.Tags, prefer)
		if ai != aj {
			return ai > aj // Prefer-hits desc
		}
		if cands[i].row.info.Priority != cands[j].row.info.Priority {
			return cands[i].row.info.Priority > cands[j].row.info.Priority // Priority desc
		}
		if cands[i].inFlight != cands[j].inFlight {
			return cands[i].inFlight < cands[j].inFlight // load balance: least-loaded peer first
		}
		if own != "" {
			// Soft cache affinity: among equally-loaded, equally-ranked peers keep the
			// caller on the backend that last served its conversation (warm prompt
			// cache). Only reached on an exact in-flight tie, so load balancing always
			// outranks stickiness.
			mi, mj := cands[i].row.info.Name == own, cands[j].row.info.Name == own
			if mi != mj {
				return mi
			}
		}
		if ri, rj := reserved[cands[i].row.info.Name], reserved[cands[j].row.info.Name]; ri != rj {
			// Foreign occupancy: prefer the entry FEWER other live conversations are
			// remembered on — landing here would risk evicting their warm cache. Also
			// what spreads brand-new conversations across idle equal peers instead of
			// piling them all onto the Name-asc first.
			return ri < rj
		}
		return cands[i].row.info.Name < cands[j].row.info.Name // deterministic fallback
	})
	return cands[0].row
}

// fallbackChain expands a request's required-tag set into the ordered list of
// sets to try: the original first, then each tag-fallback hop. A hop fires when
// some rule's Tags are all present in the current set; the next set is
// (current − rule.Tags) ∪ rule.To, so unrelated required tags (e.g. a language
// tag) survive the swap. The first matching rule per hop wins (config order =
// precedence). Bounded by maxFallbackHops and cycle-guarded (a required-set
// already visited is never expanded again), so a self-referential ruleset
// terminates instead of looping.
func fallbackChain(requires []string, rules []FallbackRule) [][]string {
	chain := [][]string{requires}
	if len(rules) == 0 || len(requires) == 0 {
		return chain
	}
	seen := map[string]bool{tagSetKey(requires): true}
	current := requires
	for hop := 0; hop < maxFallbackHops; hop++ {
		next, ok := applyFirstRule(current, rules)
		if !ok {
			break
		}
		key := tagSetKey(next)
		if seen[key] {
			break // cycle — stop expanding
		}
		seen[key] = true
		chain = append(chain, next)
		current = next
	}
	return chain
}

// applyFirstRule returns (requires − rule.Tags) ∪ rule.To for the first rule
// whose Tags are all in requires, or (nil,false) when none match. Rules with an
// empty Tags or empty To are skipped (a no-op / misconfigured rule).
func applyFirstRule(requires []string, rules []FallbackRule) ([]string, bool) {
	for _, r := range rules {
		if len(r.Tags) == 0 || len(r.To) == 0 {
			continue
		}
		if !hasAll(requires, r.Tags) {
			continue
		}
		drop := map[string]bool{}
		for _, t := range r.Tags {
			drop[t] = true
		}
		out := make([]string, 0, len(requires)+len(r.To))
		seen := map[string]bool{}
		keep := func(t string) {
			if t == "" || seen[t] {
				return
			}
			seen[t] = true
			out = append(out, t)
		}
		for _, t := range requires {
			if !drop[t] {
				keep(t)
			}
		}
		for _, t := range r.To {
			keep(t)
		}
		return out, true
	}
	return nil, false
}

// tagSetKey is an order-independent key for a tag set, used by fallbackChain's
// cycle guard so [a,b] and [b,a] count as the same visited set.
func tagSetKey(tags []string) string {
	cp := append([]string(nil), tags...)
	sort.Strings(cp)
	return strings.Join(cp, "\x00")
}

// pickStatic is the SIZING twin of pick: the entry a request would rank first
// on configuration alone — Prefer-hits → priority → Name over kind + tags +
// enabled entries, with the same tag-fallback chain applied per hop when a tag
// set has no enabled carriers. It deliberately ignores health, load, and the
// tried set (and skips maybeReload / the fallback log): sizing decisions
// (compaction budget, segment capacity, tokenizer scale) must be STABLE across
// loop-tops — a cooling blip or an in-flight tie-break must never flap the
// budget and trigger an irreversible over-compaction of healthy history
// (review). The serving pick may land elsewhere mid-failover; the
// context-400 → compact-and-retry net covers that drift.
//
// `capable`, when non-nil, filters candidates by a STRUCTURAL capability
// (e.g. providers.CanTokenize) — a pinned entry failing it returns nil rather
// than looping (the caller falls back to its estimator).
func (p *MultiPool) pickStatic(req contract.ModelRequest, capable func(*entryRow) bool) *entryRow {
	st := p.state.Load()
	if st == nil || len(st.entries) == 0 {
		return nil
	}
	ok := func(row *entryRow) bool { return capable == nil || capable(row) }
	if req.PinnedEntry != "" {
		row, found := st.byName[req.PinnedEntry]
		if !found || row.info.Priority < minEnabledPriority || !ok(row) {
			return nil
		}
		return row
	}
	wantKind := req.Kind
	if wantKind == "" {
		wantKind = contract.KindLLM
	}
	for _, requires := range fallbackChain(req.Requires, st.fallback) {
		var best *entryRow
		bestHits := -1
		for _, row := range st.entries {
			if row.info.Kind != wantKind || row.info.Priority < minEnabledPriority {
				continue
			}
			if !hasAll(row.info.Tags, requires) || !ok(row) {
				continue
			}
			hits := countMatches(row.info.Tags, req.Prefer)
			if best == nil || hits > bestHits ||
				(hits == bestHits && (row.info.Priority > best.info.Priority ||
					(row.info.Priority == best.info.Priority && row.info.Name < best.info.Name))) {
				best, bestHits = row, hits
			}
		}
		if best != nil {
			return best
		}
	}
	return nil
}

// hasFallbackCandidate reports whether — beyond `current`, the entry
// the caller is about to attempt — the request still has somewhere to
// fail over to: either an entry pick can still return once `current`
// is also excluded, or an eligible saturated entry the all-saturated
// last resort could wait on. ChatStreamWatch uses it to decide whether to
// arm the first-content timeout (armed only when a fallback exists).
func (p *MultiPool) hasFallbackCandidate(req contract.ModelRequest, exclude map[string]bool, current *entryRow, saturated []*entryRow) bool {
	probe := make(map[string]bool, len(exclude)+1)
	for k := range exclude {
		probe[k] = true
	}
	probe[current.info.Name] = true
	if _, err := p.pick(req, probe); err == nil {
		return true
	}
	for _, r := range saturated {
		if r.info.Name != current.info.Name && r.IsEligible() {
			return true
		}
	}
	return false
}

// hasAll reports whether `tags` contains every element of `need`. Tag sets are tiny
// (1-3 elements) and this runs per-candidate on every pick, so a slice scan beats
// allocating a map each call (L-S2).
func hasAll(tags, need []string) bool {
	for _, n := range need {
		if !slices.Contains(tags, n) {
			return false
		}
	}
	return true
}

// countMatches returns how many elements of `wanted` are in `tags`. Like hasAll, a
// slice scan avoids a per-call map alloc on the O(n log n) sort comparator path.
func countMatches(tags, wanted []string) int {
	n := 0
	for _, w := range wanted {
		if slices.Contains(tags, w) {
			n++
		}
	}
	return n
}
