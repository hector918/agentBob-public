package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentbob/contract"
)

type fakeTakeover struct{ live bool }

func (fakeTakeover) Screencast(string, string, func(string)) (func(), <-chan struct{}, error) {
	return nil, nil, nil
}
func (fakeTakeover) Input(string, contract.BrowserInput) error          { return nil }
func (fakeTakeover) SaveLogin(context.Context, string) error            { return nil }
func (f fakeTakeover) LiveForSid(context.Context, string) (bool, error) { return f.live, nil }
func (fakeTakeover) LiveSids(context.Context) ([]string, error)         { return nil, nil }

type fakeMinter struct{ tok string }

func (m fakeMinter) MintTakeover(string) string { return m.tok }

// TestEscalateToCoo_DM pins the human-facing path: a LIVE copy + minter exits the turn
// with the token + message in the REPLY; a dead copy / absent minter / refused token /
// unknown kind errors.
func TestEscalateToCoo_DM(t *testing.T) {
	run := func(tk contract.BrowserTakeover, mint contract.TakeoverMinter, args string) contract.ToolResult {
		return escalateToCoo{tk: tk, mint: func() contract.TakeoverMinter { return mint }}.
			Run(context.Background(), contract.ToolContext{Sid: "s1", Scope: "discord:group:1"}, json.RawMessage(args))
	}
	const okArgs = `{"kind":"browser_takeover","message":"卡在登录页"}`

	res := run(fakeTakeover{live: true}, fakeMinter{tok: "deadbeef"}, okArgs)
	if !res.OK || res.ExitRequest == nil {
		t.Fatalf("escalate should exit the turn: %+v", res)
	}
	if !strings.Contains(res.ExitRequest.Reply, "deadbeef") || !strings.Contains(res.ExitRequest.Reply, "卡在登录页") {
		t.Fatalf("reply must carry the token + message: %q", res.ExitRequest.Reply)
	}

	if res := run(fakeTakeover{live: false}, fakeMinter{tok: "x"}, okArgs); res.OK || res.ExitRequest != nil {
		t.Fatalf("dead copy must not escalate: %+v", res)
	}
	if res := run(fakeTakeover{live: true}, nil, okArgs); res.OK {
		t.Fatalf("absent minter must error: %+v", res)
	}
	if res := run(fakeTakeover{live: true}, fakeMinter{tok: ""}, okArgs); res.OK {
		t.Fatalf("empty token must error: %+v", res)
	}
	if res := run(fakeTakeover{live: true}, fakeMinter{tok: "x"}, `{"kind":"nope","message":"m"}`); res.OK {
		t.Fatalf("unknown kind must error: %+v", res)
	}

	// Re-escalation always re-mints (no suppression): a fresh token rides the reply again.
	res = run(fakeTakeover{live: true}, fakeMinter{tok: "newtok"}, okArgs)
	if !res.OK || res.ExitRequest == nil || !strings.Contains(res.ExitRequest.Reply, "newtok") {
		t.Fatalf("re-escalation must re-mint and carry the fresh token: %+v", res)
	}
}

// TestEscalateToCoo_WorkerToBridge pins the unattended agora-worker path: the token is
// PUSHED to the company bridge (not the reply), and the turn exits with an empty reply
// (no token up the caller chain). No deliverable bridge → loud error, nothing emitted.
func TestEscalateToCoo_WorkerToBridge(t *testing.T) {
	worker := contract.WithIdentity(context.Background(), "member:alice")
	tc := contract.ToolContext{Sid: "s1", Scope: "company:acme/member:alice/inbox:main"}
	const args = `{"kind":"browser_takeover","message":"卡在登录页"}`

	build := func(bridge string) (escalateToCoo, *fakeBridge) {
		fb := &fakeBridge{}
		tool := escalateToCoo{
			tk:    fakeTakeover{live: true},
			mint:  func() contract.TakeoverMinter { return fakeMinter{tok: "deadbeef"} },
			gw:    func() contract.Gateway { return fakeGW{src: fb} },
			agora: func() contract.AgoraSend { return fakeAgoraSend{bridgeScope: bridge} },
		}
		return tool, fb
	}

	// Deliverable bridge → emit to it; reply stays empty.
	tool, fb := build("company:acme/bridge:main")
	res := tool.Run(worker, tc, json.RawMessage(args))
	if !res.OK || res.ExitRequest == nil {
		t.Fatalf("worker escalate should exit: %+v", res)
	}
	if res.ExitRequest.Reply != "" {
		t.Fatalf("worker path must NOT put the token in the reply (it went to the bridge): %q", res.ExitRequest.Reply)
	}
	if len(fb.got) != 1 {
		t.Fatalf("expected one bridge emit, got %d", len(fb.got))
	}
	if fb.got[0].ChatID != "company:acme/bridge:main" || !strings.Contains(fb.got[0].Text, "deadbeef") {
		t.Fatalf("bridge emit must target the bridge with the token: %+v", fb.got[0])
	}

	// No bridge → loud error, nothing emitted.
	tool, fb = build("")
	if res := tool.Run(worker, tc, json.RawMessage(args)); res.OK || len(fb.got) != 0 {
		t.Fatalf("no-bridge worker escalate must error and emit nothing: ok=%v emitted=%d", res.OK, len(fb.got))
	}
}
