package core

import (
	"encoding/json"
	"time"
)

// Per-handle usage accounting (§13 audit) is stored as ONE row per handle with a
// single JSON blob (bob_account_handle_usage.usage_json) — a map of period-key →
// counts, downsampled by age so the blob stays bounded forever while the summed
// total stays exact (true lifetime):
//
//	< 3 months : daily   key "2006-01-02"
//	3mo – 3yr  : monthly key "2006-01"
//	> 3 years  : yearly  key "2006"
//
// ~90 daily + ~33 monthly + ~100 yearly ≈ 220 keys per century (a few KB). All the
// logic lives here in pure Go; the store just SELECTs the blob and UPSERTs the
// result — identical trivial SQL on sqlite + postgres, so there is NO JSON-SQL
// divergence to keep in parity. See docs/group-session-archive.md siblings /
// docs/accumulation-inventory.md #14.
//
// A turn's usage is recorded in ONE read-modify-write (BumpTurnUsage): turns +
// successes, per-KIND token counts (K — llm/ocr/translate, each {in,out}), and
// per-service NATIVE-unit counts (V — "kind:unit"→amount, e.g. "asr:s" audio
// seconds, "tts:c" chars). Tokens (K) and native units (V) are structurally
// disjoint so a sum of one never folds in the other's unit (docs/accounts.md §13).

// kindTok is one kind's token counts (short JSON keys to keep the blob small).
type kindTok struct {
	I int64 `json:"i"` // input tokens
	O int64 `json:"o"` // output tokens
}

// usageBucket holds one period's counts (short JSON keys to keep the blob small).
type usageBucket struct {
	T int64 `json:"t"` // turns
	S int64 `json:"s"` // successful turns
	// I/O are the pre-per-kind aggregate token counts. LEGACY: buckets written
	// before the per-kind split carry them; new writes use K instead. Still
	// summed into the grand total so old data isn't lost.
	I int64 `json:"i,omitempty"`
	O int64 `json:"o,omitempty"`
	// K is per-KIND token usage (kind → {in,out}) — the current token store.
	K map[string]kindTok `json:"k,omitempty"`
	// V is per-service NATIVE-unit usage, keyed "kind:unit" → amount (e.g.
	// "asr:s" audio seconds, "tts:c" chars). Disjoint from the token counts.
	V map[string]int64 `json:"v,omitempty"`
}

// BumpTurnUsage folds one turn's usage into now's DAILY bucket — turns/successes,
// per-kind token counts (tokens: kind → {In,Out}), and native-unit service
// counts (native: "kind:unit" → amount) — then compacts older buckets to a
// coarser precision. Returns the new JSON. Pure (no I/O); blob "" / "{}" starts
// fresh; a malformed blob returns the input unchanged + the error (caller logs;
// usage is best-effort). One marshal per turn — the single accounting write.
func BumpTurnUsage(blob string, turns, success int64, tokens map[string]KindTokens, native map[string]int64, now time.Time) (string, error) {
	m, err := parseUsage(blob)
	if err != nil {
		return blob, err
	}
	now = now.UTC()
	day := now.Format("2006-01-02")
	b := m[day]
	b.T += turns
	b.S += success
	for kind, t := range tokens {
		if t.In == 0 && t.Out == 0 {
			continue
		}
		if b.K == nil {
			b.K = map[string]kindTok{}
		}
		k := b.K[kind]
		k.I += t.In
		k.O += t.Out
		b.K[kind] = k
	}
	for key, amt := range native {
		if amt == 0 {
			continue
		}
		if b.V == nil {
			b.V = map[string]int64{}
		}
		b.V[key] += amt
	}
	m[day] = b
	compactUsage(m, now)
	out, err := json.Marshal(m)
	if err != nil {
		return blob, err
	}
	return string(out), nil
}

// SumHandleUsage returns the lifetime totals across every bucket — turns,
// successes, and grand-total tokens (legacy aggregate I/O + the per-kind K sums).
// A malformed blob sums to zero (best-effort).
func SumHandleUsage(blob string) (turns, success, in, out int64) {
	m, err := parseUsage(blob)
	if err != nil {
		return 0, 0, 0, 0
	}
	for _, b := range m {
		turns += b.T
		success += b.S
		in += b.I
		out += b.O
		for _, k := range b.K {
			in += k.I
			out += k.O
		}
	}
	return turns, success, in, out
}

