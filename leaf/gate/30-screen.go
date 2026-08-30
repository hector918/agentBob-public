package gate

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"agentbob/contract"
	"agentbob/i18n"
)

// screener is the contract.Screener implementation. It holds the per-source
// access policy (hot-swappable), the optional bare-code redeemer, and the
// rejected-senders feed. Safe for concurrent use.
type screener struct {
	mu       sync.RWMutex
	policies map[string]Policy
	// slash runs a redeemed token's batch of commands (contract.BatchPayload) — each
	// command through SlashRegistry.Dispatch at the redeem step. nil → redeem skipped.
	slash    contract.SlashRegistry
	rejected *RejectedSenders
	// tokens authenticates claim tokens (claimtoken facility). The screen mints the
	// bounce token here (a batch that admits the applicant); a redeemed token's batch is
	// run via slash. nil → no admit token in the bounce (degraded).
	tokens contract.ClaimTokens
}

// admitKind is the claim-token facility kind label for a bounce token. The facility
// treats kind as opaque (the redeem step type-asserts contract.BatchPayload, not kind),
// so this is only a human label on the minted token.
const admitKind = "gate-admit"

// admitTokenTTL bounds an unredeemed admit token. Short (F113, hector): a
// live grant code should NOT linger for days. When a bounced sender is still trying,
// each bounce (bounceReplyTTL re-arm) mints a fresh code, so an admin watching always
// has a current one; the only case a long TTL bought was "admit from a days-old bounce
// message" — not worth a week-long live credential. A short TTL also self-drains the
// orphan tokens the feed's evict/forget paths leave behind (they don't Consume the row's
// token), so adversarial identity-rotation can't accrue week-long facility entries.
const admitTokenTTL = 10 * time.Minute

// redeemTimeout bounds ONE batched command's dispatch — it does DB/policy-file I/O
// SYNCHRONOUSLY on the single inbound consume loop, so a slow/hung store would otherwise
// head-of-line-block ALL inbound until it returned (F115). A hung PG round trip is
// cancelled here so the loop recovers; the batch keeps the token (idempotent retry).
// Matches the 5s deadline the accounts provision path already uses. (Local yaml file I/O
// doesn't honor ctx, but the realistic hang is the networked store, which does.)
const redeemTimeout = 5 * time.Second

// policyFor returns the captured policy for a source. A missing source yields
// the zero Policy, which is CLOSED — a Gated source whose file failed to load
// fails shut.
func (s *screener) policyFor(source string) Policy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.policies[source]
}

// swapPolicies atomically replaces the whole policy map (hot reload).
func (s *screener) swapPolicies(p map[string]Policy) {
	s.mu.Lock()
	s.policies = p
	s.mu.Unlock()
}

