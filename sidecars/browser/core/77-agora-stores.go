package core

import "context"

// Agora — M3a store interfaces.
//
// Six sibling interfaces (Member / Company / Employment / Channel /
// Inbox / SessionMeta), plus the aggregate AgoraStore that embeds them.
// Position is no longer a stored entity: each Company carries its full
// role config inline in bob_agora_companies.permissions_yaml (TEXT, same
// shape as $BOB_HOME/agora/permissions.yaml); the agora.OrgCache holds
// the parsed-in-memory view, refreshed on write.
//
// extraction: agora persistence moved into its own
// postgres-only package, agorastore, and is NO LONGER part of
// the generic `store` package (which is a leaf again — no dependency on
// the agora subsystem). The single concrete *Store in agorastore
// satisfies core.AgoraStore. There is no sqlite agora backend; a
// sqlite-only deployment runs with a nil core.AgoraStore and agora
// features stay unwired.
//
// (AccountStore dropped A refactor — agora references broker
// credentials by name; see docs/credentials-design.md.)
//
// All methods MUST wrap backend-native errors through the impl's
// classifyErr so callers can errors.Is-check core.ErrStoreConnLost
// for the agora-halt path. Not-found lookups return ErrAgoraNotFound
// (no `(row, bool, err)` triple — agora callers want errors.Is, not
// a third return slot).
//
// Concurrency: name-based UPSERT / Update paths take a per-name lock
// with an "agora_<kind>:" prefix so multi-writer races against the
// same row serialise within the agora store.

// ---------- MemberStore ----------

// MemberStore is the persistence interface for bob_agora_members.
// Identity (name) is unique; CreateMember allocates the id, sets
// CreatedAt/UpdatedAt = now, and returns the populated Member row.
type MemberStore interface {
	CreateMember(ctx context.Context, p Member, nowUnix int64) (Member, error)
	GetMember(ctx context.Context, id string) (Member, error)
	GetMemberByName(ctx context.Context, name string) (Member, error)
	ListMembers(ctx context.Context) ([]Member, error)
	DeleteMember(ctx context.Context, id string) error
}

// ---------- CompanyStore ----------

// CompanyStore is the persistence interface for bob_agora_companies.
// Name is unique. ReportInboxID is nullable in M3a — `/agora init`
// (M3b) will create the bridge inbox and call SetReportInbox to wire
// it up.
type CompanyStore interface {
	CreateCompany(ctx context.Context, c Company, nowUnix int64) (Company, error)
	GetCompany(ctx context.Context, id string) (Company, error)
	GetCompanyByName(ctx context.Context, name string) (Company, error)
	ListCompanies(ctx context.Context) ([]Company, error)

	// Company mutations are DEDICATED setters only (no blanket UpdateCompany
	// — D24 removed the unused one): Status goes through
	// UpdateCompanyStatus, PermissionsYAML through SetPermissionsYAML, the
	// bridge inbox through SetReportInbox. Narrow updates can't quietly
	// clobber concurrently-changed columns.

	// UpdateCompanyStatus is the only kill/revive switch (D13):
	// companies are NEVER hard-deleted to avoid orphaning in-flight
	// sessions / employments / inboxes. status must be "active" or
	// "disabled"; other values return an error. Caller is responsible
	// for triggering an inbox-router reload so the gateway picks up the
	// new status (disabled companies' inboxes are then dropped at match
	// time).
	UpdateCompanyStatus(ctx context.Context, companyID, status string) error

	// SetReportInbox wires the company's bridge inbox. M3b only — M3a
	// CLI doesn't call this, but the method lands now so the bridge-
	// inbox flow has an existing seam.
	SetReportInbox(ctx context.Context, companyID, inboxID string) error

	// GetPermissionsYAML returns the Company's per-role config document
	// (TEXT column on bob_agora_companies). Empty string for Companies
	// that haven't been written to since create. Returns ErrAgoraNotFound
	// when companyID has no row.
	GetPermissionsYAML(ctx context.Context, companyID string) (string, error)

	// SetPermissionsYAML replaces the Company's per-role config document.
	// Caller is responsible for validating the body (agora.ValidateRolesYAML)
	// and refreshing the in-memory agora.OrgCache after a successful
	// write. Returns ErrAgoraNotFound when companyID has no row.
	SetPermissionsYAML(ctx context.Context, companyID, yaml string) error

	// GetCompanyPlaybook returns the Company's playbook prose (TEXT column
	// on bob_agora_companies) — the per-Company "how this company runs /
	// who to find for what" operating manual. Empty string for Companies
	// seeded with an empty playbook. Returns ErrAgoraNotFound when
	// companyID has no row. company-playbook.
	GetCompanyPlaybook(ctx context.Context, companyID string) (string, error)

	// SetCompanyPlaybook replaces the Company's playbook prose. Stored
	// RAW — never parsed / validated / reconciled (it is a collaboration
	// map, not structured config). Returns ErrAgoraNotFound when companyID
	// has no row. company-playbook.
	SetCompanyPlaybook(ctx context.Context, companyID, playbook string) error
}

