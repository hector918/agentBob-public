// Package intro is the onboarding flow for allowlisted-but-accountless senders. It is
// MECHANICAL (no LLM, no agent, no history): detect the sender's language and reply with
// one canned message telling them how to get an account. The router routes an UNBOUND
// sender here explicitly (flow/router.pick) — this flow never self-selects via Accepts.
// See docs/account-flows-and-intro.md.
package intro

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"

	"agentbob/contract"
	"agentbob/i18n"
	"agentbob/trunk"
)

// Module wires the intro flow and registers it with the flow router.
type Module struct {
	f     *flow
	state atomic.Int32 // trunk.State
}

// introSeenMax bounds the per-sender dedup set (distinct unbound senders, not per-message).
const introSeenMax = 4096

func New() *Module { return &Module{} }

func (m *Module) Name() string             { return "flow-intro" }
func (m *Module) Provides() []reflect.Type { return nil }

// Needs the flow registry (to register) + the gateway (to send the one canned reply).
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.FlowRegistry](),
		trunk.TypeOf[contract.Gateway](),
	}
}

// Optional: harmless when present; the router only routes here when accounts is wired AND
// the sender is unbound, so a no-accounts deployment never reaches it.
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(_ context.Context, reg *trunk.Registry) error {
	m.f = &flow{gw: trunk.Require[contract.Gateway](reg), reg: reg, seen: map[string]bool{}}
	trunk.Require[contract.FlowRegistry](reg).Register(m.f)
	m.state.Store(int32(trunk.StateReady))
	return nil
}

