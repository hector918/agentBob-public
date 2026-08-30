package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// This file is the tools side of the learn seam (docs/learn.md): one
// contract.Learnable per tool (its learned usage notes), the single
// contract.LearnSource the module registers into leaf/learn, and the per-tool
// FAILURE store that feeds it. The catalog's observer wrapper records a shape-only
// record on each FAILED Run; toolTarget.Traces reads them. The learned notes persist
// as INSIGHT.md files (docs/learn.md §6) — one per tool under $BOB_HOME/insights/tools/
// — and the failure records under .failures/ there, so accreted experience AND its
// pending raw material both survive a restart (symmetric with leaf/skills).

// toolFailMaxPerTool caps how many un-distilled failure records a tool keeps; past it
// the oldest are dropped so a tool that's never distilled (no learn model) can't grow
// unbounded on disk. Mirrors skills' skillFailMaxPerSkill.
const toolFailMaxPerTool = 50

// failRecordExt is this store's record-file extension (skills' mirror uses ".md").
const failRecordExt = ".json"

// toolFailure is the SHAPE-ONLY record persisted per failed tool call: the argument
// SHAPE (key names + length, never raw values — argShapeOf), the error, and any
// recovery hint — exactly the fields the distiller renders. Raw arg values never touch
// disk (they can hold user/external text or secrets; docs/learn.md §8 injection面).
type toolFailure struct {
	Shape string `json:"shape"`
	Error string `json:"error"`
	Hint  string `json:"hint"`
}

// toolFailureStore rings each tool's failed-call records as files under
// base/<tool>/<nano>-<hex>.json (base = $BOB_HOME/insights/tools/.failures). The
// catalog's observer writes via record (on a failed Run); toolTarget.Traces reads
// them; a consumed record (at <= the engine's watermark) is swept on the next read.
// Mirrors leaf/skills' skillFailureStore (consume-on-read-delete, atomic write, capped)
// — the same proven idiom, so tool experience survives a restart like skills' does.
type toolFailureStore struct{ base string }

func (s *toolFailureStore) dir(tool string) string {
	return filepath.Join(s.base, safeToolName(tool))
}

// record writes one failure record for tool (atomic tmp+rename). Best-effort: a write
// error is dropped (learning is non-critical, never blocks a tool's hot path).
func (s *toolFailureStore) record(tool string, f toolFailure) {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return
	}
	data, err := json.Marshal(f)
	if err != nil {
		return
	}
	dir := s.dir(tool)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	var rb [6]byte
	_, _ = rand.Read(rb[:])
	final := filepath.Join(dir, fmt.Sprintf("%d-%s.json", clock.Now().UnixNano(), hex.EncodeToString(rb[:])))
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, final)
	s.cap(dir)
}

// since returns this tool's failure records written strictly after t (oldest-first),
// reconstructed as Rollouts (Args = the stored shape), and SWEEPS any already-consumed
// (at <= t) — the watermark advances past consumed rows, so this is where they get
// deleted. Also reaps orphan .tmp files (a crash between WriteFile and Rename).
//
// CALLER CONTRACT: the learn engine is the SOLE caller (via toolTarget.Traces), once
// per cycle with a monotonically-advancing watermark. since() is a READ-WITH-DELETE —
// a second consumer would destroy not-yet-distilled records. Do NOT add one.
func (s *toolFailureStore) since(tool string, t time.Time) []contract.Rollout {
	dir := s.dir(tool)
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
		var tf toolFailure
		if json.Unmarshal(data, &tf) != nil {
			continue
		}
		out = append(out, contract.Rollout{OK: false, Args: tf.Shape, Error: tf.Error, Hint: tf.Hint, At: f.at})
	}
	return out
}

// cap drops the oldest records beyond toolFailMaxPerTool (best-effort).
func (s *toolFailureStore) cap(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	files := failFilesByAge(dir, entries, failRecordExt)
	for i := 0; i+toolFailMaxPerTool < len(files); i++ {
		_ = os.Remove(files[i].path)
	}
}

// sweepOrphans removes failure-record dirs whose tool is not in live (a set of
// safeToolName'd catalog names). The tool set is static per build (no reload path),
// so the module runs this ONCE at Start — a tool renamed/deleted across builds would
// otherwise keep its .failures/<tool>/ dir forever. A ReadDir error skips the sweep
// (can't enumerate → don't delete). Returns dirs removed. Mirrors leaf/skills'
// skillFailureStore.sweepOrphans.
func (s *toolFailureStore) sweepOrphans(live map[string]bool) int {
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

// safeToolName keeps a tool name filesystem-safe (the .failures/<tool>/ dir and the
// per-tool INSIGHT.md filename). Tool names are code identifiers ([A-Za-z0-9_]);
// sanitize defensively so a stray char can never escape the base dir.
func safeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
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

// argShapeOf renders a tool-call's arguments as STRUCTURE only — sorted key names +
// total length — never the raw values. Computed at RECORD time (here, in leaf/tools)
// so raw values never touch disk; the result is what the distiller appends to the
// tool description (and thus into every later system prompt). Rendering only the shape
// denies a crafted argument a channel to inject adversarial "experience" into the
// prompt (the self-accumulating injection面, docs/learn.md §8 / C3①).
func argShapeOf(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "(空)"
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) == nil && len(obj) > 0 {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Sprintf("字段[%s] 长度%d", strings.Join(keys, ","), len(args))
	}
	return fmt.Sprintf("长度%d", len(args))
}

