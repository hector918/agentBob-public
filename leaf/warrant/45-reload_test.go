package warrant

import (
	"context"
	"os"
	"testing"

	"agentbob/contract"
)

// fakePool records Recycle (Cut) calls so a test can assert which principals were
// revoked on a reload. The reload path only ever calls Recycle, never Acquire.
type fakePool struct{ recycled []string }

func (f *fakePool) Acquire(_ contract.ChannelKey, build func() (contract.Channel, error)) (contract.Channel, error) {
	return build()
}
func (f *fakePool) Recycle(identity string) { f.recycled = append(f.recycled, identity) }

// seedPermsFile writes a permissions.yaml under home (the fileStore backend the
// reload re-reads).
func seedPermsFile(t *testing.T, home, body string) {
	t.Helper()
	if err := os.WriteFile(policyPath(home), []byte(body), 0o600); err != nil {
		t.Fatalf("seed permissions.yaml: %v", err)
	}
}

// TestReloadPolicy_GoodSwap: a reload re-reads permissions.yaml and atomically
// swaps it into the live matrix, so a grant added on disk takes effect without a
// restart.
func TestReloadPolicy_GoodSwap(t *testing.T) {
	home := t.TempDir()
	m := &Manager{home: home, catalog: newFakeCat("now")}
	m.policy.Store(emptyPolicy()) // start fully closed

	ctx := context.Background()
	if ok, _ := m.Check(ctx, "user", "tool", "now"); ok {
		t.Fatal("precondition: empty matrix must deny")
	}

	seedPermsFile(t, home, "principals:\n  - user\ngrants:\n  tool:use:now:\n    user: \"on\"\n")
	if _, err := m.reloadPolicy(); err != nil {
		t.Fatalf("reloadPolicy on a good file: %v", err)
	}
	if ok, _ := m.Check(ctx, "user", "tool", "now"); !ok {
		t.Error("after reload the on-disk grant must take effect")
	}
}

// TestReloadPolicy_BrokenKeepsOld: a parse error returns without swapping — the
// good live matrix is kept (never replaced by a fail-closed one) and the error
// surfaces to the operator.
func TestReloadPolicy_BrokenKeepsOld(t *testing.T) {
	home := t.TempDir()
	good := fromFile(policyFile{
		Principals: []string{"user"},
		Grants:     map[string]map[string]string{"tool:use:now": {"user": "on"}},
	})
	m := &Manager{home: home, catalog: newFakeCat("now")}
	m.policy.Store(good)

	seedPermsFile(t, home, "grants: {unclosed") // invalid yaml
	if _, err := m.reloadPolicy(); err == nil {
		t.Fatal("reloadPolicy on a broken file must return an error")
	}
	ctx := context.Background()
	if ok, _ := m.Check(ctx, "user", "tool", "now"); !ok {
		t.Error("a broken reload must keep the prior good matrix")
	}
	if m.policyBroken.Load() {
		t.Error("a refused (non-swapping) reload must not flip policyBroken")
	}
}

// TestLostGrants: the diff returns exactly the principals whose grant set shrank
// (a principal that lost ONE capability is included even if it kept others), and an
// additive change cuts nobody.
func TestLostGrants(t *testing.T) {
	before := fromFile(policyFile{
		Principals: []string{"alice", "bob"},
		Grants: map[string]map[string]string{
			"tool:use:now":        {"alice": "on", "bob": "on"},
			"credential:use:prod": {"alice": "on"},
		},
	})
	// alice loses credential:use:prod but keeps tool:use:now; bob is unchanged.
	after := fromFile(policyFile{
		Principals: []string{"alice", "bob"},
		Grants:     map[string]map[string]string{"tool:use:now": {"alice": "on", "bob": "on"}},
	})
	if got := lostGrants(before, after); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("lostGrants = %v, want [alice] (lost prod, kept now)", got)
	}
	// Additive (after ⊇ before): nobody loses anything.
	if got := lostGrants(after, before); len(got) != 0 {
		t.Errorf("an additive reload must cut nobody, got %v", got)
	}
}

// TestReloadPolicy_DiffCutRevokes: a reload that REMOVES a grant recycles only the
// revoked principal's live channels — a still-granted principal is never cut.
func TestReloadPolicy_DiffCutRevokes(t *testing.T) {
	home := t.TempDir()
	fp := &fakePool{}
	m := &Manager{home: home, catalog: newFakeCat("now"), pool: fp}
	m.policy.Store(fromFile(policyFile{
		Principals: []string{"alice", "bob"},
		Grants:     map[string]map[string]string{"tool:use:now": {"alice": "on", "bob": "on"}},
	}))

	// On-disk edit drops alice; bob keeps the grant.
	seedPermsFile(t, home, "principals:\n  - alice\n  - bob\ngrants:\n  tool:use:now:\n    bob: \"on\"\n")
	if _, err := m.reloadPolicy(); err != nil {
		t.Fatalf("reloadPolicy: %v", err)
	}
	if len(fp.recycled) != 1 || fp.recycled[0] != "alice" {
		t.Fatalf("diff-cut recycled = %v, want [alice] only (bob kept the grant)", fp.recycled)
	}
}