// ---------- EmploymentStore ----------

// EmploymentStore is the persistence interface for bob_agora_employments.
// A single Member can hold multiple active Employments; the (member_id,
// company_id, role_name, started_at) UNIQUE constraint lets the same
// member re-join the same (Company, role) after termination (the
// started_at differs).
//
// Hire is a convenience that sets Status="active", StartedAt=now, and
// allocates an id; Terminate flips Status="terminated" + sets EndedAt.
type EmploymentStore interface {
	Hire(ctx context.Context, memberID, companyID, roleName string, nowUnix int64) (Employment, error)
	GetEmployment(ctx context.Context, id string) (Employment, error)
	ListEmploymentsByMember(ctx context.Context, memberID string) ([]Employment, error)
	// ListEmployments returns every Employment row. activeOnly=true
	// filters to status="active". Used by org-snapshot consumers
	// (dashboards / external readers) that need the full active-employment
	// list in one RTT instead of N calls of ListEmploymentsByMember.
	ListEmployments(ctx context.Context, activeOnly bool) ([]Employment, error)
	Terminate(ctx context.Context, id string, nowUnix int64) error
	DeleteEmployment(ctx context.Context, id string) error
}

// ---------- ChannelStore ----------

// ChannelStore is the persistence interface for bob_agora_communication_channels.
// CreateChannel enforces the endpoint invariant (exactly one of
// (CompanyID+RoleName, MemberID) on each side) in code, not SQL CHECK
// (dialect-portable).
//
// ListChannels supports common filter dimensions. M3a callers (admin
// CLI) pass everything = "" for "list all"; M3b's Judge will pass
// from/to to find applicable channels per send_message check.
type ChannelStore interface {
	CreateChannel(ctx context.Context, ch CommunicationChannel, nowUnix int64) (CommunicationChannel, error)
	ListChannels(ctx context.Context) ([]CommunicationChannel, error)
	DeleteChannel(ctx context.Context, id string) error
}

// ---------- InboxStore ----------

// InboxStore is the persistence interface for bob_agora_inboxes and
// bob_agora_inbox_sources.
//
// CreateInbox enforces kind-specific invariants:
//   - kind='member' → EmploymentID non-empty
//   - kind='bridge' → OwnerCompanyID non-empty, Employment/Member empty
//
// Scope is auto-derived by the caller (CLI) using "company:<slug>/
// member:<id>/inbox:<name>" so admin doesn't have to compose it
// manually; the UNIQUE index on scope catches collisions.
//
// AddInboxSource stores the matcher JSON RAW. Canonicalization
// (agora-design.md §11.4) is done by the agora-domain caller BEFORE the
// row reaches the store — agorastore is a leaf and does not import the
// agora package. The UNIQUE (source_id, matcher) index still catches
// duplicates because the caller hands canonical bytes.
type InboxStore interface {
	CreateInbox(ctx context.Context, ib Inbox, nowUnix int64) (Inbox, error)
	GetInbox(ctx context.Context, id string) (Inbox, error)
	GetInboxByScope(ctx context.Context, scope string) (Inbox, error)
	ListInboxes(ctx context.Context) ([]Inbox, error)
	ListInboxesByMember(ctx context.Context, memberID string) ([]Inbox, error)
	ListInboxesByCompany(ctx context.Context, companyID string) ([]Inbox, error)
	ArchiveInbox(ctx context.Context, id string, nowUnix int64) error

	// AddInboxSource INSERTs the inbox_source with src.Matcher stored
	// raw — the agora-domain caller canonicalizes the matcher before
	// calling this. Returns ErrStoreUniqueViolation on duplicate
	// (source_id, matcher); dup detection works because the caller
	// hands canonical bytes.
	AddInboxSource(ctx context.Context, src InboxSource, nowUnix int64) (InboxSource, error)

	// UpsertInboxSource INSERTs like AddInboxSource but, on a conflicting
	// (source_id, matcher), RE-POINTS the existing row to src.InboxID
	// (and src.OutgoingCredentialRef) instead of failing with
	// ErrStoreUniqueViolation. Used by the /wire redeem path so
	// re-redeeming a mint-token for an already-wired chat moves it to the
	// new inbox rather than erroring. Returns the resulting row (its id
	// is the pre-existing row's id on a re-point).
	UpsertInboxSource(ctx context.Context, src InboxSource, nowUnix int64) (InboxSource, error)

	RemoveInboxSource(ctx context.Context, id string) error
	ListInboxSources(ctx context.Context, inboxID string) ([]InboxSource, error)

	// UpdateInboxStatus flips bob_agora_inboxes.status (typically
	// 'active' | 'paused'). Used by admin /agora inbox pause|resume so
	// dispatcher skips items targeting paused inboxes without losing
	// them from the queue. Archived inboxes are off-limits in BOTH
	// directions (archived is terminal; revival = ReactivateInbox):
	// returns ErrAgoraInboxArchived when inboxID is archived,
	// ErrAgoraNotFound when it has no row.
	UpdateInboxStatus(ctx context.Context, inboxID, status string) error

	// SetBridgeDefaultTarget updates bob_agora_inboxes.bridge_default_target_inbox_id
	// for a bridge inbox. Used by bootstrap / admin to wire the
	// "inbound chat → first Member inbox" route AFTER both inboxes have
	// been created. R6-10 fix: prior to this method admins
	// had no way to set the column post-create because CreateInbox is
	// the only existing setter (and the target inbox doesn't exist
	// yet when the bridge is first created).
	//
	// targetInboxID="" clears the link. Returns ErrAgoraNotFound when
	// the bridge inbox id has no row.
	SetBridgeDefaultTarget(ctx context.Context, bridgeInboxID, targetInboxID string) error

	// ReactivateInbox flips an archived inbox back to active and rebinds
	// it to a new Employment. Used by Hire when a Member returns to a
	// Company they were previously terminated from — the inbox row is
	// reused (preserving session history under the same id) rather than
	// replaced. One atomic UPDATE: status='active', archived_at=NULL,
	// employment_id=employmentID. Returns ErrAgoraNotFound when inboxID
	// has no row.
	ReactivateInbox(ctx context.Context, inboxID, employmentID string, nowUnix int64) error

	// SetInboxVisibility updates bob_agora_inboxes.visibility_yaml for
	// the given inbox. Passing nil clears the filter (NULL column → no
	// filter; all tools/skills visible to this Inbox's sessions).
	// Caller is responsible for refreshing the in-memory agora.OrgCache
	// after a successful write — admin paths funnel through
	// agora.OrgCache.SetInboxVisibility, which does the write + cache
	// update atomically. Returns ErrAgoraNotFound when inboxID has no
	// row. PR-2.
	SetInboxVisibility(ctx context.Context, inboxID string, vis *InboxVisibility) error
}

