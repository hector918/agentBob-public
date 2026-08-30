package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agentbob/contract"
)

// fakeBridge satisfies contract.Source (so a fake gateway can return it) AND
// contract.MessageEmitter (so send_message can emit through it).
type fakeBridge struct {
	got []contract.MessageEvent
	err error
}

func (b *fakeBridge) Emit(ev contract.MessageEvent) error {
	if b.err != nil {
		return b.err
	}
	b.got = append(b.got, ev)
	return nil
}
func (b *fakeBridge) Name() string                                            { return contract.SourceNameInternal }
func (b *fakeBridge) Caps() contract.Caps                                     { return contract.Caps{} }
func (b *fakeBridge) Run(context.Context, chan<- contract.MessageEvent) error { return nil }
func (b *fakeBridge) NewSink(context.Context, contract.Target, string, string, contract.SinkPrefs) contract.Sink {
	return nil
}
func (b *fakeBridge) SendButtons(context.Context, contract.Target, string, []contract.Button) (string, error) {
	return "", nil
}
func (b *fakeBridge) Send(context.Context, contract.Target, string) error             { return nil }
func (b *fakeBridge) SendFile(context.Context, contract.Target, string, string) error { return nil }
func (b *fakeBridge) HealthCheck(context.Context) error                               { return nil }

type fakeGW struct{ src contract.Source }

func (f fakeGW) Events() <-chan contract.MessageEvent { return nil }
func (f fakeGW) SourceNames() []string                { return nil }
func (f fakeGW) SourceByName(name string) contract.Source {
	if name == contract.SourceNameInternal {
		return f.src
	}
	return nil
}

func runSend(gw contract.Gateway, body string) contract.ToolResult {
	tool := sendMessage{gw: func() contract.Gateway { return gw }}
	return tool.Run(context.Background(), contract.ToolContext{}, json.RawMessage(body))
}

func TestSendMessageEmits(t *testing.T) {
	fb := &fakeBridge{}
	res := runSend(fakeGW{src: fb}, `{"to":"telegram:dm:9","body":"hi"}`)
	if !res.OK {
		t.Fatalf("not OK: %+v", res)
	}
	if len(fb.got) != 1 {
		t.Fatalf("emitted %d, want 1", len(fb.got))
	}
	if fb.got[0].ChatID != "telegram:dm:9" || fb.got[0].Text != "hi" {
		t.Errorf("bad event: %+v", fb.got[0])
	}
}

// The emitted event must carry CallerSessionID = tc.Sid — the caller this
// dispatch returns to (the session-graph return edge, ④b-2).
func TestSendMessageStampsCaller(t *testing.T) {
	fb := &fakeBridge{}
	tool := sendMessage{gw: func() contract.Gateway { return fakeGW{src: fb} }}
	res := tool.Run(context.Background(), contract.ToolContext{Sid: "sid_A"}, json.RawMessage(`{"to":"telegram:dm:9","body":"hi"}`))
	if !res.OK {
		t.Fatalf("not OK: %+v", res)
	}
	if len(fb.got) != 1 {
		t.Fatalf("emitted %d, want 1", len(fb.got))
	}
	if fb.got[0].Dispatch == nil || fb.got[0].Dispatch.CallerSessionID != "sid_A" {
		t.Errorf("Dispatch = %+v, want CallerSessionID sid_A", fb.got[0].Dispatch)
	}
}

// force_new sets the dispatch frame's ForceNewSession so the router opens a FRESH
// session at the target instead of resuming the active one (an agent's "new task").
func TestSendMessageForceNew(t *testing.T) {
	fb := &fakeBridge{}
	tool := sendMessage{gw: func() contract.Gateway { return fakeGW{src: fb} }}
	res := tool.Run(context.Background(), contract.ToolContext{Sid: "sid_A"}, json.RawMessage(`{"to":"telegram:dm:9","body":"hi","force_new_session":true}`))
	if !res.OK {
		t.Fatalf("not OK: %+v", res)
	}
	if len(fb.got) != 1 || fb.got[0].Dispatch == nil || !fb.got[0].Dispatch.ForceNewSession {
		t.Errorf("Dispatch = %+v, want ForceNewSession true", fb.got[0].Dispatch)
	}
}

