package file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/core"
	"agentbob/sidecars/browser/tools/policy"
)

const patchParams = `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "目标文件路径。相对路径自动落到当前 session 的 sandbox（HOME / cwd 都是它），也可以是绝对路径或 ~/ 形式。新建 / 修改要求 path 在 sandbox 或 read_write_roots 内。"
    },
    "append": {
      "type": "string",
      "description": "直接把这段内容追加到文件末尾。不需要 diff 语法 / 上下文 / 行号——一段纯文本就行。文件不存在则自动新建（连父目录也会创建）。新建文档 / 写长内容（每轮一段，多轮累积）首选这条；最后用 deliver_file 发回用户或 read_file 检查。跟 diff 互斥。"
    },
    "diff": {
      "type": "string",
      "description": "单文件 unified diff，用于修改已存在的文件。'--- a/foo' '+++ b/foo' 头被接受但忽略——path 参数为准。hunk 行以 ' '（上下文）、'-'（删除）、'+'（增加）开头。严格上下文匹配——文件实际内容跟 @@ 行号处不一致就拒绝。新建文件请用 append 字段（更简单可靠）。跟 append 互斥。"
    }
  },
  "required": ["path"]
}`

type patch struct {
	cfg            config.FilesystemConfig
	pol            *policy.Policy
	namespaceByBot bool
}

// Patch constructs the patch tool. Tier 🟡 admin (writes to disk). pol is
// the shared D-residual gate — banned target paths refuse outright,
// approval-required paths prompt admin /approve.
func Patch(cfg config.FilesystemConfig, pol *policy.Policy, namespaceByBot bool) core.Tool {
	return patch{cfg: cfg, pol: pol, namespaceByBot: namespaceByBot}
}

func (patch) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:           "patch",
		Description:    "写文件 —— 两个模式（互斥）：(1) `append`：直接追加一段纯文本到末尾，文件不存在自动新建。新建文档 / 写长内容（分多轮 append 累积）首选这条。无需 diff 语法 / 行号 / 上下文。(2) `diff`：unified diff 改已存在的文件，严格上下文匹配。原子写（temp+rename）。常搭配 deliver_file 把生成的文件当附件发回用户。",
		Parameters:     json.RawMessage(patchParams),
		NoAutoCompress: true,
	}
}

func (patch) Tier() core.Tier { return core.TierUser }

// Serialize is true since the append mode landed. Two
// parallel append calls on the same path each read the same baseline +
// each os.Rename their temp → the second clobbers the first append's
// contribution, silently losing the lost-write bytes. The diff path
// has the same theoretical race but its strict context-match
// catches stale baselines loudly; append has no such safety net.
// Simplest fix: serialise all patch calls within a turn. Patch isn't
// a hot tool — sequential is fine and aligns with the model's typical
// "edit one file at a time" pattern.
func (patch) Serialize() bool { return true }

func (p patch) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var arg struct {
		Path   string `json:"path"`
		Diff   string `json:"diff"`
		Append string `json:"append"`
	}
	if err := json.Unmarshal(args, &arg); err != nil {
		return core.ToolResult{}, fmt.Errorf("patch: invalid arguments: %w", err)
	}
	if strings.TrimSpace(arg.Path) == "" {
		return core.ToolResult{}, fmt.Errorf("patch: path is required")
	}
	hasDiff := strings.TrimSpace(arg.Diff) != ""
	hasAppend := arg.Append != "" // append intentionally NOT trimmed — `"\n"` is a valid append payload (paragraph break)
	if hasDiff && hasAppend {
		return core.ErrResult("patch: `diff` and `append` are mutually exclusive — pick one mode per call"), nil
	}
	if !hasDiff && !hasAppend {
		return core.ToolResult{}, fmt.Errorf("patch: one of `diff` or `append` is required")
	}

	canonical, err := ResolvePath(p.cfg, p.namespaceByBot, actx, arg.Path, ModeWrite)
	if err != nil {
		return core.ErrResult(err.Error()), nil
	}
	// D-residual: even within the filesystem allowlist, certain trees
	// require admin approval to mutate (~/Documents) or are off-limits
	// entirely (~/.ssh). Apply policy on the resolved canonical path.
	if ok, refusal := policy.EnforcePath(ctx, p.pol, actx, "patch", canonical, canonical); !ok {
		return refusal, nil
	}

	if hasAppend {
		return runAppend(canonical, arg.Append)
	}
	return runDiff(canonical, arg.Diff)
}

