// Package skills is the skill catalog: a dumb, queryable set of installed skills,
// symmetric with leaf/tools. It scans two sources — internal (built-in, embedded in
// the binary) and external ($BOB_HOME/skills/external/) — parses each SKILL.md into
// a contract.Skill, and exposes them as a contract.SkillCatalog. Projection (which
// skills an identity sees) lives in warrant; activation + prompt injection live in
// the flow. This package only loads and reads.
//
// A skill is TEXT + METADATA; it carries files only when it ships scripts (a
// followup that rides the spaces mechanism — see docs/skills.md). v1 loads the
// instruction body + the typed switches; it does not run scripts or validate beyond
// the minimum (name == dirname, parseable frontmatter, size cap).
package skills

import (
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"agentbob/contract"

	"gopkg.in/yaml.v3"
)

// warn is the single skill-isolation channel: a skill that fails the minimum
// checks is dropped with a logged reason rather than failing the load.
func warn(path, reason string) {
	slog.Warn("skills: skipping skill", "path", path, "reason", reason)
}

// builtinFS holds the internal skills, extracted/scanned from the binary. Each
// subdirectory with a SKILL.md is one built-in skill.
//
//go:embed all:builtin
var builtinFS embed.FS

// maxSkillBodyBytes caps a SKILL.md — a skill body is a prompt supplement, not a
// document; an oversized file is a mistake, skipped with a WARN.
const maxSkillBodyBytes = 64 * 1024

// frontmatter is the typed mirror of a SKILL.md's leading YAML block.
type frontmatter struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Triggers    triggerList `yaml:"triggers"` // recall cues for the prompt index (see docs/skill-recall-index.md)
}

// triggerList tolerates a hand-edited triggers field in EITHER form — a YAML sequence
// (triggers: [a, b] / a block list) OR a single scalar (triggers: "超声, ultrasound" — the
// natural CSV mistake). A malformed value yields NO triggers rather than failing the whole
// frontmatter parse and silently dropping the skill from the index + skill_view (which would
// be the exact "tool over skill" regression this field fights). See docs/skill-recall-index.md.
type triggerList []string

func (t *triggerList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*t = strings.Split(node.Value, ",") // CSV or single value
	case yaml.SequenceNode:
		var s []string
		if err := node.Decode(&s); err == nil {
			*t = s
		} // odd sequence content → leave empty, keep the skill
	}
	return nil
}

// cleanTriggers trims, drops blanks, and de-dups (case-insensitively) the frontmatter
// trigger list for the prompt index. Order preserved.
func cleanTriggers(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	return out
}

// loaded is a skill plus its instruction body (the bytes after the frontmatter).
type loaded struct {
	skill contract.Skill
	body  string
}

// FileSkills is the contract.SkillCatalog: internal (embed) + external (dir), with
// external overriding internal on a name clash.
type FileSkills struct {
	externalDir string
	degraded    atomic.Bool // serving builtins-only after a failed external scan (see Degraded)

	mu        sync.RWMutex
	byName    map[string]loaded
	addenda   map[addendaKey]string                          // (origin,name) → learned addendum (appended to the body)
	onPrune   func(origin contract.SkillOrigin, name string) // optional: mirror an addendum prune to persistence (set by the module)
	onRemoved func(name string)                              // optional: fired for each skill REMOVED by a Reload (set by the module)
}

// addendaKey scopes a learned addendum by ORIGIN + name, not bare name: an internal
// skill's INSIGHT must not latch onto a later same-named EXTERNAL skill (or vice
// versa) across a reload. The catalog keys live skills by name (external overrides
// internal), so the resolved skill's Origin is the authority for which note applies.
type addendaKey struct {
	origin contract.SkillOrigin
	name   string
}

// SetAddendumPruner wires a callback invoked (outside the lock) for each skill
// whose learned addendum is dropped on Reload — the module uses it to delete the
// skill's persisted notes file too, so a removed skill's INSIGHT.md doesn't outlive
// the skill (B-26). The origin selects the right INSIGHT.md subdir. Optional; nil →
// in-memory prune only.
func (s *FileSkills) SetAddendumPruner(fn func(origin contract.SkillOrigin, name string)) {
	s.mu.Lock()
	s.onPrune = fn
	s.mu.Unlock()
}

