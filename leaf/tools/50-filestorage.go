package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"agentbob/contract"
)

const (
	maxFileRead      = 16 * 1024  // cat: model-visible read cap (large reads blow context)
	findHitCap       = 500        // find: max paths returned
	grepHitCap       = 200        // grep: max matching lines returned
	grepMaxFileBytes = 256 * 1024 // grep: skip files larger than this
)

// fsTool is the file tool over a SPACE, using coreutils command NAMES as its verbs
// (ls/cat/find/grep/mkdir/rm/mv/write) so the model needs no teaching — it already
// knows these commands. They run over the structured FileChannel (backend-agnostic:
// local fs / sftp), NEVER a shell: find/grep are implemented by WALKING the channel,
// so they work even on a shell-less SFTP backend, and write takes clean bytes (no
// shell-quoting hell). space="" → the flow's default space.
type fsTool struct{}

func (fsTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name:        "fs",
		Description: "共享文件空间里的文件命令（coreutils 风格）：cmd=ls/cat/find/grep/mkdir/rm/mv/write。space 留空=默认空间；path 是空间内相对路径。",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"cmd":{"type":"string","enum":["ls","cat","find","grep","mkdir","rm","mv","write"],"description":"要执行的命令"},` +
			`"space":{"type":"string","description":"空间名，留空=默认空间"},` +
			`"path":{"type":"string","description":"空间内相对路径：ls/cat/mkdir/rm 的目标；find/grep 的搜索根（留空=根）；mv 的源"},` +
			`"name":{"type":"string","description":"[find] 文件名匹配：glob（如 *.go）或子串"},` +
			`"pattern":{"type":"string","description":"[grep] 要搜的内容（子串）"},` +
			`"ignore_case":{"type":"boolean","description":"[grep] 忽略大小写"},` +
			`"content":{"type":"string","description":"[write] 写入的内容"},` +
			`"dst":{"type":"string","description":"[mv] 目标路径"}` +
			`},"required":["cmd"]}`),
	}
}

func (fsTool) Serialize() bool { return false }

func (fsTool) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	if tc.Channels == nil {
		return contract.ErrResult("file channels are not available in this turn")
	}
	var p struct {
		Cmd, Space, Path, Name, Pattern, Content, Dst string
		IgnoreCase                                    bool `json:"ignore_case"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return contract.ErrResult("bad arguments: " + err.Error())
	}
	ch, err := tc.Channels.OpenFile(ctx, p.Space)
	if err != nil {
		return contract.ErrResult("open space: " + err.Error())
	}
	defer ch.Close()

	switch p.Cmd {
	case "ls":
		entries, err := ch.List(ctx, p.Path)
		if err != nil {
			return contract.ErrResult("ls: " + err.Error())
		}
		raw, _ := json.Marshal(entries)
		return contract.OKResult(string(raw))
	case "cat":
		data, err := ch.Read(ctx, p.Path)
		if err != nil {
			return contract.ErrResult("cat: " + err.Error())
		}
		if len(data) > maxFileRead {
			return contract.OKResult(string(data[:maxFileRead]) +
				fmt.Sprintf("\n…[truncated — %d of %d bytes]", maxFileRead, len(data)))
		}
		return contract.OKResult(string(data))
	case "find":
		return fsFind(ctx, ch, p.Path, p.Name)
	case "grep":
		if strings.TrimSpace(p.Pattern) == "" {
			return contract.ErrResult("grep needs a 'pattern'")
		}
		return fsGrep(ctx, ch, p.Path, p.Pattern, p.IgnoreCase)
	case "mkdir":
		if err := ch.Mkdir(ctx, p.Path); err != nil {
			return contract.ErrResult("mkdir: " + err.Error())
		}
		return contract.OKResult("created " + p.Path)
	case "rm":
		if err := ch.Remove(ctx, p.Path); err != nil {
			return contract.ErrResult("rm: " + err.Error())
		}
		return contract.OKResult("removed " + p.Path)
	case "mv":
		if strings.TrimSpace(p.Dst) == "" {
			return contract.ErrResult("mv needs a 'dst' path")
		}
		if err := ch.Rename(ctx, p.Path, p.Dst); err != nil {
			return contract.ErrResult("mv: " + err.Error())
		}
		return contract.OKResult(fmt.Sprintf("moved %s → %s", p.Path, p.Dst))
	case "write":
		if err := ch.Write(ctx, p.Path, []byte(p.Content)); err != nil {
			return contract.ErrResult("write: " + err.Error())
		}
		return contract.OKResult(fmt.Sprintf("wrote %d bytes to %s", len(p.Content), p.Path))
	default:
		return contract.ErrResult("unknown cmd: " + p.Cmd).WithHint("cmd ∈ ls/cat/find/grep/mkdir/rm/mv/write")
	}
}