// sourceNames returns the loaded source names, sorted — the webui builds one
// policy-yaml editor setting per source from this.
func (s *screener) sourceNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.policies))
	for k := range s.policies {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Screen runs the gate order on one normalized inbound event:
// denylist > bare-code redeem > allowlist. ev.Text must already be
// mention-stripped by the source (a group "@bot CODE" reaches the exact-match
// redeem as "CODE"). ev.Lang is assumed pre-stamped at ingress; the redeemer
// reads it as-is.
func (s *screener) Screen(ctx context.Context, ev contract.MessageEvent) contract.Verdict {
	p := s.policyFor(ev.Source)
	if p.Denied(ev.ChatID, ev.UserID) {
		// Deliberate block — NOT recorded into the rejected-senders feed (D14). That feed
		// is for bootstrap DISCOVERY of strangers to allowlist; a denied sender is the
		// opposite, and the panel's per-row "allow" action would silently no-op on them
		// (Denied wins in Authorized()). Recording would pollute the cap-50 feed and age
		// out genuine ids. Denied senders get no reply regardless.
		return contract.Verdict{Action: contract.ScreenDrop, Reason: contract.ReasonDenied}
	}
	// A live token carries a BATCH OF COMMANDS (contract.BatchPayload) — it IS the
	// credential, bypassing the allowlist. The screen authenticates it ONCE here and runs
	// its batch through SlashRegistry.Dispatch (runBatch), under the token's FROZEN
	// authority. Anything that isn't a live batch token (an unknown/expired token, or a
	// non-batch token like a webui-unlock code pasted in chat) falls through to normal
	// gating — a leaked/foreign token is inert.
	//
	// runBatch localizes its reply from ev.Lang, which flow/inbound stamps at ingress for
	// EVERY event (including a first-contact un-allowlisted sender, the redeem path's
	// primary user — flow/inbound/10-flow.go), so no local fallback is needed.
	if s.tokens != nil && s.slash != nil {
		if tok := strings.TrimSpace(ev.Text); tok != "" {
			if _, payload, ok := s.tokens.Verify(tok); ok {
				if batch, isBatch := payload.(contract.BatchPayload); isBatch {
					reply, burn, deliver := s.runBatch(ctx, batch, ev)
					// deliver=false → an ineligible/leaked redemption committed nothing (e.g. an
					// applicant pasting their own AsAdmin=false bounce code): stay INERT and fall
					// through to normal gating (token kept, sender bounced as usual), rather than
					// emit a receipt that leaks the token's liveness.
					if deliver {
						if burn {
							// Burn ONLY when the whole batch committed. A partial/failed run KEEPS
							// the token (original TTL) so the redeemer can retry — every batched
							// command is idempotent (allowlist no-op, approve no-clobber), so a
							// re-run re-applies the failed step without duplicating.
							s.tokens.Consume(tok)
						}
						return contract.Verdict{Action: contract.ScreenRedeemed, Reply: reply}
					}
				}
			}
		}
	}
	if !p.Authorized(ev.ChatID, ev.UserID) {
		// Onboarding-open source: PASS an un-allowlisted (non-denied) sender through to the
		// router, which funnels an accountless sender to intro (→ a bare pending account for
		// the admin to approve). Access is gated downstream by the account's flow, NOT the
		// allowlist — so this is not "open to all", it's "open to ONBOARDING". Denylist
		// already won above. Default-off source keeps the closed bounce path below.
		if p.Onboarding {
			return contract.Verdict{Action: contract.ScreenPass, Onboarding: true}
		}
		shouldReply := s.rejected.Record(ev.Source, ev.ChatID, ev.UserID, ev.UserName, ev.ChatType)
		// Bounce a friendly notice carrying a freshly-minted admit token (a batch that admits
		// this applicant; a real admin redeems it by pasting the code). Sent on first contact,
		// then re-armed after bounceReplyTTL — repeats inside the window are silent (no
		// amplification). Denylisted senders above get no reply (deliberate block).
		reply := ""
		if shouldReply {
			reply = s.mintBounce(ev)
		}
		return contract.Verdict{Action: contract.ScreenDrop, Reason: contract.ReasonUnauthorized, Reply: reply}
	}
	return contract.Verdict{Action: contract.ScreenPass}
}

// mintBounce mints a fresh bounce token for the turned-away sender and returns the
// user-facing bounce. The token is a BATCH that admits THIS applicant at the exact scope
// they were bounced in: a group → that chat's allowlist (/gate allow <source> <chat>
// <uid>), a DM → source-wide (/gate allow <source> <uid>). AsAdmin=false — the code is
// shown to the applicant IN the group and must NOT let them self-admit; only a real admin
// redeeming it can run the AdminOnly /gate allow. A nil facility (degraded) → no token to
// offer, so skip the reply rather than show a dead code.
func (s *screener) mintBounce(ev contract.MessageEvent) string {
	if s.tokens == nil {
		return ""
	}
	var cmd string
	if ev.ChatType.IsGroupChat() {
		cmd = fmt.Sprintf("/gate allow %s %s %s", ev.Source, ev.ChatID, ev.UserID)
	} else {
		cmd = fmt.Sprintf("/gate allow %s %s", ev.Source, ev.UserID)
	}
	// NAMES the applicant (F140): in a busy group several strangers bounce at once, each
	// with a different token, and the token is opaque — without the name an admin reading
	// the feed can't tell which code admits whom. Falls back to the raw uid.
	who := strings.TrimSpace(ev.UserName)
	if who == "" {
		who = ev.UserID
	}
	tok := s.tokens.Mint(admitKind, contract.BatchPayload{
		Commands: []string{cmd},
		Desc:     i18n.T("gate.bounce.desc", ev.Lang, who),
		AsAdmin:  false,
	}, admitTokenTTL)
	// Stamp the fresh token on the feed entry and retire the one it replaces: every
	// bounceReplyTTL re-arm mints anew, so without this a chatty stranger accrues a live
	// token per bounce (D7). At most one token per (source,chat,user) stays live; a token
	// shown in an OLDER bounce stops working. (The short admitTokenTTL self-drains any that
	// slip the retire path.)
	if prev, _ := s.rejected.SetToken(ev.Source, ev.ChatID, ev.UserID, tok); prev != "" && prev != tok {
		s.tokens.Consume(prev)
	}
	// Localized via the sender's ingress-stamped ev.Lang (D5) — the frozen token is safe
	// to show even in a public group, so this is delivered where they were bounced (D6).
	return i18n.T("gate.bounce", ev.Lang, who, tok)
}

// IsAdmin reports whether userID is an admin on source within chatID.
func (s *screener) IsAdmin(source, chatID, userID string) bool {
	return s.policyFor(source).IsAdmin(chatID, userID)
}
