package contract

import "context"

// MemberFailureSink records a failed agora member turn's trajectory SNAPSHOT for
// failure-driven role-guidance learning (docs/wip-member-learn.md). Provided by
// leaf/agora; called by the agora flow on a degraded member turn. The flow passes the
// turn's scope + snapshot only — agora resolves scope → inbox → member → active
// employments → the (company,role) pairs, and records the snapshot under EACH (a member
// may hold several roles; the per-role distiller filters which the failure was about,
// same as cross-skill attribution). snapshot is a multi-line text record (user ask +
// action outline + degraded output).
type MemberFailureSink interface {
	RecordFailure(ctx context.Context, scope, snapshot string)
}

// GroupRouter is the NARROW agora seam the session resolver consults to route a fanned
// group (multiple member inboxes on one chat) to a specific member's sub-scope. The
// resolver needs only this, not the full Agora surface (decouple); contract.Agora
// satisfies it. Optional — nil when no agora is bootstrapped (then groups route 1:1).
type GroupRouter interface {
	RouteGroupScope(ctx context.Context, ev MessageEvent) (subScope string, members []string, ok bool)
}

// Agora is the org-graph capability: it routes a chat scope to an agora inbox,
// assembles the per-turn agora context (identity + directory + playbook), and
// self-authorizes the turn's tool/skill bag from the company's per-role grants.
//
// Provided by leaf/agora (Optional — absent when no agora is bootstrapped);
// consumed by the agora flow. agora SUPPLIES the projection for an agora turn
// (MemberProjection / RoleProjection from the company permissions_yaml grants); it
// does NOT judge — warrant is the single judge over whatever GrantSet it's handed
// (docs §14). Channel vending (warrant.File/Exec) is the one residual agora leans on
// warrant for directly.
//
// ④a (read graph) scope: routing + context + self-auth. The return leg (worker
// output → caller) is deliberately NOT part of this interface: it is wired in the
// session MECHANISM layer (leaf/session's targetFor picks the destination, the
// bridge source's returnSink re-emits; docs/agora-port.md ④b-2) — do not re-add a
// RouteReturn here. Only StampInbox (per-message agora history attribution) remains
// deferred.
type Agora interface {
	// InboxForScope reports the agora inbox a chat scope routes to, if any.
	// ok=false for a non-agora scope (no inbox_source matches) — the agora flow's
	// Accepts uses it to claim only agora-routed work. A pure in-process read.
	InboxForScope(ctx context.Context, scope string) (inboxID string, ok bool)

	// ScopeIsAgora reports whether a chat scope is an agora scope AT ALL — bound to
	// ANY inbox: the 1:1 chat/native case (single inbox, incl. a bridge) OR a
	// virtual-group fan (>1 member inbox, which InboxForScope deliberately leaves
	// unresolved). The onboarding path reads it to decide whether to grant the agora
	// flow when admitting a sender — INDEPENDENT of which member a future message
	// addresses (that stays per-message routing). Use this, NOT InboxForScope, for
	// the "is this scope agora" question, so a busy multi-member group isn't a false
	// negative. A pure in-process read.
	ScopeIsAgora(ctx context.Context, scope string) bool

	// RoleProjection collapses a (company, role)'s grants into one GrantSet (bundles
	// already expanded to tool:use:arrangement_* at config load). company matches id OR
	// name; unknown → empty (fail-closed). A SUPPLY operation — agora does not judge;
	// the caller checks membership (e.g. the arrangement dispatcher: proj.Granted(
	// "tool:use:arrangement_pull")). A pure in-process read. See docs §14.
	RoleProjection(ctx context.Context, company, role string) GrantSet

	// CompanyBridgeScope returns the scope of a company's BRIDGE inbox (its "talk-to-human"
	// line), or ("",false) when the company has none. Lets a non-chat producer (e.g. an
	// arrangement reporting a finished product) deliver to the company's operator without
	// knowing the def author — the commander may itself be an agent. A pure in-process read.
	CompanyBridgeScope(ctx context.Context, companyID string) (scope string, ok bool)

	// RouteGroupScope picks the member sub-scope for a GROUP chat event bound to a
	// virtual-group fan (>1 member inbox on the chat): a "^<name>" token, else the
	// group's default member. Returns (subScope,members,true) when a member is picked
	// (route to "<group>#<member>"); ("",members,true) when it IS a fanned group but no
	// member was picked (caller asks "找谁", members lists the choices); ("",nil,false)
	// when not a fanned group (route normally). A pure in-process read.
	RouteGroupScope(ctx context.Context, ev MessageEvent) (subScope string, members []string, ok bool)

	// InboxScopes returns every chat scope that routes to an inbox — its own native
	// scope plus each wired inbox_source's scope — for the webui chat-log reader
	// (flow/agora pages the merged history across these scopes). The inverse of
	// InboxForScope; nil/empty when the inbox is unknown. A pure in-process read.
	InboxScopes(ctx context.Context, inboxID string) []string

	// TurnContext assembles the per-turn agora context for a scope: the resolved
	// inbox, the minted warrant principal, and the pre-rendered directory + playbook
	// prompt text (the prompt layer never imports agora). A pure read over the
	// in-process org mirror. A scope that does not resolve to a live member inbox
	// yields a no-grants principal so a hub identity can never leak into an agora turn.
	TurnContext(ctx context.Context, scope string) AgoraTurn

	// MemberProjection collapses an inbox member's authorization into ONE GrantSet: the
	// cross-company INTERSECTION of its (company, role) grants, narrowed by the inbox's
	// visibility filter (dormant today → no narrowing). bundles already expanded. An
	// inbox with no live member/role → empty (fail-closed). A SUPPLY operation — agora
	// does NOT judge; the flow hands this projection to warrant (Authorize /
	// AuthorizeSkills / ResolveCredential) which is the single judge. See docs §14.
	MemberProjection(ctx context.Context, inboxID string) GrantSet
	// DecorateToolSpecs enriches space-taking tool specs with the member's reachable
	// company spaces (the model can't pick a space it isn't shown). DECORATION, not
	// judgment — agora enriches; warrant then judges the result against MemberProjection.
	// specs are copied (the shared catalog is not mutated). See docs §14.
	DecorateToolSpecs(ctx context.Context, inboxID string, specs []ToolSpec) []ToolSpec

	// BridgeRouting reports how a scope that is a BRIDGE inbox routes (a bridge never
	// runs an LLM turn — it forwards/re-routes). isBridge=false for a non-bridge scope
	// (the flow runs a normal turn). When isBridge: externalTarget is the bound
	// external chat (for an internal/worker message → forward outbound; zero Target if
	// the bridge has no external binding), and defaultInboxScope is the downstream
	// member inbox an external/human message re-routes to ("" if unset). companyLabel is
	// the bridge's owning company name ("" if unresolved) — outbound forwards prefix it so
	// several companies' bridges can share one operator chat and stay distinguishable.
	BridgeRouting(ctx context.Context, scope string) (externalTarget Target, defaultInboxScope string, companyLabel string, isBridge bool)

	// IsPaused reports whether the company this scope belongs to is paused. While
	// paused the agora flow stops the company's WHOLE link — member turns AND bridge
	// routing — and bounces every arriving message with a notice (per-message, not
	// deduped: the sender resends after resume). Pause is an in-memory toggle (not
	// persisted; a restart clears it). A scope that does not resolve, or whose company
	// is not paused → false.
	IsPaused(scope string) bool

	// PausedMemberScope reports whether scope maps to an agora member inbox that EXISTS
	// but is PAUSED (its owner company still active) — the reversible inbox-level pause.
	// Such a scope stays AGORA's: the flow claims it and bounces "member paused" rather
	// than letting the normal catch-all answer a company group with generic bob (F161).
	// Company-level pause is the separate IsPaused path. False for a live / non-agora
	// scope. Pure in-memory read.
	PausedMemberScope(ctx context.Context, scope string) bool

	// MemberScopesForRole returns the own-inbox scope of every ACTIVE member who holds
	// (company, role) — company matched by id OR name. The arrangement dispatcher uses it
	// to nudge a stage's role-holders to pull work. nil when nothing matches
	// (fail-closed: the dispatcher nudges no one). A pure in-process read over the org
	// mirror.
	MemberScopesForRole(ctx context.Context, company, role string) []string

	// CompanyActive reports whether a company (matched by id OR name) is live — it
	// exists, is not disabled, and is not paused. The arrangement dispatcher gates on it:
	// a disabled/paused company's buckets are NOT dispatched (its items stay queued and
	// resume when the company comes back), rather than parked unmet. A pure in-process read.
	CompanyActive(ctx context.Context, company string) bool
}

