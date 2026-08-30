package config

import "time"

// RetrievalConfig configures the cold-memory retrieval feed (bob → external
// chat-retrieval-system leaf). Off by default — the whole feature is inert
// until enabled AND a base_url is set. See docs/cold-memory-retrieval.md.
type RetrievalConfig struct {
	// Enabled gates the feed entirely: nil/false → no drainer goroutine,
	// no enqueue, zero behavior change. Default false (unlike source health,
	// this is opt-in — the leaf service must exist first).
	Enabled *bool `yaml:"enabled"`
	// BaseURL of the leaf, e.g. "http://10.0.0.5:8000". Required when enabled.
	BaseURL string `yaml:"base_url"`
	// APIKey, when set, is sent as X-API-Key on every leaf call. Must match
	// the leaf's API_KEY env. Empty = no header (leaf with auth disabled).
	APIKey string `yaml:"api_key"`
	// TimeoutSec — per HTTP call timeout. Default 10.
	TimeoutSec int `yaml:"timeout_sec"`
	// TickSec — drainer poll interval. Default 5.
	TickSec int `yaml:"tick_sec"`
	// Batch — rows per /messages/batch push. Default + hard cap 200 (the
	// leaf rejects batches > 200).
	Batch int `yaml:"batch"`
	// TaggingEnabled (Phase 1.5) turns on the in-drain small-model tagger:
	// before each batch is pushed, sentiment/is_commitment/intent are filled.
	// Default false (Phase 1 feeds raw). Requires Enabled too.
	TaggingEnabled *bool `yaml:"tagging_enabled"`
}

// EnabledEff defaults to false (opt-in).
func (r RetrievalConfig) EnabledEff() bool {
	return r.Enabled != nil && *r.Enabled
}

// ConfiguredEff is the single predicate for "the feed is actually live": both
// enabled AND a base_url is set. The write side (enqueue sink), the drainer,
// the recall tool and the /recall slash all gate on this — if they diverge
// (e.g. write side gating only on EnabledEff while the drainer also needs
// BaseURL), an enabled-but-base_url-empty config grows the outbox unbounded
// with no reader. See docs/cold-memory-retrieval.md.
func (r RetrievalConfig) ConfiguredEff() bool {
	return r.EnabledEff() && r.BaseURL != ""
}

// TimeoutEff returns the per-call timeout, defaulting to 10s.
func (r RetrievalConfig) TimeoutEff() time.Duration {
	if r.TimeoutSec <= 0 {
		return 10 * time.Second
	}
	return time.Duration(r.TimeoutSec) * time.Second
}

// TickEff returns the drainer interval, defaulting to 5s.
func (r RetrievalConfig) TickEff() time.Duration {
	if r.TickSec <= 0 {
		return 5 * time.Second
	}
	return time.Duration(r.TickSec) * time.Second
}

// BatchEff returns the batch size, defaulting to 200 and clamped to the
// leaf's 200 cap.
func (r RetrievalConfig) BatchEff() int {
	if r.Batch <= 0 || r.Batch > 200 {
		return 200
	}
	return r.Batch
}

// TaggingEnabledEff defaults to false (Phase 1 feeds raw).
func (r RetrievalConfig) TaggingEnabledEff() bool {
	return r.TaggingEnabled != nil && *r.TaggingEnabled
}
