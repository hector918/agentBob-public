package gate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/i18n"
	"agentbob/trunk"
)

// Module is the gate: it loads each source's access policy from dir and registers
// a contract.Screener on the trunk. A redeemed claim token carries a batch of slash
// commands (contract.BatchPayload); the screen runs them through the SlashRegistry.
type Module struct {
	dir    string // directory holding <source>.yaml policy files
	scr    *screener
	gr     *granter             // the access granter; held so the /gate slash can call it
	tokens contract.ClaimTokens // claim-token facility: bounce token mint/verify
	state  atomic.Int32         // trunk.State
}

// New returns a gate module that loads policy files from dir on Start. The
// screener is created here (with an empty, closed policy map) so the post-Start
// accessors Reload/Rejected are never a nil-deref even if a webui holding a
// *Module races ahead of trunk Start; Start swaps in the loaded policies.
func New(dir string) *Module {
	const maxRejected = 50
	return &Module{
		dir: dir,
		scr: &screener{policies: map[string]Policy{}, rejected: NewRejectedSenders(maxRejected)},
	}
}

func (m *Module) Name() string { return "gate" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.Screener](),
		trunk.TypeOf[contract.AccessGranter](),
	}
}

// Needs the panel registry (the gate describes its webui layer) and the slash
// registry (the /gate command allowlists a rejected sender) — same
// publish-into-a-registry pattern as model.
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.PanelRegistry](),
		trunk.TypeOf[contract.SlashRegistry](),
		trunk.TypeOf[contract.ClaimTokens](), // gate-admit token mint/verify
	}
}

// Optional is false: without the screen, a Gated source has no authorization,
// so a failed policy load must abort startup rather than fail open.
func (m *Module) Optional() bool { return false }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

// Start loads the access policies and provides the contract.Screener. The screen runs a
// redeemed token's batch of commands through the SlashRegistry.
func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	policies, err := LoadAll(m.dir)
	if err != nil {
		m.state.Store(int32(trunk.StateFailed))
		return fmt.Errorf("gate: load policies: %w", err)
	}
	// The screen runs a redeemed token's batch through the slash registry (its commands
	// are ordinary /commands other modules register at their Start; by runtime redeem all
	// are registered).
	m.scr.slash = trunk.Require[contract.SlashRegistry](reg)
	// Claim-token facility: the screen mints the bounce token here. Hard Need → registered
	// before this Start.
	m.tokens = trunk.Require[contract.ClaimTokens](reg)
	m.scr.tokens = m.tokens
	m.scr.swapPolicies(policies)
	trunk.Provide[contract.Screener](reg, m.scr)
	// The access granter (binding's auto-allowlist + admin writes) is published
	// for accounts to consume; it writes the same policy files Reload re-reads.
	m.gr = &granter{dir: m.dir, reload: m.Reload, rejected: m.scr.rejected,
		denied: func(source, chatID, uid string) bool { return m.scr.policyFor(source).Denied(chatID, uid) }}
	trunk.Provide[contract.AccessGranter](reg, m.gr)
	// The policy-editor list is derived from the policy files already on disk (one
	// `code` editor per source that has a policy). A fresh deploy with no policy files
	// starts editor-less and gains one as sources are allowlisted.
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.panel())
	// /gate allowlists a rejected sender from the webui (the panel's per-row "allow"
	// action prefills it). Admin-only; the registry rejects non-admin senders.
	trunk.Require[contract.SlashRegistry](reg).Register(contract.SlashCommand{
		Name:      "gate",
		DescKey:   "slash.gate.desc",
		AdminOnly: true,
		Handler:   m.slashGate,
	})
	m.state.Store(int32(trunk.StateReady))
	return nil
}

