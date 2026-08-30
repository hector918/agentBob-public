package warrant

import (
	"os"
	"path/filepath"
	"testing"
)

// A present-but-corrupt permissions source must FAIL CLOSED: deny-all + broken,
// never the allow-all gap that warrant-going-absent used to cause.
func TestLoadOrFailClosed_CorruptDeniesAll(t *testing.T) {
	home := t.TempDir()
	// `grants` typed as a scalar can't decode into the map → Unmarshal errors.
	if err := os.WriteFile(filepath.Join(home, "permissions.yaml"),
		[]byte("principals: [admin]\ngrants: not-a-map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, broken := LoadOrFailClosed(newFileStore(home))
	if !broken {
		t.Fatal("a corrupt permissions file must report broken=true")
	}
	if p.allows("tool:use:now", "admin") {
		t.Fatal("fail-closed: a broken policy must deny every capability")
	}
}

// A MISSING file is not an error — it is an empty (deny-all) policy, not broken.
func TestLoadOrFailClosed_MissingIsEmptyNotBroken(t *testing.T) {
	p, broken := LoadOrFailClosed(newFileStore(t.TempDir()))
	if broken {
		t.Fatal("a missing permissions file is not 'broken' — it's an empty policy")
	}
	if p.allows("tool:use:now", "admin") {
		t.Fatal("an empty policy denies everything")
	}
}

// Part ②: within a parseable file, a garbage cell value is fail-closed to off —
// the valid grants stay usable, the broken one just doesn't grant.
func TestFromFile_GarbageCellDenied(t *testing.T) {
	p := fromFile(policyFile{
		Principals: []string{"admin"},
		Grants: map[string]map[string]string{
			"tool:use:good": {"admin": "on"},
			"tool:use:bad":  {"admin": "onn"}, // typo → off
		},
	})
	if !p.allows("tool:use:good", "admin") {
		t.Fatal("a valid 'on' cell must grant")
	}
	if p.allows("tool:use:bad", "admin") {
		t.Fatal("a garbage cell value must be fail-closed to off")
	}
}
