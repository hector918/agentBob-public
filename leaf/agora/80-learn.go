package agora

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// 80-learn.go — the agora side of failure-driven ROLE-guidance learning
// (docs/wip-member-learn.md). This is an ISOLATED peripheral file: agora's core
// (operations / routing / slash / orgcache) is untouched. It mirrors the ordinary-skill
// learner (leaf/skills/20-learn.go) but keyed by (company,role):
//
//   - The agora flow reports a degraded member turn's SNAPSHOT via contract.MemberFailureSink
//     (passing scope + snapshot only). RecordFailure resolves scope → inbox → member →
//     active employments → the (company,role) pairs (an IN-MEMORY org-cache read, the same
//     one authorization already does) and rings the snapshot as a file under EACH role —
//     a member may hold several; the per-role distiller filters relevance.
//   - memberLearnSource enumerates (company,role) that have failures → one Learnable each;
//     leaf/learn's failure gate + reflection distill writes the role's guidance file.
//   - guidance() reads a role's learned text for TurnContext to inject into the prompt.
//
// Storage is files under $BOB_HOME/agora/ (no DB → no schema bump → no wipe-on-bump).

const (
	// memberFailMaxPerRole caps un-distilled snapshots per (company,role) so a role that's
	// never distilled (no learn model) can't grow unbounded on disk.
	memberFailMaxPerRole = 50
	// maxInjectedGuidance bounds the joined role-guidance text injected into a member's
	// prompt (a many-role member, or a hand-edited oversized file, can't bloat the prompt).
	maxInjectedGuidance = 3000
)

// memberLearn is the MemberFailureSink + the holder of the two file stores + the resolver.
type memberLearn struct {
	org    *OrgCache
	router *InboxRouter
	fail   *memberFailStore
	ins    *memberInsightStore
}

func newMemberLearn(home string, org *OrgCache, router *InboxRouter) *memberLearn {
	base := filepath.Join(home, "agora")
	return &memberLearn{
		org:    org,
		router: router,
		fail:   &memberFailStore{base: filepath.Join(base, ".failures")},
		ins:    &memberInsightStore{base: filepath.Join(base, "insights")},
	}
}

// RecordFailure (contract.MemberFailureSink): resolve the turn's (company,role) pairs and
// ring the snapshot under each. Best-effort, never blocks the flow.
func (m *memberLearn) RecordFailure(_ context.Context, scope, snapshot string) {
	if m == nil || strings.TrimSpace(snapshot) == "" {
		return
	}
	for _, cr := range m.resolveRoles(scope) {
		m.fail.put(cr[0], cr[1], snapshot)
	}
}

// memberIDForScope resolves a chat scope to the member behind its inbox ("" for a
// non-member / bridge / unknown scope).
func (m *memberLearn) memberIDForScope(scope string) string {
	if m == nil || m.router == nil {
		return ""
	}
	ib, ok := m.router.ResolveByScope(scope)
	if !ok {
		return ""
	}
	return ib.MemberID
}

// resolveRoles maps a chat scope to the distinct active (company,role) pairs of its
// member. resolveRoles(scope) = rolesForMember(memberIDForScope(scope)).
func (m *memberLearn) resolveRoles(scope string) [][2]string {
	return m.rolesForMember(m.memberIDForScope(scope))
}

// rolesForMember returns the distinct active (company,role) pairs a member holds — an
// in-memory org-cache read (same as authorization). The caller that already has the
// member id (TurnContext) uses this directly, avoiding a redundant scope re-resolve.
func (m *memberLearn) rolesForMember(memberID string) [][2]string {
	if m == nil || m.org == nil || memberID == "" {
		return nil
	}
	var out [][2]string
	seen := map[string]bool{}
	for _, e := range m.org.ActiveEmploymentsByMember(memberID) {
		if e.CompanyID == "" || e.RoleName == "" {
			continue
		}
		k := e.CompanyID + "\x00" + e.RoleName
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, [2]string{e.CompanyID, e.RoleName})
	}
	return out
}

// guidance returns the learned text for one (company,role).
func (m *memberLearn) guidance(co, role string) string {
	if m == nil || m.ins == nil {
		return ""
	}
	return m.ins.read(co, role)
}

