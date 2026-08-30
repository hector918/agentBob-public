package router

import (
	"context"
	"sync"
	"testing"

	"agentbob/contract"
	"agentbob/trunk"
)

// fakeAccounts is a minimal contract.Accounts for the router tests. flow is the stored
// account.flow column (AccountFor returns it; the router derives entitlement from it via
// contract.EntitledFlows — one read, no FlowsFor). bound = AccountFor ok; paused sets
// Status.
type fakeAccounts struct {
	bound    bool
	flow     string // account.flow ("" = a real bare account with no entitlement)
	flowBlip bool   // true → simulate a store blip (FlowKnown=false): flow UNREAD, not empty
	paused   bool
}

func (f fakeAccounts) AccountFor(context.Context, contract.MessageEvent) (contract.AccountInfo, bool) {
	st := ""
	if f.paused {
		st = "paused"
	}
	return contract.AccountInfo{Flow: f.flow, Status: st, FlowKnown: f.bound && !f.flowBlip}, f.bound
}
func (f fakeAccounts) LangFor(context.Context, string, string) (string, bool) { return "", false }
func (f fakeAccounts) RememberLang(context.Context, string, string, string)   {}

// pickName runs pick and returns the chosen flow name (or "<nil>") plus the deny key.
func pickName(r *Module, ctx context.Context, work contract.TurnWork) (string, string) {
	f, deny := r.pick(ctx, work)
	return flowName(f), deny
}

// withAccounts builds a router whose acc() resolves the given fake.
func withAccounts(fa contract.Accounts, flows ...contract.Flow) *Module {
	reg := trunk.NewRegistry()
	trunk.Provide[contract.Accounts](reg, fa)
	r := New()
	r.reg = reg
	for _, f := range flows {
		r.Register(f)
	}
	return r
}

func flowName(f contract.Flow) string {
	if f == nil {
		return "<nil>"
	}
	return f.Name()
}

func TestRouter_UnboundSplit(t *testing.T) {
	ctx := context.Background()
	r := withAccounts(fakeAccounts{bound: false},
		&recFlow{name: "normal", priority: 0},
		&recFlow{name: "intro", priority: 0, accept: func(contract.TurnWork) bool { return false }}, // never self-selects
	)
	human := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, UserID: "u1", Text: "hi"}

	if got, _ := pickName(r, ctx, contract.TurnWork{Events: []contract.MessageEvent{human}}); got != "intro" {
		t.Fatalf("unbound → want intro, got %v", got)
	}
	// internal-first batch with a human event → still intro (router + intro use the SAME anchor)
	internal := contract.MessageEvent{Source: contract.SourceNameInternal}
	if got, _ := pickName(r, ctx, contract.TurnWork{Events: []contract.MessageEvent{internal, human}}); got != "intro" {
		t.Fatalf("internal-first unbound → want intro, got %v", got)
	}
	// all-internal batch → no human → self-select normal, not intro
	if got, _ := pickName(r, ctx, contract.TurnWork{Events: []contract.MessageEvent{internal}}); got != "normal" {
		t.Fatalf("all-internal → want normal, got %v", got)
	}
	// admin (unbound) → runs normally, not intro (admin skips the intro front gate; normal floor)
	admin := human
	admin.IsAdmin = true
	if got, _ := pickName(r, ctx, contract.TurnWork{Events: []contract.MessageEvent{admin}}); got != "normal" {
		t.Fatalf("admin → want normal, got %v", got)
	}
	// bound + entitled to normal → runs normal, not intro.
	rb := withAccounts(fakeAccounts{bound: true, flow: "normal"},
		&recFlow{name: "normal"},
		&recFlow{name: "intro", accept: func(contract.TurnWork) bool { return false }},
	)
	if got, _ := pickName(rb, ctx, contract.TurnWork{Events: []contract.MessageEvent{human}}); got != "normal" {
		t.Fatalf("bound+normal → want normal, got %v", got)
	}
	// bound but NO flow (a bare onboarding account) → no entitlement → funneled to intro
	// (normal is no longer a free floor).
	rbare := withAccounts(fakeAccounts{bound: true, flow: ""},
		&recFlow{name: "normal"},
		&recFlow{name: "intro", accept: func(contract.TurnWork) bool { return false }},
	)
	if got, _ := pickName(rbare, ctx, contract.TurnWork{Events: []contract.MessageEvent{human}}); got != "intro" {
		t.Fatalf("bound+no-flow → want intro, got %v", got)
	}
}

