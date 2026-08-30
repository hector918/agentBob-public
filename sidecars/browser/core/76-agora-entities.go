package core

// Agora — M3a entity types.
//
// Schema lives in `bob_agora_*` side tables (sqlite v16 / pg v11). Per
// docs/agora-design.md §3 + §11.8 the agora data model is SINGLE-
// BACKEND ONLY — the fallback wrapper routes every agora call to the
// primary backend and returns ErrStoreConnLost when on secondary so
// callers can halt rather than silently operate on an empty mirror.
//
// House rule reminders:
//   - The `sessions` (sqlite) / `bob_sessions` (pg) tables are NEVER
//     extended with agora columns, and there is NO per-session agora side
//     table. Real-time agora identity is recomputed per turn (chat scope →
//     inbox → role/company) and stashed on AgentCtx.InboxID; history
//     attribution is stamped per MESSAGE via `messages.agora_inbox_id`
//     (not per session). The old `bob_agora_session_meta` / SessionMetaStore
//     was removed — see docs/agora.md §一·五.
//   - Tables are prefixed `bob_agora_` on both backends so SQL paths line
//     up regardless of which one is primary.
//
// Constraints enforced in the CRUD layer (not SQL CHECK — too dialect-
// specific between sqlite and pg):
//   1. inbox_sources.matcher is canonicalized before INSERT (11.4)
//   2. communication_channels: exactly one of (from_company_id+from_role_name,
//      from_member_id) and (to_company_id+to_role_name, to_member_id) must be set
//   3. inboxes.kind='bridge' → owner_company_id set, employment/member NULL
//   4. inboxes.kind='member' → employment_id set
//   5. each Company carries its full role/grants config inline as
//      permissions_yaml (TEXT, same shape as $BOB_HOME/agora/permissions.yaml);
//      admin edits the yaml directly via the webui editor, no per-role rows.
//
// Account entity dropped (A refactor): agora no longer holds
// Account rows. Credentials live in the credential broker (see
// docs/credentials-design.md). Inbox_sources reference the outbound
// broker credential by name via OutgoingCredentialRef. Until the
// broker C0-C2 milestone ships, a temporary filesystem-baseline lookup
// at ~/.bob/credentials/<name>.yaml resolves the outbound config.

// Mirrors ErrAgoraNotFound — see core/75-agora-role.go.
// "not found" is a normal control-flow outcome; callers errors.Is-check
// and surface a user-friendly "no such X" without logging it as a
// backend failure.
//
// Defined in core/75-agora-role.go (the existing agora file) per the
// task spec; declared there so the entire agora subsystem has a single
// "not found" sentinel.

// ---------- Members ----------

// Member is one row in bob_agora_members. Members are durable identities
// — they survive Companies / Employments / Inboxes and may be employed
// by multiple Companies simultaneously via Employments.
//
// ToolsKnown is a JSON-serialised list of tool names the Member is
// allowed to invoke (M5+ refinement on top of the role-audience gate).
// MemoryPrompt is the Member-level memory snippet (M5+ usage).
//
// No PrimaryInbox field (deleted): cross-Company send_message
// resolves the target inbox via the same-Company rule + an explicit
// CommunicationChannel; there is no Member-level fallback.
type Member struct {
	ID           string
	Name         string
	ModelPrefTag string
	PromptStyle  string
	ToolsKnown   []string // JSON array
	MemoryPrompt string
	CreatedAt    int64
	UpdatedAt    int64
}

// ---------- Companies ----------

