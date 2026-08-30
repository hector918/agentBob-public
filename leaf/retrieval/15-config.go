package retrieval

import (
	"time"

	"agentbob/config"
)

// Config is the `retrieval:` section of config.yaml — the cold-memory feed
// (bob → external chat-retrieval leaf). OFF by default: the whole feature is inert
// until Enabled AND a BaseURL is set. See docs/cold-memory-trunk.md.
type Config struct {
	// Enabled gates the feature entirely: false → the module registers nothing
	// (recall tool reports unavailable, flow skips enqueue), zero behavior change.
	Enabled bool `yaml:"enabled"`
	// BaseURL of the leaf, e.g. "http://10.0.0.5:8000". Required when enabled.
	BaseURL string `yaml:"base_url"`
	// APIKey, when set, is sent as X-API-Key on every leaf call (matches the leaf's
	// API_KEY env). Empty = no header (leaf with auth disabled).
	APIKey string `yaml:"api_key"`
	// TimeoutSec — per HTTP call timeout. Default 10.
	TimeoutSec int `yaml:"timeout_sec"`
	// TickSec — feeder drain interval. Default 5.
	TickSec int `yaml:"tick_sec"`
	// Batch — rows per /messages/batch push. Default + hard cap 200 (the leaf
	// rejects batches > 200).
	Batch int `yaml:"batch"`
}

// Configured is the single predicate for "the feed is live": enabled AND a
// base_url is set. Everything (module Start, feeder, recall tool, flow enqueue)
// hangs off the registry registrations this gates.
func (c Config) Configured() bool { return c.Enabled && c.BaseURL != "" }

// Timeout returns the per-call HTTP timeout, defaulting to 10s.
func (c Config) Timeout() time.Duration {
	if c.TimeoutSec <= 0 {
		return 10 * time.Second
	}
	return time.Duration(c.TimeoutSec) * time.Second
}

// Tick returns the feeder drain interval, defaulting to 5s.
func (c Config) Tick() time.Duration {
	if c.TickSec <= 0 {
		return 5 * time.Second
	}
	return time.Duration(c.TickSec) * time.Second
}

// BatchSize returns the per-push row count, defaulting to and capped at the
// leaf's 200 limit.
func (c Config) BatchSize() int {
	if c.Batch <= 0 || c.Batch > 200 {
		return 200
	}
	return c.Batch
}

// fileShape is config.yaml as far as this module reads it — just its own section.
type fileShape struct {
	Retrieval Config `yaml:"retrieval"`
}

// LoadConfig reads the `retrieval:` section from config.yaml (missing file / section
// → disabled defaults).
func LoadConfig(path string) (Config, error) {
	fs, err := config.Load(path, fileShape{}) // defaults live in the accessors (Timeout/Tick/BatchSize)
	return fs.Retrieval, err
}
