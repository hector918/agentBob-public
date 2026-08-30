package arch

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// heartwoodAllowed is the SANCTIONED set of top-level packages under heartwood/. heartwood
// is the shared foundation every leaf/flow module may direct-import (alongside contract/
// trunk), so a member must be a GENUINELY shared, no-upward-deps primitive that MUST
// behave identically wherever it is produced — e.g. prompt.SanitizeSpeaker (the same
// "[name]:" defense everywhere) and prompt.EstTokens (compaction and the model pool must
// agree byte-for-byte). It is NOT a convenience home for a single-consumer helper or a
// subsystem with heavy external deps (ffmpeg, a network backend).
//
// Adding a package here is an architectural commitment, not a casual move — DISCUSS with
// hector FIRST, then list it below. This guard fails on any un-listed heartwood package so
// a casual addition can't slip in unreviewed.
var heartwoodAllowed = map[string]bool{
	"prompt": true, // prompt-text hygiene (SanitizeSpeaker) + shared token sizing (EstTokens)
	"clock":  true, // DB-calibrated process clock; clock.Now direct-imported across leaf/flow
	"files":  true, // sandbox file store (files.Store) + inbound sweeper; shared fs primitive
	"scrub":  true, // credential-redaction defense (scrub.Scrub) — must be byte-identical everywhere
}

// TestHeartwoodGuard fails when a package appears under heartwood/ that is not in
// heartwoodAllowed (a casual addition), or when the allowlist names a package that no
// longer exists (drift). Either way the allowlist must be a deliberate, reviewed edit.
func TestHeartwoodGuard(t *testing.T) {
	root := moduleRootDir(t)
	base := filepath.Join(root, "heartwood")

	seen := map[string]bool{}
	err := filepath.WalkDir(base, func(dir string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || dir == base {
			return err
		}
		if _, perr := build.ImportDir(dir, 0); perr != nil {
			return nil // no buildable (non-test) Go package here — nothing to gate
		}
		rel, _ := filepath.Rel(base, dir)
		// Gate on the TOP-level heartwood package (a subpackage rides its parent's approval).
		top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		seen[top] = true
		if !heartwoodAllowed[top] {
			t.Errorf("heartwood/%s is NOT a sanctioned foundation package — heartwood is for genuinely shared, no-upward-deps primitives, not a dumping ground. Discuss with hector BEFORE adding to heartwood; if approved, add %q to heartwoodAllowed.", top, top)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk heartwood: %v", err)
	}
	for name := range heartwoodAllowed {
		if !seen[name] {
			t.Errorf("heartwoodAllowed lists %q but no such buildable heartwood/%s exists — remove it from the allowlist (keep it honest).", name, name)
		}
	}
}
