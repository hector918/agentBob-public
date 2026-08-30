package agora

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"agentbob/contract"
	"agentbob/leaf/agora/store"
)

// This file ports skeleton agora/85-send-resolver.go into trunk, adapting from
// skeleton's callerInboxID to trunk's callerScope: the send_message tool only
// knows the turn's session SCOPE, so each method first resolves callerScope →
// inbox via the InboxRouter (the same router InboxForScope uses), then reuses the
// already-ported BuildDirectory (50-directory.go) for reachability. All string
// in/out (contract.AgoraSend) so leaf/tools never imports agora types.
//
// AgoraSend is implemented on *Impl (not a separate struct): it shares the same
// OrgCache + InboxRouter + buildDirectory as TurnContext/AuthorizeTools, so "who
// can I send to" and "who is in my directory" are provably one answer.

// callerInboxID resolves the caller's session scope to its agora inbox id via the
// router (active-only). "" for a non-agora scope.
func (a *Impl) callerInboxID(scope string) string {
	if a == nil || a.router == nil {
		return ""
	}
	ib, ok := a.router.ResolveByScope(scope)
	if !ok {
		return ""
	}
	return ib.ID
}

// looksLikeSessionScope reports whether toArg is already a session scope (vs a
// Member name): it carries one of the scope-shape markers, OR it matches the own
// Scope of some inbox in the cache. Ported from skeleton looksLikeSessionScope.
func (a *Impl) looksLikeSessionScope(toArg string) bool {
	if strings.Contains(toArg, ":dm:") || strings.Contains(toArg, ":group:") || strings.Contains(toArg, ":inbox:") {
		return true
	}
	// An inbox's own Scope (e.g. "co:acme/alice") has no marker — match it directly.
	if a != nil && a.router != nil {
		if _, ok := a.router.ResolveByScope(toArg); ok {
			return true
		}
	}
	return false
}