// AgoraTurn is the per-turn agora context the flow consumes — a THIN value (no
// heavy agora entity leaks out of leaf/agora; boundary A). Directory + Playbook
// are pre-rendered strings the flow drops into the identity prompt layer.
type AgoraTurn struct {
	// InboxID the scope resolved to ("" when the scope is not agora-routed). The
	// flow passes it to AuthorizeTools / AuthorizeSkills.
	InboxID string
	// MemberID is the inbox's member (the employee filling the role) — finer than the
	// per-ROLE Principal. The flow stamps it (WithMember) so a worker's browser login MASTER
	// is per-member: same role, different members may hold different site logins. "" when the
	// scope is not a live member inbox.
	MemberID string
	// Principal is the warrant principal the flow stamps on ctx (WithIdentity):
	// "company:<co>/role:<role>" for a live member inbox, else a no-grants sentinel
	// so a hub identity can never leak into an agora turn. Used by channel vending +
	// billing + logging — NOT by AuthorizeTools (agora self-authorizes by inboxID).
	Principal string
	// Identity is the rendered member IDENTITY card — who this agent IS this turn
	// (member name, each active employment's company name + role, plus the member's
	// PromptStyle persona; NO internal IDs — prompt hygiene). The agora flow injects it
	// into the IDENTITY prompt layer, REPLACING the default "你是 bob" persona (other
	// layers untouched). "" when the scope is not a live member inbox → flow keeps default.
	Identity string
	// Directory is the rendered contacts list (who this turn can reach) for the
	// agora flow's PLATFORM prompt layer (a slot distinct from the identity layer);
	// "" when there are no contacts.
	Directory string
	// Playbook is the rendered company operating manual for the same platform prompt
	// layer; "" when the company has none.
	Playbook string
	// Spaces is the set of file-space names this turn may open BEYOND its own default
	// workspace — one per company the inbox's member is employed by (a member in N
	// companies → N entries). The flow loads them into BoundChannels.Allowed (the
	// hard gate) and the space-taking tools advertise them; "" / nil when the member
	// has no companies. The names are raw scope strings (warrant flattens them to a
	// path component itself).
	Spaces []string
}