// runDiff is the unified-diff write path — what `patch` did before
// when `diff` was the only mode.
func runDiff(canonical, diffText string) (core.ToolResult, error) {
	hunks, err := parsePatchHunks(diffText)
	if err != nil {
		return errResult("parse diff: " + err.Error()), nil
	}
	if len(hunks) == 0 {
		return errResult("diff has no hunks"), nil
	}

	// Detect new-file creation (first hunk is @@ -0,0 +N,M @@).
	isNewFile := hunks[0].origStart == 0 && hunks[0].origLines == 0
	var existing []byte
	// origMode tracks the original file's mode bits so the post-patch
	// write preserves +x on scripts etc. T7 fix: the prior
	// hardcoded 0o644 silently downgraded executable files to non-
	// executable on every patch — breaking shebang scripts.
	origMode := os.FileMode(0o644)
	if !isNewFile {
		fi, statErr := os.Stat(canonical)
		if statErr == nil {
			origMode = fi.Mode().Perm()
		}
		existing, err = os.ReadFile(canonical)
		if err != nil {
			if os.IsNotExist(err) {
				return errResult(fmt.Sprintf("file %s does not exist; for new files the first hunk must be @@ -0,0 ...", canonical)), nil
			}
			return errResult(fmt.Sprintf("read %s: %v", canonical, err)), nil
		}
	} else if _, statErr := os.Stat(canonical); statErr == nil {
		return errResult(fmt.Sprintf("file %s already exists but the patch is a new-file creation (@@ -0,0 ...)", canonical)), nil
	}

	// T7 fix: detect CRLF in the existing file so we can match hunk
	// context lines (which are always LF-normalised in unified diff)
	// against the file body and restore CRLF on write. Pre-fix:
	// CRLF files silently failed "context mismatch" because the \r
	// stayed glued to each line during string-split. Detection is a
	// simple "does the file's first newline have \r before it?" — if
	// the file is single-line we leave it as LF (no CRLF to preserve).
	hadCRLF := false
	if i := bytes.IndexByte(existing, '\n'); i > 0 && existing[i-1] == '\r' {
		hadCRLF = true
		existing = bytes.ReplaceAll(existing, []byte("\r\n"), []byte("\n"))
	}

	updated, applied, err := applyHunks(existing, hunks)
	if err != nil {
		return errResult("apply: " + err.Error()), nil
	}

	// Restore CRLF if the original used it.
	if hadCRLF {
		updated = bytes.ReplaceAll(updated, []byte("\n"), []byte("\r\n"))
	}

	// Ensure parent dir exists for new files. R5A-9: 0o750 (owner+group),
	// not 0o755 — bob-created dirs shouldn't be world-readable.
	if isNewFile {
		if err := os.MkdirAll(filepath.Dir(canonical), 0o750); err != nil {
			return errResult(fmt.Sprintf("create parent dir: %v", err)), nil
		}
	}
	// Atomic write: temp + rename. T7 fix: use os.CreateTemp so a
	// crash between WriteFile and Rename leaves a uniquely-named
	// orphan rather than always-the-same path that would block future
	// patches with a confusing "rename: file exists" error. Every error
	// branch below os.Remove's the temp, so a leftover only happens on a
	// hard crash / kill between Write and Rename — low probability. Note
	// there is NO automatic sweep for these: the sandbox dir-sweep only
	// walks by_sessions_scope, so a temp left under a read_write_root
	// (e.g. /workspace) is never reclaimed. We rename the temp INTO place
	// inside the same dir to keep the rename atomic.
	tmpFile, err := os.CreateTemp(filepath.Dir(canonical), filepath.Base(canonical)+".bobpatch.*")
	if err != nil {
		return errResult(fmt.Sprintf("create temp: %v", err)), nil
	}
	tmp := tmpFile.Name()
	if _, err := tmpFile.Write(updated); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return errResult(fmt.Sprintf("write temp: %v", err)), nil
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return errResult(fmt.Sprintf("close temp: %v", err)), nil
	}
	// Preserve original mode (or 0o644 for new files).
	if err := os.Chmod(tmp, origMode); err != nil {
		_ = os.Remove(tmp)
		return errResult(fmt.Sprintf("chmod temp: %v", err)), nil
	}
	if err := os.Rename(tmp, canonical); err != nil {
		_ = os.Remove(tmp)
		return errResult(fmt.Sprintf("rename: %v", err)), nil
	}

	out := map[string]any{
		"path":           canonical,
		"hunks_applied":  applied,
		"new_file":       isNewFile,
		"new_size_bytes": len(updated),
	}
	b, _ := json.Marshal(out)
	return core.OKResult(string(b)), nil
}

