package file

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/core"
	"agentbob/sidecars/browser/tools/policy"
)

const searchFilesParams = `{
  "type": "object",
  "properties": {
    "root": {
      "type": "string",
      "description": "搜索根目录（相对路径按工作目录解析；也可绝对或 ~）。必须在允许 root 内。"
    },
    "pattern": {
      "type": "string",
      "description": "kind=filename：glob，匹配 basename（如 *.go、test_*）。kind=content：RE2 正则，按行匹配。kind=both（默认）：两者都试。"
    },
    "kind": {
      "type": "string",
      "enum": ["filename", "content", "both"],
      "description": "匹配模式。默认 'both'。"
    },
    "max_hits": {
      "type": "integer",
      "description": "返回命中数上限。服务端硬上限 1000，默认 100。",
      "minimum": 1,
      "maximum": 1000
    }
  },
  "required": ["root", "pattern"]
}`

const (
	searchDefaultMaxHits = 100
	searchHardMaxHits    = 1000
	searchMaxDepth       = 10
	searchMaxFilesScan   = 10000
)

type searchFiles struct {
	cfg            config.FilesystemConfig
	pol            *policy.Policy
	namespaceByBot bool
}

// SearchFiles constructs the search_files tool. pol is the shared
// D-residual gate — searches with a root under a banned tree refuse
// outright, with no descent.
func SearchFiles(cfg config.FilesystemConfig, pol *policy.Policy, namespaceByBot bool) core.Tool {
	return searchFiles{cfg: cfg, pol: pol, namespaceByBot: namespaceByBot}
}

func (searchFiles) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "search_files",
		Description: "在目录树里按文件名（glob）或内容（regex）搜文件。深度上限 10，扫描上限 10000 文件（超过返回 truncated:true）。自动跳过 .git / node_modules / __pycache__ 等噪声目录。",
		Parameters:  json.RawMessage(searchFilesParams),
	}
}

func (searchFiles) Tier() core.Tier { return core.TierUser }
func (searchFiles) Serialize() bool { return false }