// SetRemovedPruner wires a callback invoked (outside the lock) for each skill NAME
// a Reload removes from the catalog — regardless of whether it ever learned an
// addendum. The module uses it to sweep the skill's failure-snapshot dir
// (.failures/<name>), which SetAddendumPruner alone would miss for a skill that
// failed but never distilled. Optional; nil → no mirror.
func (s *FileSkills) SetRemovedPruner(fn func(name string)) {
	s.mu.Lock()
	s.onRemoved = fn
	s.mu.Unlock()
}

// NewCatalog returns a skill catalog reading built-ins from the embed and externals
// from $home/skills/external. Reload populates it.
func NewCatalog(home string) *FileSkills {
	return &FileSkills{
		externalDir: filepath.Join(home, "skills", "external"),
		byName:      map[string]loaded{},
		addenda:     map[addendaKey]string{},
	}
}

// setAddendum stores a skill's learned addendum; addendum reads it. Symmetric with
// the tools catalog — the learn engine's skillTarget writes via setAddendum, and
// Read appends it to the body so it reaches the model. A reload does NOT clear
// addenda (learned notes survive a rescan).
func (s *FileSkills) setAddendum(origin contract.SkillOrigin, name, text string) {
	s.mu.Lock()
	s.addenda[addendaKey{origin, name}] = text
	s.mu.Unlock()
}

func (s *FileSkills) addendum(origin contract.SkillOrigin, name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.addenda[addendaKey{origin, name}]
}

// Reload rescans both sources. Internal first, then external (external wins on a
// name clash). A skill that fails the minimum checks is skipped with a WARN — the
// only isolation channel. Best-effort: a missing external dir is not an error.
func (s *FileSkills) Reload() error {
	next := map[string]loaded{}
	scanEmbed(builtinFS, "builtin", next)
	seen := map[string]bool{}
	if err := scanDir(s.externalDir, next, seen); err != nil {
		// Fail-closed: a transient external-dir READ error (permission blip, NFS
		// hiccup, volume not yet mounted) must NOT look like "all external skills
		// removed" — that would drop them from the catalog AND irreversibly prune
		// their learned addenda from memory + DB. Keep the current state untouched
		// and report; a genuinely-missing dir is not an error (scanDir returns nil).
		return fmt.Errorf("skills reload: external scan failed, keeping current skills: %w", err)
	}

	s.mu.Lock()
	// Names removed by this reload — mirrored to persistence below (failure
	// snapshots), independent of the addenda bookkeeping. A name still SEEN on
	// disk (present but skipped as invalid — e.g. mid-edit broken frontmatter)
	// is NOT removed: its snapshots must survive the round-trip.
	var removed []string
	for name := range s.byName {
		if _, ok := next[name]; !ok && !seen[name] {
			removed = append(removed, name)
		}
	}
	s.byName = next
	// Learned addenda survive a rescan (a re-found skill keeps its notes), but a
	// skill REMOVED across the reload drops its addendum — else the map grows
	// unboundedly with every (origin,name) ever seen. Keyed by ORIGIN+name: an
	// addendum is live only when the catalog now holds that SAME-origin skill (an
	// internal note must not be kept alive by a later same-named external skill, and
	// must be pruned when the external one supersedes it).
	var pruned []addendaKey
	for k := range s.addenda {
		// Mirror the `removed` diff's seen-guard: an EXTERNAL skill still present on
		// disk but skipped as invalid (mid-edit broken frontmatter) is absent from
		// `next`, yet its learned memory must survive the round-trip. Without this its
		// addendum would be dropped AND onPrune would delete external/<name>/INSIGHT.md
		// from disk — fixing the frontmatter and reloading would then restore the skill
		// with blank, non-regenerable memory. The intentional external-supersedes-
		// internal prune of an INTERNAL addendum is unaffected (origin != external).
		if k.origin == contract.OriginExternal && seen[k.name] {
			continue
		}
		if l, ok := next[k.name]; !ok || l.skill.Origin != k.origin {
			delete(s.addenda, k)
			pruned = append(pruned, k)
		}
	}
	onPrune, onRemoved := s.onPrune, s.onRemoved
	s.mu.Unlock()
	// Mirror the in-memory prune to persistence (best-effort, outside the lock) so a
	// removed skill's INSIGHT.md doesn't outlive the skill on disk (B-26).
	if onPrune != nil {
		for _, k := range pruned {
			onPrune(k.origin, k.name)
		}
	}
	if onRemoved != nil {
		for _, name := range removed {
			onRemoved(name)
		}
	}
	// A full two-source scan succeeded, so List() is complete again (a failed
	// Reload returned above and kept whatever state — including degraded — it had).
	s.degraded.Store(false)
	return nil
}