// runAppend is the plain-text append write path. Reads
// the existing file (or treats missing as empty for first-write),
// concatenates the new content, writes back atomically via the same
// temp+rename pattern the diff path uses — keeps the atomicity
// contract uniform across both modes.
//
// CRLF preservation mirrors the diff path: if the existing file uses
// CRLF, the appended content's bare LFs are upgraded to CRLF on
// write so the file stays consistent for whoever opens it next.
//
// Why read-then-rewrite instead of O_APPEND open: the diff path
// already does this and admins / future readers expect uniform
// "patch is atomic, never leaves a half-file" semantics. O_APPEND
// would also work but would diverge the contract for one mode.
func runAppend(canonical, content string) (core.ToolResult, error) {
	var (
		existing []byte
		newFile  bool
		origMode = os.FileMode(0o644)
	)
	if data, rErr := os.ReadFile(canonical); rErr == nil {
		existing = data
		if fi, sErr := os.Stat(canonical); sErr == nil {
			origMode = fi.Mode().Perm()
		}
	} else if os.IsNotExist(rErr) {
		newFile = true
	} else {
		return errResult(fmt.Sprintf("read %s: %v", canonical, rErr)), nil
	}

	hadCRLF := false
	if i := bytes.IndexByte(existing, '\n'); i > 0 && existing[i-1] == '\r' {
		hadCRLF = true
		existing = bytes.ReplaceAll(existing, []byte("\r\n"), []byte("\n"))
	}
	// Normalise the appended payload to LF FIRST. Without this, a
	// Windows-paste payload containing `\r\n` lands in `updated` as
	// raw `\r\n`; the final `\n → \r\n` upgrade then double-encodes
	// those into `\r\r\n` (broken mid-file CRLF). Mirrors the
	// normalize-then-restore discipline applied to `existing` above.
	normContent := strings.ReplaceAll(content, "\r\n", "\n")
	updated := append(existing, []byte(normContent)...)
	if hadCRLF {
		updated = bytes.ReplaceAll(updated, []byte("\n"), []byte("\r\n"))
	}

	if newFile {
		if err := os.MkdirAll(filepath.Dir(canonical), 0o750); err != nil {
			return errResult(fmt.Sprintf("create parent dir: %v", err)), nil
		}
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(canonical), filepath.Base(canonical)+".bobpatch.*")
	if err != nil {
		return errResult(fmt.Sprintf("create temp: %v", err)), nil
	}
	tmp := tmpFile.Name()
	if _, err := tmpFile.Write(updated); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return errResult(fmt.Sprintf("write temp: %v", err)), nil
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return errResult(fmt.Sprintf("close temp: %v", err)), nil
	}
	if err := os.Chmod(tmp, origMode); err != nil {
		_ = os.Remove(tmp)
		return errResult(fmt.Sprintf("chmod temp: %v", err)), nil
	}
	if err := os.Rename(tmp, canonical); err != nil {
		_ = os.Remove(tmp)
		return errResult(fmt.Sprintf("rename: %v", err)), nil
	}

	out := map[string]any{
		"path":           canonical,
		"mode":           "append",
		"new_file":       newFile,
		"appended_bytes": len(content),
		"new_size_bytes": len(updated),
	}
	b, _ := json.Marshal(out)
	return core.OKResult(string(b),
		"文件已写入 sandbox。若这是要交给用户的产出，先用 deliver_file 发出、再标 todo 完成——否则你标完成时用户还没拿到文件。短 markdown 直接普通回复即可，不必走文件。"), nil
}

