package warrant

import (
	"context"
	"encoding/json"
	"testing"

	"agentbob/contract"
)

type fakeTool struct{ name string }

func (t fakeTool) Spec() contract.ToolSpec { return contract.ToolSpec{Name: t.name} }
func (fakeTool) Serialize() bool           { return false }
func (fakeTool) Run(context.Context, contract.ToolContext, json.RawMessage) contract.ToolResult {
	return contract.OKResult("")
}

type fakeCat struct {
	byName map[string]contract.Tool
	specs  []contract.ToolSpec
}

func newFakeCat(names ...string) *fakeCat {
	c := &fakeCat{byName: map[string]contract.Tool{}}
	for _, n := range names {
		c.byName[n] = fakeTool{name: n}
		c.specs = append(c.specs, contract.ToolSpec{Name: n})
	}
	return c
}
func (c *fakeCat) Specs() []contract.ToolSpec { return c.specs }
func (c *fakeCat) Lookup(n string) (contract.Tool, bool) {
	t, ok := c.byName[n]
	return t, ok
}

// TestReconcile: the matrix is a pure mirror of registered tools — a new tool
// goes in OFF for everyone (admin flips on later), and a tool that disappears is
// deleted. Round-trips through permissions.yaml; idempotent on a stable set.
func TestReconcile(t *testing.T) {
	home := t.TempDir()
	p := emptyPolicy()
	changed := reconcile(p, "tool", []string{"now", "clarify"}, false)
	if !changed {
		t.Fatal("reconcile of new tools should report a change")
	}
	// New capability: OFF for every principal (no permission-guessing).
	if p.allows("tool:use:now", "admin") || p.allows("tool:use:now", "user") {
		t.Error("a new tool must default OFF for everyone")
	}
	// The row exists (default-off), so a second reconcile is a no-op.
	if reconcile(p, "tool", []string{"now", "clarify"}, false) {
		t.Error("second reconcile over the same tools should be a no-op")
	}
	// Round-trip through permissions.yaml (an admin-flipped grant survives reload).
	p.granted["tool:use:now"]["admin"] = true
	if err := newFileStore(home).Save(p); err != nil {
		t.Fatal(err)
	}
	p2, err := newFileStore(home).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !p2.allows("tool:use:now", "admin") {
		t.Error("reload lost the admin-flipped grant")
	}
	// DELETE: drop "clarify" from the registered set → its row is removed.
	if !reconcile(p2, "tool", []string{"now"}, false) {
		t.Fatal("removing a tool should report a change")
	}
	if _, ok := p2.granted["tool:use:clarify"]; ok {
		t.Error("a tool that's no longer registered must be deleted from the matrix")
	}
	if _, ok := p2.granted["tool:use:now"]; !ok {
		t.Error("a still-registered tool must survive reconcile")
	}
}

// TestReconcileKindIsolation: reconciling one kind never deletes another kind's
// rows (the "<kind>:use:" prefix scoping).
func TestReconcileKindIsolation(t *testing.T) {
	p := emptyPolicy()
	reconcile(p, "tool", []string{"now"}, false)
	reconcile(p, "skill", []string{"summarize"}, false)
	// Reconcile tools again with "now" gone (a NON-empty set that omits it) — "now"
	// is dropped, but the skill row must NOT be touched.
	reconcile(p, "tool", []string{"other"}, false)
	if _, ok := p.granted["tool:use:now"]; ok {
		t.Error("a removed tool should be gone")
	}
	if _, ok := p.granted["skill:use:summarize"]; !ok {
		t.Error("skill row must survive a tool reconcile")
	}
}

