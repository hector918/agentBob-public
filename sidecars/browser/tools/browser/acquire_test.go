package browser

import (
	"context"
	"strings"
	"testing"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/core"
)

// fakeGate allows credential:use:<name> only for names in `allow`.
type fakeGate struct{ allow map[string]bool }

func (g fakeGate) Visible(context.Context, string, string) bool { return true }
func (g fakeGate) Check(_ context.Context, kind, name string) core.PermDecision {
	if kind == "credential" && g.allow[name] {
		return core.PermDecision{Kind: core.PermAllow}
	}
	return core.PermDecision{Kind: core.PermDeny, Reason: "test deny"}
}

func newTestPool(t *testing.T) *Pool {
	t.Helper()
	p := NewPool(config.BrowserConfig{}, config.FilesystemConfig{}, false)
	t.Cleanup(p.CloseAll) // also stops the custodian goroutine
	return p
}

// TestProfileRoute pins the opt-in routing: legacy unless (account bound AND
// gate wired AND profile credential granted); granted → namespaced key + vault dir.
func TestProfileRoute(t *testing.T) {
	ctx := context.Background()
	p := newTestPool(t)

	// No gate wired → legacy regardless of account (reducible leaf).
	if useP, _, _, err := p.profileRoute(ctx, core.AgentCtx{AccountID: "ac_hector"}); useP || err != nil {
		t.Fatalf("nil gate must route legacy; got useProfile=%v err=%v", useP, err)
	}

	// Capability NOT granted → legacy (opt-in fallback, not a denial).
	p.SetProfileGate(fakeGate{allow: map[string]bool{}})
	if useP, _, _, _ := p.profileRoute(ctx, core.AgentCtx{AccountID: "ac_hector"}); useP {
		t.Fatal("without the browser-profile capability, must route legacy")
	}

	p.SetProfileGate(fakeGate{allow: map[string]bool{"browser-profile": true}})

	// No identity (no account / no member) → legacy even with the capability.
	if useP, _, _, _ := p.profileRoute(ctx, core.AgentCtx{AccountID: ""}); useP {
		t.Fatal("empty identity must route legacy")
	}
	// Capability granted + an account → profile path: per-identity key + vault dir.
	useP, key, dir, err := p.profileRoute(ctx, core.AgentCtx{AccountID: "ac_hector"})
	if err != nil || !useP {
		t.Fatalf("granted account must route profile; got useProfile=%v err=%v", useP, err)
	}
	if key != "browser_ac_hector" {
		t.Fatalf("profile key = %q, want browser_ac_hector (namespaced — no chat-scope collision)", key)
	}
	if !strings.Contains(dir, "browser-profiles") {
		t.Fatalf("profile dir = %q, want a path under the browser-profiles vault subtree", dir)
	}

	// Agora: MemberID takes precedence over AccountID — the browser login
	// belongs to the member the turn acts as, not the triggering person.
	p.SetProfileGate(fakeGate{allow: map[string]bool{"browser-profile": true}})
	useP, key, _, err = p.profileRoute(ctx, core.AgentCtx{MemberID: "mem_ocr", AccountID: "ac_someone"})
	if err != nil || !useP {
		t.Fatalf("granted member must route profile; got useProfile=%v err=%v", useP, err)
	}
	if key != "browser_mem_ocr" {
		t.Fatalf("agora profile key = %q, want browser_mem_ocr (member, not the triggerer's account)", key)
	}
}

// TestAcquireFor_Busy: when the profile is held by another scope, AcquireFor
// fail-fasts WITHOUT spawning chromium — so it's exercisable headless.
func TestAcquireFor_Busy(t *testing.T) {
	ctx := context.Background()
	p := newTestPool(t)
	p.SetProfileGate(fakeGate{allow: map[string]bool{"browser-profile": true}})

	if !p.custodian.Checkout("browser_ac_hector", "scope-other") {
		t.Fatal("precondition: another scope should hold the profile")
	}
	_, err := p.AcquireFor(ctx, core.AgentCtx{AccountID: "ac_hector", SessionScope: "scope-mine"})
	if err == nil {
		t.Fatal("AcquireFor must fail-fast when the profile is held by another scope")
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Fatalf("error = %q, want a 'busy' fail-fast", err.Error())
	}
}

// TestPurgeSession_ReleasesProfileCheckout pins the release door: PurgeSession
// (the /close-session + agora-finalize purger path) frees the profile checkout
// the scope held, so another scope can then claim it. The profile dir itself is
// NOT deleted (release ≠ logout) — but here we only assert the hold is freed.
func TestPurgeSession_ReleasesProfileCheckout(t *testing.T) {
	p := newTestPool(t)

	if !p.custodian.Checkout("browser_ac_hector", "scopeA") {
		t.Fatal("precondition checkout failed")
	}
	// Another scope is blocked while scopeA holds it.
	if p.custodian.Checkout("browser_ac_hector", "scopeB") {
		t.Fatal("scopeB should be blocked before purge")
	}
	p.PurgeSession("scopeA") // the release door
	// Now free → scopeB can claim it.
	if !p.custodian.Checkout("browser_ac_hector", "scopeB") {
		t.Fatal("PurgeSession(scopeA) must release the profile checkout it held")
	}
}
