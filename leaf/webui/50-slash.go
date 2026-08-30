package webui

import (
	"context"
	"sort"
	"strings"

	"agentbob/contract"
	"agentbob/i18n"
)

// slashWebui mints a one-time admin unlock token and replies it to the admin.
// The admin pastes it into the dashboard's input box (POST /api/token) to open
// the management session. AdminOnly is enforced by the registry.
func (m *Module) slashWebui(_ context.Context, sc contract.SlashContext) error {
	tok := m.auth.Mint()
	return sc.Sink.Finish(i18n.T("slash.webui.token_minted", sc.Lang, tok))
}

// slashTakeover mints a coverage-locked capability token for the live browser COPY of the
// caller's chat (the worker's copy is keyed by the chat scope — browserd's takeover is
// scope-keyed), so a human can clear a login wall / captcha bob can't, then 保存登陆.
// DM-only for a non-admin: the token rides the reply, so a group reply would leak it
// (an admin may run it anywhere — they already hold the keys, and may need to reach a
// group/worker session's browser; the reply warns it's group-visible). Guards a live
// browser before minting. The takeover seam resolves lazily (provider Starts after webui);
// absent → reports unavailable.
func (m *Module) slashTakeover(ctx context.Context, sc contract.SlashContext) error {
	takeover := m.takeover()
	if takeover == nil {
		return sc.Sink.Finish(i18n.T("slash.webui.takeover_unavailable", sc.Lang))
	}
	ev := sc.Event
	if !sc.IsAdmin && ev.ChatType != contract.ChatDM {
		return sc.Sink.Finish(i18n.T("slash.webui.takeover_dm_only", sc.Lang))
	}
	scope := ev.RoutingScope()
	live, err := takeover.LiveForSid(ctx, scope)
	if err != nil { // backend outage ≠ "no live browser" — a retry may work
		return sc.Sink.Finish(i18n.T("slash.webui.takeover_unavailable", sc.Lang))
	}
	// Virtual-group fallback: a fanned member's turn runs (and keys its browser copy)
	// under the member SUB-scope "<group>#<member>", which the bare group scope can't
	// see — without this an admin /takeover in an agora group always reads "no live
	// browser". Enumerate live copies (the existing picker query) and match this
	// group's "#" prefix; "/takeover ^alice" (or "alice") picks among several live
	// members, else the sorted-first match wins (the chosen sub-scope is echoed in the
	// reply). Group-only: a DM scope also uses "#" for its thread suffix.
	// An EXPLICIT member arg ("/takeover ^alice") forces the member match even when
	// the bare group scope itself has a live browser — the admin named alice, who may
	// be stuck on a login wall while the group-scope copy is live. Only the no-arg
	// case defers to the bare-scope browser.
	want := strings.TrimPrefix(strings.TrimSpace(sc.Args), "^")
	memberPick := ""
	if (!live || want != "") && ev.ChatType.IsGroupChat() {
		sids, lerr := takeover.LiveSids(ctx)
		if lerr != nil { // backend outage ≠ "no live browser" — a retry may work
			return sc.Sink.Finish(i18n.T("slash.webui.takeover_unavailable", sc.Lang))
		}
		var match []string
		for _, s := range sids {
			if base, _, ok := contract.SplitMemberSubScope(s); ok && base == scope {
				match = append(match, s)
			}
		}
		sort.Strings(match) // deterministic pick when several members are live
		for _, s := range match {
			if _, member, _ := contract.SplitMemberSubScope(s); want == "" || member == want {
				scope, live, memberPick = s, true, s
				break
			}
		}
	}
	// An explicit member arg that matched nothing must fail LOUDLY: falling
	// through would mint for the bare group scope (live from the no-arg probe
	// above) — a token for the WRONG browser, byte-identical to a legit no-arg
	// reply, and a 保存登陆 there writes back the wrong master. Covers a released
	// copy, a typo'd name, and a member arg outside a group (never matchable).
	if want != "" && memberPick == "" {
		return sc.Sink.Finish(i18n.T("slash.webui.takeover_member_no_live", sc.Lang, want))
	}
	if !live {
		return sc.Sink.Finish(i18n.T("slash.webui.takeover_no_live", sc.Lang))
	}
	tok := m.auth.MintScoped(scope)
	if tok == "" {
		return sc.Sink.Finish(i18n.T("slash.webui.takeover_mint_failed", sc.Lang))
	}
	warn := ""
	if ev.ChatType != contract.ChatDM { // admin ran it in a group — the token is visible there
		warn = i18n.T("slash.webui.takeover_group_visible_warn", sc.Lang)
	}
	reply := i18n.T("slash.webui.takeover_token_minted", sc.Lang, warn, tok)
	if memberPick != "" {
		// Echo WHICH member's browser the token covers: the sub-scope is an opaque
		// key — appended raw, not translated (same pattern as /close-session's sid).
		reply += "\n· " + memberPick
	}
	return sc.Sink.Finish(reply)
}