// TestReconcileEmptyIsNoPrune: an empty registered set means "don't know" (a
// failed/absent catalog), so the prune is skipped — grants are NOT wiped.
func TestReconcileEmptyIsNoPrune(t *testing.T) {
	p := emptyPolicy()
	reconcile(p, "tool", []string{"now"}, false)
	p.granted["tool:use:now"]["admin"] = true // an admin-flipped grant
	if reconcile(p, "tool", nil, false) {
		t.Error("empty reconcile should report no change (no prune)")
	}
	if _, ok := p.granted["tool:use:now"]; !ok {
		t.Fatal("a transient empty catalog must NOT wipe existing grants")
	}
	if !p.allows("tool:use:now", "admin") {
		t.Error("the admin-flipped grant must survive an empty reconcile")
	}
}

// TestReconcileAddOnlyKeepsUnseen: an add-only reconcile (skills catalog degraded to
// builtins-only) still ADDS new default-off rows but never prunes — the external
// skills' grant rows (incl. admin-flipped ones) survive the degraded window (F63).
func TestReconcileAddOnlyKeepsUnseen(t *testing.T) {
	p := emptyPolicy()
	reconcile(p, "skill", []string{"builtin_a", "external_b"}, false)
	p.granted["skill:use:external_b"]["admin"] = true // an admin-flipped grant
	// Degraded catalog: only the builtin is listed, plus a NEW builtin appears.
	if !reconcile(p, "skill", []string{"builtin_a", "builtin_c"}, true) {
		t.Fatal("add-only reconcile must still add the new capability's row")
	}
	if _, ok := p.granted["skill:use:builtin_c"]; !ok {
		t.Error("add-only reconcile lost the newly-registered capability")
	}
	if !p.allows("skill:use:external_b", "admin") {
		t.Fatal("add-only reconcile must NOT prune the temporarily-unseen skill's grant")
	}
}

// degradedSkillCat is a stub carrying the optional Degraded() capability.
type degradedSkillCat struct {
	stubSkillCat
	degraded bool
}

func (d degradedSkillCat) Degraded() bool { return d.degraded }

// TestSkillCatalogDegraded: the optional type-assert reads the bit when present and
// takes a plain catalog (no Degraded method) at face value.
func TestSkillCatalogDegraded(t *testing.T) {
	if skillCatalogDegraded(stubSkillCat{}) {
		t.Error("a catalog without Degraded() must count as complete")
	}
	if skillCatalogDegraded(degradedSkillCat{degraded: false}) {
		t.Error("Degraded()=false must count as complete")
	}
	if !skillCatalogDegraded(degradedSkillCat{degraded: true}) {
		t.Error("Degraded()=true must be seen through the type-assert")
	}
}

// TestCheckAndAuthorize: Check is fail-closed; Authorize drops tools the
// principal can't use.
func TestCheckAndAuthorize(t *testing.T) {
	p := fromFile(policyFile{
		Principals: []string{"admin", "user"},
		Grants: map[string]map[string]string{
			"tool:use:now":    {"admin": "on", "user": "on"},
			"tool:use:secret": {"admin": "on", "user": "off"},
		},
	})
	cat := newFakeCat("now", "secret")
	m := &Manager{catalog: cat}
	m.policy.Store(p)
	ctx := context.Background()

	if ok, _ := m.Check(ctx, "user", "tool", "now"); !ok {
		t.Error("user should be allowed now")
	}
	if ok, _ := m.Check(ctx, "user", "tool", "secret"); ok {
		t.Error("user should be denied secret")
	}
	if ok, _ := m.Check(ctx, "user", "tool", "ghost"); ok {
		t.Error("unknown capability must fail closed")
	}

	userBag := m.Authorize(ctx, m.Grants(ctx, "user"), cat.Specs())
	if len(userBag.Specs()) != 1 || userBag.Specs()[0].Name != "now" {
		t.Errorf("user bag = %v, want [now]", userBag.Specs())
	}
	if _, ok := userBag.Lookup("secret"); ok {
		t.Error("user bag must not resolve secret")
	}
	adminBag := m.Authorize(ctx, m.Grants(ctx, "admin"), cat.Specs())
	if len(adminBag.Specs()) != 2 {
		t.Errorf("admin bag len = %d, want 2", len(adminBag.Specs()))
	}
}
