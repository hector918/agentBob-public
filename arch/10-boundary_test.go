package arch

import (
	"go/build"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	flowagora "agentbob/flow/agora"
	"agentbob/flow/inbound"
	"agentbob/flow/intro"
	"agentbob/flow/normal"
	"agentbob/flow/router"
	"agentbob/heartwood/clock"
	"agentbob/heartwood/files"
	"agentbob/heartwood/prompt"
	"agentbob/leaf/accounts"
	"agentbob/leaf/adminline"
	"agentbob/leaf/agora"
	"agentbob/leaf/arrangement"
	"agentbob/leaf/asr"
	"agentbob/leaf/claimtoken"
	"agentbob/leaf/credentials"
	"agentbob/leaf/gate"
	"agentbob/leaf/learn"
	"agentbob/leaf/model"
	"agentbob/leaf/modelgate"
	"agentbob/leaf/pgpool"
	"agentbob/leaf/retrieval"
	"agentbob/leaf/session"
	"agentbob/leaf/skills"
	"agentbob/leaf/slash"
	"agentbob/leaf/stoma"
	"agentbob/leaf/tools"
	"agentbob/leaf/turn"
	"agentbob/leaf/urllib"
	"agentbob/leaf/warrant"
	"agentbob/leaf/webui"
	"agentbob/trunk"
)

// node is one module's trunk connections: the capabilities it publishes and the
// ones it consumes. Across all modules these edges ARE the inter-module wiring —
// the only sanctioned way one module reaches another.
type node struct {
	provides []string
	needs    []string
}

// wantGraph is the APPROVED module connection graph. TestModuleGraph fails on any
// drift — a module that gains or loses a Provides/Needs edge, or a module added
// or removed. That failure is the point: a new inter-module connection must be
// reviewed (read the diff) and approved HERE before it ships.
//
// To approve a change: confirm the new edge is intended, then edit this map to
// match. Do NOT blindly update it to turn a red test green — the red IS the
// review gate. When you add a new module, add it here too (and to registeredModules).
//
// OPTIONAL edges (TryRequire, resolved lazily) are NOT in this graph — they are
// not declared Needs, so reflection can't see them. They are now MACHINE-CHECKED
// in 20-optional_test.go::wantOptional (TestOptionalEdges scans every module's
// TryRequire[contract.X]/[trunk.X] sites and fails on drift) — that map is the
// inventory; each edge's degradation rationale lives in a comment at its
// resolution site. (This replaced a hand-written list here that had silently
// rotted — S-2.)
//
// The trunk's Housekeeper (mechanism 4) is provided by the TRUNK, not a module —
// modules resolve it with TryRequire[trunk.Housekeeper] and register sweeps; it
// is invisible to this module graph (e.g. adminline registers a prune task).
var wantGraph = map[string]node{
	"pgpool": {provides: []string{"contract.DB"}},
	"clock":  {}, // no Provides/Needs — soft-TryRequires contract.DB to calibrate the process clock

	"claimtoken":    {provides: []string{"contract.ClaimTokens"}},
	"gate":          {provides: []string{"contract.AccessGranter", "contract.Screener"}, needs: []string{"contract.ClaimTokens", "contract.PanelRegistry", "contract.SlashRegistry"}},
	"webui":         {provides: []string{"contract.PanelRegistry", "contract.TakeoverMinter"}, needs: []string{"contract.ClaimTokens", "contract.SlashRegistry"}},
	"model":         {provides: []string{"contract.ImageCatalog", "contract.ModelPool"}, needs: []string{"contract.DB", "contract.PanelRegistry", "contract.SlashRegistry"}},
	"asr":           {provides: []string{"contract.Transcriber"}}, // soft-TryRequires contract.ModelPool (KindASR backend)
	"accounts":      {provides: []string{"contract.APIKeys", "contract.AccountProvisioner", "contract.Accounts", "contract.ConsumptionReporter"}, needs: []string{"contract.ClaimTokens", "contract.DB", "contract.PanelRegistry", "contract.SlashRegistry"}},
	"modelgate":     {needs: []string{"contract.ModelPool"}}, // + optional contract.APIKeys / contract.PanelRegistry (TryRequire)
	"adminline":     {provides: []string{"contract.AdminLine"}, needs: []string{"contract.DB", "contract.Gateway"}},
	"credentials":   {provides: []string{"contract.Broker"}},
	"slash":         {provides: []string{"contract.SlashRegistry"}},
	"session":       {provides: []string{"contract.ChatHistory", "contract.MessageIndexer", "contract.MessageStore", "contract.SessionManager", "contract.SessionResume"}, needs: []string{"contract.DB", "contract.Gateway", "contract.PanelRegistry", "contract.SlashRegistry"}},
	"prompt":        {provides: []string{"contract.PromptFactory"}},
	"turn":          {provides: []string{"contract.Turn"}, needs: []string{"contract.MessageStore", "contract.ModelPool"}},
	"flow-router":   {provides: []string{"contract.FlowRegistry", "contract.TurnHandler"}},
	"flow-normal":   {needs: []string{"contract.FlowRegistry", "contract.Gateway", "contract.PanelRegistry", "contract.PromptFactory", "contract.Turn"}},
	"flow-agora":    {needs: []string{"contract.FlowRegistry", "contract.Gateway", "contract.PromptFactory", "contract.Turn"}},
	"flow-intro":    {needs: []string{"contract.FlowRegistry", "contract.Gateway"}},
	"learn":         {provides: []string{"contract.LearnRegistry"}, needs: []string{"contract.PanelRegistry"}},
	"urllib":        {provides: []string{"contract.URLLibrary"}, needs: []string{"contract.DB"}},
	"retrieval":     {provides: []string{"contract.RetrievalClient", "contract.RetrievalFeed"}, needs: []string{"contract.DB"}},
	"skills":        {provides: []string{"contract.SkillCatalog", "contract.SkillFailureSink"}, needs: []string{"contract.PanelRegistry"}},
	"tools":         {provides: []string{"contract.BrowserControlHold", "contract.ChannelPool", "contract.ToolCatalog"}, needs: []string{"contract.PanelRegistry"}},
	"warrant":       {provides: []string{"contract.Warrant"}, needs: []string{"contract.ChannelPool", "contract.PanelRegistry", "contract.SlashRegistry", "contract.ToolCatalog"}},
	"agora":         {provides: []string{"contract.Agora", "contract.AgoraSend", "contract.MemberFailureSink"}, needs: []string{"contract.ClaimTokens", "contract.DB", "contract.PanelRegistry", "contract.SlashRegistry"}},
	"stoma":         {provides: []string{"contract.Gateway"}, needs: []string{"contract.PanelRegistry"}},
	"arrangement":   {provides: []string{"contract.Arrangements"}, needs: []string{"contract.DB", "contract.PanelRegistry", "contract.SlashRegistry"}},
	"inbound":       {needs: []string{"contract.Gateway", "contract.Screener", "contract.SlashRegistry"}},
	"files-sweeper": {}, // no Provides/Needs — only TryRequires the trunk Housekeeper
}

