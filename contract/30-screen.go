package contract

import "context"

// AccessGranter mutates a source's access policy: it grants a sender allowlist
// access (binding's side effect — so a just-bound first-contact sender's next
// message passes the gate) or toggles their admin status, writing the source's
// policy file and reloading the live policy. Provided by leaf/gate; consumed by
// leaf/accounts. Keyed on the REAL (source, uid) the gate matches — the bound
// source name and gate-uid, NOT the normalized accounts handle. A nil/absent
// granter means binding still records identity but cannot auto-allowlist (the
// operator adds access by hand).
type AccessGranter interface {
	// Allow adds uid to source's allowlist (idempotent). added=false if it was
	// already present. Writes the policy file, reloads the live policy, and
	// forgets any rejected-sender record for uid on source.
	Allow(ctx context.Context, source, uid string) (added bool, err error)
	// AllowInChat adds uid to source's PER-CHAT allowlist (groups[chatID].allowlist_add),
	// for a group-minted admit token whose frozen scope IS that chat — so a group redeem
	// grants where the member actually talks, not source-wide. Idempotent; writes +
	// reloads like Allow. (Authorized() already honors groups[chatID].allowlist_add.)
	AllowInChat(ctx context.Context, source, chatID, uid string) (added bool, err error)
}

// ScreenAction is the inbound screen's routing verdict on one event — its
// fate on the access axis.
type ScreenAction int

const (
	// ScreenPass — the event proceeds into the pipeline.
	ScreenPass ScreenAction = iota
	// ScreenDrop — the event must be discarded (see Verdict.Reason).
	ScreenDrop
	// ScreenRedeemed — the event WAS a live bare code and has been consumed by
	// its handler; it must not proceed. Verdict.Reply carries the localized
	// confirmation (may be "") for the caller to deliver per its own platform
	// policy.
	ScreenRedeemed
)

// ScreenReason says why a ScreenDrop verdict was issued, so callers can log
// with the right wording (a block reads differently from a missing allowlist
// row).
type ScreenReason int

const (
	ReasonNone ScreenReason = iota
	ReasonDenied
	ReasonUnauthorized
)

// Verdict is the inbound screen's result for one event.
type Verdict struct {
	Action ScreenAction
	Reason ScreenReason // set when Action == ScreenDrop
	Reply  string       // a reply to deliver to the sender: a localized redeem confirmation (ScreenRedeemed) or a first-contact bounce (ScreenDrop); may be ""
	// Onboarding is true when Action==ScreenPass was granted by a source's onboarding
	// opt-in to an UN-allowlisted (not-yet-approved) sender — as opposed to an allowlisted
	// sender. The inbound flow uses it to withhold the slash fork from such senders (their
	// "/..." goes to the turn → intro onboarding, not a side-effectful command), so an
	// un-approved stranger on an open source can't run session-mutating slashes.
	Onboarding bool
}

// Screener is the single inbound sender screen + admin authority. It runs the
// access-axis triage on each inbound event (denylist > bare-code redeem >
// allowlist) and answers admin questions from the same per-source access
// policy it owns. Registered on the trunk by the gate module; consulted by the
// central inbound flow (its only caller today — no source screens earlier in its
// own protocol phase).
type Screener interface {
	// Screen runs the gate order on one normalized inbound event and returns
	// its fate (Pass / Drop / Redeemed). ev.Text must already be
	// mention-stripped by the source. Safe for concurrent use.
	Screen(ctx context.Context, ev MessageEvent) Verdict
	// IsAdmin reports whether userID is an admin on source within chatID, per
	// that source's access policy. Today its only caller is the ingress admin
	// stamp; it also covers any future late callback handler that can't rely on
	// the message-event-time flag (a click can land long after the event).
	IsAdmin(source, chatID, userID string) bool
}