// Company is one row in bob_agora_companies. Owns the per-role grants
// config (inline in PermissionsYAML) + a bridge Inbox (via ReportInboxID).
// Config is free-form JSON for per-Company defaults the dispatcher /
// Judge may read.
//
// PermissionsYAML is the FULL per-Company role config — same yaml shape
// as $BOB_HOME/agora/permissions.yaml (top-level `roles:` map → per-role
// `grants:`). Stored as TEXT (empty default ”); parsed once into
// agora.OrgCache at startup + on every write. Admin edits the document
// directly via the webui yaml editor.
//
// ReportInboxID is the Company's bridge inbox — admin's "send messages
// to this Company" entry point. Created by `/agora init` (M3b) and
// nullable in M3a (CreateCompany leaves it empty; admin sets it later).
//
// Playbook is the per-Company operating manual (free prose, raw TEXT) —
// the collaboration map injected into every role's identity layer next to
// the directory. Unlike PermissionsYAML it is never parsed / validated /
// reconciled. See docs design "company-playbook".
type Company struct {
	ID              string
	Name            string
	Config          map[string]any // JSON; treat as read-only (pg fresh per GetCompany, mutating leaks if cached)
	PermissionsYAML string         // full permissions.yaml-shape document; '' until first write
	// Playbook is the per-Company "how this company runs / who to find
	// for what" operating manual — plain prose read by ALL agora roles in
	// the company (injected alongside the directory). Stored raw, never
	// parsed / validated / reconciled. Seeded with a default template on
	// company create. company-playbook.
	Playbook      string
	ReportInboxID string // nullable; "" when unset
	// Status is the company-level kill switch: "active" (default) or
	// "disabled". Disabled = gateway drops new inbound (inbox-router
	// match) + send-resolver refuses cross-co sends INTO it; in-flight
	// work continues and outbound still flows. Companies are never
	// hard-deleted; admin disables instead. D13.
	Status    string
	CreatedAt int64
	UpdatedAt int64
}

// ---------- Employments ----------

// Employment is one row in bob_agora_employments — the link between a
// Member and a (Company, role) pair. Status is "active" or "terminated";
// EndedAt is nil-equivalent (0) while active and set on terminate.
//
// One Member may hold multiple active Employments (multi-Company); the
// (member_id, company_id, role_name, started_at) UNIQUE constraint lets
// a Member re-join the same (Company, role) after termination without
// colliding with the prior row. RoleName resolves into the Company's
// PermissionsYAML to recover the grants for this Employment.
type Employment struct {
	ID        string
	MemberID  string
	CompanyID string
	RoleName  string
	Status    string // "active" | "terminated"
	StartedAt int64
	EndedAt   int64 // 0 when active
}

// ---------- Company-role skill overrides ----------

// CompanyRoleSkillOverride is one row in bob_agora_company_role_skill_overrides
// — a per-(Company, role) fork of a baseline skill (docs/agora-role-skill-fork.md).
// Manifest is the JSON-marshalled SkillManifest overlay delta; it shares the
// baseline skill NAME (identity unchanged) and only swaps the execution part
// (body / rubric; v1 carries no scripts). RoleName is a free-text
// Employment.RoleName — the org-structural unit a manager's process branches on.
type CompanyRoleSkillOverride struct {
	CompanyID string
	RoleName  string
	SkillName string
	Manifest  string // json.Marshal(SkillManifest)
	UpdatedAt int64
}

// ---------- Channels ----------

// CommunicationChannel is one row in bob_agora_communication_channels.
// Channels are permission edges between (Company, role) pairs / Members
// — required by the Judge for cross-team send_message / report (M3b
// wiring).
//
// Endpoint invariant (enforced in the CRUD layer rather than via SQL
// CHECK — dialect-portable):
//
//	exactly one of [(FromCompanyID + FromRoleName), FromMemberID] set
//	exactly one of [(ToCompanyID   + ToRoleName),   ToMemberID]   set
//
// AllowedKinds is the JSON list of action kinds the channel permits
// (["send_message"] — the report / send_message:with_validation kinds
// retired with reports_to). An empty list is "deny-all": a channel that
// permits no kind makes its target unreachable (BuildDirectory's
// channelPermitsSend gate). EffectiveFrom / EffectiveTo are nullable
// unix-second windows; 0 means "no bound on that side".
type CommunicationChannel struct {
	ID            string
	FromCompanyID string // paired with FromRoleName; both empty when FromMemberID is set
	FromRoleName  string
	FromMemberID  string // empty when (FromCompanyID + FromRoleName) is set
	ToCompanyID   string
	ToRoleName    string
	ToMemberID    string
	AllowedKinds  []string
	EffectiveFrom int64 // 0 = no bound
	EffectiveTo   int64 // 0 = no bound
	CreatedAt     int64
	CreatedBy     string
	Reason        string
}