// errStopWalk halts fsWalk early once a cap is hit (not a real error).
var errStopWalk = fmt.Errorf("stop walk")

// fsMaxDepth is a hard backstop on recursion depth — independent of the per-result
// hit caps — so an arbitrary FileChannel backend (a symlink cycle, a pathologically
// deep tree) can't spin the walk forever. Hitting it stops descending and returns a
// PARTIAL result, not an error.
const fsMaxDepth = 64

// fsWalk recursively visits every FILE under root (space-relative paths) via the
// FileChannel — no shell, so it works on a shell-less SFTP backend. onFile may
// return errStopWalk to halt early.
func fsWalk(ctx context.Context, ch contract.FileChannel, root string, onFile func(rel string) error) error {
	return fsWalkAt(ctx, ch, root, 0, onFile)
}

func fsWalkAt(ctx context.Context, ch contract.FileChannel, root string, depth int, onFile func(rel string) error) error {
	if depth > fsMaxDepth {
		return nil // backstop against cycles / pathological depth
	}
	entries, err := ch.List(ctx, root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		// Skip self/parent: SFTP / readdir-style backends list "." and "..", and
		// path.Join(root, ".") == root would recurse forever.
		if e.Name == "" || e.Name == "." || e.Name == ".." {
			continue
		}
		rel := e.Name
		if root != "" {
			rel = path.Join(root, e.Name)
		}
		if e.IsDir {
			if err := fsWalkAt(ctx, ch, rel, depth+1, onFile); err != nil {
				return err
			}
			continue
		}
		if err := onFile(rel); err != nil {
			return err
		}
	}
	return nil
}

// nameMatches honours find's -name semantics: glob (path.Match) when the pattern
// has glob metachars, else a forgiving substring match. Empty pattern matches all.
func nameMatches(pattern, base string) bool {
	if pattern == "" {
		return true
	}
	if strings.ContainsAny(pattern, "*?[") {
		ok, err := path.Match(pattern, base)
		return err == nil && ok
	}
	return strings.Contains(base, pattern)
}

// fsFind walks root and returns paths whose basename matches name.
func fsFind(ctx context.Context, ch contract.FileChannel, root, name string) contract.ToolResult {
	var hits []string
	truncated := false
	err := fsWalk(ctx, ch, root, func(rel string) error {
		if nameMatches(name, path.Base(rel)) {
			hits = append(hits, rel)
			if len(hits) >= findHitCap {
				truncated = true
				return errStopWalk
			}
		}
		return nil
	})
	if err != nil && err != errStopWalk {
		return contract.ErrResult("find: " + err.Error())
	}
	out := strings.Join(hits, "\n")
	if truncated {
		out += fmt.Sprintf("\n…[truncated — first %d matches]", findHitCap)
	}
	if out == "" {
		out = "(no matches)"
	}
	return contract.OKResult(out)
}

// fsGrep walks root, reading each (text, non-huge) file and emitting path:line:text
// for substring hits. Binary / oversized files are skipped.
func fsGrep(ctx context.Context, ch contract.FileChannel, root, pattern string, ignoreCase bool) contract.ToolResult {
	needle := pattern
	if ignoreCase {
		needle = strings.ToLower(needle)
	}
	var b strings.Builder
	hits := 0
	truncated := false
	err := fsWalk(ctx, ch, root, func(rel string) error {
		data, rerr := ch.Read(ctx, rel)
		if rerr != nil || len(data) > grepMaxFileBytes || bytes.IndexByte(data, 0) >= 0 {
			return nil // skip unreadable / too big / binary
		}
		for i, line := range strings.Split(string(data), "\n") {
			hay := line
			if ignoreCase {
				hay = strings.ToLower(hay)
			}
			if strings.Contains(hay, needle) {
				fmt.Fprintf(&b, "%s:%d:%s\n", rel, i+1, strings.TrimRight(line, "\r"))
				hits++
				if hits >= grepHitCap {
					truncated = true
					return errStopWalk
				}
			}
		}
		return nil
	})
	if err != nil && err != errStopWalk {
		return contract.ErrResult("grep: " + err.Error())
	}
	out := b.String()
	if truncated {
		out += fmt.Sprintf("…[truncated — first %d matches]\n", grepHitCap)
	}
	if out == "" {
		out = "(no matches)"
	}
	return contract.OKResult(out)
}
