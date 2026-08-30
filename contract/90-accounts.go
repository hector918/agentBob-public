package contract

import (
	"context"
	"slices"
	"strings"
)

// Accounts is the identity & per-account policy authority. For the minimal slice
// it answers one question the router asks per turn — which flow strategy handles
// this sender — keyed off the sender's cross-entry handle (platform-family +
// stable uid). Billing rides a separate, narrower seam (ConsumptionReporter)
// consumed by the model pool, so the turn core and flow stay oblivious to it.
//
// Provided by leaf/accounts; AccountFor consumed by the flow router (which derives
// the entitled flow set from AccountInfo.Flow via EntitledFlows — no second store
// round-trip). An absent Accounts means default behavior: the router self-selects,
// so the pipeline runs without it.
type Accounts interface {
	// AccountFor returns the account bound to this event's handle, or ok=false when
	// the sender's handle is unbound. Read-only — for identity display (/whoami) and
	// for routing (the router reads AccountInfo.Flow through EntitledFlows).
	AccountFor(ctx context.Context, ev MessageEvent) (info AccountInfo, ok bool)

	// LangFor returns the reply language remembered for this sender (source+uid),
	// ok=false when none. The per-SENDER stable seed: a signal-less message (a slash
	// command) replies in the sender's established language. The flow consults it as
	// a fallback when the message itself carries no language signal.
	LangFor(ctx context.Context, source, uid string) (lang string, ok bool)
	// RememberLang seeds the sender's reply language (best-effort, seed-once: a no-op
	// when lang is empty/"default" or the sender already has one). The flow calls it
	// on a real-language message so a later signal-less message from the same sender
	// resolves correctly — including in a group, where lang is per-sender not per-chat.
	RememberLang(ctx context.Context, source, uid, lang string)
}

// AccountProvisioner is the WRITE seam for onboarding, kept SEPARATE from the
// read-only Accounts so the routing hot-path stays read-only. Provided by
// leaf/accounts; consumed LAZILY by the onboarding path (intro creates a bare
// identity; admin approval grants access via /accounts approve, same-package).
// Identity (account) and access (flow) are layered: EnsureBareAccount mints
// identity with NO access. It keys on the platform-FAMILY handle (one identity
// across every per-bot source in the family), derived via MessageEvent.AccountHandle.
type AccountProvisioner interface {
	// EnsureBareAccount binds the event's handle to an account if it has none,
	// creating a NEW account with NO flow entitlement (identity only — access is
	// granted separately at admin approval, NOT here, and NOT auto-allowlisted). A
	// handle already bound returns its existing account (created=false).
	EnsureBareAccount(ctx context.Context, ev MessageEvent) (accountID string, created bool, err error)
}

// AccountInfo is the account bound to a sender's handle, for display (e.g.
// /whoami) and routing. Flow is the granted entitlement comma-list (parsed via
// EntitledFlows); a KNOWN-empty Flow grants NOTHING — a bare onboarding account
// pending approval — there is NO implicit "normal" default.
type AccountInfo struct {
	ID          string
	DisplayName string
	Flow        string
	Status      string // "active" | "paused" ("" = active); paused → access falls back to intro
	// FlowKnown is true when Flow was actually read from the store. It is FALSE only on a
	// transient store blip (the handle resolved but the account row didn't read), where
	// Flow is unknown — NOT empty. The router fails CLOSED on !FlowKnown (F43, reversing
	// D27): a non-admin whose entitlement couldn't be read is refused for that turn rather
	// than served on an unverified assumption — enforcement must not lean on a read that
	// didn't happen; it self-heals next turn. A KNOWN-empty Flow is treated as "no
	// entitlement" (a bare onboarding account). Without this flag the two are
	// indistinguishable (both Flow=="").
	FlowKnown bool
}

// EntitledFlows parses a stored account.flow field (a comma-list) into the entitled
// flow-name set, de-duped, blanks dropped, order preserved. NO implicit floor: an
// empty flow grants NOTHING (not even "normal") — access is a granted entitlement, so a
// bare/onboarding account with no flow can use no path until one is granted (the router
// then funnels it to intro). Existing deployments are migrated to carry "normal"
// explicitly (accounts schema v4) so no one loses basic access in the flip. The single
// shared parse rule for the entitlement axis: the router derives a caller's entitled set
// from the AccountInfo.Flow it already read (no second store round-trip).
func EntitledFlows(flow string) []string {
	var out []string
	for _, f := range strings.Split(flow, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}