// TestRouter_PausedFrontGate pins the paused branch of pick()'s front gate: a paused
// non-admin goes to intro (which words the suspension notice itself) BEFORE the path
// runs; with no intro registered suspension fails CLOSED (refused, never the full flow —
// otherwise /accounts pause is inert); an admin is never gated.
func TestRouter_PausedFrontGate(t *testing.T) {
	ctx := context.Background()
	human := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, UserID: "u1", Text: "hi"}
	work := contract.TurnWork{Events: []contract.MessageEvent{human}}

	// bound + entitled + paused non-admin, intro registered → intro (not the full flow).
	r := withAccounts(fakeAccounts{bound: true, flow: "normal", paused: true},
		&recFlow{name: "normal"},
		&recFlow{name: "intro", accept: func(contract.TurnWork) bool { return false }},
	)
	if got, _ := pickName(r, ctx, work); got != "intro" {
		t.Fatalf("paused non-admin → want intro, got %v", got)
	}
	// same paused account, NO intro registered → refused, never the full flow.
	rNoIntro := withAccounts(fakeAccounts{bound: true, flow: "normal", paused: true},
		&recFlow{name: "normal"},
	)
	if got, deny := pickName(rNoIntro, ctx, work); got != "<nil>" || deny != "flow.router.not_permitted" {
		t.Fatalf("paused non-admin without intro → want refused, got flow=%v deny=%q", got, deny)
	}
	// admin + paused → runs normal (admins skip the front gate entirely).
	admin := human
	admin.IsAdmin = true
	if got, _ := pickName(r, ctx, contract.TurnWork{Events: []contract.MessageEvent{admin}}); got != "normal" {
		t.Fatalf("paused admin → want normal, got %v", got)
	}
	// D19: operator pause-immunity spans EVERY path — a paused ADMIN entitled to agora at
	// an agora-wired chat routes to agora, NOT intro/refused. Pins that the front gate's
	// `!ev.IsAdmin` skip is deliberate: an operator is never locked out of any path by a
	// pause row (company/member pause still guards the agora side independently).
	agoraChat := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, UserID: "u1", IsAdmin: true}
	rAgora := withAccounts(fakeAccounts{bound: true, flow: "normal,agora", paused: true},
		&recFlow{name: "normal", priority: 0},
		&recFlow{name: "agora", priority: 10, accept: func(contract.TurnWork) bool { return true }},
		&recFlow{name: "intro", priority: 0, accept: func(contract.TurnWork) bool { return false }},
	)
	if got, deny := pickName(rAgora, ctx, contract.TurnWork{Events: []contract.MessageEvent{agoraChat}}); got != "agora" {
		t.Fatalf("paused admin entitled to agora → want agora (operator immunity), got flow=%v deny=%q", got, deny)
	}
}

// recFlow records the work it was asked to run and reports a fixed accept rule.
type recFlow struct {
	name     string
	priority int
	accept   func(contract.TurnWork) bool
	mode     contract.SessionMode

	mu  sync.Mutex
	ran []contract.TurnWork
}

func (f *recFlow) Name() string  { return f.name }
func (f *recFlow) Priority() int { return f.priority }
func (f *recFlow) Mode() contract.SessionMode {
	if f.mode == "" {
		return contract.SessionModeNone
	}
	return f.mode
}
func (f *recFlow) Accepts(w contract.TurnWork) bool {
	if f.accept == nil {
		return true
	}
	return f.accept(w)
}
func (f *recFlow) RunTurn(_ context.Context, w contract.TurnWork) {
	f.mu.Lock()
	f.ran = append(f.ran, w)
	f.mu.Unlock()
}
func (f *recFlow) DumpPrompt(_ context.Context, w contract.TurnWork) string { return "dump:" + f.name }
func (f *recFlow) count() int                                               { f.mu.Lock(); defer f.mu.Unlock(); return len(f.ran) }

