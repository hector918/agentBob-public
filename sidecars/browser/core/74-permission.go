package core

// Permissions: identity-based capability gate for tools / skills / credentials.
//
// Design: docs/permissions-design.md.
//
// What lives here:
//   - PermIdentity / PermRequest / PermDecision / PermDecisionKind —
//     the API surface consumed by Judge (permissions/40-gate-impl.go) +
//     the gateway identity-resolve path (pipeline-gateway/22-identity.go).
//
// What used to live here (deleted, R8-6):
//   - PermIdentityRow / PermHandleBindingRow / PermApprovalRow
//   - PermApprovalPending / PermApprovalAllowed / PermApprovalDenied /
//     PermApprovalOrphaned constants
//   - PermissionStore interface (identities / bindings / approvals CRUD)
//
// The design (docs/permissions-design.md) settled on identities supplied
// per-call via core.WithIdentities(ctx, ids) rather than persisted in a
// `permissions_identities` table, and on synchronous Allow/Deny rather
// than async approvals. The store-side surface was scaffolding from an
// earlier iteration and never had a caller; the schema tables it backed
// (permissions_identities / permissions_handle_bindings /
// permissions_approvals) are dropped by sqlite v18 / pg v13.

// PermIdentity is the open-schema "who is this caller" description handed
// to Judge. Convention (see docs/permissions-design.md §Reserved keywords):
//
//   - "name"            required, unique primary key
//   - "grants"          []string — allowed action patterns (pure allow-list:
//     a missing grant = no permission. There is NO deny that wins over an
//     allow — the old "blacklist" deny-overrides-allow key was removed
// as a dead, no-producer feature.)
//   - "grants:<scope>"  reserved syntax for v1+1 ABAC matching (unused)
//   - "_*"              system-managed metadata (caller doesn't set)
//   - any other key     free descriptive attribute (company / position /
//     role / team / email / ...), judge ignores them
//
// Value type is `any` (not `string`) so descriptive attrs can later
// carry typed values without breaking the schema.
//
// `Perm` prefix is for namespacing — the permissions types follow a
// consistent `Perm`-prefixed convention to avoid colliding with other
// core types.
//
// CONTRACT: treat as read-only. IdentityResolver builds a fresh map per
// resolve call; consumers (rich engine, gate decisions) must not mutate.
type PermIdentity map[string]any

// PermRequest is one item in the bundle passed to Judge. Bundles let a
// tool declare "I'll call browser_navigate AND web_search AND
// skill:research" in one shot — Judge returns a single PermDecision over
// the whole bundle (any item Denied → bundle Denied).
//
// Action string for matching is built as `Kind:Name` (plus `:Verb` when
// non-empty). Args is reserved for future attribute-aware rules; v1
// rich impl doesn't read it.
type PermRequest struct {
	Kind string         // "tool" / "skill" / "approval" / ...
	Name string         // "web_search" / "research" / "*"
	Verb string         // optional 3rd segment (e.g. "run", "set")
	Args map[string]any // tool-specific args; rich v1 ignores
}

// PermDecisionKind is the verdict from Judge. Only two outcomes:
// Allow or Deny — every grant evaluation is static and immediate.
type PermDecisionKind int

const (
	// PermAllow — caller may proceed.
	PermAllow PermDecisionKind = iota

	// PermDeny — caller must not proceed. Reason is user-facing.
	PermDeny
)

// PermDecision is the return value of Judge. Allow or Deny only; the
// earlier async-approval shape (Pending + ApprovalID) was removed
// as never-emitted dead surface. Re-introducing async
// approval would require a new Kind + a new field; see
// `docs/permissions-design.md` §"未来扩展".
type PermDecision struct {
	Kind   PermDecisionKind
	Reason string // user-facing message for Deny
}