// AgoraSend is the send_message tool's agora seam (Stage B, docs/agora-port.md
// §5.2/§6.1): target name-resolution + send authorization, all keyed by SCOPE
// (agora resolves scope→inbox itself) and string-in/out so leaf/tools never imports
// agora types. Provided by leaf/agora; consumed LAZILY by send_message (absent →
// the tool degrades to Stage A: raw scope emit, no name-resolution / no send
// check). 鉴权 = 通讯录 = 同一份逻辑.
type AgoraSend interface {
	// ResolveTarget maps a send_message `to` (a Member name OR an explicit scope) to a
	// destination session scope, from the caller's scope. note is a human-readable
	// explanation of the resolution; err on no-such-target / lookup failure. NOTE: the
	// only caller is an agora-principal send (send_message routes non-agora callers
	// straight to Stage A raw emit WITHOUT calling this), and for that caller err is a
	// HARD DENY (fail-closed) — it never falls back to raw emit, so send authorization
	// can't be evaded with an unresolvable `to`.
	ResolveTarget(ctx context.Context, callerScope, toArg string) (targetScope, note string, err error)
	// CheckSend authorizes a send AND returns the normalized delivery scope. denied = ""
	// allows; a non-empty string is the denial reason. Allowed iff targetScope is in the
	// caller's reachable directory (same-company auto + own-company bridge). Self-send
	// denied. A non-nil err is a lookup failure the caller MUST treat as fail-closed. No
	// admin allow-all bypass. deliver (valid only when allowed) is the scope to actually
	// emit to = the RESOLVED inbox's OWN scope — for a bridge target, the bridge inbox's
	// own scope, NOT the verbatim external chat scope, so a worker's internal event lands
	// on the inbox session and never mixes into the operator's external-chat session (F87).
	CheckSend(ctx context.Context, callerScope, targetScope string) (deliver, denied string, err error)
	// ResolveCredentialName authorizes an `as=<name>` against the caller's role grants
	// (credential:use:<name>); "" asName → ("", nil) passthrough. err when the role
	// is not granted the named credential.
	ResolveCredentialName(ctx context.Context, callerScope, asName string) (string, error)
	// ResolveBridge resolves the caller's own company BRIDGE scope — the company's
	// "to-human bus" — for an escalation to a human (escalate_to_coo's worker path).
	// ("", false, nil) = the company has no bridge; a non-nil err = ambiguous (the
	// caller is employed by multiple companies with bridges, so which COO is unclear)
	// or a lookup failure (treat as fail-closed). (scope, deliverable, nil) otherwise,
	// where deliverable reports whether the bridge has a bound external chat (a human
	// on the other end). The returned scope is already caller-reachable (it comes from
	// the caller's own directory), so no separate CheckSend is needed.
	ResolveBridge(ctx context.Context, callerScope string) (bridgeScope string, deliverable bool, err error)
}