// hunk is one diff hunk parsed out of a unified-diff stream.
type hunk struct {
	origStart int // 1-indexed; 0 means new file
	origLines int
	newStart  int
	newLines  int
	body      []string // lines without their leading ' '/'-'/'+' marker; sigils kept in `sigils`
	sigils    []byte   // one byte per body line
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parsePatchHunks parses a unified diff (one file). Header lines (e.g.
// "--- a/foo", "+++ b/foo", "diff --git ...", "index ...") are skipped
// until the first "@@". A second "+++" with /dev/null in v1 is rejected
// (no deletes). Multi-file diffs (a second "--- " after hunks) are
// rejected — caller should send one file at a time.
func parsePatchHunks(diff string) ([]hunk, error) {
	lines := strings.Split(diff, "\n")
	var hunks []hunk
	var cur *hunk
	sawFirstFile := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		// Multi-file detection: a "--- " after we already have hunks → multi.
		if strings.HasPrefix(line, "--- ") {
			if sawFirstFile && len(hunks) > 0 {
				return nil, fmt.Errorf("multi-file diffs not supported in v1; send one file at a time")
			}
			sawFirstFile = true
			cur = nil
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			if strings.Contains(line, "/dev/null") {
				return nil, fmt.Errorf("file deletion (+++ /dev/null) not supported in v1; use a different mechanism if you really mean to delete")
			}
			continue
		}
		if strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "new file mode ") || strings.HasPrefix(line, "old mode ") || strings.HasPrefix(line, "new mode ") {
			continue
		}
		if m := hunkHeaderRe.FindStringSubmatch(line); m != nil {
			h := hunk{}
			h.origStart, _ = strconv.Atoi(m[1])
			if m[2] != "" {
				h.origLines, _ = strconv.Atoi(m[2])
			} else {
				h.origLines = 1
			}
			h.newStart, _ = strconv.Atoi(m[3])
			if m[4] != "" {
				h.newLines, _ = strconv.Atoi(m[4])
			} else {
				h.newLines = 1
			}
			hunks = append(hunks, h)
			cur = &hunks[len(hunks)-1]
			continue
		}
		if cur == nil {
			// Pre-first-hunk junk lines (or ones we don't recognize) — skip.
			continue
		}
		// Hunk body line. First byte is ' ', '-', '+', or '\' (no-newline marker).
		// An empty line ends the hunk — diff producers always emit a leading
		// space for blank context lines (git, diff -u). Trailing "" from
		// strings.Split is the diff string's terminator, not a body line.
		if line == "" {
			cur = nil
			continue
		}
		// Stop parsing more body lines if the hunk's quota is met. Some diff
		// producers append trailing junk after the hunk; this prevents it from
		// leaking in.
		if cur != nil && hunkBodyComplete(*cur) {
			cur = nil
		}
		if cur == nil {
			continue
		}
		switch line[0] {
		case ' ', '-', '+':
			cur.body = append(cur.body, line[1:])
			cur.sigils = append(cur.sigils, line[0])
		case '\\':
			// "\ No newline at end of file" — informational; ignore.
			continue
		default:
			// Unknown sigil — assume hunk ended.
			cur = nil
		}
	}
	return hunks, nil
}

