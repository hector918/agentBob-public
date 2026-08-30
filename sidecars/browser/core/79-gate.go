package core

import "context"

// Gate is the dispatch-time authorization check consulted by the
// session / tool / skill / credential layers. Living in core (not in
// permissions) is deliberate: removing the permissions plugin must
// not break session execution — registries, dispatch, and the
// credential broker only see this interface and a always-allow
// baseline. The permissions package supplies a richer impl via
// permissions.NewGate() when it's wired in.
//
// Two call shapes:
//
//   - Visible: cheap bool used at registry filter time (SpecsForSession,
//     SkillsForSession). The model never sees tools/skills the gate
//     hides — reduces token spend on certain-to-fail attempts.
//   - Check: full PermDecision (Kind + Reason) used at dispatch time so
//     the deny-envelope can show the Reason to the model.
//
// The no-plugin path is the nil-Gate convention: call sites treat a nil
// Gate as "always allow" (no enforcement), so bob runs end-to-end
// without permissions/ — the source-yaml admin / allow / denylist +
// agora audiences-gate still do their own filtering above. The
// permissions plugin's gate consults Judge and the ctx-stashed identity
// bundle when wired in.
type Gate interface {
	Visible(ctx context.Context, kind, name string) bool
	Check(ctx context.Context, kind, name string) PermDecision
}

// Identity stash on ctx — used by helpers (credential broker, future
// tool wrappers) that can't easily plumb the identity bundle through
// their existing signatures. Lives here (not in permissions/) so the
// vault + future helpers can call IdentitiesFromCtx without importing
// the permissions package — keeps the "remove permissions" path clean.
//
// The bundle is shallow-stored on the ctx: downstream goroutines that
// derive child ctx see the same slice header. Treat the returned slice
// as read-only; mutating elements in place is undefined.

// ctxKeyIdentities is the private ctx key — unexported struct{} so no
// other package can collide on it accidentally.
type ctxKeyIdentities struct{}

// WithIdentities returns a derived ctx carrying `ids` for downstream
// helpers (credential broker, future tool wrappers) to read via
// IdentitiesFromCtx. Pass nil / empty slice to clear (e.g. when
// transitioning to a "no caller" subordinate goroutine).
//
// Idempotent: calling twice replaces the prior stash; the older bundle
// is no longer visible on the new ctx.
func WithIdentities(ctx context.Context, ids []PermIdentity) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxKeyIdentities{}, ids)
}

// IdentitiesFromCtx returns the identity bundle stashed by
// WithIdentities, or nil when no bundle is attached. Callers should
// treat nil + empty slice the same way — neither says anything about
// WHO is calling.
//
// The returned slice is the same slice header stored on the ctx;
// callers MUST NOT mutate it (treat as read-only).
func IdentitiesFromCtx(ctx context.Context) []PermIdentity {
	if ctx == nil {
		return nil
	}
	v := ctx.Value(ctxKeyIdentities{})
	if v == nil {
		return nil
	}
	ids, ok := v.([]PermIdentity)
	if !ok {
		return nil
	}
	return ids
}

// systemIdentity is the canonical "non-agent caller with wildcard
// credential access" identity used by startup / sweep / housekeeping
// paths. Name starts with "_" so it sorts to the top of audit dumps
// and so admins greppping credential-broker.log can tell at a glance
// that the call came from a non-agent surface.
var systemIdentity = PermIdentity{
	"name":   "_system",
	"grants": []any{"credential:use:*"},
}

// WithSystemIdentity stashes the canonical "_system" wildcard identity
// onto ctx — explicit opt-in for sweep / startup / non-agent paths that
// legitimately need wildcard credential access. D3
// replaces the prior silent fallback inside the credentials broker's
// checkPermission: agent-flow paths that forget to stash an identity
// now see a loud deny, while non-agent flows make their wildcard
// privilege explicit at the call site.
//
// Idempotent: if ctx already carries any identity bundle, the existing
// stash is preserved (don't accidentally downgrade a real caller to
// _system). To force the system identity, pass a freshly derived ctx.
func WithSystemIdentity(ctx context.Context) context.Context {
	if len(IdentitiesFromCtx(ctx)) > 0 {
		return ctx
	}
	return WithIdentities(ctx, []PermIdentity{systemIdentity})
}
