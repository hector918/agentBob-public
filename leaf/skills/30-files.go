package skills

// 30-files.go — SkillSet.Files: a skill's auxiliary files (scripts/templates),
// read from the embed (internal) or the external dir. The script mechanism
// (docs/skills-rebuild.md §7) materializes these into the session space so the
// model can run them via terminal. Reserved meta files are excluded: SKILL.md (its
// body is already in the prompt), RUBRIC.md (the turn's grading criteria — leaking
// it into the model's workspace lets the model read and game the rubric gate), and
// INSIGHT.md (learned notes, already appended to the body). A symlink in an external
// skill is refused (exfil guard, same as the loader). Bounded by per-file + total
// size caps so a fat skill can't blow memory.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"agentbob/contract"
)

const (
	maxSkillFileBytes  = 1 << 20 // 1 MB per auxiliary file
	maxSkillFilesTotal = 8 << 20 // 8 MB per skill, all files
)

// Files returns the skill's auxiliary files. Internal skills read from the embed;
// external skills walk their on-disk dir. Unknown name → error.
func (s *FileSkills) Files(name string) ([]contract.SkillFile, error) {
	s.mu.RLock()
	l, ok := s.byName[name]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("skill %q not found", name)
	}
	if l.skill.Origin == contract.OriginInternal {
		return embedSkillFiles(l.skill.Path)
	}
	return dirSkillFiles(l.skill.Path)
}

// embedSkillFiles walks the embedded skill dir (root = "builtin/<name>").
func embedSkillFiles(root string) ([]contract.SkillFile, error) {
	var out []contract.SkillFile
	var total int64
	err := fs.WalkDir(builtinFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := forwardRel(root, p)
		if rel == "" || isReservedSkillFile(rel) {
			return nil
		}
		b, rerr := builtinFS.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if int64(len(b)) > maxSkillFileBytes {
			warn(p, "skill file exceeds per-file size cap — skipping")
			return nil
		}
		total += int64(len(b))
		if total > maxSkillFilesTotal {
			return fmt.Errorf("skill files exceed total size cap")
		}
		out = append(out, contract.SkillFile{Rel: rel, Data: b})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// dirSkillFiles walks an external skill dir on disk, refusing symlinks.
func dirSkillFiles(root string) ([]contract.SkillFile, error) {
	var out []contract.SkillFile
	var total int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Exfil guard: never follow a symlink (file OR dir) out of the skill tree.
		if d.Type()&os.ModeSymlink != 0 {
			warn(p, "skill file is a symlink — refusing to follow (exfil guard)")
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel := forwardRel(root, p)
		if rel == "" || isReservedSkillFile(rel) {
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if fi.Size() > maxSkillFileBytes {
			warn(p, "skill file exceeds per-file size cap — skipping")
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			warn(p, "skill file unreadable — skipping")
			return nil
		}
		total += int64(len(b))
		if total > maxSkillFilesTotal {
			return fmt.Errorf("skill files exceed total size cap")
		}
		out = append(out, contract.SkillFile{Rel: rel, Data: b})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// isReservedSkillFile reports whether rel is a skill meta file that must never be
// materialized into the model's workspace: SKILL.md (already in the prompt body),
// RUBRIC.md (the rubric-gate grading criteria — exposing it lets the model game the
// gate), and INSIGHT.md (learned notes, already appended to the body).
//
// Matching is not exact-only: a crash-orphaned temp (skillInsightStore.put writes
// INSIGHT.md.tmp then renames — a crash leaves the .tmp) or an editor artifact
// (RUBRIC.md~, RUBRIC.md.bak, vim's swap .RUBRIC.md.swp) is a VARIANT of a reserved
// name and must be excluded just the same, or it ships the rubric criteria out. A
// variant is the reserved name plus a non-alphanumeric suffix marker (.tmp / ~ / .bak),
// optionally behind a single leading dot (the .swp swap form). A legit sibling with an
// alphanumeric continuation (e.g. SKILL.mdx) still ships.
func isReservedSkillFile(rel string) bool {
	base := rel
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimPrefix(base, ".") // vim swap: .RUBRIC.md.swp → RUBRIC.md.swp
	for _, r := range []string{"SKILL.md", "RUBRIC.md", "INSIGHT.md"} {
		if base == r {
			return true
		}
		if strings.HasPrefix(base, r) {
			if c := base[len(r)]; !isAlphanumericByte(c) {
				return true // reserved name + a suffix marker (.tmp / ~ / .bak / .swp)
			}
		}
	}
	return false
}

// isAlphanumericByte reports whether c is an ASCII letter or digit — used to tell a
// reserved-name suffix marker (a '.', '~', …) from a legit name continuation.
func isAlphanumericByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// forwardRel returns p relative to root in forward-slash form, or "" if p == root
// or escapes root.
func forwardRel(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}
