package agora

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"

	"agentbob/leaf/agora/store"
)

// OrgCache is the single in-memory mirror of the agora pg org graph: per-Company
// role yaml (parsed permissions_yaml) plus the raw entity rows (Companies /
// Members / Employments / Inboxes) every hot-path consumer reads.
//
// Read paths hit RWMutex.RLock + map lookup, never pg. Writes land via:
//   - in-process write-through helpers (SetCompanyRolesFromYAML /
//     SetInboxVisibility): store-write FIRST, then cache update, under the
//     per-Company writeMu so concurrent writers stay ordered.
//   - admin /agora reload → Load (full atomic rebuild from the store).
//
// Invariant: an empty / unparseable permissions_yaml lands as a NON-nil
// empty-Roles entry (never nil) so a known Company can't fall through a lookup
// and leak cross-tenant visibility. Parse failure stores the fail-closed empty
// entry AND surfaces the error to the caller.
type OrgCache struct {
	mu           sync.RWMutex
	companyRoles map[string]*RolesConfig // companyID → parsed permissions_yaml (non-nil for every known Company)
	companies    map[string]store.Company
	members      map[string]store.Member
	employments  map[string]store.Employment // all rows (active + terminated) — callers filter
	inboxes      map[string]store.Inbox      // all rows (incl. archived) — callers filter

	loadMu  sync.Mutex
	writeMu sync.Map // companyID → *sync.Mutex (per-Company write-through serialisation)
}

// OrgSnapshot is the org-structure point-in-time view (BuildDirectory, webui).
type OrgSnapshot struct {
	Companies   []store.Company
	Members     []store.Member
	Employments []store.Employment // active only
	Inboxes     []InboxSnap        // active + paused (no archived)
}

// InboxSnap embeds the store.Inbox + operational counters that only make sense
// in a snapshot view.
type InboxSnap struct {
	store.Inbox
	ActiveSessions int // 0 when Snapshot was passed a nil sessionCounter
}

// NewOrgCache returns an empty cache. Load populates it from the agora store.
func NewOrgCache() *OrgCache {
	return &OrgCache{
		companyRoles: map[string]*RolesConfig{},
		companies:    map[string]store.Company{},
		members:      map[string]store.Member{},
		employments:  map[string]store.Employment{},
		inboxes:      map[string]store.Inbox{},
	}
}

// emptyRoles returns a fresh non-nil empty RolesConfig — the fail-closed shape
// stored for Companies with empty / unparseable permissions_yaml.
func emptyRoles() *RolesConfig { return &RolesConfig{Roles: map[string]RoleDef{}} }

// RolesFor returns the parsed RolesConfig for companyID, or nil when the Company
// isn't cached. Known Companies always return non-nil.
func (c *OrgCache) RolesFor(companyID string) *RolesConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.companyRoles[companyID]
}

// CompanyByID returns the Company row + ok=true when companyID is known.
func (c *OrgCache) CompanyByID(companyID string) (store.Company, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	co, ok := c.companies[companyID]
	return co, ok
}

