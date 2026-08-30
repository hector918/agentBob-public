// Package warrant is the capability authority: it judges whether a principal may
// use a capability (tool:use:X / credential:use:Y / skill:use:Z) and projects a
// tool set down to the authorized subset. The policy is a principal×capability
// matrix. Identity is an opaque principal string the flow mints — warrant only
// matches it against the table.
//
// This file is the storage-AGNOSTIC engine: the matrix type, the fail-closed
// lookup, and the PolicyStore seam. The on-disk yaml backend lives in
// 12-store-file.go; reconcile (the catalog mirror) in 20-reconcile.go. Splitting
// storage from semantics lets a future agora DB backend (per-company/role/flow
// permissions) reuse this exact engine so behaviour is identical everywhere.
//
// SLICE 2a: Check + Authorize over the matrix + boot reconcile. Channel vending
// (Exec/File) + Cut grow the contract in slice 2b. The live matrix is hot-
// reloadable via the admin /permission reload slash (atomic swap; see
// Manager.reloadPolicy) — no restart needed to pick up permissions.yaml edits.
package warrant

import "strings"

// policy is the parsed, queryable matrix: capability → set of principals granted.
type policy struct {
	principals []string
	granted    map[string]map[string]bool // capability → principal → true
}

func emptyPolicy() *policy {
	return &policy{granted: map[string]map[string]bool{}}
}

// allows reports whether principal is granted capability (fail-closed: an
// unknown capability or principal → false).
func (p *policy) allows(capability, principal string) bool {
	if p == nil {
		return false
	}
	row, ok := p.granted[capability]
	if !ok {
		return false
	}
	return row[principal]
}

// lostGrants returns the principals that held at least one capability in old but
// no longer hold it in next — the set whose live pooled channels must be Cut on a
// reload so a permission REMOVAL takes effect at once. Additive reloads (a new
// default-off row, a newly granted capability) return nobody. Per-principal: the
// pool keys revocation by identity, so a principal that lost ANY grant has all its
// channels recycled (a still-authorized one is simply re-acquired + re-checked).
func lostGrants(old, next *policy) []string {
	if old == nil {
		return nil
	}
	lost := map[string]bool{}
	for capability, row := range old.granted {
		for principal, was := range row {
			if was && !next.allows(capability, principal) {
				lost[principal] = true
			}
		}
	}
	if len(lost) == 0 {
		return nil
	}
	out := make([]string, 0, len(lost))
	for id := range lost {
		out = append(out, id)
	}
	return out
}

// capString builds the canonical capability literal (verb pinned to "use").
func capString(kind, name string) string { return kind + ":use:" + name }

// truthy parses the on/off cell (on/true/yes/1 → true; anything else → false).
// Fail-closed by construction: a typo'd cell value is "off", not granted.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "yes", "1":
		return true
	default:
		return false
	}
}

// PolicyStore is the permission matrix's storage backend. Only loading + saving
// the matrix is backend-specific; the SEMANTICS (allows / reconcile / fail-closed
// degrade) are storage-agnostic and live in this engine. The file backend
// (12-store-file.go) is the only one today; an agora DB backend (per-company /
// role / flow permissions) can implement this same interface and reuse the engine
// so behaviour is identical regardless of where the matrix is stored.
type PolicyStore interface {
	// Load returns the stored matrix. A MISSING source is NOT an error — it returns
	// an empty (deny-all) policy. A present-but-corrupt source IS an error.
	Load() (*policy, error)
	// Save persists the matrix (e.g. after reconcile adds default-off rows).
	Save(p *policy) error
}

// LoadOrFailClosed loads the matrix from store, FAILING CLOSED on a corrupt
// source: a load error yields a deny-all empty policy and broken=true — never an
// allow-all gap. The caller stays active with the deny-all matrix and alerts an
// admin. Shared by every backend so file and DB permissions degrade identically.
func LoadOrFailClosed(store PolicyStore) (p *policy, broken bool) {
	p, err := store.Load()
	if err != nil {
		return emptyPolicy(), true
	}
	return p, false
}