// Degraded reports whether the catalog is serving the BUILT-IN set only because the
// external scan failed (loadBuiltinsOnly) — i.e. List() is known-incomplete until a
// successful Reload clears it. Consumers that must not read "temporarily unseen" as
// "removed" (warrant's grant-row reconcile prunes skill:use:<name> rows against
// List()) discover this via an optional type-assert, like contract.SkillReloader —
// contract.SkillCatalog itself stays minimal and test stubs needn't implement it.
func (s *FileSkills) Degraded() bool { return s.degraded.Load() }

// loadBuiltinsOnly applies ONLY the compiled-in built-in skills (embed FS, zero disk
// I/O) — the boot fallback when the external scan keeps failing, so the catalog never
// comes up empty over an external-dir blip. External skills join on the next successful
// Reload. Boot-only: at boot no learned addenda are loaded yet, so there is nothing to
// prune.
func (s *FileSkills) loadBuiltinsOnly() {
	next := map[string]loaded{}
	scanEmbed(builtinFS, "builtin", next)
	s.mu.Lock()
	s.byName = next
	s.mu.Unlock()
	// Mark List() as known-incomplete so consumers (warrant's reconcile, the
	// module's orphan sweep) don't treat the missing external skills as removed.
	s.degraded.Store(true)
}

func (s *FileSkills) List() []contract.SkillInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]contract.SkillInfo, 0, len(s.byName))
	for _, l := range s.byName {
		out = append(out, l.skill.SkillInfo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *FileSkills) Get(name string) (contract.Skill, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.byName[name]
	return l.skill, ok
}

func (s *FileSkills) Read(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.byName[name]
	if !ok {
		return "", false
	}
	body := l.body
	// Learned notes ride at the tail of the body — keyed by the RESOLVED skill's
	// origin so an internal note can't bleed onto a same-named external skill.
	if a := s.addenda[addendaKey{l.skill.Origin, name}]; a != "" {
		body += "\n\n" + a
	}
	return body, ok
}

var _ contract.SkillCatalog = (*FileSkills)(nil)

// --- scanning ----------------------------------------------------------------

// scanEmbed walks the embedded built-in tree; each subdir with a SKILL.md is a
// skill (Origin internal).
func scanEmbed(efs embed.FS, root string, into map[string]loaded) {
	entries, err := efs.ReadDir(root)
	if err != nil {
		return // no built-ins embedded
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := root + "/" + e.Name()
		raw, err := efs.ReadFile(dir + "/SKILL.md")
		if err != nil {
			continue
		}
		add(into, e.Name(), dir, raw, contract.OriginInternal)
	}
}

// scanDir walks an external skills directory; each subdir with a SKILL.md is a
// skill (Origin external). A missing dir (or a subdir without a SKILL.md) is
// silently empty; any OTHER read error — on the dir OR an individual SKILL.md — is
// returned so Reload can fail-closed instead of treating a transient I/O blip as
// "the skill is gone" (which would prune live skills + their addenda).
// seen records every dir that HAS a SKILL.md (valid or not) — Reload uses it to
// tell "skill genuinely deleted from disk" apart from "skill present but skipped
// as invalid", so a mid-edit broken frontmatter can't get the skill's failure
// snapshots swept as if it were uninstalled. nil → not tracked.
func scanDir(root string, into map[string]loaded, seen map[string]bool) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name())
		// A symlinked SUBDIR is already skipped above (ReadDir reports it as a
		// symlink, not a dir); readFileNoSymlink guards the SKILL.md itself.
		raw, rerr := readFileNoSymlink(path, "SKILL.md")
		if rerr == nil || rerr == errIsSymlink {
			if seen != nil {
				seen[e.Name()] = true // a SKILL.md exists here, however broken
			}
		}
		if rerr != nil {
			if os.IsNotExist(rerr) || rerr == errIsSymlink {
				continue // no SKILL.md here / refused symlink (already warned)
			}
			// A transient per-file read error (EACCES/EIO blip) must fail the whole
			// reload closed — not look like "no SKILL.md here", which would prune the
			// skill AND its learned addendum (see Reload's fail-closed contract).
			return rerr
		}
		add(into, e.Name(), path, raw, contract.OriginExternal)
	}
	return nil
}