// CompanyIDByIDOrName resolves a company reference (id OR name) to its canonical id,
// or "" when unknown. A cheap RLock + map read: an O(1) id hit, falling back to a scan
// for a name match (company counts are tiny). Lets hot-path callers resolve a company
// ref without a full Snapshot deep-copy. (L-AG-S1)
func (c *OrgCache) CompanyIDByIDOrName(ref string) string {
	if ref == "" {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, ok := c.companies[ref]; ok {
		return ref // ref is already a canonical id (map is id-keyed)
	}
	for id, co := range c.companies {
		if co.Name == ref {
			return id
		}
	}
	return ""
}

// MemberByID returns the Member row + ok=true when memberID is known.
func (c *OrgCache) MemberByID(memberID string) (store.Member, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.members[memberID]
	return m, ok
}

// InboxByID returns the Inbox row + ok=true when known (any status).
func (c *OrgCache) InboxByID(inboxID string) (store.Inbox, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ib, ok := c.inboxes[inboxID]
	return ib, ok
}

// ActiveEmploymentsByMember returns the member's active employments — the set a
// member-owned inbox intersects grants across.
func (c *OrgCache) ActiveEmploymentsByMember(memberID string) []store.Employment {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []store.Employment
	for _, e := range c.employments {
		if e.MemberID == memberID && e.Status == "active" {
			out = append(out, e)
		}
	}
	return out
}

// PermissionsWriter is the narrow store surface SetCompanyRolesFromYAML needs.
type PermissionsWriter interface {
	SetPermissionsYAML(ctx context.Context, companyID, yaml string, nowUnix int64) error
}

// VisibilityWriter is the narrow store surface SetInboxVisibility needs.
type VisibilityWriter interface {
	SetInboxVisibility(ctx context.Context, inboxID string, vis *store.InboxVisibility) error
}

// SetCompanyRolesFromYAML is the write-through entrypoint for in-process
// permissions_yaml edits: store-write FIRST, then cache update, serialised per
// Company. The cached Company row's PermissionsYAML is also refreshed so a later
// Snapshot reflects the new body without a full Load. Empty body → fail-closed
// empty entry (invariant); parse failure stores the empty entry AND returns the
// parse error (the store write still happened).
func (c *OrgCache) SetCompanyRolesFromYAML(ctx context.Context, w PermissionsWriter, companyID string, body []byte, nowUnix int64) error {
	if w == nil {
		return fmt.Errorf("agora orgcache: SetCompanyRolesFromYAML: nil store")
	}
	mu := c.writeMuFor(companyID)
	mu.Lock()
	defer mu.Unlock()
	if err := w.SetPermissionsYAML(ctx, companyID, string(body), nowUnix); err != nil {
		return err
	}
	cfg, perr := parseRolesYAML(body)
	if perr != nil {
		cfg = emptyRoles()
	}
	c.mu.Lock()
	c.companyRoles[companyID] = cfg
	if co, ok := c.companies[companyID]; ok {
		co.PermissionsYAML = string(body)
		c.companies[companyID] = co
	}
	c.mu.Unlock()
	return perr
}

// SeedCompanyRolesFromYAML registers a Company's parsed permissions_yaml WITHOUT
// a store write — for callers that already persisted via CreateCompany /
// SetPermissionsYAML and just need the in-memory entry populated. Empty body /
// parse failure → fail-closed empty entry.
func (c *OrgCache) SeedCompanyRolesFromYAML(companyID string, body []byte) error {
	cfg, perr := parseRolesYAML(body)
	if perr != nil {
		cfg = emptyRoles()
	}
	c.mu.Lock()
	c.companyRoles[companyID] = cfg
	c.mu.Unlock()
	return perr
}

// SetInboxVisibility is the write-through entrypoint for in-process per-Inbox
// visibility edits: store-write FIRST, then cache update. nil vis clears the
// filter. The store handle is passed in (not held) so the cache stays storeless
// and unit-testable. Returns the store error untouched (incl. not-found).
func (c *OrgCache) SetInboxVisibility(ctx context.Context, w VisibilityWriter, inboxID string, vis *store.InboxVisibility) error {
	if w == nil {
		return fmt.Errorf("agora orgcache: SetInboxVisibility: nil store")
	}
	if inboxID == "" {
		return fmt.Errorf("agora orgcache: SetInboxVisibility: empty inboxID")
	}
	if err := w.SetInboxVisibility(ctx, inboxID, vis); err != nil {
		return err
	}
	c.mu.Lock()
	if cur, ok := c.inboxes[inboxID]; ok {
		cur.Visibility = vis
		c.inboxes[inboxID] = cur
	}
	c.mu.Unlock()
	return nil
}

// UpsertCompany / UpsertMember / UpsertEmployment / UpsertInbox are the
// cache-only counterparts to the store Create*/Hire calls: the caller
// passes the freshly-written row in so the mirror reflects the store immediately
// instead of going stale until the next Load.
func (c *OrgCache) UpsertCompany(co store.Company) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.companies[co.ID] = co
}

func (c *OrgCache) UpsertMember(m store.Member) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.members[m.ID] = m
}