// registration pairs a constructed module with its import-path-qualified
// constructor as called in cmd/bob/main.go, so the fixture is machine-checkable
// against the real source (see TestRegistrationMatchesMain).
type registration struct {
	ctor string // e.g. "agentbob/leaf/pgpool.New"
	mod  trunk.Module
}

// registeredModules mirrors cmd/bob/main.go's t.Register sequence: the same
// modules in the SAME ORDER (Provides/Needs are static — the constructor args
// don't affect them). The sync is not comment-only: TestRegistrationMatchesMain
// (40-registration_test.go) scans main.go's actual t.Register calls and fails on
// any set OR order drift, and TestStartOrderSkillsBeforeWarrant replays the
// trunk's topo sort on this order (registration order is the sequencer's FIFO
// tiebreak, so it must be the real one).
func registeredModules() []registration {
	return []registration{
		{"agentbob/heartwood/files.NewSweeper", files.NewSweeper(nil, 0, 0, "")},
		{"agentbob/leaf/pgpool.New", pgpool.New("")},
		{"agentbob/heartwood/clock.New", clock.New()},
		{"agentbob/leaf/claimtoken.New", claimtoken.New()},
		{"agentbob/leaf/gate.New", gate.New("")},
		{"agentbob/leaf/webui.New", webui.New("")},
		{"agentbob/leaf/model.New", model.New("")},
		{"agentbob/leaf/asr.New", asr.New()},
		{"agentbob/leaf/credentials.New", credentials.New("")},
		{"agentbob/leaf/accounts.New", accounts.New()},
		{"agentbob/leaf/modelgate.New", modelgate.New("")},
		{"agentbob/leaf/slash.New", slash.New()},
		{"agentbob/leaf/session.New", session.New(session.Config{})},
		{"agentbob/heartwood/prompt.New", prompt.New()},
		{"agentbob/leaf/learn.New", learn.New()},
		{"agentbob/leaf/urllib.New", urllib.New()},
		{"agentbob/leaf/retrieval.New", retrieval.New(retrieval.Config{})},
		{"agentbob/leaf/tools.New", tools.New("")},
		{"agentbob/leaf/skills.New", skills.New("")},
		{"agentbob/leaf/warrant.New", warrant.New("")},
		{"agentbob/leaf/agora.New", agora.New("")},
		{"agentbob/leaf/arrangement.New", arrangement.New()},
		{"agentbob/leaf/turn.New", turn.New()},
		{"agentbob/flow/router.New", router.New()},
		{"agentbob/flow/normal.New", normal.New("")},
		{"agentbob/flow/agora.New", flowagora.New("")},
		{"agentbob/flow/intro.New", intro.New()},
		{"agentbob/leaf/stoma.New", stoma.New(nil)},
		{"agentbob/leaf/adminline.New", adminline.New("")},
		{"agentbob/flow/inbound.New", inbound.New()},
	}
}

