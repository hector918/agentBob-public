package arch

import (
	"os"
	"path/filepath"
	"testing"
)

// heartwood/scrub is the credential-redaction defense (scrub.Scrub). sidecars/browser
// is a SEPARATE go module (its own go.mod, built into the chromium sidecar container),
// so it cannot import heartwood — it carries a hand-copied fork of scrub under
// sidecars/browser/scrub/. The two MUST stay byte-identical: the heartwoodAllowed
// comment (50-heartwood_test.go) already promises scrub is "byte-identical everywhere",
// and 76-retrieval.go sends browserd session content through the sidecar copy to remote
// storage — a stale fork silently leaks secrets the main copy learned to redact.
//
// This drift happened for real: the fork froze at b0aa0a32 while the main copy took four
// rounds of redaction fixes, undetected across four audit rounds because each only read
// the main copy. This guard makes that impossible — any divergence fails CI, converting
// the byte-identity promise from a comment into an enforced invariant.
//
// If a file must legitimately differ (it should not — scrub is pure, leaf-free), add it
// to scrubParityExempt with a written reason; an unexplained diff is a bug.
var scrubParityExempt = map[string]bool{}

func TestScrubCopiesByteIdentical(t *testing.T) {
	root := moduleRootDir(t)
	canonical := filepath.Join(root, "heartwood", "scrub")
	fork := filepath.Join(root, "sidecars", "browser", "scrub")

	// Every .go file in the canonical dir must have a byte-identical counterpart in the
	// fork (and no counterpart may be missing). The reverse direction is covered by the
	// name-set check below.
	names := goFileNames(t, canonical)
	forkNames := goFileNames(t, fork)

	for name := range names {
		if scrubParityExempt[name] {
			continue
		}
		if !forkNames[name] {
			t.Errorf("scrub drift: heartwood/scrub/%s has no counterpart in sidecars/browser/scrub/ — the fork is missing a file", name)
			continue
		}
		want, err := os.ReadFile(filepath.Join(canonical, name))
		if err != nil {
			t.Fatalf("read heartwood/scrub/%s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(fork, name))
		if err != nil {
			t.Fatalf("read sidecars/browser/scrub/%s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("scrub drift: sidecars/browser/scrub/%s is NOT byte-identical to heartwood/scrub/%s — "+
				"re-sync the fork (copy the canonical file over) or the sidecar leaks secrets the main copy redacts", name, name)
		}
	}
	for name := range forkNames {
		if scrubParityExempt[name] || names[name] {
			continue
		}
		t.Errorf("scrub drift: sidecars/browser/scrub/%s has no counterpart in heartwood/scrub/ — a stray file in the fork", name)
	}
}

// goFileNames returns the set of *.go file base names directly under dir.
func goFileNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
			continue
		}
		names[e.Name()] = true
	}
	if len(names) == 0 {
		t.Fatalf("no .go files under %s — the scrub copy moved or vanished; update this guard", dir)
	}
	return names
}