func (c *OrgCache) UpsertEmployment(e store.Employment) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.employments[e.ID] = e
}

func (c *OrgCache) UpsertInbox(ib store.Inbox) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inboxes[ib.ID] = ib
}

// OrgLoader is the narrow set of list-only store methods Load needs — kept
// narrow so the cache can be unit-tested against a stub.
type OrgLoader interface {
	ListCompanies(ctx context.Context) ([]store.Company, error)
	ListMembers(ctx context.Context) ([]store.Member, error)
	ListEmployments(ctx context.Context, activeOnly bool) ([]store.Employment, error)
	ListInboxes(ctx context.Context) ([]store.Inbox, error)
}

// Load enumerates every entity from the store, parses each Company's
// permissions_yaml, and atomically swaps the cache's maps under one mu.Lock() —
// readers see either the entire old set or the entire new set, never a partial
// state. Bad yaml on a per-Company parse is logged + stored as the fail-closed
// empty entry; the whole Load still succeeds. loadMu serialises concurrent Loads.
func (c *OrgCache) Load(ctx context.Context, ldr OrgLoader) error {
	if ldr == nil {
		return nil
	}
	c.loadMu.Lock()
	defer c.loadMu.Unlock()
	cos, err := ldr.ListCompanies(ctx)
	if err != nil {
		return fmt.Errorf("agora orgcache: list companies: %w", err)
	}
	ms, err := ldr.ListMembers(ctx)
	if err != nil {
		return fmt.Errorf("agora orgcache: list members: %w", err)
	}
	emps, err := ldr.ListEmployments(ctx, false)
	if err != nil {
		return fmt.Errorf("agora orgcache: list employments: %w", err)
	}
	ibs, err := ldr.ListInboxes(ctx)
	if err != nil {
		return fmt.Errorf("agora orgcache: list inboxes: %w", err)
	}

	nextCompanies := make(map[string]store.Company, len(cos))
	nextRoles := make(map[string]*RolesConfig, len(cos))
	for _, co := range cos {
		nextCompanies[co.ID] = co
		if co.PermissionsYAML == "" {
			nextRoles[co.ID] = emptyRoles()
		} else if cfg, perr := parseRolesYAML([]byte(co.PermissionsYAML)); perr != nil {
			slog.Warn("agora orgcache: bad permissions_yaml — entry stored as fail-closed empty-Roles map",
				"company_id", co.ID, "company_name", co.Name, "err", perr)
			nextRoles[co.ID] = emptyRoles()
		} else {
			nextRoles[co.ID] = cfg
		}
	}
	nextMembers := make(map[string]store.Member, len(ms))
	for _, m := range ms {
		nextMembers[m.ID] = m
	}
	nextEmployments := make(map[string]store.Employment, len(emps))
	for _, e := range emps {
		nextEmployments[e.ID] = e
	}
	nextInboxes := make(map[string]store.Inbox, len(ibs))
	for _, ib := range ibs {
		nextInboxes[ib.ID] = ib
	}

	c.mu.Lock()
	c.companies = nextCompanies
	c.companyRoles = nextRoles
	c.members = nextMembers
	c.employments = nextEmployments
	c.inboxes = nextInboxes
	c.mu.Unlock()
	return nil
}