// guidanceForMember concatenates the guidance of every (company,role) a member holds,
// bounded by maxInjectedGuidance — the prompt-injection helper for TurnContext. The cap
// guards against a many-role member (or a hand-edited oversized guidance file) inflating
// the system prompt without limit.
func (m *memberLearn) guidanceForMember(memberID string) string {
	var parts []string
	for _, cr := range m.rolesForMember(memberID) {
		if g := strings.TrimSpace(m.guidance(cr[0], cr[1])); g != "" {
			parts = append(parts, g)
		}
	}
	return capRunes(strings.Join(parts, "\n\n"), maxInjectedGuidance)
}

// --- LearnSource (registered into leaf/learn) --------------------------------

type memberLearnSource struct {
	fail *memberFailStore
	ins  *memberInsightStore
}

func (s memberLearnSource) Targets(context.Context) []contract.Learnable {
	pairs := s.fail.pairs()
	out := make([]contract.Learnable, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, memberTarget{co: p[0], role: p[1], fail: s.fail, ins: s.ins})
	}
	return out
}

// memberTarget is one (company,role)'s contract.Learnable.
type memberTarget struct {
	co, role string
	fail     *memberFailStore
	ins      *memberInsightStore
}

func (t memberTarget) Key() string                             { return "member:" + t.co + ":" + t.role }
func (t memberTarget) Current(context.Context) (string, error) { return t.ins.read(t.co, t.role), nil }
func (t memberTarget) Apply(_ context.Context, revised string) error {
	return t.ins.put(t.co, t.role, revised)
}
func (t memberTarget) Traces(_ context.Context, since time.Time) ([]contract.Rollout, error) {
	return t.fail.since(t.co, t.role, since), nil
}

// --- failure-snapshot store (files, keyed by company/role) -------------------

// failRecordExt is this store's record-file extension (tools' mirror uses ".json").
const failRecordExt = ".md"

type memberFailStore struct{ base string }

func (s *memberFailStore) dir(co, role string) string {
	return filepath.Join(s.base, safeSeg(co), safeSeg(role))
}

func (s *memberFailStore) put(co, role, snapshot string) {
	dir := s.dir(co, role)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	var rb [6]byte
	_, _ = rand.Read(rb[:])
	final := filepath.Join(dir, fmt.Sprintf("%d-%s.md", clock.Now().UnixNano(), hex.EncodeToString(rb[:])))
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(snapshot), 0o600); err != nil {
		return
	}
	if os.Rename(tmp, final) == nil {
		capDir(dir, memberFailMaxPerRole)
	}
}

// since returns this role's snapshots written after t (oldest-first), SWEEPING consumed
// ones (at <= t) and orphan .tmp files, and reaping the role/company dir once its last
// snapshot is gone. SOLE caller = the learn engine (read-with-delete).
func (s *memberFailStore) since(co, role string, t time.Time) []contract.Rollout {
	dir := s.dir(co, role)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	reapOrphanTmps(dir, entries)
	var out []contract.Rollout
	for _, f := range failFilesByAge(dir, entries, failRecordExt) {
		if !f.at.After(t) {
			_ = os.Remove(f.path)
			continue
		}
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		out = append(out, contract.Rollout{OK: false, Snapshot: string(data), At: f.at})
	}
	// Cleanup lives HERE (since is the SOLE snapshot consumer): a role dir only ever
	// empties when this sweep removes its last snapshot, so removing it now keeps
	// pairs() — which the webui learn panel polls every few seconds — a PURE read.
	// os.Remove on a non-empty dir is a harmless no-op, so it only fires once empty.
	if len(out) == 0 {
		if os.Remove(dir) == nil { // succeeds only when the role dir is now empty
			_ = os.Remove(filepath.Dir(dir)) // company dir, only once its last role is gone
		}
	}
	return out
}

// pairs lists the (company,role) that currently have PENDING failures — the dynamic
// target set. PURE read (the webui learn panel polls it every few seconds): a role
// whose snapshots were all swept leaves an empty dir, which pairs simply skips (no
// target); the empty dir itself is reaped by since() at the moment it consumes the
// last snapshot, NOT here — so this never mutates the filesystem.
func (s *memberFailStore) pairs() [][2]string {
	cos, err := os.ReadDir(s.base)
	if err != nil {
		return nil
	}
	var out [][2]string
	for _, co := range cos {
		if !co.IsDir() {
			continue
		}
		roleParent := filepath.Join(s.base, co.Name())
		roles, err := os.ReadDir(roleParent)
		if err != nil {
			continue
		}
		for _, role := range roles {
			if !role.IsDir() {
				continue
			}
			if dirHasSnapshot(filepath.Join(roleParent, role.Name())) {
				out = append(out, [2]string{co.Name(), role.Name()})
			}
			// empty (all-swept) dirs are reaped by since(), not here — pairs is pure.
		}
	}
	return out
}

