package skills

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// This is the skill side of the learn seam, symmetric with leaf/tools/70-learn.go:
// one contract.Learnable per skill (its learned usage notes), registered as a
// contract.LearnSource into leaf/learn. The notes are stored on the catalog (read
// by Read, which appends them to the body) and persisted as INSIGHT.md files
// (docs/learn.md §6) under $BOB_HOME/skills/<origin>/<name>/INSIGHT.md so they
// survive a restart — internal (embedded) skills get a writable mirror dir there
// since the binary embed has no on-disk home of its own.
//
// The failure-trace source is skillFailureStore below (docs/wip-skill-learn-reflect.md):
// the agora flow, after a member's turn ends Partial, reports a medium trajectory
// SNAPSHOT per engaged skill via contract.SkillFailureSink; the store rings those
// snapshots as files, and skillTarget.Traces hands them (as Rollouts with .Snapshot set)
// to leaf/learn's failure gate. The distiller then reflects on the real trajectory.
// Failure-only; ordinary (rubric-less) skills only — rubric stays its own path.

// skillTarget is one skill's contract.Learnable.
type skillTarget struct {
	cat    *FileSkills
	name   string
	origin contract.SkillOrigin // selects the INSIGHT.md subdir (internal/external)
	store  *skillInsightStore   // learned-notes persistence; nil → in-memory only
	fail   *skillFailureStore   // failure-snapshot trace source; nil → no traces
}

func (t skillTarget) Key() string { return "skill:" + t.name }
func (t skillTarget) Current(context.Context) (string, error) {
	return t.cat.addendum(t.origin, t.name), nil
}

// Traces hands the engine this skill's recorded failure snapshots since `since`.
func (t skillTarget) Traces(_ context.Context, since time.Time) ([]contract.Rollout, error) {
	if t.fail == nil {
		return nil, nil
	}
	return t.fail.since(t.name, since), nil
}

func (t skillTarget) Apply(_ context.Context, revised string) error {
	t.cat.setAddendum(t.origin, t.name, revised)
	if t.store != nil {
		return t.store.put(t.origin, t.name, revised)
	}
	return nil
}

// skillLearnSource is the module's single contract.LearnSource: one target per
// skill, enumerated fresh each sweep.
type skillLearnSource struct {
	cat   *FileSkills
	store *skillInsightStore
	fail  *skillFailureStore
}

func (s skillLearnSource) Targets(context.Context) []contract.Learnable {
	infos := s.cat.List()
	out := make([]contract.Learnable, 0, len(infos))
	for _, info := range infos {
		out = append(out, skillTarget{cat: s.cat, name: info.Name, origin: info.Origin, store: s.store, fail: s.fail})
	}
	return out
}

// --- INSIGHT.md persistence (docs/learn.md §6) -------------------------------

// skillInsightStore persists each skill's learned notes as
// base/<origin>/<name>/INSIGHT.md. base is $BOB_HOME/skills, so an EXTERNAL skill's
// insight lands right in its own dir (alongside SKILL.md), and an INTERNAL skill —
// which has no on-disk dir of its own — gets a writable mirror under skills/internal/.
type skillInsightStore struct{ base string }

func (s *skillInsightStore) file(origin contract.SkillOrigin, name string) string {
	return filepath.Join(s.base, string(origin), name, "INSIGHT.md")
}

// put writes the skill's insight (atomic tmp+rename). Empty text removes the file —
// an integral rewrite that produced nothing leaves no empty INSIGHT.md behind.
func (s *skillInsightStore) put(origin contract.SkillOrigin, name, text string) error {
	final := s.file(origin, name)
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

// all loads every persisted INSIGHT.md at startup, keyed by (origin, skill name ==
// dir name). The origin is kept (not collapsed to bare name) so an internal skill's
// note and a same-named external skill's note stay distinct — the catalog applies
// each only to its own-origin skill (B: addendum-by-name latch).
//
// Per-file fail-open: an unreadable INSIGHT.md (or origin dir) is warned and skipped,
// returning the partial map — one bad file must not blank EVERY skill's Current(),
// which a later distill would then use as an empty base and overwrite the intact
// on-disk notes from.
func (s *skillInsightStore) all() map[addendaKey]string {
	out := map[addendaKey]string{}
	for _, origin := range []contract.SkillOrigin{contract.OriginInternal, contract.OriginExternal} {
		dir := filepath.Join(s.base, string(origin))
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) { // missing dir = no insights of this origin yet
				slog.Warn("skills: load learned insights: origin dir unreadable — skipping", "dir", dir, "err", err)
			}
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name(), "INSIGHT.md"))
			if err != nil {
				if !os.IsNotExist(err) { // no INSIGHT.md in this skill dir — fine
					slog.Warn("skills: load learned insight failed — skipping", "origin", origin, "skill", e.Name(), "err", err)
				}
				continue
			}
			out[addendaKey{origin, e.Name()}] = string(data)
		}
	}
	return out
}