// Snapshot returns a deep-copied org-structure view. Filters: Employments =
// active only; Inboxes = active + paused (archived dropped). sessionCounter is
// optional (nil → ActiveSessions stays 0).
func (c *OrgCache) Snapshot(ctx context.Context, sessionCounter func(ctx context.Context, inboxID string) int) OrgSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := OrgSnapshot{
		Companies:   make([]store.Company, 0, len(c.companies)),
		Members:     make([]store.Member, 0, len(c.members)),
		Employments: make([]store.Employment, 0, len(c.employments)),
		Inboxes:     make([]InboxSnap, 0, len(c.inboxes)),
	}
	for _, co := range c.companies {
		out.Companies = append(out.Companies, copyCompany(co))
	}
	for _, m := range c.members {
		out.Members = append(out.Members, copyMember(m))
	}
	for _, e := range c.employments {
		if e.Status != "active" {
			continue
		}
		out.Employments = append(out.Employments, e)
	}
	for _, ib := range c.inboxes {
		if ib.Status == "archived" {
			continue
		}
		as := 0
		if sessionCounter != nil {
			as = sessionCounter(ctx, ib.ID)
		}
		out.Inboxes = append(out.Inboxes, InboxSnap{Inbox: copyInbox(ib), ActiveSessions: as})
	}
	// Sort by ID so the snapshot order is STABLE across calls — the maps above
	// iterate in random order, which would make the webui graph's nodes jump
	// between polls (positions are assigned by slice index).
	sort.Slice(out.Companies, func(i, j int) bool { return out.Companies[i].ID < out.Companies[j].ID })
	sort.Slice(out.Members, func(i, j int) bool { return out.Members[i].ID < out.Members[j].ID })
	sort.Slice(out.Employments, func(i, j int) bool { return out.Employments[i].ID < out.Employments[j].ID })
	sort.Slice(out.Inboxes, func(i, j int) bool { return out.Inboxes[i].ID < out.Inboxes[j].ID })
	return out
}

// copyCompany / copyInbox deep-copy the heap-shared parts (slices / the
// visibility pointer) so a Snapshot caller can mutate the returned value without
// poisoning the cache. (copyMember is a plain value copy — Member is all scalar.)
func copyCompany(in store.Company) store.Company {
	out := in
	if in.Config != nil {
		out.Config = make(map[string]any, len(in.Config))
		for k, v := range in.Config {
			out.Config[k] = v
		}
	}
	return out
}

// copyMember returns a value copy of a Member. Member has no reference-typed
// fields, so a plain copy is a full deep copy.
func copyMember(in store.Member) store.Member { return in }

func copyInbox(in store.Inbox) store.Inbox {
	out := in
	if in.Visibility != nil {
		v := *in.Visibility
		if in.Visibility.HiddenTools != nil {
			v.HiddenTools = append([]string(nil), in.Visibility.HiddenTools...)
		}
		if in.Visibility.HiddenSkills != nil {
			v.HiddenSkills = append([]string(nil), in.Visibility.HiddenSkills...)
		}
		out.Visibility = &v
	}
	return out
}

// setRoles is a test-friendly helper for code holding a parsed config (no yaml
// round-trip). Production paths go through Set*/Seed*FromYAML.
func (c *OrgCache) setRoles(companyID string, cfg *RolesConfig) {
	if cfg == nil {
		cfg = emptyRoles()
	}
	c.mu.Lock()
	c.companyRoles[companyID] = cfg
	c.mu.Unlock()
}

// writeMuFor returns the per-Company write-through mutex, lazily allocating it
// on first use (company-count is small + each mutex is tiny).
func (c *OrgCache) writeMuFor(companyID string) *sync.Mutex {
	v, _ := c.writeMu.LoadOrStore(companyID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// DropCompanyWriteMu releases the per-Company write-through mutex entry after the
// company is hard-deleted (writeMuFor allocates lazily but never reclaims, so a
// long-lived process would accumulate one entry per deleted company). Delete only —
// NEVER reset the mutex inside writeMuFor/Load, which could hand two concurrent
// writers different mutexes. The DeleteCompany cascade (leaf/agora/60-operations.go)
// should call this after the row is gone.
func (c *OrgCache) DropCompanyWriteMu(companyID string) {
	c.writeMu.Delete(companyID)
}

// parseRolesYAML parses a permissions_yaml body into a normalized RolesConfig
// (non-nil .Roles map). Empty body short-circuits to an empty config.
func parseRolesYAML(body []byte) (*RolesConfig, error) {
	if len(body) == 0 {
		return emptyRoles(), nil
	}
	cfg := &RolesConfig{}
	if err := yaml.Unmarshal(body, cfg); err != nil {
		return nil, err
	}
	if cfg.Roles == nil {
		cfg.Roles = map[string]RoleDef{}
	}
	return cfg, nil
}
