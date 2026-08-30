package webui

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/claimtoken"
)

type capSink struct{ out string }

func (c *capSink) ContentDelta(string)        {}
func (c *capSink) TraceDelta(string)          {}
func (c *capSink) Finish(s string) error      { c.out = s; return nil }
func (c *capSink) SendFile(_, _ string) error { return nil }
func (c *capSink) LastSent() string           { return c.out }

type fakeTk struct {
	live bool     // LiveForSid answer for the bare chat scope
	sids []string // LiveSids answer — the member sub-scope enumeration
}

func (fakeTk) Screencast(string, string, func(string)) (func(), <-chan struct{}, error) {
	return nil, nil, nil
}
func (fakeTk) Input(string, contract.BrowserInput) error          { return nil }
func (fakeTk) SaveLogin(context.Context, string) error            { return nil }
func (f fakeTk) LiveForSid(context.Context, string) (bool, error) { return f.live, nil }
func (f fakeTk) LiveSids(context.Context) ([]string, error)       { return f.sids, nil }

// TestSlashTakeover pins the /takeover guards: DM-only for a non-admin, the no-live guard,
// and that a live browser in a DM (or any chat for an admin) mints a token. The token is
// keyed by the chat SCOPE — the worker copy browserd's takeover targets.
func TestSlashTakeover(t *testing.T) {
	run := func(admin bool, ct contract.ChatType, tk contract.BrowserTakeover, args string) string {
		m := &Module{auth: NewAuth(claimtoken.New()), takeover: func() contract.BrowserTakeover { return tk }}
		s := &capSink{}
		_ = m.slashTakeover(context.Background(), contract.SlashContext{
			IsAdmin: admin, Sink: s, Args: args,
			Event: contract.MessageEvent{Source: "discord", ChatType: ct, ChatID: "x"},
		})
		return s.out
	}

	// non-admin in a GROUP → refused (token would leak in the group reply).
	// (i18n: no catalog in test, so i18n.T returns the key — assert on the key.)
	if out := run(false, contract.ChatGroup, fakeTk{live: true}, ""); !strings.Contains(out, "takeover_dm_only") {
		t.Fatalf("group non-admin must be refused DM-only: %q", out)
	}
	// non-admin in a DM with a live browser → token minted.
	if out := run(false, contract.ChatDM, fakeTk{live: true}, ""); !strings.Contains(out, "takeover_token_minted") {
		t.Fatalf("DM + live must mint a token: %q", out)
	}
	// DM but no live browser → no token.
	if out := run(false, contract.ChatDM, fakeTk{live: false}, ""); strings.Contains(out, "takeover_token_minted") {
		t.Fatalf("no-live must not mint: %q", out)
	}
	// admin in a GROUP → allowed (not DM-bound) and mints.
	if out := run(true, contract.ChatGroup, fakeTk{live: true}, ""); !strings.Contains(out, "takeover_token_minted") {
		t.Fatalf("admin in group must mint: %q", out)
	}
}

// TestSlashTakeoverMemberArg pins the explicit-member addressing contract: "/takeover
// ^alice" must reach ALICE's sub-scope browser or fail loudly — never fall back to the
// bare group scope's copy (a silent fallback mints a token for the WRONG browser, and a
// 保存登陆 there writes back the wrong master).
func TestSlashTakeoverMemberArg(t *testing.T) {
	const scope = "discord:group:x" // ScopeFor("discord", ChatGroup, "x", ...)
	run := func(tk contract.BrowserTakeover, args string) string {
		m := &Module{auth: NewAuth(claimtoken.New()), takeover: func() contract.BrowserTakeover { return tk }}
		s := &capSink{}
		_ = m.slashTakeover(context.Background(), contract.SlashContext{
			IsAdmin: true, Sink: s, Args: args,
			Event: contract.MessageEvent{Source: "discord", ChatType: contract.ChatGroup, ChatID: "x"},
		})
		return s.out
	}

	// Named member is live → mints and echoes the chosen sub-scope.
	out := run(fakeTk{live: true, sids: []string{scope + "#alice", scope + "#bob"}}, "^alice")
	if !strings.Contains(out, "takeover_token_minted") || !strings.Contains(out, scope+"#alice") {
		t.Fatalf("live named member must mint + echo the sub-scope: %q", out)
	}
	// Named member NOT live while the bare group scope IS → must refuse (with the
	// member's name), never silently mint the bare-scope token (F101).
	out = run(fakeTk{live: true, sids: nil}, "^alice")
	if strings.Contains(out, "takeover_token_minted") {
		t.Fatalf("no-live named member must not fall back to the bare scope: %q", out)
	}
	// (no catalog in test → i18n.T returns the bare key, so the member name the
	// real string echoes isn't visible here — assert on the dedicated key.)
	if !strings.Contains(out, "takeover_member_no_live") {
		t.Fatalf("no-live named member must be refused loudly: %q", out)
	}
	// Same, when another member is live: name mismatch must not pick the wrong member.
	out = run(fakeTk{live: false, sids: []string{scope + "#bob"}}, "^alice")
	if strings.Contains(out, "takeover_token_minted") || !strings.Contains(out, "takeover_member_no_live") {
		t.Fatalf("mismatched member name must not pick another member: %q", out)
	}
	// No-arg fallback is untouched: bare scope dead but a member is live → sorted-first
	// member wins and is echoed.
	out = run(fakeTk{live: false, sids: []string{scope + "#bob", scope + "#alice"}}, "")
	if !strings.Contains(out, "takeover_token_minted") || !strings.Contains(out, scope+"#alice") {
		t.Fatalf("no-arg fallback must still pick the sorted-first live member: %q", out)
	}
}