// slashGate handles "/gate allow <source> <user-id>": it allowlists uid on
// source via the granter (writes the policy file, hot-reloads, forgets the
// rejected record). AdminOnly is enforced by the registry before this runs.
func (m *Module) slashGate(ctx context.Context, sc contract.SlashContext) error {
	const usage = "usage: /gate allow <source> <user-id>   (or: /gate allow <source> <chat-id> <user-id>)" // returned error (logs); user sees the i18n version
	f := strings.Fields(sc.Args)
	if len(f) < 3 || f[0] != "allow" {
		_ = sc.Sink.Finish(i18n.T("slash.gate.usage", sc.Lang))
		return errors.New(usage)
	}
	var source, uid string
	switch len(f) {
	case 3: // /gate allow <source> <user-id> (raw, source-level)
		source, uid = f[1], f[2]
	case 4: // /gate allow <source> <chat-id> <user-id> (per-chat allowlist_add)
		// The per-chat grant the panel prefills for a GROUP sender — scopes the admit to
		// that one chat (groups[chat].allowlist_add) instead of source-wide. This is also
		// the form a group bounce token's batch carries. Returns within the case (the
		// shared tail below is the source-level Allow).
		chatID := f[2]
		source, uid = f[1], f[3]
		changed, err := m.gr.AllowInChat(ctx, source, chatID, uid)
		return m.finishGrant(sc, changed, err, uid, source)
	default:
		_ = sc.Sink.Finish(i18n.T("slash.gate.usage", sc.Lang))
		return errors.New(usage)
	}
	changed, err := m.gr.Allow(ctx, source, uid)
	return m.finishGrant(sc, changed, err, uid, source)
}

// finishGrant is the shared tail of every /gate allow variant: a failed grant is
// reported to the admin and returned (the registry logs it); otherwise reply
// already_allowed / allowlisted per changed.
func (m *Module) finishGrant(sc contract.SlashContext, changed bool, err error, uid, source string) error {
	if err != nil {
		// Applicant on the denylist (F139): a clear "remove the denylist first" beats the raw
		// error string. RETURN the error either way — the grant did NOT happen, so when this
		// runs inside a redeemed token's batch, runBatch must see the failure (mark ❌, keep
		// the token for retry once the denylist is lifted) rather than a false ✅ + burn.
		if errors.Is(err, errUserDenied) {
			_ = sc.Sink.Finish(i18n.T("slash.gate.denied", sc.Lang, uid, source))
			return err
		}
		_ = sc.Sink.Finish(i18n.T("slash.gate.allow_failed", sc.Lang, err.Error()))
		return err
	}
	// Allowlisting opens the ACCESS axis only — the sender still needs their account's
	// flow granted (/accounts approve), or they keep getting intro's "waiting for an
	// admin" notice while the admin believes the grant is done. The hint rides on BOTH
	// replies (already_allowed included: re-running a bounce token lands there, and that
	// is exactly when the admin is wondering why the sender still can't talk). It reaches
	// the admin who redeemed a bounce token too — runBatch folds this reply into the
	// receipt. Phrased conditionally, so it stays truthful on a no-accounts deployment.
	key := "slash.gate.allowlisted"
	if !changed {
		key = "slash.gate.already_allowed"
	}
	return sc.Sink.Finish(i18n.T(key, sc.Lang, uid, source) + "\n" + i18n.T("slash.gate.approve_hint", sc.Lang))
}

// Reload re-scans the policy directory and atomically swaps the live map.
// Invoked by the granter's write paths (Allow/AllowInChat) and the webui
// policy-yaml editor's save (90-panel.go).
func (m *Module) Reload() error {
	policies, err := LoadAll(m.dir)
	if err != nil {
		return fmt.Errorf("gate: reload policies: %w", err)
	}
	m.scr.swapPolicies(policies)
	return nil
}

// Rejected exposes the rejected-senders feed for the admin webui panel. The
// granter holds the same feed and forgets rows (ForgetUser / ForgetUserInChat)
// after a grant.
func (m *Module) Rejected() *RejectedSenders { return m.scr.rejected }

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// compile-time: Module is a trunk.Module.
var _ trunk.Module = (*Module)(nil)
