// 12-store-file.go is the FILE backend of the permission matrix: the
// permissions.yaml read/parse/write. It is the only storage detail here — the
// matrix SEMANTICS (allows / reconcile / fail-closed degrade) live in the engine
// (10-policy.go, 20-reconcile.go) and are storage-agnostic, so a future agora DB
// backend (per-company/role/flow permissions) implements the same PolicyStore
// interface and reuses the identical engine → behaviour matches regardless of
// where the matrix is stored.
package warrant

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// policyFile is the on-disk matrix: the known principals + a per-capability grant
// map (capability → principal → on/off). One yaml an admin reads at a glance.
type policyFile struct {
	Principals []string                     `yaml:"principals"`
	Grants     map[string]map[string]string `yaml:"grants"`
}

// fileStore is the permissions.yaml PolicyStore backend (one matrix per $BOB_HOME).
type fileStore struct{ home string }

func newFileStore(home string) *fileStore { return &fileStore{home: home} }

// Load reads + parses permissions.yaml into the engine's policy. A MISSING file →
// empty (deny-all) policy, no error (everything denied until reconcile seeds it);
// a present-but-malformed file IS an error → the engine fails closed (see
// LoadOrFailClosed), never allow-all.
func (f *fileStore) Load() (*policy, error) {
	path := policyPath(f.home)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return emptyPolicy(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("warrant: read %s: %w", path, err)
	}
	var pf policyFile
	if err := yaml.Unmarshal(raw, &pf); err != nil {
		return nil, fmt.Errorf("warrant: parse %s: %w", path, err)
	}
	return fromFile(pf), nil
}

// Save atomically writes the matrix to permissions.yaml (sorted for a stable,
// reviewable diff). yaml.Marshal emits map keys sorted, so "skill:use:*" and
// "tool:use:*" grants land in two contiguous blocks — easy for an admin to scan.
func (f *fileStore) Save(p *policy) error {
	pf := p.toFile()
	sort.Strings(pf.Principals)
	data, err := yaml.Marshal(pf)
	if err != nil {
		return fmt.Errorf("warrant: marshal policy: %w", err)
	}
	return f.SaveRaw(data)
}

// SaveRaw atomically writes pre-rendered yaml bytes to permissions.yaml VERBATIM
// (tmp+rename, 0600). Unlike Save it does not re-serialize from a *policy, so a
// caller holding the admin's exact edited text (the webui editor) preserves its
// comments + ordering — the file stays the one an admin would hand-edit. The bytes
// MUST already be validated as parseable (the caller does so before calling).
func (f *fileStore) SaveRaw(data []byte) error {
	path := policyPath(f.home)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("warrant: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("warrant: rename %s: %w", path, err)
	}
	return nil
}

var _ PolicyStore = (*fileStore)(nil)

// fromFile bridges the on-disk matrix into the engine's queryable policy. Each
// cell goes through truthy, so a garbage value (typo) is fail-closed to off — a
// single bad cell denies that grant without breaking the rest of the file.
func fromFile(f policyFile) *policy {
	p := emptyPolicy()
	p.principals = f.Principals
	for capability, row := range f.Grants {
		set := make(map[string]bool, len(row))
		for principal, state := range row {
			set[principal] = truthy(state)
		}
		p.granted[capability] = set
	}
	return p
}

func (p *policy) toFile() policyFile {
	f := policyFile{Principals: p.principals, Grants: map[string]map[string]string{}}
	for capability, row := range p.granted {
		cell := make(map[string]string, len(row))
		for principal, on := range row {
			cell[principal] = offOn(on)
		}
		f.Grants[capability] = cell
	}
	return f
}

func offOn(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func policyPath(home string) string { return filepath.Join(home, "permissions.yaml") }
