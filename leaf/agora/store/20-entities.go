package store

import "agentbob/contract"

// Agora entity types. These live HERE, in the agora module's own store
// package — NOT in contract. Per docs/agora-port.md §4 boundary A: agora
// entities are the module's own data backed by self-managed `bob_agora_*`
// tables; they never cross the trunk. The contract.Agora interface (a later
// stage) expresses agora to the rest of the system via IDs + thin values +
// pre-rendered strings, so these heavy structs stay inside the module.
//
// House rules carried over from the skeleton:
//   - The session tables are NEVER extended with agora columns and there is
//     NO per-session agora side table. Real-time agora identity is recomputed
//     per turn (chat scope → inbox → role/company); history attribution is
//     stamped per MESSAGE (a later stage), not per session.
//   - Tables are prefixed `bob_agora_`.
//
// Constraints enforced in the CRUD layer (not SQL CHECK — too dialect-
// specific):
//   1. inbox_sources bind ONE external chat via generic coordinates
//      (source_id + chat_type + chat_id + thread_id); the UNIQUE index over
//      those four catches duplicate wirings. No matcher JSON / canonicalization.
//   2. inboxes.kind='bridge' → owner_company_id set, member NULL.
//   3. inboxes.kind='member' → member_id set.
//   4. each Company carries its full role/grants config inline as
//      PermissionsYAML (same shape as $BOB_HOME/permissions.yaml).

// ---------- Members ----------

// Member is one row in bob_agora_members. Members are durable identities —
// they survive Companies / Employments / Inboxes and may be employed by
// multiple Companies simultaneously via Employments.
type Member struct {
	ID           string
	Name         string
	ModelPrefTag string
	PromptStyle  string
	CreatedAt    int64
	UpdatedAt    int64
}

// ---------- Companies ----------

// Company is one row in bob_agora_companies. Owns the per-role grants config
// (inline in PermissionsYAML) + a bridge Inbox (via ReportInboxID). Config is
// free-form JSON for per-Company defaults.
//
// PermissionsYAML is the FULL per-Company role config — same yaml shape as
// $BOB_HOME/permissions.yaml (top-level grants matrix). Stored as TEXT (empty
// default ""); parsed once into the OrgCache at startup + on every write.
//
// ReportInboxID is the Company's bridge inbox — the admin's "send messages to
// this Company" entry point. Nullable ("" when unset).
//
// Playbook is the per-Company operating manual (free prose, raw TEXT) — the
// collaboration map injected into the agora turn's PLATFORM prompt layer next to the
// directory. Unlike PermissionsYAML it is never parsed / validated / reconciled.
type Company struct {
	ID              string
	Name            string
	Config          map[string]any // JSON; treat as read-only
	PermissionsYAML string         // full permissions.yaml-shape document; "" until first write
	Playbook        string         // raw operating manual prose
	ReportInboxID   string         // nullable; "" when unset
	// Status is the company-level kill switch: "active" (default) or
	// "disabled". Disabled = gateway drops new inbound + send-resolver refuses
	// cross-co sends INTO it; in-flight work continues and outbound still flows.
	// Companies are never hard-deleted; admin disables instead.
	Status    string
	CreatedAt int64
	UpdatedAt int64
}

// ---------- Employments ----------

// Employment is one row in bob_agora_employments — the link between a Member
// and a (Company, role) pair. Status is "active" or "terminated"; EndedAt is 0
// while active and set on terminate.
//
// One Member may hold multiple active Employments (multi-Company); a partial unique
// index (member_id, company_id, role_name) WHERE ended_at IS NULL keeps at most one
// ACTIVE row per triple while letting a Member re-join the same (Company, role) after
// termination (the old row's ended_at is set, so it leaves the index) — even within
// the same wall-clock second. RoleName resolves into the Company's PermissionsYAML to
// recover the grants for this Employment.
type Employment struct {
	ID        string
	MemberID  string
	CompanyID string
	RoleName  string
	Status    string // "active" | "terminated"
	StartedAt int64
	EndedAt   int64 // 0 when active
}

// ---------- Inboxes ----------

// Inbox is one row in bob_agora_inboxes. Inbox is the routing-aggregation
// point — one Inbox can receive from many sources and an inbox_source can route
// into many Inboxes via matchers.
//
// Kind is "member" (the common case, owned by a Member) or "bridge"
// (admin-facing inbox for a Company).
//
// BridgeDefaultTargetInboxID — for bridge inboxes only — the default downstream
// inbox an inbound bridge message routes to.
//
// ValidationDefault is "auto" | "manual" | "notify" | "none" — the inbox-wide
// default validation mode applied to new sessions when the task didn't specify one.
//
// IdleTimeoutSeconds / FinalizeGraceSeconds tune the dispatcher's per-session
// finalize cadence. Context is the Inbox-level memory snippet.
type Inbox struct {
	ID                         string
	Kind                       string // "member" | "bridge"
	MemberID                   string // empty when bridge
	OwnerCompanyID             string // empty when member; set when bridge
	Scope                      string // bridge: "company:<Name>/bridge:main"; member: "member:<Name>/inbox:<name|companyID>"
	Name                       string
	Status                     string // "active" | "paused" | "archived"
	CreatedAt                  int64
	ArchivedAt                 int64 // 0 when active
	BridgeDefaultTargetInboxID string
	ValidationDefault          string // auto | manual | notify | none
	IdleTimeoutSeconds         int
	FinalizeGraceSeconds       int
	Context                    string
	// Visibility is the optional per-Inbox tool/skill HIDE filter (a denylist).
	// nil / empty = nothing hidden (all granted tools/skills visible). Persisted as
	// yaml in bob_agora_inboxes.visibility_yaml.
	Visibility *InboxVisibility
}

// InboxVisibility is the per-Inbox visibility filter — a DENYLIST: HiddenTools /
// HiddenSkills are entity names to HIDE from this inbox's sessions. A name NOT in
// the list is visible. Empty / nil lists hide nothing (everything granted stays
// visible). Visibility only ever NARROWS (it can hide a granted entity, never grant
// one) — agora's permission surface only shrinks. Wildcards unsupported (exact name).
type InboxVisibility struct {
	HiddenTools  []string `yaml:"hidden_tools,omitempty"`
	HiddenSkills []string `yaml:"hidden_skills,omitempty"`
}

// ---------- Inbox Sources ----------

// InboxSource is one row in bob_agora_inbox_sources. It binds ONE external chat
// to an Inbox, bidirectionally — the external chat is identified by generic
// coordinates (SourceID + ChatType + ChatID + optional ThreadID):
//
//   - inbound routing: the bound chat's scope = contract.ScopeFor over these
//     coordinates (the ONE scope grammar, shared with the session resolver), so
//     the router never re-derives per-source scope shapes.
//   - outbound delivery: the reply Target is {Source: SourceID, ChatID, ThreadID}
//     directly — the coordinates ARE the address.
//
// The UNIQUE (source_id, chat_type, chat_id, thread_id) index catches duplicate
// wirings. No matcher JSON, no canonicalization.
//
// OutgoingCredentialRef is the optional broker credential name to use when
// replying via this Inbox (defaults to the source-internal in-process emit when
// empty).
type InboxSource struct {
	ID                    string
	InboxID               string
	SourceID              string
	ChatType              contract.ChatType
	ChatID                string
	ThreadID              string
	OutgoingCredentialRef string // optional; broker credential name
	IsDefault             bool   // group binding: this member is the group's default addressee (when @bot has no #member)
	CreatedAt             int64
}
