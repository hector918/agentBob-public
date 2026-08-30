package gate

import (
	"context"
	"strings"

	"agentbob/contract"
	"agentbob/i18n"
)

// runBatch executes a redeemed token's batch of commands (contract.BatchPayload) through
// the slash registry and assembles the redeem receipt. Returns the localized reply, burn
// (Consume the token iff EVERY command succeeded — else keep it for an idempotent retry),
// and deliver (whether the screen should return the receipt at all; see below).
//
// Authority: each command runs with IsAdmin = batch.AsAdmin OR the redeemer's REAL admin
// status (resolved here via the screener's own IsAdmin, because ev.IsAdmin is not stamped
// until AFTER Screen returns — flow/inbound). So an AsAdmin=false bounce code only runs
// its AdminOnly /gate allow when a real admin redeems it; an admin-minted AsAdmin=true
// code runs the batch as admin for whoever presents it (bearer pre-authorization).
//
// deliver is false when the redeemer is NOT a real admin AND nothing committed — an
// ineligible/leaked redemption (e.g. an applicant pasting their own AsAdmin=false bounce
// code) then stays INERT: the screen falls through to normal gating (token kept, sender
// bounced as usual), instead of emitting a receipt that would be a liveness oracle.
//
// Info hygiene (D11): the raw command carries internal ids (source/chat/uid/account/inbox)
// + admin CLI syntax. It is shown ONLY to a real admin (who asked to see what they ran); a
// non-admin redeemer sees only each command's own localized reply, never the command.
func (s *screener) runBatch(ctx context.Context, batch contract.BatchPayload, ev contract.MessageEvent) (reply string, burn, deliver bool) {
	realAdmin := s.IsAdmin(ev.Source, ev.ChatID, ev.UserID)
	asAdmin := batch.AsAdmin || realAdmin

	var b strings.Builder
	if d := strings.TrimSpace(batch.Desc); d != "" {
		b.WriteString(d)
		b.WriteString("\n\n")
	}
	b.WriteString(i18n.T("gate.batch.header", ev.Lang))

	allOK, anyOK := true, false
	for _, cmd := range batch.Commands {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue
		}
		// A per-command event copy carries the command text (Dispatch parses ev.Text) and
		// the frozen authority. A capturing sink buffers the command's own reply instead of
		// delivering it separately — the deltas fold into this single receipt.
		cev := ev
		cev.Text = cmd
		cev.IsAdmin = asAdmin
		cap := &captureSink{}
		// Bound each command's synchronous store/fs I/O so a slow store can't wedge the
		// single consume loop (F115); the deadline covers only Dispatch.
		cctx, cancel := context.WithTimeout(ctx, redeemTimeout)
		err := s.slash.Dispatch(cctx, contract.SlashContext{Event: cev, Sink: cap, IsAdmin: asAdmin, Lang: ev.Lang})
		cancel()

		mark := "✅"
		if err != nil {
			mark = "❌"
			allOK = false
		} else {
			anyOK = true
		}
		b.WriteString("\n" + mark)
		if realAdmin {
			b.WriteString(" " + cmd) // admin CLI + ids only for a real admin
		}
		if r := strings.TrimSpace(cap.final); r != "" {
			b.WriteString(" " + r)
		}
	}
	return b.String(), allOK, realAdmin || anyOK
}

// captureSink is a Sink that buffers a slash command's final reply (Finish) instead of
// delivering it — runBatch folds each batched command's reply into the one redeem
// receipt. ContentDelta/TraceDelta are dropped (nothing renders live); LastSent is ""
// (no platform message).
type captureSink struct{ final string }

func (c *captureSink) ContentDelta(string)        {}
func (c *captureSink) TraceDelta(string)          {}
func (c *captureSink) Finish(full string) error   { c.final = full; return nil }
func (c *captureSink) SendFile(_, _ string) error { return nil }
func (c *captureSink) LastSent() string           { return "" }
