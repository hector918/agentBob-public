package arch

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// wantProvides is the APPROVED set of PROVIDER edges per module — the provider-side
// counterpart to wantOptional's consumer-side ledger. TestModuleGraph already checks
// Provides() via reflection, but that only sees STATIC declarations; a module that
// publishes a capability DYNAMICALLY (a conditional trunk.Provide[contract.X] inside
// Start, not mirrored in Provides()) is invisible to it — contract.BrowserTakeover
// was exactly such a blind spot (leaf/tools provides it only when a browser URL is
// configured). This map is scanned from the real trunk.Provide[...] call sites, so
// it catches every provider edge including the dynamic ones, and fails on any drift
// — a new/removed/moved provider must be reviewed and approved HERE.
//
// trunk.Housekeeper is NOT here: the trunk provides it internally, not via a module
// trunk.Provide call, so the source scan never sees it (consumers TryRequire it —
// tracked in wantOptional).
var wantProvides = map[string][]string{
	"flow/router":      {"contract.FlowRegistry", "contract.TurnHandler"},
	"leaf/accounts":    {"contract.APIKeys", "contract.AccountProvisioner", "contract.Accounts", "contract.ConsumptionReporter"},
	"leaf/adminline":   {"contract.AdminLine"},
	"leaf/agora":       {"contract.Agora", "contract.AgoraSend", "contract.MemberFailureSink"},
	"leaf/claimtoken":  {"contract.ClaimTokens"},
	"leaf/credentials": {"contract.Broker"},
	"leaf/gate":        {"contract.AccessGranter", "contract.Screener"},
	"leaf/learn":       {"contract.LearnRegistry"},
	"leaf/asr":         {"contract.Transcriber"},
	"leaf/model":       {"contract.ImageCatalog", "contract.ModelPool"},
	"leaf/pgpool":      {"contract.DB"},
	"prompt":           {"contract.PromptFactory"},
	"leaf/retrieval":   {"contract.RetrievalClient", "contract.RetrievalFeed"},
	"leaf/session":     {"contract.ChatHistory", "contract.MessageIndexer", "contract.MessageStore", "contract.SessionManager", "contract.SessionResume"},
	"leaf/skills":      {"contract.SkillCatalog", "contract.SkillFailureSink"},
	"leaf/slash":       {"contract.SlashRegistry"},
	"leaf/stoma":       {"contract.Gateway"},
	"leaf/arrangement": {"contract.Arrangements"},
	"leaf/tools":       {"contract.BrowserControlHold", "contract.BrowserTakeover", "contract.ChannelPool", "contract.ToolCatalog"},
	"leaf/turn":        {"contract.Turn"},
	"leaf/urllib":      {"contract.URLLibrary"},
	"leaf/warrant":     {"contract.Warrant"},
	"leaf/webui":       {"contract.PanelRegistry", "contract.TakeoverMinter"},
}

var provideRe = regexp.MustCompile(`Provide\[((?:contract|trunk)\.\w+)\]`)

// scanProvideEdges walks the module tree and returns module → set of capabilities
// it publishes via trunk.Provide[...] (production sources only).
func scanProvideEdges(t *testing.T) map[string]map[string]bool {
	t.Helper()
	root := moduleRootDir(t)
	got := map[string]map[string]bool{}
	for _, top := range []string{"leaf", "flow", "heartwood"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return err
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(root, path)
			mod := moduleKeyOf(rel)
			if mod == "" {
				return nil
			}
			for _, m := range provideRe.FindAllStringSubmatch(string(data), -1) {
				if got[mod] == nil {
					got[mod] = map[string]bool{}
				}
				got[mod][m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	return got
}

// TestProvideEdges asserts the live set of trunk.Provide edges matches wantProvides
// — the provider-side drift gate that TestModuleGraph (reflection on Provides())
// cannot see for dynamically-provided capabilities.
func TestProvideEdges(t *testing.T) {
	got := scanProvideEdges(t)
	for mod := range union2(got, wantProvides) {
		gotSet := sortedKeys(got[mod])
		want := append([]string(nil), wantProvides[mod]...)
		sort.Strings(want)
		if !reflect.DeepEqual(gotSet, want) {
			t.Errorf("module %q provider (trunk.Provide) edges drifted:\n  approved = %v\n  now      = %v\nreview the change and update wantProvides in arch/30-provide_test.go.", mod, want, gotSet)
		}
	}
}

// TestNoProvidedButNeverRequired asserts every capability some module PROVIDES is
// consumed by at least one Require/TryRequire somewhere. A capability provided but
// never required is a dead wire that no other arch guard catches — it was exactly
// how contract.SessionLookup rotted silently (provided + implemented by leaf/session
// after the takeover axis went scope-keyed, but no consumer ever resolved it). This
// closes that blind spot.
func TestNoProvidedButNeverRequired(t *testing.T) {
	provided := scanProvideEdges(t)
	consumed := scanConsumerCaps(t) // Require ∪ TryRequire, all modules

	for mod, caps := range provided {
		for cap := range caps {
			if !consumed[cap] {
				t.Errorf("%s provides %s but NOTHING Requires/TryRequires it — a provided-but-never-consumed capability is a dead wire; remove the Provide (and the interface) or wire a consumer.", mod, cap)
			}
		}
	}
}

// scanConsumerCaps returns the set of every capability consumed via Require[...] or
// TryRequire[...] across the module tree (the regex `Require\[` matches both, since
// TryRequire ends in "Require["), plus edges declared through the flows' generic
// lazy resolvers (lazyDepRe — see 20-optional_test.go).
func scanConsumerCaps(t *testing.T) map[string]bool {
	t.Helper()
	root := moduleRootDir(t)
	res := []*regexp.Regexp{regexp.MustCompile(`Require\[((?:contract|trunk)\.\w+)\]`), lazyDepRe}
	consumed := map[string]bool{}
	for _, top := range []string{"leaf", "flow", "heartwood", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return err
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			src := stripLineComments(string(data)) // comment text must not count as consumption
			for _, re := range res {
				for _, m := range re.FindAllStringSubmatch(src, -1) {
					consumed[m[1]] = true
				}
			}
			return nil
		})
	}
	return consumed
}

// stripLineComments blanks out // line-comment tails so commented-out or documented
// Require[...] references don't count as live consumption. Simple truncate-at-`//`:
// it can over-strip a `//` sitting inside a string literal, but Require/TryRequire
// call sites never live inside such strings, so the consumer scan is unaffected.
func stripLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if j := strings.Index(line, "//"); j >= 0 {
			lines[i] = line[:j]
		}
	}
	return strings.Join(lines, "\n")
}