func (m *Module) Stop(context.Context) error {
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// flow is the contract.Flow: the mechanical onboarding responder.
type flow struct {
	gw  contract.Gateway
	reg *trunk.Registry

	accOnce  sync.Once
	accounts contract.Accounts // OPTIONAL, lazy — distinguishes a PAUSED account from a truly unbound one

	provOnce sync.Once
	prov     contract.AccountProvisioner // OPTIONAL, lazy — mints the bare pending account on first contact

	mu   sync.Mutex
	seen map[string]bool // "source:uid:msgKind" → already sent this process (don't re-spam, but onboard vs paused dedup separately)
}

// acc lazily resolves the optional accounts authority. Nil when accounts isn't wired
// (then every unbound/paused sender just gets the generic onboard message).
func (f *flow) acc() contract.Accounts {
	if f.reg == nil {
		return nil
	}
	f.accOnce.Do(func() { f.accounts, _ = trunk.TryRequire[contract.Accounts](f.reg) })
	return f.accounts
}

// provider lazily resolves the optional account provisioner (the onboarding write seam).
// Nil when accounts isn't wired → intro just replies, mints no account.
func (f *flow) provider() contract.AccountProvisioner {
	if f.reg == nil {
		return nil
	}
	f.provOnce.Do(func() { f.prov, _ = trunk.TryRequire[contract.AccountProvisioner](f.reg) })
	return f.prov
}

func (f *flow) Name() string                   { return "intro" }
func (f *flow) Priority() int                  { return 0 } // never self-selected; router returns it explicitly
func (f *flow) Accepts(contract.TurnWork) bool { return false }
func (f *flow) Mode() contract.SessionMode     { return contract.SessionModeNone }

// DumpPrompt: intro has no model prompt (it's a mechanical canned reply).
func (f *flow) DumpPrompt(context.Context, contract.TurnWork) string {
	return "(intro flow: mechanical onboarding reply — no model, no prompt)"
}

// RunTurn sends ONE localized onboarding message per unbound sender (then stays silent),
// purely mechanically — no model call, no history write.
func (f *flow) RunTurn(ctx context.Context, work contract.TurnWork) {
	ev, ok := contract.FirstHumanEvent(work.Events)
	if !ok {
		return
	}
	// A PAUSED account (bound but suspended) gets a distinct "suspended" notice; everyone
	// else gets the "registered, pending admin approval" notice. Dedup keys on the message
	// kind too, so a sender onboarded earlier still gets the suspended notice if their
	// account is later paused.
	msgKey := "intro.pending"
	bound := false
	if acc := f.acc(); acc != nil {
		var info contract.AccountInfo
		info, bound = acc.AccountFor(ctx, ev)
		if bound && info.Status == "paused" {
			msgKey = "intro.paused"
		}
	}
	replyKey := introReplyKey(msgKey, ev.ChatType.IsGroupChat())
	dedup := introDedupKey(ev.Source, ev.ChatID, ev.UserID, replyKey)
	f.mu.Lock()
	seen := f.seen[dedup]
	f.mu.Unlock()
	if seen {
		return // already sent this notice to this sender — stay quiet rather than re-send every message
	}
	// Not greeted this window. For an unbound sender mint the durable bare (flow-less)
	// account so the admin has something to approve — gated behind the seen-check so it
	// runs once per greeting, NOT on every message (idempotent + best-effort regardless;
	// nil provisioner on a no-accounts deploy → just reply).
	if !bound {
		if prov := f.provider(); prov != nil {
			if _, _, err := prov.EnsureBareAccount(ctx, ev); err != nil {
				slog.Warn("flow-intro: could not mint bare onboarding account", "source", ev.Source, "err", err)
			}
		}
	}

	src := f.gw.SourceByName(work.Target.Source)
	if src == nil {
		slog.Debug("flow-intro: no reply sink — message skipped", "source", work.Target.Source)
		return // NOT marked seen — a later message retries once the source resolves
	}
	// ev.Lang is the ingress-stamped reply language (override pin → detect → memory);
	// reuse it instead of re-detecting so the intro reply honors an admin override pin.
	msg := i18n.T(replyKey, ev.Lang)
	if err := src.NewSink(ctx, work.Target, work.Scope, work.Sid, work.Prefs).Finish(msg); err != nil {
		slog.Warn("flow-intro: reply failed", "err", err, "kind", replyKey)
		return // NOT marked seen — a transient send failure must not permanently swallow the one onboarding line (L-D1 intro)
	}
	// Mark seen only AFTER a successful send. The tradeoff is a tiny double-send window
	// if two of this sender's messages race before the first lands — far better than
	// silently dropping the sole onboarding message on a transient failure.
	f.mu.Lock()
	if len(f.seen) >= introSeenMax {
		f.seen = map[string]bool{} // simple bound (an allow_all source could feed unbounded UIDs); re-sending after a flush is harmless
	}
	f.seen[dedup] = true
	f.mu.Unlock()
	// F150: a real greet was delivered — let the arbiter's drain cleared-ack fire. The
	// dedup / paused / no-sink no-ops all returned earlier without reaching here, so an
	// all-deduped drain stays correctly silent (no false "已处理 N 条").
	if work.OnOutput != nil {
		work.OnOutput()
	}
}

// introReplyKey picks the canned-message key. In a shared GROUP the reply is public, so
// it returns a NEUTRAL "let's DM" nudge rather than disclose account status (pending /
// suspended) to the whole room (F151); a DM keeps the status-specific message.
func introReplyKey(msgKey string, isGroup bool) string {
	if isGroup {
		return "intro.group_dm_nudge"
	}
	return msgKey
}

// introDedupKey is the per-(source, chat, sender, reply) key bounding intro re-sends. It
// includes the CHAT (F151) so a group notice can't permanently silence a later DM ask —
// per-sender-only dedup let a group greet mute the DM. Keying on the REPLY sent keeps a
// group at one neutral nudge across a pending→paused transition, while a DM still
// re-notifies on that transition (its key flips with msgKey).
func introDedupKey(source, chatID, uid, replyKey string) string {
	return source + ":" + chatID + ":" + uid + ":" + replyKey
}

var _ contract.Flow = (*flow)(nil)