// SumTokensByKind returns lifetime per-kind token totals (kind → {In,Out}) from
// the K sections. Legacy aggregate I/O (pre-per-kind buckets) is NOT attributed
// to a kind here — it only shows in SumHandleUsage's grand total. Malformed
// blob → nil.
func SumTokensByKind(blob string) map[string]KindTokens {
	m, err := parseUsage(blob)
	if err != nil {
		return nil
	}
	out := map[string]KindTokens{}
	for _, b := range m {
		for kind, k := range b.K {
			t := out[kind]
			t.In += k.I
			t.Out += k.O
			out[kind] = t
		}
	}
	return out
}

// AddBlob folds one handle's usage_json blob into the account rollup via the
// three read seams (SumHandleUsage / SumTokensByKind / SumServiceUsage). The
// store's per-account JOIN calls this once per bound handle; identical on both
// backends (no JSON-SQL divergence). Lazy-inits the maps.
func (u *AccountUsage) AddBlob(blob string) {
	t, s, in, out := SumHandleUsage(blob)
	u.Turns += t
	u.Success += s
	u.InTokens += in
	u.OutTokens += out
	for kind, kt := range SumTokensByKind(blob) {
		if u.TokensByKind == nil {
			u.TokensByKind = map[string]KindTokens{}
		}
		x := u.TokensByKind[kind]
		x.In += kt.In
		x.Out += kt.Out
		u.TokensByKind[kind] = x
	}
	for k, n := range SumServiceUsage(blob) {
		if u.Native == nil {
			u.Native = map[string]int64{}
		}
		u.Native[k] += n
	}
}

// SumServiceUsage returns lifetime native-service totals (key "kind:unit" →
// amount) summed across every bucket. Malformed blob → nil (best-effort).
func SumServiceUsage(blob string) map[string]int64 {
	m, err := parseUsage(blob)
	if err != nil {
		return nil
	}
	out := map[string]int64{}
	for _, b := range m {
		for k, v := range b.V {
			out[k] += v
		}
	}
	return out
}

func parseUsage(blob string) (map[string]usageBucket, error) {
	m := map[string]usageBucket{}
	if blob == "" || blob == "{}" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		return map[string]usageBucket{}, err
	}
	return m, nil
}

// compactUsage folds daily buckets older than 3 months into their month key, then
// monthly buckets older than 3 years into their year key. Precision is told from
// the key length (10=day, 7=month, 4=year). Lossless (sums carry forward).
func compactUsage(m map[string]usageBucket, now time.Time) {
	dayCut := now.AddDate(0, -3, 0) // daily older than this → monthly
	monCut := now.AddDate(-3, 0, 0) // monthly older than this → yearly
	// pass 1: day → month
	for k, v := range m {
		if len(k) != 10 {
			continue
		}
		d, err := time.Parse("2006-01-02", k)
		if err != nil {
			continue
		}
		if d.Before(dayCut) {
			foldInto(m, d.Format("2006-01"), v)
			delete(m, k)
		}
	}
	// pass 2: month → year (includes months just created by pass 1)
	for k, v := range m {
		if len(k) != 7 {
			continue
		}
		mo, err := time.Parse("2006-01", k)
		if err != nil {
			continue
		}
		if mo.Before(monCut) {
			foldInto(m, mo.Format("2006"), v)
			delete(m, k)
		}
	}
}

func foldInto(m map[string]usageBucket, key string, v usageBucket) {
	b := m[key]
	b.T += v.T
	b.S += v.S
	b.I += v.I
	b.O += v.O
	for kind, k := range v.K {
		if b.K == nil {
			b.K = map[string]kindTok{}
		}
		dst := b.K[kind]
		dst.I += k.I
		dst.O += k.O
		b.K[kind] = dst
	}
	for k, n := range v.V {
		if b.V == nil {
			b.V = map[string]int64{}
		}
		b.V[k] += n
	}
	m[key] = b
}