// ---------- CompanyRoleSkillOverrideStore ----------

// CompanyRoleSkillOverrideStore is the persistence interface for
// bob_agora_company_role_skill_overrides — per-(Company, role) skill fork
// overlays (docs/agora-role-skill-fork.md). One row per (company_id,
// role_name, skill_name); manifest is a JSON SkillManifest delta. Set is an
// UPSERT on the composite PK. Get returns ("", false, nil) when absent (a
// missing fork means the baseline stands — symmetric with SkillOverrideReader,
// NOT the ErrAgoraNotFound convention, because "no fork" is the common path,
// not an error). Delete reports whether a row existed (idempotent). List is
// the webui discovery face (docs/agora-role-skill-fork.md §8).
type CompanyRoleSkillOverrideStore interface {
	SetCompanyRoleSkillOverride(ctx context.Context, companyID, roleName, skillName, manifest string, nowUnix int64) error
	GetCompanyRoleSkillOverride(ctx context.Context, companyID, roleName, skillName string) (string, bool, error)
	DeleteCompanyRoleSkillOverride(ctx context.Context, companyID, roleName, skillName string) (bool, error)
	ListCompanyRoleSkillOverrides(ctx context.Context, companyID string) ([]CompanyRoleSkillOverride, error)
}

// ---------- AgoraStore ----------

// AgoraStore is the aggregate persistence handle for the agora layer —
// every Member / Company / Employment / Channel / Inbox CRUD method, PLUS
// the M1 role-assignment store and the M2/M3b dispatch queue, in one
// interface.
//
// extraction: agora persistence moved OUT of the generic
// `store` package into its own postgres-only package
// (agorastore). The generic store (sqlite / postgres /
// fallback) no longer satisfies these methods — agora callers take a
// `core.AgoraStore` explicitly instead of type-asserting the
// `core.SessionStore`. The store layer is therefore a leaf again (no
// dependency on the agora subsystem).
//
// DispatchStore is folded in here because agora is
// postgres-only: a sqlite-only deployment has no dispatch table, so there is
// no reason to keep it reachable via a generic core.SessionStore
// type-assertion. One handle covers everything agora. (RoleStore was likewise
// folded in until, when the dead role-assignment store was removed.)
//
// Postgres-only: there is no sqlite agora backend. A sqlite-only
// deployment has no agora tables; agora is constructed with a nil
// AgoraStore and its features stay unwired.
type AgoraStore interface {
	MemberStore
	CompanyStore
	EmploymentStore
	ChannelStore
	InboxStore
	DispatchStore
	CompanyRoleSkillOverrideStore
}