// dirHasSnapshot reports whether dir holds at least one .md snapshot file.
func dirHasSnapshot(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), failRecordExt) {
			return true
		}
	}
	return false
}

// --- guidance store (files, keyed by company/role) ---------------------------

type memberInsightStore struct{ base string }

func (s *memberInsightStore) file(co, role string) string {
	return filepath.Join(s.base, safeSeg(co), safeSeg(role)+".md")
}

func (s *memberInsightStore) read(co, role string) string {
	data, err := os.ReadFile(s.file(co, role))
	if err != nil {
		return ""
	}
	return string(data)
}

// put writes the role's guidance (atomic tmp+rename). Empty → remove the file.
func (s *memberInsightStore) put(co, role, text string) error {
	final := s.file(co, role)
	if strings.TrimSpace(text) == "" {
		if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return err
	}
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// --- shared fail-store mechanism ---------------------------------------------
//
// KEPT IN SYNC — BYTE-IDENTICAL — with the other copies (leaf/tools/70-learn.go ↔
// leaf/skills/20-learn.go ↔ leaf/agora/80-learn.go).
// arch forbids a cross-leaf import (TestModuleImportBoundary) and contract/ is the
// interface seam (no filesystem mechanism), so this small ring-store mechanism is
// MIRRORED rather than shared. Diff all copies when touching any.

// tmpOrphanGrace is the minimum age a .tmp must reach before reapOrphanTmps treats
// it as a crash orphan to reap — long enough that a concurrent writer's
// WriteFile→Rename window can never be mistaken for one.
const tmpOrphanGrace = time.Minute

// reapOrphanTmps removes crash-orphaned .tmp files (a crash between WriteFile and
// Rename) from dir. Only a .tmp past tmpOrphanGrace is reaped: the record writer
// runs on a separate goroutine with no shared lock, and reaping a FRESH .tmp would
// delete its in-flight write — the rename would then silently lose the record.
func reapOrphanTmps(dir string, entries []os.DirEntry) {
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if info, ierr := e.Info(); ierr == nil && time.Since(info.ModTime()) < tmpOrphanGrace {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

type failFile struct {
	path string
	at   time.Time
}

// failFilesByAge lists the record files carrying extension ext in dir, oldest-first.
// At comes from the UnixNano timestamp encoded in the FILENAME (failFileTime) — the
// authoritative, strictly-increasing write time. Filesystem mtime is too coarse (1s
// on some FS) to keep the engine's max(At) in-flight guard sound.
func failFilesByAge(dir string, entries []os.DirEntry, ext string) []failFile {
	var out []failFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		out = append(out, failFile{path: filepath.Join(dir, e.Name()), at: failFileTime(e.Name(), ext)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// failFileTime parses the UnixNano prefix of a "<nano>-<hex>"+ext record filename.
// Unparseable (not one of ours) → zero time, so it sorts oldest and is swept first.
func failFileTime(name, ext string) time.Time {
	base := strings.TrimSuffix(name, ext)
	if i := strings.IndexByte(base, '-'); i > 0 {
		if nano, err := strconv.ParseInt(base[:i], 10, 64); err == nil {
			return time.Unix(0, nano)
		}
	}
	return time.Time{}
}

// --- end shared fail-store mechanism ------------------------------------------

// capDir drops the oldest .md snapshots beyond n (best-effort).
func capDir(dir string, n int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	files := failFilesByAge(dir, entries, failRecordExt)
	for i := 0; i+n < len(files); i++ {
		_ = os.Remove(files[i].path)
	}
}

// capRunes truncates s to at most n runes (on a rune boundary).
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n]))
}

// safeSeg keeps a path segment (company id / role name) filesystem-safe.
func safeSeg(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

var _ contract.MemberFailureSink = (*memberLearn)(nil)
