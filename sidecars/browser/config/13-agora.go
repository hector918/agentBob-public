package config

import (
	"time"
)

// AgoraConfig tunes the agora dispatcher. Most settings are baked into
// the package's defaults; the dials below are exposed because deployments
// with very different volume profiles want different retention windows.
//
// R7-12: prune knobs for the dispatch_queue table — see
// agora/60-dispatcher.go for the gate semantics. Time-based, independent
// of tick rate.
type AgoraConfig struct {
	// PruneInterval — gap between dispatch_queue prune sweeps. Default 1h.
	PruneInterval time.Duration `yaml:"prune_interval"`
	// PruneKeepDays — retention window for 'done' / 'cancelled' rows
	// before deletion. Default 30. 'failed' rows are always kept.
	PruneKeepDays int `yaml:"prune_keep_days"`

	// RequireAccount gates agora service on the sender having a bound account:
	// when true, a turn routing to an agora inbox whose triggering sender has no
	// account_id is refused with a "请先绑定账户" reply instead of being served.
	// Global switch (per docs/accounts.md §13.3 D); default false = off (agora
	// serves everyone as today). Bound accounts are unaffected.
	RequireAccount bool `yaml:"require_account"`

	// DirectoryCap caps how many reachable contacts the agora-directory
	// prompt source lists per agora session (same_company first, then
	// via_channel). A real org's per-person reachable set is bounded by
	// structure (a team + a few channels + the bridge), so this is a safety
	// belt that almost never bites. 0 = no cap. When hit, deterministic
	// truncation + an overflow note. Pointer so an explicit 0 (no cap) is
	// distinguishable from unset (default 50) — same pattern as skills'
	// IndexVisibleCap.
	DirectoryCap *int `yaml:"directory_cap"`
}

// DirectoryCapEff returns the effective agora-directory contact cap with
// the default applied: unset → 50, explicit ≤0 → 0 (no cap).
func (a AgoraConfig) DirectoryCapEff() int {
	if a.DirectoryCap == nil {
		return 50
	}
	if *a.DirectoryCap <= 0 {
		return 0
	}
	return *a.DirectoryCap
}