// allModules returns every registered module, in registration order.
func allModules() []trunk.Module {
	regs := registeredModules()
	mods := make([]trunk.Module, len(regs))
	for i, r := range regs {
		mods[i] = r.mod
	}
	return mods
}

// TestModuleGraph asserts the live module connection graph matches the approved
// wantGraph. A red test means some module's trunk Provides/Needs changed — review
// the reported edge and, if intended, update wantGraph.
func TestModuleGraph(t *testing.T) {
	got := map[string]node{}
	for _, m := range allModules() {
		name := m.Name()
		if _, dup := got[name]; dup {
			t.Fatalf("two modules share the name %q — names must be unique", name)
		}
		got[name] = node{provides: typeNames(m.Provides()), needs: typeNames(m.Needs())}
	}

	for name := range union(got, wantGraph) {
		g, inGot := got[name]
		w, inWant := wantGraph[name]
		switch {
		case inGot && !inWant:
			t.Errorf("NEW module %q (provides=%v needs=%v) is not in wantGraph — review its connections and add it.", name, g.provides, g.needs)
		case !inGot && inWant:
			t.Errorf("module %q is in wantGraph but no longer constructed — remove it from wantGraph (and allModules) if intended.", name)
		default:
			if !reflect.DeepEqual(g.provides, w.provides) {
				t.Errorf("module %q Provides changed: approved=%v now=%v — a new published capability is an inter-module connection; review then update wantGraph.", name, w.provides, g.provides)
			}
			if !reflect.DeepEqual(g.needs, w.needs) {
				t.Errorf("module %q Needs changed: approved=%v now=%v — a new dependency is an inter-module connection; review then update wantGraph.", name, w.needs, g.needs)
			}
		}
	}
}

// TestModuleImportBoundary asserts no package under leaf/ or flow/ imports a
// DIFFERENT leaf/flow module directly (sub-packages of the same module are fine).
// Modules couple ONLY through contract interfaces discovered via the trunk; a
// direct cross-module import is a trunk-bypassing connection that TestModuleGraph
// can't see, so it is forbidden outright (no approval ledger — use the trunk).
func TestModuleImportBoundary(t *testing.T) {
	root := moduleRootDir(t)
	// heartwood/ is the tree-out foundation (clock, files, prompt, …) used by leaf
	// modules as direct-import UTILITIES (clock.Now, files.Store, prompt.SanitizeSpeaker)
	// — like contract/trunk, so it stays a valid import target (moduleOf returns "" for
	// it). We DO walk it as a source here so a foundation file that reached INTO a
	// leaf/flow module would be caught, matching TestOptionalEdges' coverage.
	for _, top := range []string{"leaf", "flow", "heartwood"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(dir string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			pkg, perr := build.ImportDir(dir, 0)
			if perr != nil {
				return nil // no buildable (non-test) Go files here — nothing to check
			}
			rel, _ := filepath.Rel(root, dir)
			self := "agentbob/" + filepath.ToSlash(rel)
			selfMod := moduleOf(self)
			for _, imp := range pkg.Imports { // production imports only (test files excluded)
				if m := moduleOf(imp); m != "" && m != selfMod {
					t.Errorf("%s imports %s — a leaf/flow module must not reach across to another module; couple via a contract interface through the trunk instead.", self, imp)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
}

// moduleOf returns the leaf/flow module root of an import path
// (agentbob/leaf/session/messages → agentbob/leaf/session), or "" if the path is
// not under leaf/ or flow/ (contract, trunk, shared infra — always allowed).
//
// flow/compose is NOT a module — it is the SHARED turn-composition library both
// flows reuse (architecture-vision §7: a flow copies orchestration, not logic; the
// logic lives once in compose). It registers no trunk.Module and couples to nothing;
// it is shared infra like contract/trunk, so it is allowed to be imported across
// flows and is excluded here.
func moduleOf(importPath string) string {
	if importPath == "agentbob/flow/compose" {
		return ""
	}
	parts := strings.Split(importPath, "/")
	if len(parts) >= 3 && parts[0] == "agentbob" && (parts[1] == "leaf" || parts[1] == "flow") {
		return strings.Join(parts[:3], "/")
	}
	return ""
}

// typeNames renders capability types as sorted, comparable strings ("contract.DB").
// Empty → nil so a module with no edges compares equal to an omitted wantGraph field.
func typeNames(ts []reflect.Type) []string {
	if len(ts) == 0 {
		return nil
	}
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.String())
	}
	sort.Strings(out)
	return out
}

func union(a, b map[string]node) map[string]struct{} {
	keys := map[string]struct{}{}
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return keys
}

// moduleRootDir walks up from the test's working directory to the dir holding
// go.mod (the module root).
func moduleRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test working directory")
		}
		dir = parent
	}
}
