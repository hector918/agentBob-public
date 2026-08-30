package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"agentbob/trunk"
)

// scanMainRegistrations parses cmd/bob/main.go and returns the import-path-
// qualified constructor of every t.Register(...) argument, in source order.
// It resolves the two non-uniform shapes: a direct pkg.New(...) argument and a
// variable registered later (credMod := credentials.New(home); t.Register(credMod)).
// Any argument shape it cannot resolve is a Fatal — the scan must fail loud, not
// open, or a new registration style would escape the sync guard.
func scanMainRegistrations(t *testing.T) []string {
	t.Helper()
	root := moduleRootDir(t)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(root, "cmd", "bob", "main.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse cmd/bob/main.go: %v", err)
	}

	// package identifier → import path (aliases like flowagora respected).
	imports := map[string]string{}
	for _, spec := range file.Imports {
		p, uerr := strconv.Unquote(spec.Path.Value)
		if uerr != nil {
			t.Fatalf("unquote import %s: %v", spec.Path.Value, uerr)
		}
		name := path.Base(p)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = p
	}

	// ctorOf resolves a pkg.Fn(...) call expression to "import/path.Fn", or "".
	ctorOf := func(e ast.Expr) string {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return ""
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return ""
		}
		p, ok := imports[pkg.Name]
		if !ok {
			return ""
		}
		return p + "." + sel.Sel.Name
	}

	vars := map[string]string{} // local var name → constructor it was assigned
	var order []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt: // e.g. credMod := credentials.New(home)
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
				if id, ok := n.Lhs[0].(*ast.Ident); ok {
					if c := ctorOf(n.Rhs[0]); c != "" {
						vars[id.Name] = c
					}
				}
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Register" {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != "t" || len(n.Args) != 1 {
				return true
			}
			if c := ctorOf(n.Args[0]); c != "" {
				order = append(order, c)
				return true
			}
			if id, ok := n.Args[0].(*ast.Ident); ok {
				if c := vars[id.Name]; c != "" {
					order = append(order, c)
					return true
				}
			}
			t.Fatalf("cmd/bob/main.go %s: t.Register argument is neither pkg.New*(...) nor a variable assigned from one — extend scanMainRegistrations to resolve it", fset.Position(n.Pos()))
		}
		return true
	})
	return order
}

// TestRegistrationMatchesMain asserts the arch fixture (registeredModules in
// 10-boundary_test.go) equals cmd/bob/main.go's real t.Register sequence — same
// modules, same order. Before this test the sync was comment-only, so a module
// with hard Needs but no Provide/TryRequire sites (flow/inbound's shape) could be
// registered in main.go, forgotten here, and ship with every arch guard green.
// Order matters too: registration order is the trunk sequencer's FIFO tiebreak,
// and TestStartOrderSkillsBeforeWarrant replays the topo sort on the fixture's order.
func TestRegistrationMatchesMain(t *testing.T) {
	got := scanMainRegistrations(t)
	regs := registeredModules()
	want := make([]string, len(regs))
	for i, r := range regs {
		want[i] = r.ctor
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("arch fixture drifted from cmd/bob/main.go's t.Register sequence:\n  main.go = %v\n  fixture = %v\nupdate registeredModules in arch/10-boundary_test.go to match (same modules, SAME order).", got, want)
	}
}

// topoMirror replays the trunk sequencer's start-order computation on the given
// modules: Kahn's algorithm over hard Needs with a FIFO queue, so among
// simultaneously-ready modules registration order is preserved. It MUST stay a
// faithful mirror of trunk/20-lifecycle.go::topoOrder (unexported) — if the
// trunk's tiebreak ever changes, change this too.
func topoMirror(t *testing.T, mods []trunk.Module) []trunk.Module {
	t.Helper()
	n := len(mods)
	providerOf := make(map[reflect.Type]int, n)
	for i, m := range mods {
		for _, c := range m.Provides() {
			if prev, dup := providerOf[c]; dup {
				t.Fatalf("capability %s provided by both %q and %q", c, mods[prev].Name(), m.Name())
			}
			providerOf[c] = i
		}
	}
	indeg := make([]int, n)
	dependents := make([][]int, n)
	for i, m := range mods {
		seen := make(map[int]bool)
		for _, need := range m.Needs() {
			p, ok := providerOf[need]
			if !ok {
				t.Fatalf("module %q needs %s but no module provides it", m.Name(), need)
			}
			if p == i || seen[p] {
				continue
			}
			seen[p] = true
			dependents[p] = append(dependents[p], i)
			indeg[i]++
		}
	}
	queue := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	order := make([]trunk.Module, 0, n)
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		order = append(order, mods[i])
		for _, d := range dependents[i] {
			indeg[d]--
			if indeg[d] == 0 {
				queue = append(queue, d)
			}
		}
	}
	if len(order) != n {
		t.Fatal("dependency cycle among modules")
	}
	return order
}

// TestStartOrderSkillsBeforeWarrant asserts the invariant that actually matters —
// COMPUTED start order, not main.go text position. warrant's at-Start reconcile
// TryRequires the SkillCatalog (soft edge, invisible to the topo sort); the text
// guard TestSkillsRegisteredBeforeWarrant only pins the FIFO tiebreak input, which
// decides ties — if skills ever gained a hard Need on a later-clearing provider,
// warrant could start first while the text guard stayed green. This test builds
// the modules in real registration order (guarded by TestRegistrationMatchesMain)
// and replays the trunk's topo sort.
func TestStartOrderSkillsBeforeWarrant(t *testing.T) {
	order := topoMirror(t, allModules())
	idx := map[string]int{}
	for i, m := range order {
		idx[m.Name()] = i
	}
	si, sok := idx["skills"]
	wi, wok := idx["warrant"]
	if !sok || !wok {
		t.Fatalf("skills/warrant module not found in computed start order %v", idx)
	}
	if si > wi {
		t.Errorf("computed start order runs warrant (#%d) BEFORE skills (#%d) — warrant's at-Start skill-grant reconcile (TryRequire[SkillCatalog]) would miss the catalog and deny every skill grant. Fix the dependency structure or registration order so skills starts first.", wi, si)
	}
}
