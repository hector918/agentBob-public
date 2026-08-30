package contract

import "context"

// GrantSet is one RESOLVED authorization projection: the set of capability strings
// ("tool:use:X" / "skill:use:X" / "credential:use:X") granted for one request. It is
// the SINGLE currency between the SUPPLIERS that collapse their sources into one
// projection (warrant.Grants from its matrix; agora.MemberProjection /
// agora.RoleProjection from the company permissions_yaml) and the JUDGE that filters
// against it (warrant.Authorize / AuthorizeSkills / ResolveCredential; the arrangement
// dispatcher). Membership is the ONE judgment, defined once here. See docs §14.
type GrantSet map[string]struct{}

// Granted reports whether capability is in the projection (exact match; fail-closed —
// a nil set grants nothing).
func (g GrantSet) Granted(capability string) bool {
	_, ok := g[capability]
	return ok
}

// Warrant is the capability JUDGE — one unified set of operations over a supplied
// GrantSet, regardless of flow. It does NOT resolve who/which-flow; the flow hands it
// the projection (its own, via Grants, or agora's). It filters a catalog/vault against
// that projection (Authorize / AuthorizeSkills / ResolveCredential) and vends gated
// channels. See docs §14.
//
// Provided by leaf/warrant; consumed by the flows.
type Warrant interface {
	// Grants collapses warrant's OWN matrix into the projection for a principal — the
	// outer-bob supply (agora supplies its own via MemberProjection/RoleProjection).
	// Empty/unknown principal → empty set (fail-closed).
	Grants(ctx context.Context, identity string) GrantSet
	// Check reports whether identity may use kind:name against warrant's matrix
	// (Grants(identity).Granted) — the level-2 per-resource gate (e.g. an ssh space's
	// credential). For outer-bob/space credentials only.
	Check(ctx context.Context, identity, kind, name string) (allow bool, reason string)
	// Authorize filters specs to the ones the GrantSet admits and returns them as a
	// ToolSet (specs + lookup) — the turn's authorized tool bag.
	Authorize(ctx context.Context, grants GrantSet, specs []ToolSpec) ToolSet
	// AuthorizeSkills is the symmetric skill projection (skill:use:X) over the GrantSet.
	AuthorizeSkills(ctx context.Context, grants GrantSet, infos []SkillInfo) SkillSet
	// ResolveCredential scans the vault for `kind`, keeps names the GrantSet admits
	// (credential:use:<name>), requires exactly one, and builds it via the broker (the
	// secret never leaves the broker). By-kind-unique: 0 → none / >1 → config error.
	ResolveCredential(ctx context.Context, grants GrantSet, kind string) (any, error)
	// File vends a file channel to a space for identity (gated + backend-resolved:
	// a local space needs no credential; a remote space's credential gate lands
	// in slice 3). It is the level-2 (per-resource) gate seen at run time.
	File(ctx context.Context, identity, space string) (FileChannel, error)
	// Exec vends a continuous shell session to a space (pooled — reused across
	// calls). Same gating as File.
	Exec(ctx context.Context, identity, space string) (ExecChannel, error)
	// Cut revokes a principal's live channels (a permission change) — it signals
	// the pool to close them. The actual close is the pool's (it holds them).
	Cut(identity string)
}

type identityKeyT struct{}

// WithIdentity stamps the principal string on ctx so deep callers (the broker,
// once it lands) can gate without it being threaded through every signature.
func WithIdentity(ctx context.Context, identity string) context.Context {
	if identity == "" {
		return ctx
	}
	return context.WithValue(ctx, identityKeyT{}, identity)
}

// IdentityFrom returns the principal on ctx; ok=false when none is set.
func IdentityFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(identityKeyT{}).(string)
	return id, ok
}

type memberKeyT struct{}

// WithMember stamps the per-MEMBER identity on ctx — finer than the principal (which is
// per-role): same role, different members may hold different browser logins. The flow
// stamps it (agora member id / normal bound account) so the browser tool keys its login
// MASTER per member. Separate from WithIdentity (the principal still gates tools/channels);
// "" → unchanged (the browser tool falls back to the principal).
func WithMember(ctx context.Context, member string) context.Context {
	if member == "" {
		return ctx
	}
	return context.WithValue(ctx, memberKeyT{}, member)
}

// MemberFrom returns the per-member identity on ctx; ok=false when none is set.
func MemberFrom(ctx context.Context) (string, bool) {
	m, ok := ctx.Value(memberKeyT{}).(string)
	return m, ok
}