func TestRouter_HighestPriorityFirst(t *testing.T) {
	r := New()
	catchAll := &recFlow{name: "normal", priority: 0} // accepts everything
	special := &recFlow{name: "special", priority: 10, accept: func(w contract.TurnWork) bool {
		return w.Scope == "vip"
	}}
	// Register low-priority first to prove ordering is by Priority, not insertion.
	r.Register(catchAll)
	r.Register(special)

	r.RunTurn(context.Background(), contract.TurnWork{Sid: "s1", Scope: "vip"})
	if special.count() != 1 || catchAll.count() != 0 {
		t.Fatalf("vip work went to special=%d catchAll=%d, want 1/0", special.count(), catchAll.count())
	}

	r.RunTurn(context.Background(), contract.TurnWork{Sid: "s2", Scope: "other"})
	if special.count() != 1 || catchAll.count() != 1 {
		t.Fatalf("non-vip work went to special=%d catchAll=%d, want 1/1", special.count(), catchAll.count())
	}
}

func TestRouter_NoFlowAcceptsDropsWithoutPanic(t *testing.T) {
	r := New()
	r.Register(&recFlow{name: "picky", priority: 0, accept: func(contract.TurnWork) bool { return false }})
	// Must not panic when nothing accepts.
	r.RunTurn(context.Background(), contract.TurnWork{Sid: "s"})
}

func TestRouter_ArbiterSeamsAreStubbed(t *testing.T) {
	r := New()
	if r.Blocked(context.Background(), "any") {
		t.Fatal("single-shot router must never report a session blocked")
	}
	// NotePending is a no-op; just assert it doesn't panic.
	r.NotePending(context.Background(), "any", contract.MessageEvent{}, contract.PendingQueued)
}

// TestRouter_PathPermission pins the path-first + permission-gate semantic: the PATH is
// scope-decided (Accepts, priority), then PERMISSION gates it — a bound caller without
// entitlement for the chosen path is REFUSED (deny key), never rerouted to a lesser flow.
func TestRouter_PathPermission(t *testing.T) {
	ctx := context.Background()
	human := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, UserID: "u1"}
	// normal (pri 0, accepts all) + agora-like (pri 10) registered.
	build := func(fa fakeAccounts, agoraAccepts bool) *Module {
		return withAccounts(fa,
			&recFlow{name: "normal", priority: 0},
			&recFlow{name: "agora", priority: 10, accept: func(contract.TurnWork) bool { return agoraAccepts }},
		)
	}
	work := contract.TurnWork{Events: []contract.MessageEvent{human}}

	// flow=normal, agora-wired chat (agora Accepts) → path=agora, NOT entitled → REFUSED
	// (the prior reroute-to-normal is superseded — no leaking the worker's scope to a stranger).
	if got, deny := pickName(build(fakeAccounts{bound: true, flow: "normal"}, true), ctx, work); got != "<nil>" || deny != "flow.router.not_permitted" {
		t.Fatalf("[normal] at agora chat → want refused, got flow=%v deny=%q", got, deny)
	}
	// flow=normal,agora at agora chat → path=agora, entitled → agora.
	if got, _ := pickName(build(fakeAccounts{bound: true, flow: "normal,agora"}, true), ctx, work); got != "agora" {
		t.Fatalf("[normal,agora] at agora chat → want agora, got %v", got)
	}
	// agora doesn't Accept here (plain chat) → path=normal (floor) → permitted → normal.
	if got, _ := pickName(build(fakeAccounts{bound: true, flow: "normal,agora"}, false), ctx, work); got != "normal" {
		t.Fatalf("plain chat → want normal, got %v", got)
	}
	// KNOWN-empty flow means NO access (a bare onboarding account), NOT a fail-open blip —
	// the floor is gone. No intro registered in this build → refused (not_permitted).
	if got, deny := pickName(build(fakeAccounts{bound: true, flow: ""}, true), ctx, work); got != "<nil>" || deny != "flow.router.not_permitted" {
		t.Fatalf("known-empty flow at agora chat → want refused, got flow=%v deny=%q", got, deny)
	}
	// STORE BLIP (flow UNREAD, FlowKnown=false) now fails CLOSED for a NON-admin on EVERY
	// path (F43, reversing D27): a plain chat and an agora chat both refuse — enforcement
	// must not run a path on an entitlement that wasn't read (self-heals next turn).
	if got, deny := pickName(build(fakeAccounts{bound: true, flow: "", flowBlip: true}, true), ctx, work); got != "<nil>" || deny != "flow.router.not_permitted" {
		t.Fatalf("store blip at agora chat → want refused (fail-closed), got flow=%v deny=%q", got, deny)
	}
	if got, deny := pickName(build(fakeAccounts{bound: true, flow: "", flowBlip: true}, false), ctx, work); got != "<nil>" || deny != "flow.router.not_permitted" {
		t.Fatalf("store blip at plain chat → want refused (fail-closed, F43), got flow=%v deny=%q", got, deny)
	}
	// ...but an ADMIN on a blip still passes the basic floor — platform-known, never gated
	// on a store read, so an accounts hiccup never locks the operator out.
	adminBlip := human
	adminBlip.IsAdmin = true
	if got, deny := pickName(build(fakeAccounts{bound: true, flow: "", flowBlip: true}, false), ctx,
		contract.TurnWork{Events: []contract.MessageEvent{adminBlip}}); got != "normal" || deny != "" {
		t.Fatalf("admin on a store blip at plain chat → want normal (never locked out), got flow=%v deny=%q", got, deny)
	}
	// A blip stays refused even WITH intro registered: the intro funnel is only for a
	// KNOWN-bare account — routing an entitled member to intro on a hiccup would send a
	// false "registered, pending approval" notice.
	rBlipIntro := withAccounts(fakeAccounts{bound: true, flow: "", flowBlip: true},
		&recFlow{name: "normal", priority: 0},
		&recFlow{name: "agora", priority: 10},
		&recFlow{name: "intro", priority: 0, accept: func(contract.TurnWork) bool { return false }},
	)
	if got, deny := pickName(rBlipIntro, ctx, work); got != "<nil>" || deny != "flow.router.not_permitted" {
		t.Fatalf("store blip at agora chat with intro registered → want refused (not intro), got flow=%v deny=%q", got, deny)
	}
	// admin WITHOUT +agora at an agora chat → still REFUSED — admin is a normal-side
	// authority, not an agora one (agora authority is the company's).
	admin := human
	admin.IsAdmin = true
	adminWork := contract.TurnWork{Events: []contract.MessageEvent{admin}}
	if got, deny := pickName(build(fakeAccounts{bound: true, flow: "normal"}, true), ctx, adminWork); got != "<nil>" || deny != "flow.router.not_permitted" {
		t.Fatalf("admin without +agora at agora chat → want refused, got flow=%v deny=%q", got, deny)
	}
}