// ---------- Inboxes ----------

// Inbox is one row in bob_agora_inboxes. Inbox is the routing-aggregation
// point per §4 — one Inbox can receive from many sources (telegram chat
// + email + internal source) and an inbox_source can route into many
// Inboxes via matchers.
//
// Kind is "member" (the common case, owned by an Employment) or
// "bridge" (admin-facing inbox for a Company; M3b adds special routing).
//
// BridgeDefaultTargetInboxID — for bridge inboxes only — the default
// downstream inbox an inbound bridge message routes to (e.g. CEO's
// member inbox). M3a stores the column; M3b wires the behaviour.
//
// ValidationDefault is "auto" | "manual" | "notify" | "none" — the
// inbox-wide default validation mode applied to new sessions when the
// task didn't specify one (overridden per-session via SessionMeta).
//
// IdleTimeoutSeconds / FinalizeGraceSeconds tune the dispatcher's
// per-session finalize cadence (§7).
//
// Context is the Inbox-level memory snippet — M5+ usage; schema present
// in M3a so the column exists by the time the feature ships.
type Inbox struct {
	ID                         string
	Kind                       string // "member" | "bridge"
	EmploymentID               string // empty when bridge
	MemberID                   string // empty when bridge
	OwnerCompanyID             string // empty when member; set when bridge
	Scope                      string // "company:<Name>/member:<Name>/inbox:<name>"
	Name                       string
	Status                     string // "active" | "archived"
	CreatedAt                  int64
	ArchivedAt                 int64 // 0 when active
	BridgeDefaultTargetInboxID string
	ValidationDefault          string // auto | manual | notify | none
	IdleTimeoutSeconds         int
	FinalizeGraceSeconds       int
	Context                    string
	// Visibility is the optional per-Inbox tool/skill filter (PR-2
	//). nil = no filter (all tools/skills visible to this
	// Inbox's sessions); non-nil = whitelist intersected with
	// grants-based visibility at SpecsForSession / SkillsForSession time.
	// Persisted as yaml in bob_agora_inboxes.visibility_yaml.
	Visibility *InboxVisibility
}

// InboxVisibility is the per-Inbox visibility filter. Tools and Skills
// are whitelists of entity names; an empty slice = fail-closed (no
// entity of that kind visible to this Inbox's sessions). A nil
// *InboxVisibility on the Inbox means "no filter at all" — distinct
// from an InboxVisibility whose Tools / Skills are both empty / nil
// (which fail-closes both). Wildcards are not supported in v1 — match
// is exact on entity name.
type InboxVisibility struct {
	Tools  []string `yaml:"tools,omitempty"`
	Skills []string `yaml:"skills,omitempty"`
}

// ---------- Inbox Sources ----------

// InboxSource is one row in bob_agora_inbox_sources. Maps an external
// source (telegram / email / internal) + a matcher (e.g. {"chat_id":
// -100123}) to an Inbox. Matcher is stored as a canonicalized JSON
// string — alphabetical keys, no whitespace, known field type-coerce
// (e.g. {"chat_id":"-100"} → {"chat_id":-100}) — so duplicate configs
// fail the UNIQUE (source_id, matcher) constraint instead of silently
// inserting a parallel row (11.4).
//
// OutgoingCredentialRef is the optional broker credential name to use
// when replying via this Inbox (defaults to the source-internal
// in-process emit when empty). Until the broker C0-C2 ships, the
// dispatcher resolves this against a filesystem baseline at
// ~/.bob/credentials/<ref>.yaml (chat_id_pool / default_chat) — see
// agora/95-baseline-credentials.go. A non-resolving ref falls back to
// the matcher's chat_id (legacy behaviour) with a WARN log.
type InboxSource struct {
	ID                    string
	InboxID               string
	SourceID              string
	Matcher               string // canonicalized JSON string
	OutgoingCredentialRef string // optional; broker credential name
	CreatedAt             int64
}