// delete removes one skill's INSIGHT.md for a SPECIFIC origin — best-effort orphan
// cleanup when a reload drops a skill (or an external skill supersedes a same-named
// internal one) (B-26). The catalog's pruner now passes the origin, so this no longer
// has to clear both subdirs blindly. An external skill's file is usually already gone
// with its dir; this also clears an internal mirror left behind when the binary stops
// shipping a built-in.
func (s *skillInsightStore) delete(origin contract.SkillOrigin, name string) error {
	if err := os.Remove(s.file(origin, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	// The per-skill dir existed only to hold the note — prune it when now empty
	// (os.Remove on a non-empty dir fails, which is the desired no-op).
	_ = os.Remove(filepath.Dir(s.file(origin, name)))
	return nil
}

// --- failure-snapshot trace source (contract.SkillFailureSink) ---------------

// skillFailMaxPerSkill caps how many un-distilled failure snapshots a skill keeps;
// past it the oldest are dropped so a skill that's never distilled (no learn model)
// can't grow unbounded on disk.
const skillFailMaxPerSkill = 50

// failRecordExt is this store's record-file extension (tools' mirror uses ".json").
const failRecordExt = ".md"

// skillFailureStore rings each skill's failed-turn SNAPSHOTS as files under
// base/<name>/<id>.md (base = $BOB_HOME/skills/.failures). The agora flow writes via
// RecordFailure; skillTarget.Traces reads them; a consumed snapshot (mtime <= the
// engine's watermark) is swept on the next read. Implements contract.SkillFailureSink.
type skillFailureStore struct{ base string }

func (s *skillFailureStore) dir(skill string) string {
	return filepath.Join(s.base, safeSkillName(skill))
}

// removeSkill drops a skill's whole snapshot dir — the module mirrors catalog
// removals here (Reload's removed-pruner) so uninstalled/renamed skills don't
// leave transcripts behind. Best-effort.
func (s *skillFailureStore) removeSkill(skill string) {
	_ = os.RemoveAll(s.dir(skill))
}

// sweepOrphans removes snapshot dirs whose skill is not in live (a set of
// safeSkillName'd catalog names). Covers skills removed while the process was
// down, which the Reload-time removed-pruner can't see. Returns dirs removed.
func (s *skillFailureStore) sweepOrphans(live map[string]bool) int {
	entries, err := os.ReadDir(s.base)
	if err != nil {
		return 0 // no failures dir yet — nothing to sweep
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || live[e.Name()] {
			continue
		}
		if os.RemoveAll(filepath.Join(s.base, e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// safeSkillName keeps a skill name filesystem-safe. Skill names are dir names already
// ([A-Za-z0-9._-]); sanitize defensively so a stray char can't escape base.
func safeSkillName(name string) string {
	var b strings.Builder
	for _, r := range name {
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

// RecordFailure writes one failure snapshot for skill (contract.SkillFailureSink).
// Best-effort: a write error is logged, not propagated (learning is non-critical).
func (s *skillFailureStore) RecordFailure(skill, snapshot string) {
	skill = strings.TrimSpace(skill)
	if skill == "" || strings.TrimSpace(snapshot) == "" {
		return
	}
	dir := s.dir(skill)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("skills: record failure snapshot failed", "skill", skill, "err", err)
		return
	}
	var rb [6]byte
	_, _ = rand.Read(rb[:])
	final := filepath.Join(dir, fmt.Sprintf("%d-%s.md", clock.Now().UnixNano(), hex.EncodeToString(rb[:])))
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(snapshot), 0o600); err != nil {
		slog.Warn("skills: record failure snapshot failed", "skill", skill, "err", err)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		slog.Warn("skills: record failure snapshot failed", "skill", skill, "err", err)
		return
	}
	s.cap(dir)
}

// cap drops the oldest snapshots beyond skillFailMaxPerSkill (best-effort).
func (s *skillFailureStore) cap(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	files := failFilesByAge(dir, entries, failRecordExt)
	for i := 0; i+skillFailMaxPerSkill < len(files); i++ {
		_ = os.Remove(files[i].path)
	}
}

// since returns this skill's snapshots written strictly after t (oldest-first), and
// SWEEPS any already-consumed (at <= t) — the watermark advances past consumed rows, so
// this is where they get deleted (the "temp file, consumed → gone" contract). Also reaps
// orphan .tmp files (a crash between WriteFile and Rename).
//
// CALLER CONTRACT: the learn engine is the SOLE caller (10-engine.go), once per cycle
// with a monotonically-advancing watermark. since() is a READ-WITH-DELETE — a second
// consumer calling it would destroy not-yet-distilled snapshots. Do NOT add one.
func (s *skillFailureStore) since(skill string, t time.Time) []contract.Rollout {
	dir := s.dir(skill)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	reapOrphanTmps(dir, entries)
	var out []contract.Rollout
	for _, f := range failFilesByAge(dir, entries, failRecordExt) {
		if !f.at.After(t) {
			_ = os.Remove(f.path) // consumed last cycle → sweep
			continue
		}
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		out = append(out, contract.Rollout{OK: false, Snapshot: string(data), At: f.at})
	}
	return out
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

var _ contract.SkillFailureSink = (*skillFailureStore)(nil)
