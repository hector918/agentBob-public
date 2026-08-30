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

// wantOptional is the APPROVED set of OPTIONAL (TryRequire) edges per module —
// the machine-checked counterpart to wantGraph. wantGraph only sees HARD Needs
// (via reflection on Needs()); optional edges are invisible to it, so before this
// test they lived only in a hand-written comment ledger that silently rotted
// (S-2: it had drifted — 5 edges unlisted). TestOptionalEdges scans every leaf/
// flow module's source for TryRequire[contract.X] / TryRequire[trunk.X] and fails
// on any drift, so a new optional cross-module edge must be reviewed and approved
// HERE, exactly like a new hard Need is in wantGraph.
//
// Each edge documents what the consumer does without the provider (graceful
// degradation) at its resolution site; this map is the inventory, not the rationale.
var wantOptional = map[string][]string{
	"flow/agora":   {"contract.Agora", "contract.ChatHistory", "contract.MemberFailureSink", "contract.MessageIndexer", "contract.MessageStore", "contract.PanelRegistry", "contract.RetrievalFeed", "contract.SkillCatalog", "contract.SkillFailureSink", "contract.ToolCatalog", "contract.URLLibrary", "contract.Warrant"},
	"flow/inbound": {"contract.Accounts", "contract.SessionManager"},
	"flow/intro":   {"contract.AccountProvisioner", "contract.Accounts"},
	"flow/normal":  {"contract.MessageIndexer", "contract.RetrievalFeed", "contract.SkillCatalog", "contract.ToolCatalog", "contract.URLLibrary", "contract.Warrant"},
	"flow/router":  {"contract.Accounts", "contract.Gateway"},

	"leaf/accounts":  {"contract.AccessGranter", "contract.Agora", "trunk.Housekeeper"},
	"leaf/adminline": {"trunk.Housekeeper"},
	"leaf/agora":     {"contract.AccessGranter", "contract.LearnRegistry", "contract.SessionManager", "contract.SkillCatalog", "contract.ToolCatalog"},
	"leaf/learn":     {"contract.ModelPool", "trunk.Housekeeper"},
	// retrieval → PanelRegistry: the panel is drawn only when a webui is present
	// (same posture as modelgate); feed + recall work without one.
	"leaf/retrieval": {"contract.PanelRegistry", "trunk.Housekeeper"},
	"leaf/model":     {"contract.AdminLine", "contract.ConsumptionReporter", "trunk.Housekeeper"},
	"leaf/modelgate": {"contract.APIKeys", "contract.ImageCatalog", "contract.PanelRegistry"}, // key auth + panel + the image style catalog, all lazy; absent → 401s / no panel / no styles to offer
	// session → SkillCatalog is NOT here: it has none. warrant → SkillCatalog (an
	// at-Start reconcile that needs skills first) is below; its ordering is held by
	// cmd registration order + this ledger, NOT a hard Need — a hard Need would let
	// an absent (Optional) skills module skip the (Optional) warrant entirely and
	// turn tool enforcement allow-all, a WORSE failure than the fail-closed
	// skill-deny a late reconcile yields (S-4).
	"leaf/asr":         {"contract.ModelPool"},                                                                                                        // lazily resolves the pool's KindASR backend
	"leaf/session":     {"contract.Accounts", "contract.Agora", "contract.Transcriber", "contract.Turn", "contract.TurnHandler", "trunk.Housekeeper"}, // Turn: /session 上下文量表(只读内存 peek;turn 在 session 之后启动)
	"leaf/skills":      {"contract.LearnRegistry"},
	"leaf/stoma":       {"contract.AdminLine"},
	"leaf/arrangement": {"contract.Agora", "contract.Gateway", "contract.SessionManager", "trunk.Housekeeper"},
	"leaf/tools":       {"contract.AgoraSend", "contract.Arrangements", "contract.Gateway", "contract.ImageCatalog", "contract.LearnRegistry", "contract.ModelPool", "contract.RetrievalClient", "contract.TakeoverMinter", "contract.URLLibrary", "trunk.Housekeeper"}, // ImageCatalog: image_create 的画风清单+提示词说明，与 modelgate 共用一份
	"leaf/urllib":      {"trunk.Housekeeper"},
	"leaf/warrant":     {"contract.AdminLine", "contract.Broker", "contract.SkillCatalog", "trunk.Housekeeper"},                     // Housekeeper: exec-home retention sweep (D45)
	"leaf/webui":       {"contract.AdminLine", "contract.BrowserControlHold", "contract.BrowserTakeover", "contract.SessionResume"}, // takeover endpoints + /takeover + control-hold/resume + boot admin-token push (absent → degraded)

	// Tree-out foundation modules: registered like leaf/flow but living under
	// heartwood/. Their TryRequire edges were invisible to this guard until the walk
	// was widened to heartwood below.
	"clock": {"contract.DB"},
	"files": {"trunk.Housekeeper"},
}