// toolTarget is one tool's contract.Learnable: its learned notes live in the catalog
// (read by Specs), its failure records in the store. It does NOT implement
// contract.Scorer, so the engine uses the integral-rewrite guard (no replay gate).
type toolTarget struct {
	cat   *catalog
	fail  *toolFailureStore // failure-record trace source; nil → no traces
	name  string
	store *insightStore // learned-notes persistence; nil → in-memory only
}

func (t toolTarget) Key() string                             { return "tool:" + t.name }
func (t toolTarget) Current(context.Context) (string, error) { return t.cat.addendum(t.name), nil }

// Traces hands the engine this tool's recorded failure records since `since`.
func (t toolTarget) Traces(_ context.Context, since time.Time) ([]contract.Rollout, error) {
	if t.fail == nil {
		return nil, nil
	}
	return t.fail.since(t.name, since), nil
}

func (t toolTarget) Apply(_ context.Context, revised string) error {
	t.cat.setAddendum(t.name, revised)
	if t.store != nil {
		return t.store.put(t.name, revised)
	}
	return nil
}

// learnSource is the module's single contract.LearnSource: one target per tool,
// enumerated fresh each sweep.
type learnSource struct {
	cat   *catalog
	fail  *toolFailureStore
	store *insightStore
}

func (s learnSource) Targets(context.Context) []contract.Learnable {
	names := s.cat.names()
	out := make([]contract.Learnable, 0, len(names))
	for _, n := range names {
		out = append(out, toolTarget{cat: s.cat, fail: s.fail, name: n, store: s.store})
	}
	return out
}

// --- INSIGHT.md persistence (docs/learn.md §6) -------------------------------

// insightStore persists each tool's learned notes as a markdown file under dir
// ($BOB_HOME/insights/tools/). Tools are code (no directory of their own), so the
// per-tool insight lives in a central dir as <tool>.md. Optional: the catalog works
// fine without it (notes are then in-memory).
type insightStore struct{ dir string }

// insightFile maps a tool name to its file, keyed by the same sanitized name as the
// .failures dir (safeToolName) so both stores always agree on a tool's identity.
func insightFile(name string) string {
	return safeToolName(name) + ".md"
}

// put writes the tool's insight (atomic tmp+rename). Empty text removes the file —
// an integral rewrite that produced nothing leaves no empty INSIGHT.md behind.
func (s *insightStore) put(name, text string) error {
	final := filepath.Join(s.dir, insightFile(name))
	if strings.TrimSpace(text) == "" {
		if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// all loads every persisted insight at startup, keyed by tool name.
//
// Per-file fail-open (mirrors skills' skillInsightStore.all): an unreadable file is
// warned and skipped, returning the partial map — one bad file must not blank EVERY
// tool's Current(), which a later distill would then use as an empty base and
// overwrite the intact on-disk notes from.
func (s *insightStore) all() map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if !os.IsNotExist(err) { // missing dir = first run, no insights yet
			slog.Warn("tools: load learned insights: dir unreadable — skipping", "dir", s.dir, "err", err)
		}
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Sweep an orphan tmp from a crash between WriteFile and Rename — bounded (one
		// per tool, overwritten on the next put) but nothing else ever removes it.
		if strings.HasSuffix(e.Name(), ".tmp") {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			slog.Warn("tools: load learned insight failed — skipping", "file", e.Name(), "err", err)
			continue
		}
		out[strings.TrimSuffix(e.Name(), ".md")] = string(data)
	}
	return out
}

// sweepOrphans removes INSIGHT.md files whose tool is not in live (a set of
// safeToolName'd catalog names) — a tool renamed/deleted across builds would
// otherwise keep its stale note forever AND reload it into memory every boot. The
// tool set is static per build, so the module runs this once at Start, before all().
// A ReadDir error skips the sweep (can't enumerate → don't delete). Returns files
// removed.
func (s *insightStore) sweepOrphans(live map[string]bool) int {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0 // no insights dir yet — nothing to sweep
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue // .tmp orphans are all()'s to reap
		}
		if live[strings.TrimSuffix(e.Name(), ".md")] {
			continue
		}
		if os.Remove(filepath.Join(s.dir, e.Name())) == nil {
			removed++
		}
	}
	return removed
}