func TestSendMessageBusyIsError(t *testing.T) {
	res := runSend(fakeGW{src: &fakeBridge{err: contract.ErrBridgeBusy}}, `{"to":"x","body":"y"}`)
	if res.OK {
		t.Error("want failure when bridge busy")
	}
}

func TestSendMessageEmptyArgs(t *testing.T) {
	res := runSend(fakeGW{src: &fakeBridge{}}, `{"to":"","body":""}`)
	if res.OK {
		t.Error("want failure on empty to/body")
	}
}

func TestSendMessageNoGateway(t *testing.T) {
	res := runSend(nil, `{"to":"x","body":"y"}`)
	if res.OK {
		t.Error("want failure when gateway absent")
	}
}

// ---- Stage B (fake AgoraSend) ----

// fakeAgoraSend implements contract.AgoraSend: ResolveTarget maps "bob" → a fixed
// scope; CheckSend denies when targetScope == denyScope; ResolveCredentialName
// echoes the name unless empty.
type fakeAgoraSend struct {
	resolveTo   string // what ResolveTarget returns for a name (non-scope) `to`
	resolveErr  error  // when set, ResolveTarget errors (→ Stage A fallback)
	denyScope   string // CheckSend denies this exact targetScope
	bridgeScope string // ResolveBridge returns this (+ deliverable when non-empty)
}

func (f fakeAgoraSend) ResolveTarget(_ context.Context, _, toArg string) (string, string, error) {
	if f.resolveErr != nil {
		return "", "", f.resolveErr
	}
	if f.resolveTo != "" {
		return f.resolveTo, "fake", nil
	}
	return toArg, "verbatim", nil
}
func (f fakeAgoraSend) CheckSend(_ context.Context, _, targetScope string) (string, string, error) {
	if targetScope == f.denyScope {
		return "", "denied by fake", nil
	}
	return targetScope, "", nil
}
func (f fakeAgoraSend) ResolveCredentialName(_ context.Context, _, asName string) (string, error) {
	return asName, nil
}
func (f fakeAgoraSend) ResolveBridge(_ context.Context, _ string) (string, bool, error) {
	return f.bridgeScope, f.bridgeScope != "", nil // deliverable iff a bridge is set
}

// runSendB drives send_message as an AGORA caller (a company:role principal on
// ctx) — the only path that triggers Stage B (target resolution + channel auth).
func runSendB(gw contract.Gateway, as contract.AgoraSend, scope, body string) contract.ToolResult {
	ctx := contract.WithIdentity(context.Background(), "company:acme/role:boss")
	tool := sendMessage{
		gw:    func() contract.Gateway { return gw },
		agora: func() contract.AgoraSend { return as },
	}
	return tool.Run(ctx, contract.ToolContext{Scope: scope}, json.RawMessage(body))
}

func TestSendMessageStageBNameResolves(t *testing.T) {
	fb := &fakeBridge{}
	as := fakeAgoraSend{resolveTo: "co:acme/bob"}
	res := runSendB(fakeGW{src: fb}, as, "co:acme/alice", `{"to":"bob","body":"hi"}`)
	if !res.OK {
		t.Fatalf("not OK: %+v", res)
	}
	if len(fb.got) != 1 || fb.got[0].ChatID != "co:acme/bob" {
		t.Fatalf("want emit to resolved scope co:acme/bob, got %+v", fb.got)
	}
}

func TestSendMessageStageBDenyIsError(t *testing.T) {
	fb := &fakeBridge{}
	as := fakeAgoraSend{resolveTo: "co:globex/erin", denyScope: "co:globex/erin"}
	res := runSendB(fakeGW{src: fb}, as, "co:acme/bob", `{"to":"erin","body":"hi"}`)
	if res.OK {
		t.Fatal("want denial when CheckSend denies")
	}
	if len(fb.got) != 0 {
		t.Fatalf("must not emit on denial, got %+v", fb.got)
	}
}