// ResolveTarget maps a send_message `to` (a Member name OR an explicit scope) to a
// destination session scope, from the caller's scope.
//
// Resolution (event-driven, no implicit fallbacks — ported from skeleton):
//  1. toArg already looks like a scope → use it verbatim.
//  2. Otherwise resolve toArg as a Member name → that member's active member-inbox
//     in the caller's SAME company (the only route — there is no cross-company
//     reachability).
//
// err = "not an agora-resolvable target" (the tool falls back to Stage A raw emit).
func (a *Impl) ResolveTarget(ctx context.Context, callerScope, toArg string) (string, string, error) {
	if a == nil {
		return toArg, "", nil // resolver disabled — pass through unchanged
	}
	toArg = strings.TrimSpace(toArg)
	// (1) explicit scope — use verbatim.
	if a.looksLikeSessionScope(toArg) {
		return toArg, "explicit scope", nil
	}
	if a.org == nil {
		return "", "", fmt.Errorf("no Member named %q (resolver has no org cache)", toArg)
	}
	// (2) treat toArg as a Member name across ALL the caller's companies. One frozen
	// snapshot drives the whole walk (caller member → its active companies → each
	// company's active employments → member name → active inbox) so it is internally
	// consistent. We COLLECT every active colleague named toArg across the caller's
	// companies rather than returning the first: a bare name that matches in MORE THAN
	// ONE of them is AMBIGUOUS, and silently picking first-by-company-name would deliver
	// to the wrong alice with no warning. So >1 match → an error listing the candidate
	// inbox scopes, telling the worker to disambiguate with to=<inbox-scope> (mirrors
	// ResolveBridge's multi-bridge ambiguity). (L-AG-D2)
	snap := a.org.Snapshot(ctx, nil)
	callerCompanyIDs := callerCompanyIDsInSnap(snap, a.callerInboxID(callerScope))
	if len(callerCompanyIDs) == 0 {
		return "", "", fmt.Errorf("caller has no live agora company — resolve %q via an explicit scope (to=<inbox-scope>)", toArg)
	}
	memberName := make(map[string]string, len(snap.Members))
	for _, m := range snap.Members {
		memberName[m.ID] = m.Name
	}
	// A colleague's company inbox is member-owned, scoped MemberOwnedInboxScope(
	// memberName, companyID) — one candidate scope per company (callerCompanyIDs is
	// unique), so matches carries no intra-company duplicates.
	var matches []string
	for _, companyID := range callerCompanyIDs {
		employed := false
		for _, e := range snap.Employments {
			if e.Status == "active" && e.CompanyID == companyID && memberName[e.MemberID] == toArg {
				employed = true
				break
			}
		}
		if !employed {
			continue
		}
		want := MemberOwnedInboxScope(toArg, companyID)
		for _, ib := range snap.Inboxes {
			if ib.Inbox.Scope == want && ib.Inbox.Status == "active" {
				matches = append(matches, want)
				break
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", "", fmt.Errorf("no active colleague named %q in any of your companies", toArg)
	case 1:
		return matches[0], "colleague match: " + matches[0], nil
	default:
		return "", "", fmt.Errorf("你的多家公司里都有叫 %q 的同事，无法确定发给谁 — 请用 to=<inbox-scope> 指明是哪一个（%s）", toArg, strings.Join(matches, "、"))
	}
}

// callerCompanyIDsInSnap resolves a caller inbox id to ALL of its agora companies
// from a FROZEN snapshot, NAME-sorted (deterministic):
//   - bridge inbox (OwnerCompanyID set): the single owning company.
//   - member inbox (MemberID set): every company the member is actively employed
//     at (the union — a multi-company member reaches colleagues in any of them).
//
// Empty for an inbox not in the snapshot, a member with no active employment, or a
// disabled company. The snapshot-local twin of buildDirectory's company derivation
// — used so ResolveTarget reads one consistent snapshot.
func callerCompanyIDsInSnap(snap OrgSnapshot, inboxID string) []string {
	if inboxID == "" {
		return nil
	}
	var caller store.Inbox
	found := false
	for _, ib := range snap.Inboxes {
		if ib.Inbox.ID == inboxID {
			caller = ib.Inbox
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	companyByID := make(map[string]store.Company, len(snap.Companies))
	for _, co := range snap.Companies {
		companyByID[co.ID] = co
	}

	ids := make(map[string]struct{})
	switch {
	case caller.OwnerCompanyID != "":
		ids[caller.OwnerCompanyID] = struct{}{}
	case caller.MemberID != "":
		for _, e := range snap.Employments {
			if e.Status == "active" && e.MemberID == caller.MemberID {
				ids[e.CompanyID] = struct{}{}
			}
		}
	}

	companies := make([]store.Company, 0, len(ids))
	for id := range ids {
		co, ok := companyByID[id]
		if !ok || co.Status == "disabled" {
			continue
		}
		companies = append(companies, co)
	}
	sort.Slice(companies, func(i, j int) bool { return companies[i].Name < companies[j].Name })
	out := make([]string, len(companies))
	for i, co := range companies {
		out[i] = co.ID
	}
	return out
}

// CheckSend authorizes a send: "" = allow, a non-empty denial reason otherwise.
// 鉴权 = 通讯录 = 同一份逻辑: a send is allowed exactly when the target inbox is in the
// caller's reachable directory (BuildDirectory: same-company colleagues auto +
// own-company bridge). There is no cross-company path (a member in two companies
// reaches each company's colleagues via that company's own inbox). Self-send
// denied. A non-nil err is a lookup failure the caller treats as fail-closed.
// There is deliberately no admin allow-all bypass: an admin escape inside the
// judge would let any future caller skip authorization entirely.
func (a *Impl) CheckSend(ctx context.Context, callerScope, targetScope string) (string, string, error) {
	if a == nil || a.org == nil || a.router == nil {
		return targetScope, "", nil // resolver disabled — fail open (Stage A semantics), deliver as-given
	}
	callerInboxID := a.callerInboxID(callerScope)
	if callerInboxID == "" {
		return targetScope, "", nil // non-agora caller — no agora context, allow (Stage A), deliver as-given
	}
	// Receiver inbox via scope (own scope OR a chat scope routed to an inbox).
	receiver, ok := a.router.ResolveByScope(targetScope)
	if !ok {
		// We are past the non-agora allow above, so the caller IS an agora worker. A
		// target scope that resolves to NO agora inbox (e.g. a raw external chat like
		// "telegram:dm:<victim>") is OUTSIDE the colleague/bridge directory — DENY
		// (fail-closed). Allow-on-unknown is correct only for the non-agora Stage-A
		// caller, which already returned above; letting an agora worker through here
		// would let it emit a worker-controlled message into any external chat.
		return "", "目标不在你的 agora 通讯录里 — 只能发给同公司同事或本公司 bridge", nil
	}
	// Self-send guard: a member addressing its own inbox would loop.
	if receiver.ID == callerInboxID {
		return "", "self-send not allowed (would loop) — address a different inbox", nil
	}
	// Reachability = authorization. buildDirectory already filters disabled companies
	// and terminated/archived employments + inboxes — one membership test keeps "can I
	// send" and "is it in my directory" the same answer.
	dir, okDir := a.buildDirectory(ctx, callerInboxID)
	if !okDir {
		// Caller inbox no longer builds a directory (terminated/disabled mid-turn) →
		// fail-closed.
		return "", "你的 agora 身份已失效，无法发送", nil
	}
	for _, c := range dir.Contacts {
		if c.InboxScope == receiver.Scope {
			return receiver.Scope, "", nil // deliver to the colleague inbox's own scope (== targetScope here)
		}
	}
	// Any of the caller's companies' bridge inboxes is auto-reachable (the "talk-to-
	// human" bus, resolved by buildDirectory across the caller's whole company set). But a
	// bridge with NO bound external chat silently drops the message (routeBridge OUTBOUND
	// log-drops on ext.Source==""), so the worker must NOT be told it was delivered —
	// reject an undeliverable bridge here (parity with escalate_to_coo's deliverable
	// check), reusing BridgeRouting so authorization and deliverability are one answer.
	for _, bs := range dir.BridgeScopes {
		if receiver.Scope == bs {
			if ext, _, _, isBridge := a.BridgeRouting(ctx, bs); !isBridge || ext.Source == "" {
				return "", "目标 bridge 未绑定真人聊天，消息发不出去（需管理员 wire bridge）", nil
			}
			// F87: deliver to the BRIDGE inbox's OWN scope, not the verbatim external chat
			// scope the worker addressed — the internal event then lands on the inbox
			// session, not mixed into the operator's external-chat session (routeBridge
			// direction stays unambiguous). No-op for a colleague (own scope == target).
			return receiver.Scope, "", nil
		}
	}
	return "", fmt.Sprintf("目标 %q 不在你的通讯录里 — 只有你各公司的同事和 bridge 可达", receiver.Scope), nil
}

// ResolveCredentialName authorizes an `as=<asName>` request by checking the
// caller member's grants for "credential:use:<asName>" — the SAME member-company
// intersection AuthorizeTools projects from (memberOwnedGrants). "" asName →
// ("", nil) passthrough. Returns the name on a hit; an error explaining how to
// grant otherwise. Fail-closed: under-wired (no resolver / no inbox / no member)
// → deny.
func (a *Impl) ResolveCredentialName(_ context.Context, callerScope, asName string) (string, error) {
	asName = strings.TrimSpace(asName)
	if asName == "" {
		return "", nil // no `as=` → passthrough (dispatcher uses the inbox default)
	}
	if a == nil || a.org == nil {
		return "", fmt.Errorf("agora: credential resolver not configured (cannot authorise as=%q)", asName)
	}
	callerInboxID := a.callerInboxID(callerScope)
	if callerInboxID == "" {
		return "", fmt.Errorf("agora: turn has no agora inbox — cannot resolve as=%q", asName)
	}
	ib, ok := a.org.InboxByID(callerInboxID)
	if !ok || ib.MemberID == "" {
		return "", fmt.Errorf("agora: no member identity for this turn — cannot resolve as=%q", asName)
	}
	if _, granted := a.memberOwnedGrants(ib.MemberID)["credential:use:"+asName]; granted {
		return asName, nil
	}
	return "", fmt.Errorf("agora: your role does not grant credential %q (edit company permissions yaml)", asName)
}

// BridgeRouting reports how a bridge-inbox scope routes (a bridge never runs an
// LLM turn — flow/agora forwards/re-routes). isBridge=false for a non-bridge
// scope (the flow then runs a normal agora turn). When isBridge:
//   - externalTarget: the bridge's bound external chat (its first inbox_source),
//     for an internal/worker message → forward outbound. Zero Target when the
//     bridge has no external binding.
//   - defaultInboxScope: the bridge's BridgeDefaultTargetInboxID → that inbox's
//     own Scope, for an external/human message → re-route downstream. "" when
//     unset or the target inbox is missing.
//
// inbox_sources are read fresh (bridge routing is rare — not on the member-turn
// hot path) via the router's source loader (the same loader the router Reloads
// from), so this needs no separate store handle.
func (a *Impl) BridgeRouting(ctx context.Context, scope string) (contract.Target, string, string, bool) {
	if a == nil || a.router == nil {
		return contract.Target{}, "", "", false
	}
	ib, ok := a.router.ResolveByScope(scope)
	if !ok || ib.Kind != "bridge" {
		return contract.Target{}, "", "", false // non-agora / non-bridge → run a normal turn
	}

	// externalTarget = the bridge's bound external chat (first inbox_source).
	var ext contract.Target
	if a.router.loader != nil {
		// ListInboxSources already filters WHERE inbox_id = ib.ID, so every row IS
		// this bridge's source — take the first (S-36, was a no-op inner filter).
		if srcs, err := a.router.loader.ListInboxSources(ctx, ib.ID); err == nil && len(srcs) > 0 {
			s := srcs[0]
			ext = contract.Target{Source: s.SourceID, ChatID: s.ChatID, ThreadID: s.ThreadID}
		}
	}

	// defaultInboxScope = the downstream member inbox's own scope (re-route target).
	var defScope string
	if ib.BridgeDefaultTargetInboxID != "" && a.org != nil {
		if dib, ok := a.org.InboxByID(ib.BridgeDefaultTargetInboxID); ok {
			defScope = dib.Scope
		}
	}

	// companyLabel = the bridge's owning company name, so an outbound forward can stamp
	// WHICH company it came from (several bridges can share one operator chat).
	var companyLabel string
	if ib.OwnerCompanyID != "" && a.org != nil {
		if co, ok := a.org.CompanyByID(ib.OwnerCompanyID); ok {
			companyLabel = co.Name
		}
	}
	return ext, defScope, companyLabel, true
}

// ResolveBridge resolves the caller's own company bridge scope (the "to-human bus")
// for escalate_to_coo's worker path. Reuses the SAME directory buildDirectory computes
// for CheckSend (reachability = authorization), so the returned scope is already
// caller-reachable. ("", false, nil) when the caller's company has no bridge; an error
// when the caller has MULTIPLE reachable bridges (ambiguous — can't pick a COO without
// a target) or the identity no longer resolves (fail-closed). deliverable reports
// whether the single bridge has a bound external chat (BridgeRouting's external target).
func (a *Impl) ResolveBridge(ctx context.Context, callerScope string) (bridgeScope string, deliverable bool, err error) {
	if a == nil || a.org == nil {
		return "", false, fmt.Errorf("agora: bridge resolver not configured")
	}
	callerInboxID := a.callerInboxID(callerScope)
	if callerInboxID == "" {
		return "", false, fmt.Errorf("agora: turn has no agora inbox — cannot resolve bridge")
	}
	dir, ok := a.buildDirectory(ctx, callerInboxID)
	if !ok {
		return "", false, fmt.Errorf("agora: your agora identity is no longer valid")
	}
	switch len(dir.BridgeScopes) {
	case 0:
		return "", false, nil // no bridge — the caller decides how to fail
	case 1:
		bs := dir.BridgeScopes[0]
		ext, _, _, isBridge := a.BridgeRouting(ctx, bs)
		return bs, isBridge && ext.Source != "", nil
	default:
		return "", false, fmt.Errorf("你受雇于多家公司、有多个 bridge，无法确定该升级给哪家的 COO（%s）", strings.Join(dir.BridgeScopes, "、"))
	}
}

var _ contract.AgoraSend = (*Impl)(nil)