var tryRequireRe = regexp.MustCompile(`TryRequire\[((?:contract|trunk)\.\w+)\]`)

// lazyDepRe catches optional edges declared through the flows' generic lazy
// resolvers (lazyDep[contract.X] — both flow/normal and flow/agora now use that spelling):
// their internal TryRequire is generic (TryRequire[T]), so the capability name
// only appears at the field declaration. A lazy-resolver type wrapping an
// optional dep MUST be named lazy* or this scan goes blind to its edges —
// TestLazyWrapperNamingConvention enforces that naming.
var lazyDepRe = regexp.MustCompile(`\blazy\w*\[((?:contract|trunk)\.\w+)\]`)

// genericTryRequireRe matches a BARE type-parameter instantiation like TryRequire[T]
// — the body of a generic wrapper around trunk.TryRequire, as opposed to a concrete
// TryRequire[contract.X] / TryRequire[trunk.X] direct edge (tryRequireRe catches those;
// the `[A-Z]` first char excludes the lowercase-prefixed contract./trunk. forms and the
// `func TryRequire[T any]` definition, whose `[T any]` has a space before the `]`).
var genericTryRequireRe = regexp.MustCompile(`TryRequire\[[A-Z]\w*\]`)

// lazyTypeDeclRe matches the declaration of a lazy* generic wrapper type.
var lazyTypeDeclRe = regexp.MustCompile(`type\s+lazy\w*\[`)

