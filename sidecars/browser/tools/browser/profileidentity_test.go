package browser

import (
	"context"
	"testing"

	"agentbob/sidecars/browser/core"
)

type stubGate struct{ allow bool }

func (g stubGate) Visible(context.Context, string, string) bool { return true }
func (g stubGate) Check(context.Context, string, string) core.PermDecision {
	if g.allow {
		return core.PermDecision{Kind: core.PermAllow}
	}
	return core.PermDecision{Kind: core.PermDeny}
}

// ProfileIdentity is the single bob-side gate decision shared by
// Pool.profileRoute and the browserd thin client — pin its routing rules.
func TestProfileIdentity(t *testing.T) {
	ctx := context.Background()
	allow := stubGate{allow: true}
	deny := stubGate{allow: false}

	cases := []struct {
		name string
		gate core.Gate
		actx core.AgentCtx
		want string
	}{
		{"nil gate → legacy", nil, core.AgentCtx{AccountID: "ac_x"}, ""},
		{"no identity → legacy", allow, core.AgentCtx{}, ""},
		{"denied → legacy", deny, core.AgentCtx{AccountID: "ac_x"}, ""},
		{"account granted", allow, core.AgentCtx{AccountID: "ac_x"}, "ac_x"},
		// The browser login belongs to the identity the turn ACTS AS:
		// agora MemberID (server-resolved) beats the triggering AccountID.
		{"member wins over account", allow, core.AgentCtx{MemberID: "mb_m", AccountID: "ac_x"}, "mb_m"},
	}
	for _, c := range cases {
		if got := ProfileIdentity(ctx, c.gate, c.actx); got != c.want {
			t.Errorf("%s: ProfileIdentity = %q, want %q", c.name, got, c.want)
		}
	}
}
