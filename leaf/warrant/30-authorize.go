package warrant

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"agentbob/contract"
)

// Manager implements contract.Warrant over a loaded policy + the tool catalog.
// home roots the space dirs ($BOB_HOME/spaces/<space>) the file channels open.
type Manager struct {
	// policy is the live matrix, held behind an atomic pointer so /permission
	// reload can swap a freshly-loaded matrix in while concurrent turns read it:
	// a reader sees either the entire old matrix or the entire new one, never a
	// half-built map. Load() may return nil before the first Store — allows() is
	// nil-safe (fail-closed), so that window denies rather than panics.
	policy atomic.Pointer[policy]
	// writeMu serializes policy WRITES (the load→reconcile→save→swap sequence): a
	// /permission reload, a /skills reload, and the webui panel save can otherwise
	// interleave their read-modify-write and lose one editor's change (the atomic pointer
	// only makes each swap tear-free, not the multi-step update atomic). Readers stay
	// lock-free on the atomic pointer; only writers contend.
	writeMu      sync.Mutex
	catalog      contract.ToolCatalog
	skillCat     contract.SkillCatalog // nil → no skills to project
	home         string
	pool         contract.ChannelPool // keeps stateful (exec) channels alive; nil → unpooled
	broker       contract.Broker      // remote-space secret backend; nil → remote unavailable
	spacesBroken bool                 // spaces.yaml present but unparseable → space routing fail-closed
	spaces       *spaceConfig         // space → backend; nil → all local

	// policyBroken is true when the permission source was present-but-corrupt at
	// boot: the matrix is deny-all (fail-closed) and an admin is summoned on the
	// first turn (adminLine resolved lazily — it may start after warrant).
	// brokenOnce fires that alert exactly once. A successful /permission reload
	// clears it (reload refuses to swap a broken file, so it only ever recovers).
	policyBroken atomic.Bool
	adminLine    func() contract.AdminLine
	brokenOnce   sync.Once
}

// alertIfBroken summons an admin (once) when the permission matrix loaded
// fail-closed, so a corrupt permissions source surfaces loudly instead of
// silently denying everything. Called from the per-turn projection entries, by
// when the (optional, late-starting) admin line has registered.
func (m *Manager) alertIfBroken(ctx context.Context) {
	if !m.policyBroken.Load() {
		return
	}
	m.brokenOnce.Do(func() {
		if m.adminLine == nil {
			return
		}
		if al := m.adminLine(); al != nil {
			al.Notify(ctx, contract.AdminError, "warrant",
				"permissions 无法解析,已 fail-closed 拒绝所有工具/技能;修好后重启 bob 恢复")
		}
	})
}

// Check judges one capability for a principal (fail-closed). This is the
// explicit single-capability gate (one decision per call), so it's the
// chokepoint where every decision is audited — both allow and deny.
//
// The Authorize / AuthorizeSkills projection loops are deliberately NOT
// logged here: they are bulk filters that run per-turn over the whole
// catalog, so logging each per-tool grant test would flood the file with
// N lines/turn without recording an explicit decision (the model simply
// never sees the ungranted tools). Check is the one boundary that sees a
// real gate decision.
func (m *Manager) Check(_ context.Context, identity, kind, name string) (bool, string) {
	cap := capString(kind, name)
	if m.policy.Load().allows(cap, identity) {
		logDecision(true, identity, cap, "")
		return true, ""
	}
	reason := fmt.Sprintf("%s not granted to %q", cap, identity)
	logDecision(false, identity, cap, reason)
	return false, reason
}

// Grants collapses warrant's OWN matrix into the projection for a principal — the
// outer-bob SUPPLY (the flow hands this to Authorize/ResolveCredential; agora supplies
// its own via MemberProjection). Empty/unknown principal → empty set (fail-closed).
// alertIfBroken fires here — this is where warrant's (possibly broken) matrix is read.
func (m *Manager) Grants(ctx context.Context, identity string) contract.GrantSet {
	m.alertIfBroken(ctx)
	out := contract.GrantSet{}
	pol := m.policy.Load()
	if pol == nil {
		return out
	}
	for cap, row := range pol.granted {
		if row[identity] {
			out[cap] = struct{}{}
		}
	}
	return out
}

// Authorize filters specs to the tools the GrantSet admits and returns them as a
// ToolSet (the turn's authorized bag). The grants come from the flow (warrant.Grants
// for outer-bob, agora.MemberProjection for a member) — warrant is the single JUDGE,
// it does not resolve who. Tools whose grant is absent are dropped; the model never
// sees them.
func (m *Manager) Authorize(ctx context.Context, grants contract.GrantSet, specs []contract.ToolSpec) contract.ToolSet {
	out := contract.NewToolSubset()
	for _, spec := range specs {
		if !grants.Granted(capString("tool", spec.Name)) {
			continue
		}
		if tool, ok := m.catalog.Lookup(spec.Name); ok {
			out.Add(spec, tool)
		}
	}
	id, _ := contract.IdentityFrom(ctx)
	logAuthorize("tool", id, specNames(out.Specs()))
	return out
}

// specNames pulls the tool names out of a spec slice for the authorize summary.
func specNames(specs []contract.ToolSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// AuthorizeSkills is the symmetric skill projection (skill:use:X) over the GrantSet.
func (m *Manager) AuthorizeSkills(ctx context.Context, grants contract.GrantSet, infos []contract.SkillInfo) contract.SkillSet {
	out := contract.NewSkillSubset(m.skillCat)
	granted := make([]string, 0, len(infos))
	for _, info := range infos {
		if !grants.Granted(capString("skill", info.Name)) {
			continue
		}
		out.Add(info)
		granted = append(granted, info.Name)
	}
	id, _ := contract.IdentityFrom(ctx)
	logAuthorize("skill", id, granted)
	return out
}

// ResolveCredential scans the vault for `kind`, keeps the names the GrantSet admits
// (credential:use:<name>), requires exactly one, and builds it via the broker (docs
// §14.2). The secret never leaves the broker. By-kind-unique: 0 → "none authorized";
// >1 → a config error (one member, one credential of a kind). The candidate scan
// stays silent (a bulk filter, like Authorize); only the winner is audited.
func (m *Manager) ResolveCredential(ctx context.Context, grants contract.GrantSet, kind string) (any, error) {
	if m.broker == nil {
		return nil, fmt.Errorf("warrant: no credential broker — %s credentials unavailable", kind)
	}
	names, err := m.broker.NamesByKind(ctx, kind)
	if err != nil {
		return nil, fmt.Errorf("warrant: list %s credentials: %w", kind, err)
	}
	var authorized []string
	for _, n := range names {
		if grants.Granted(capString("credential", n)) {
			authorized = append(authorized, n)
		}
	}
	switch len(authorized) {
	case 0:
		return nil, fmt.Errorf("没有授权给你的 %s 凭证（管理员需在凭证管理里配置并授权 credential:use:）", kind)
	case 1:
		// Audit the credential actually used (one allow line); principal from ctx so
		// the line is correct under either source (warrant matrix / agora projection).
		id, _ := contract.IdentityFrom(ctx)
		logDecision(true, id, capString("credential", authorized[0]), "")
		return m.broker.Build(ctx, authorized[0])
	default:
		// Don't leak the vault credential names to the tool/model (never the name) —
		// report the count only.
		return nil, fmt.Errorf("授权了多套 %s 凭证（%d 套）—— 一个成员只应有一套，请管理员拆成多个成员各授一套", kind, len(authorized))
	}
}

var _ contract.Warrant = (*Manager)(nil)