// TestOptionalEdges asserts the live set of TryRequire edges matches wantOptional.
func TestOptionalEdges(t *testing.T) {
	root := moduleRootDir(t)
	got := map[string]map[string]bool{}
	for _, top := range []string{"leaf", "flow", "heartwood"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
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
			for _, re := range []*regexp.Regexp{tryRequireRe, lazyDepRe} {
				for _, m := range re.FindAllStringSubmatch(string(data), -1) {
					if got[mod] == nil {
						got[mod] = map[string]bool{}
					}
					got[mod][m[1]] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}

	for mod := range union2(got, wantOptional) {
		gotSet := sortedKeys(got[mod])
		want := append([]string(nil), wantOptional[mod]...)
		sort.Strings(want)
		if !reflect.DeepEqual(gotSet, want) {
			t.Errorf("module %q optional (TryRequire) edges drifted:\n  approved = %v\n  now      = %v\nreview the change and update wantOptional in arch/20-optional_test.go.", mod, want, gotSet)
		}
	}
}

// TestLazyWrapperNamingConvention is a guard on the optional-edge guard. TestOptionalEdges
// discovers optional edges declared through a generic lazy wrapper via lazyDepRe, which
// only matches wrappers NAMED lazy*. A generic TryRequire[T] wrapper by ANY OTHER name
// would carry cross-module soft edges invisible to that scan, slipping past the
// wantOptional approval gate unreviewed. This enforces the naming lazyDepRe's comment
// merely asserts: any file that instantiates a bare TryRequire[T] must also declare a
// lazy* wrapper type. (trunk's own `func TryRequire[T any]` definition lives under trunk/,
// outside this walk, and wouldn't match genericTryRequireRe anyway.)
func TestLazyWrapperNamingConvention(t *testing.T) {
	root := moduleRootDir(t)
	for _, top := range []string{"leaf", "flow", "heartwood"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if genericTryRequireRe.Match(data) && !lazyTypeDeclRe.Match(data) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s instantiates a bare TryRequire[T] but declares no `type lazy…[` wrapper — "+
					"lazyDepRe goes blind to its optional edges, bypassing the wantOptional gate. "+
					"Name the wrapper lazy* (see flow/normal, flow/agora's lazyDep).", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
}

// moduleKeyOf maps a repo-relative file path to its module key ("leaf/<m>" /
// "flow/<m>"), or "" for files not directly under a module (flow/compose is the
// shared library, not a module).
func moduleKeyOf(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 {
		return ""
	}
	// Tree-out foundation modules living under heartwood/ (registered like leaf/flow):
	// their module key is the sub-package name (heartwood/clock → "clock").
	if parts[0] == "heartwood" {
		return parts[1]
	}
	key := parts[0] + "/" + parts[1]
	if key == "flow/compose" {
		return ""
	}
	return key
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func union2(a map[string]map[string]bool, b map[string][]string) map[string]struct{} {
	keys := map[string]struct{}{}
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return keys
}

// TestSkillsRegisteredBeforeWarrant pins the one load-bearing startup ORDER the graph
// guards can't see: warrant's at-Start reconcile TryRequires the SkillCatalog (a SOFT
// edge — invisible to topoOrder, see wantOptional's S-4 note), so skills MUST be
// registered before warrant in cmd/bob/main.go or the reconcile runs against an empty
// catalog and quietly denies every skill grant. Registration order is only the topo
// sorter's FIFO tiebreak — the COMPUTED start order is asserted by
// TestStartOrderSkillsBeforeWarrant (40-registration_test.go); this text check pins
// the tiebreak input at its source.
func TestSkillsRegisteredBeforeWarrant(t *testing.T) {
	root := moduleRootDir(t)
	src, err := os.ReadFile(filepath.Join(root, "cmd", "bob", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/bob/main.go: %v", err)
	}
	body := string(src)
	si := strings.Index(body, "skills.New(")
	wi := strings.Index(body, "warrant.New(")
	if si < 0 || wi < 0 {
		t.Fatalf("could not locate skills.New (%d) / warrant.New (%d) registration in cmd/bob/main.go", si, wi)
	}
	if si > wi {
		t.Error("cmd/bob/main.go registers warrant BEFORE skills — warrant's at-Start skill-grant reconcile (TryRequire[SkillCatalog]) would miss the catalog and deny every skill grant. Register skills first.")
	}
}

// TestPgpoolRegisteredBeforeClock pins the second load-bearing startup ORDER the graph
// guards can't enforce: clock TryRequires the DB (a SOFT edge — see wantOptional's
// "clock": {"contract.DB"} entry), falling OPEN to the host clock if the DB isn't up at
// Start. So pgpool (which provides contract.DB) MUST be registered — and thus started —
// before clock, or every DB-calibrated timestamp silently runs on the (potentially
// skewed) host clock for the run. Same mechanism as the skills/warrant guard above:
// assert on the source registration order directly.
func TestPgpoolRegisteredBeforeClock(t *testing.T) {
	root := moduleRootDir(t)
	src, err := os.ReadFile(filepath.Join(root, "cmd", "bob", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/bob/main.go: %v", err)
	}
	body := string(src)
	pi := strings.Index(body, "pgpool.New(")
	ci := strings.Index(body, "clock.New(")
	if pi < 0 || ci < 0 {
		t.Fatalf("could not locate pgpool.New (%d) / clock.New (%d) registration in cmd/bob/main.go", pi, ci)
	}
	if pi > ci {
		t.Error("cmd/bob/main.go registers clock BEFORE pgpool — clock's TryRequire[DB] would miss the pool at Start and fall open to the host clock, so every DB-calibrated timestamp runs on the host clock. Register pgpool first.")
	}
}

// TestLearnRegisteredBeforeConsumers pins the third load-bearing startup ORDER:
// tools, skills and agora each TryRequire[contract.LearnRegistry] AT START (not via
// a lazy thunk) and call AddSource immediately — their comments say "learn is
// registered before <us> in cmd/bob, so it has started by now". learn's only hard
// Need is the PanelRegistry, so nothing in the graph forces it early; the order is
// held purely by cmd's Register sequence (topo FIFO tiebreak). If learn slipped
// below any consumer, boot would succeed silently with all learning off for the run.
func TestLearnRegisteredBeforeConsumers(t *testing.T) {
	root := moduleRootDir(t)
	src, err := os.ReadFile(filepath.Join(root, "cmd", "bob", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/bob/main.go: %v", err)
	}
	body := string(src)
	// The "(" prefix anchors each token to its t.Register( call site — plain
	// "agora.New(" would also match flowagora.New(.
	li := strings.Index(body, "(learn.New(")
	if li < 0 {
		t.Fatal("could not locate learn.New registration in cmd/bob/main.go")
	}
	for _, consumer := range []string{"(tools.New(", "(skills.New(", "(agora.New("} {
		ci := strings.Index(body, consumer)
		if ci < 0 {
			t.Fatalf("could not locate %s registration in cmd/bob/main.go", strings.Trim(consumer, "("))
		}
		if li > ci {
			t.Errorf("cmd/bob/main.go registers %s BEFORE learn — its at-Start TryRequire[LearnRegistry]+AddSource would miss the registry and silently disable learning for the run. Register learn first.", strings.Trim(consumer, "("))
		}
	}
}