// errIsSymlink marks a refused symlinked file (exfil guard). The caller skips the
// file (readFileNoSymlink already warned) rather than treating it as a transient
// read failure.
var errIsSymlink = errors.New("file is a symlink")

// readFileNoSymlink Lstats dir/name and reads it, refusing to follow a symlink
// (warn + errIsSymlink) — os.ReadFile would follow it and ingest the target (e.g. a
// secret outside the skills dir) as prompt text. The single copy of the exfil guard,
// shared by the SKILL.md and RUBRIC.md read paths; each caller keeps its own
// disposition on the returned error.
func readFileNoSymlink(dir, name string) ([]byte, error) {
	p := filepath.Join(dir, name)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		warn(dir, name+" is a symlink — refusing to follow (exfil guard)")
		return nil, errIsSymlink
	}
	return os.ReadFile(p)
}

// add parses one skill's bytes and inserts it (external overrides internal — the
// caller scans internal first). Minimum checks: size cap, parseable frontmatter,
// name == dirname; a failure is a WARN-and-skip.
func add(into map[string]loaded, dirName, path string, raw []byte, origin contract.SkillOrigin) {
	if len(raw) > maxSkillBodyBytes {
		warn(path, "SKILL.md exceeds size cap")
		return
	}
	fm, body, err := parseSkillMD(raw)
	if err != nil {
		warn(path, err.Error())
		return
	}
	if fm.Name == "" || fm.Name != dirName {
		warn(path, fmt.Sprintf("frontmatter name %q must equal directory name %q", fm.Name, dirName))
		return
	}
	into[fm.Name] = loaded{
		skill: contract.Skill{
			SkillInfo: contract.SkillInfo{
				Name:        fm.Name,
				Description: strings.TrimSpace(fm.Description),
				Triggers:    cleanTriggers(fm.Triggers),
				Origin:      origin,
			},
			Path:   path,
			Rubric: readRubric(path, origin),
		},
		body: strings.TrimSpace(body),
	}
}

// maxRubricBytes caps a RUBRIC.md — a rubric is grounding CRITERIA, not a document;
// it is sent whole into the judge prompt every gate, so an oversized one would blow
// the judge's context (→ judge error → gate fails open, silently). An over-cap file
// drops its gate with a WARN (a truncated rubric is worse than none — the judge
// would grade against half the criteria). Mirrors SKILL.md's maxSkillBodyBytes guard.
const maxRubricBytes = 32 * 1024

// readRubric reads a skill's optional RUBRIC.md (the turn's rubric-gate criteria)
// from the same dir as its SKILL.md. Missing / unreadable / oversized → "" (the skill
// simply has no gate). Read verbatim — never summarized. Mirrors the SKILL.md read
// path: the embed FS for internal skills, the filesystem for external ones (with the
// same symlink exfil guard the SKILL.md read applies).
func readRubric(path string, origin contract.SkillOrigin) string {
	var raw []byte
	if origin == contract.OriginInternal {
		b, err := builtinFS.ReadFile(path + "/RUBRIC.md")
		if err != nil {
			return ""
		}
		raw = b
	} else {
		b, err := readFileNoSymlink(path, "RUBRIC.md")
		if err != nil {
			return "" // missing / symlink-refused (warned) / unreadable → no gate
		}
		raw = b
	}
	if len(raw) > maxRubricBytes {
		warn(path, "RUBRIC.md exceeds size cap — skipping (no rubric gate for this skill)")
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// parseSkillMD splits the leading `---`-delimited YAML frontmatter from the body.
func parseSkillMD(raw []byte) (frontmatter, string, error) {
	text := string(raw)
	if !strings.HasPrefix(text, "---") {
		return frontmatter{}, "", fmt.Errorf("SKILL.md must open with a --- frontmatter block")
	}
	rest := strings.TrimPrefix(text, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return frontmatter{}, "", fmt.Errorf("SKILL.md frontmatter is not closed with ---")
	}
	fmText := rest[:end]
	body := rest[end+len("\n---"):]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:] // drop the rest of the closing --- line
	} else {
		body = ""
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return frontmatter{}, "", fmt.Errorf("frontmatter YAML: %w", err)
	}
	return fm, body, nil
}