// hunkBodyComplete reports whether we've already seen enough body lines to
// satisfy the @@ header's origLines + newLines counts. Used to stop reading
// at the boundary so trailing diff junk doesn't bleed in.
func hunkBodyComplete(h hunk) bool {
	origCount, newCount := 0, 0
	for _, sig := range h.sigils {
		switch sig {
		case ' ':
			origCount++
			newCount++
		case '-':
			origCount++
		case '+':
			newCount++
		}
	}
	return origCount >= h.origLines && newCount >= h.newLines
}

// applyHunks applies all hunks to existing in order, strict context match.
// Returns (updated, hunks_applied, error).
//
// File line semantics: we split on "\n"; if existing ends with "\n" the
// last element is "" (empty trailing element) which we preserve and re-join
// with "\n". If existing does NOT end with "\n", the last element is the
// last partial line. We try to preserve the original trailing-newline shape.
func applyHunks(existing []byte, hunks []hunk) ([]byte, int, error) {
	var fileLines []string
	hadTrailingNewline := false
	if len(existing) > 0 {
		s := string(existing)
		hadTrailingNewline = strings.HasSuffix(s, "\n")
		if hadTrailingNewline {
			s = strings.TrimSuffix(s, "\n")
		}
		fileLines = strings.Split(s, "\n")
	} else {
		hadTrailingNewline = true // new file: end with newline by convention
	}

	offset := 0
	applied := 0
	for hi, h := range hunks {
		// New-file hunk
		if h.origStart == 0 && h.origLines == 0 {
			if hi != 0 {
				return nil, applied, fmt.Errorf("hunk %d is new-file (@@ -0,0) but it's not the first hunk", hi+1)
			}
			if len(fileLines) > 0 {
				return nil, applied, fmt.Errorf("new-file hunk @@ -0,0 but file is not empty")
			}
			for i, sig := range h.sigils {
				if sig != '+' {
					return nil, applied, fmt.Errorf("new-file hunk has a non-'+' line: body[%d]=%q", i, h.body[i])
				}
				fileLines = append(fileLines, h.body[i])
			}
			applied++
			continue
		}

		// Locate the hunk's window in fileLines.
		origIdx := h.origStart - 1 + offset // 0-indexed
		if origIdx < 0 {
			return nil, applied, fmt.Errorf("hunk %d: orig_start %d would be before file start", hi+1, h.origStart)
		}

		// Build expected original lines (context + removed) and new lines (context + added).
		var expected, replacement []string
		for i, sig := range h.sigils {
			switch sig {
			case ' ':
				expected = append(expected, h.body[i])
				replacement = append(replacement, h.body[i])
			case '-':
				expected = append(expected, h.body[i])
			case '+':
				replacement = append(replacement, h.body[i])
			}
		}

		if origIdx+len(expected) > len(fileLines) {
			return nil, applied, fmt.Errorf("hunk %d: expected %d lines starting at %d, but file only has %d lines", hi+1, len(expected), h.origStart, len(fileLines))
		}
		actual := fileLines[origIdx : origIdx+len(expected)]
		for i := range expected {
			if actual[i] != expected[i] {
				return nil, applied, fmt.Errorf("hunk %d: context mismatch at file line %d: expected %q, got %q", hi+1, h.origStart+i, expected[i], actual[i])
			}
		}

		// Splice
		next := make([]string, 0, len(fileLines)-len(expected)+len(replacement))
		next = append(next, fileLines[:origIdx]...)
		next = append(next, replacement...)
		next = append(next, fileLines[origIdx+len(expected):]...)
		fileLines = next
		offset += len(replacement) - len(expected)
		applied++
	}

	out := strings.Join(fileLines, "\n")
	if hadTrailingNewline && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return []byte(out), applied, nil
}