func TestSendMessageStageBResolveErrDenies(t *testing.T) {
	// An AGORA caller whose `to` can't be resolved must be DENIED — it must NOT fall
	// through to the unchecked raw-emit path (that would evade channel auth).
	fb := &fakeBridge{}
	as := fakeAgoraSend{resolveErr: errors.New("no such member")}
	res := runSendB(fakeGW{src: fb}, as, "co:acme/alice", `{"to":"telegram:dm:9","body":"hi"}`)
	if res.OK {
		t.Fatal("agora caller with unresolvable target must be denied, not raw-emitted")
	}
	if len(fb.got) != 0 {
		t.Fatalf("must not emit on resolve failure, got %+v", fb.got)
	}
}

// TestSendMessageStageBRawScopeStillChecked: an agora caller passing a raw scope
// `to` (ResolveTarget returns it verbatim) is STILL channel-checked — no bypass.
func TestSendMessageStageBRawScopeStillChecked(t *testing.T) {
	fb := &fakeBridge{}
	as := fakeAgoraSend{denyScope: "co:globex/erin"} // resolveTo empty → verbatim passthrough
	res := runSendB(fakeGW{src: fb}, as, "co:acme/alice", `{"to":"co:globex/erin","body":"hi"}`)
	if res.OK {
		t.Fatal("raw-scope target from an agora caller must still pass CheckSend")
	}
	if len(fb.got) != 0 {
		t.Fatalf("must not emit on denial, got %+v", fb.got)
	}
}

// TestSendMessageAsRejected: `as=` isn't carriable yet → rejected, not silently dropped.
func TestSendMessageAsRejected(t *testing.T) {
	fb := &fakeBridge{}
	res := runSendB(fakeGW{src: fb}, fakeAgoraSend{resolveTo: "co:acme/bob"}, "co:acme/alice", `{"to":"bob","body":"hi","as":"botoken"}`)
	if res.OK {
		t.Fatal("as= must be rejected until credential carry lands")
	}
}

// An AGORA caller resolves the target via AgoraSend and emits to the resolved scope
// (Stage B; loop/fan-out is handled by org permissions + the model pool, no runtime
// dispatch gate).
func TestSendMessageAgoraResolvesAndEmits(t *testing.T) {
	fb := &fakeBridge{}
	ctx := contract.WithIdentity(context.Background(), "company:acme/role:boss")
	tool := sendMessage{
		gw:    func() contract.Gateway { return fakeGW{src: fb} },
		agora: func() contract.AgoraSend { return fakeAgoraSend{resolveTo: "co:acme/bob"} },
	}
	res := tool.Run(ctx, contract.ToolContext{Scope: "co:acme/alice", Sid: "sid_A"}, json.RawMessage(`{"to":"bob","body":"hi"}`))
	if !res.OK {
		t.Fatalf("agora-resolved send must succeed: %+v", res)
	}
	if len(fb.got) != 1 || fb.got[0].ChatID != "co:acme/bob" {
		t.Fatalf("want emit to resolved scope co:acme/bob, got %+v", fb.got)
	}
}

func TestSendMessageNoAgoraStageA(t *testing.T) {
	// A NON-agora caller (no company:role principal) → Stage A raw emit, even when
	// AgoraSend is wired (the principal gates Stage B, not the resolver's presence).
	fb := &fakeBridge{}
	tool := sendMessage{
		gw:    func() contract.Gateway { return fakeGW{src: fb} },
		agora: func() contract.AgoraSend { return fakeAgoraSend{resolveTo: "co:acme/bob"} },
	}
	// ctx carries no agora principal → Stage A.
	res := tool.Run(context.Background(), contract.ToolContext{Scope: "telegram:dm:9"}, json.RawMessage(`{"to":"telegram:dm:9","body":"hi"}`))
	if !res.OK {
		t.Fatalf("not OK: %+v", res)
	}
	if len(fb.got) != 1 || fb.got[0].ChatID != "telegram:dm:9" {
		t.Fatalf("Stage A emit regressed (non-agora caller must raw-emit): %+v", fb.got)
	}
}