// TestRouter_ReadOnlyProbesSkipPermission: DumpPrompt (/prompt) and FlowInfo (/session)
// select the scope's PATH but are NOT subject to the runtime permission gate — an admin
// must be able to inspect any scope (e.g. an agora inbox they aren't a member of), even
// when a real turn there would be refused. (Regression guard: pick() gained the gate.)
func TestRouter_ReadOnlyProbesSkipPermission(t *testing.T) {
	ctx := context.Background()
	human := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, UserID: "u1"}
	work := contract.TurnWork{Events: []contract.MessageEvent{human}}
	// account NOT entitled to agora; the agora-like flow Accepts the scope.
	r := withAccounts(fakeAccounts{bound: true, flow: "normal"},
		&recFlow{name: "normal", priority: 0},
		&recFlow{name: "agora", priority: 10, accept: func(contract.TurnWork) bool { return true }},
	)
	// A real turn (pick) REFUSES the non-entitled caller on the agora path.
	if f, deny := r.pick(ctx, work); f != nil || deny != "flow.router.not_permitted" {
		t.Fatalf("pick must refuse non-entitled, got flow=%v deny=%q", flowName(f), deny)
	}
	// But the read-only probes inspect the path regardless of permission.
	if got := r.DumpPrompt(ctx, work); got != "dump:agora" {
		t.Fatalf("DumpPrompt must inspect the scope's path regardless of permission, got %q", got)
	}
	if name, _, ok := r.FlowInfo(ctx, work); !ok || name != "agora" {
		t.Fatalf("FlowInfo must report the path regardless of permission, got %q/%v", name, ok)
	}
}

var _ contract.TurnHandler = (*Module)(nil)