func (s searchFiles) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Root    string `json:"root"`
		Pattern string `json:"pattern"`
		Kind    string `json:"kind"`
		MaxHits int    `json:"max_hits"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("search_files: invalid arguments: %w", err)
	}
	if strings.TrimSpace(p.Root) == "" || strings.TrimSpace(p.Pattern) == "" {
		return core.ToolResult{}, fmt.Errorf("search_files: root and pattern are required")
	}
	if p.Kind == "" {
		p.Kind = "both"
	}
	if p.Kind != "filename" && p.Kind != "content" && p.Kind != "both" {
		return core.ToolResult{}, fmt.Errorf("search_files: kind must be filename, content, or both")
	}
	// T9 fix: `**` recursive glob is not supported by Go's
	// filepath.Match (which is single-level glob). Pre-fix, a model
	// passing `**/*.go` would get 0 hits with no explanation — the
	// pattern matched no basename. Now we reject explicitly so the
	// model knows what to use instead. The tool already walks
	// recursively; the user just sets pattern=`*.go` and the walk
	// finds matches at any depth.
	if (p.Kind == "filename" || p.Kind == "both") && strings.Contains(p.Pattern, "**") {
		return errResult(
			"pattern uses ** (recursive glob), which isn't supported. " +
				"The tool already walks recursively — pass plain basename glob like '*.go' and it will match at any depth."), nil
	}
	maxHits := p.MaxHits
	if maxHits <= 0 {
		maxHits = searchDefaultMaxHits
	}
	if maxHits > searchHardMaxHits {
		maxHits = searchHardMaxHits
	}

	rootCanonical, err := ResolvePath(s.cfg, s.namespaceByBot, actx, p.Root, ModeRead)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// D-residual: if the resolved root is under a banned tree, refuse
	// before descending — don't even open the walk.
	if ok, refusal := policy.EnforcePath(ctx, s.pol, actx, "search_files", rootCanonical, rootCanonical); !ok {
		return refusal, nil
	}
	rstat, err := os.Stat(rootCanonical)
	if err != nil {
		return errResult(fmt.Sprintf("stat %s: %v", rootCanonical, err)), nil
	}
	if !rstat.IsDir() {
		return errResult(fmt.Sprintf("%s is not a directory", rootCanonical)), nil
	}

	// Pre-compile content regex once if needed.
	var contentRe *regexp.Regexp
	if p.Kind == "content" || p.Kind == "both" {
		if re, err := regexp.Compile(p.Pattern); err != nil {
			if p.Kind == "content" {
				return errResult(fmt.Sprintf("invalid regex %q: %v", p.Pattern, err)), nil
			}
			// 'both' mode: invalid regex → treat as filename-only.
			contentRe = nil
		} else {
			contentRe = re
		}
	}

	skip := map[string]bool{}
	for _, d := range s.cfg.SearchSkipDirsEff() {
		skip[d] = true
	}

	type hit struct {
		Path       string `json:"path"`
		LineNumber int    `json:"line_number,omitempty"`
		Line       string `json:"line,omitempty"`
		Snippet    string `json:"snippet,omitempty"`
		MatchKind  string `json:"match_kind"` // "filename" | "content"
	}
	var hits []hit
	filesScanned := 0
	truncated := false
	rootSep := strings.Count(rootCanonical, string(filepath.Separator))

	walkErr := filepath.WalkDir(rootCanonical, func(path string, d fs.DirEntry, walkInErr error) error {
		if walkInErr != nil {
			return nil // skip unreadable subtree
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if filesScanned >= searchMaxFilesScan {
			truncated = true
			return filepath.SkipAll
		}
		if len(hits) >= maxHits {
			return filepath.SkipAll
		}

		// Depth limit
		depth := strings.Count(path, string(filepath.Separator)) - rootSep
		if depth > searchMaxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		base := d.Name()
		if d.IsDir() {
			if skip[base] {
				return filepath.SkipDir
			}
			return nil
		}

		// Symlink-escape defense (mirrors read_file's per-file EvalSymlinks
		// containment). WalkDir does NOT follow directory symlinks, but it
		// DOES report symlinked FILES as entries — and scanContent /
		// IsBinary open them with os.Open, which follows the link. A model
		// can `ln -s /etc/passwd leak.txt` inside an allowed root (ln is
		// not policy-blocked) and have its content scanned + returned.
		// Resolve the real target and require it to stay under the (already
		// policy-validated) search root; skip the entry if it escapes.
		if d.Type()&fs.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(path)
			if err != nil || !isUnder(real, rootCanonical) {
				return nil // dangling or escapes the sandbox — never scan
			}
			// A symlink pointing at a directory inside the root: WalkDir
			// won't descend it (it reports the symlink, not the tree), and
			// we don't want to open it as a file either. Skip.
			if fi, statErr := os.Stat(real); statErr != nil || fi.IsDir() {
				return nil
			}
		}

		filesScanned++

		// kind=filename / both: glob against basename.
		if p.Kind == "filename" || p.Kind == "both" {
			if ok, _ := filepath.Match(p.Pattern, base); ok {
				hits = append(hits, hit{Path: path, MatchKind: "filename"})
				if len(hits) >= maxHits {
					return filepath.SkipAll
				}
			}
		}

		// kind=content / both: line scan with the regex (skip binary).
		if (p.Kind == "content" || p.Kind == "both") && contentRe != nil {
			if bin, _ := IsBinary(path); bin {
				return nil
			}
			if h, incomplete, err := scanContent(path, contentRe, maxHits-len(hits)); err == nil {
				if incomplete {
					truncated = true
				}
				for _, m := range h {
					hits = append(hits, hit{
						Path:       path,
						LineNumber: m.lineNumber,
						Line:       m.line,
						Snippet:    m.snippet,
						MatchKind:  "content",
					})
					if len(hits) >= maxHits {
						return filepath.SkipAll
					}
				}
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll && ctx.Err() == nil {
		return errResult(fmt.Sprintf("walk %s: %v", rootCanonical, walkErr)), nil
	}

	if filesScanned >= searchMaxFilesScan && len(hits) < maxHits {
		truncated = true
	}

	out := map[string]any{
		"hits":          hits,
		"truncated":     truncated,
		"files_scanned": filesScanned,
	}
	b, err := json.Marshal(out)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("search_files: marshal result: %w", err)
	}
	return core.OKResult(string(b)), nil
}

type contentMatch struct {
	lineNumber int
	line       string
	snippet    string
}

// scanContent reads `path` line-by-line and returns at most `cap` lines
// matching `re`. Caps line length to keep snippets reasonable. The returned
// `incomplete` is true when the scan stopped early because a single line
// exceeded maxLineBytes (bufio.ErrTooLong) — that line and everything after it
// went unscanned, so the caller must not report the content search as complete.
func scanContent(path string, re *regexp.Regexp, cap int) (hits []contentMatch, incomplete bool, err error) {
	if cap <= 0 {
		return nil, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	// maxLineBytes is the real per-line ceiling: a single line above it trips
	// bufio.ErrTooLong and stops the scan. The initial buffer must not exceed
	// it, or the documented limit is dead — bufio.Scanner only enforces `max`
	// once a line outgrows the current buffer, so a larger initial buffer
	// silently raises the effective cap and pushes detection past this value.
	const maxLineBytes = 64 * 1024
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, maxLineBytes), maxLineBytes)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if !re.MatchString(line) {
			continue
		}
		snippet := line
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		hits = append(hits, contentMatch{lineNumber: lineNo, line: line, snippet: snippet})
		if len(hits) >= cap {
			return hits, false, nil
		}
	}
	// Scanner stops silently on bufio.ErrTooLong (a line over maxLineBytes);
	// flag it so the result carries truncated:true rather than masquerading as
	// a complete scan of a minified/single-line file.
	if errors.Is(sc.Err(), bufio.ErrTooLong) {
		incomplete = true
	}
	return hits, incomplete, nil
}
