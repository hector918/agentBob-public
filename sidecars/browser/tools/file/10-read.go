package file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"agentbob/sidecars/browser/config"
	"agentbob/sidecars/browser/core"
	"agentbob/sidecars/browser/tools/policy"
)

const readFileParams = `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "文件路径（相对路径按工作目录解析；也可绝对或 ~）。必须落在允许 root 内。软链按真实路径校验，指向 root 外即拒绝。"
    },
    "max_bytes": {
      "type": "integer",
      "description": "可选的读取字节上限。服务端硬上限默认 1 MB。超大文件返回 truncated:true + 前 N 字节。",
      "minimum": 1
    }
  },
  "required": ["path"]
}`

type readFile struct {
	cfg            config.FilesystemConfig
	pol            *policy.Policy
	namespaceByBot bool
}

// ReadFile constructs the read_file tool. cfg supplies the path-allowlist
// and read-cap; namespaceByBot mirrors memory.namespace_by_bot so the
// session sandbox path stays consistent with the memory tree; pol is the
// shared D-residual gate — banned trees stay unreadable even if the
// filesystem allowlist would otherwise permit them.
func ReadFile(cfg config.FilesystemConfig, pol *policy.Policy, namespaceByBot bool) core.Tool {
	return readFile{cfg: cfg, pol: pol, namespaceByBot: namespaceByBot}
}

func (readFile) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:           "read_file",
		Description:    "读文件返回内容。路径可相对（按工作目录）或绝对 / ~，须在允许 root 内。返回 size_bytes、line_count、可能截断的 content。二进制文件（null-byte 检测）和目录被拒。",
		Parameters:     json.RawMessage(readFileParams),
		NoAutoCompress: true, // 模型要的是原文，不能被 summarize 掉
	}
}

func (readFile) Tier() core.Tier { return core.TierUser }
func (readFile) Serialize() bool { return false }

func (r readFile) Run(ctx context.Context, actx core.AgentCtx, args json.RawMessage) (core.ToolResult, error) {
	var p struct {
		Path     string `json:"path"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return core.ToolResult{}, fmt.Errorf("read_file: invalid arguments: %w", err)
	}
	if strings.TrimSpace(p.Path) == "" {
		return core.ToolResult{}, fmt.Errorf("read_file: path is required")
	}

	canonical, err := ResolvePath(r.cfg, r.namespaceByBot, actx, p.Path, ModeRead)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// D-residual: even within read_roots, certain trees should never be
	// exposed (~/.ssh keys, /etc/shadow if mounted, ...). Banned paths
	// refuse outright with no approval prompt.
	if ok, refusal := policy.EnforcePath(ctx, r.pol, actx, "read_file", canonical, canonical); !ok {
		return refusal, nil
	}

	stat, err := os.Stat(canonical)
	if err != nil {
		return errResult(fmt.Sprintf("stat %s: %v", canonical, err)), nil
	}
	if stat.IsDir() {
		return errResult(fmt.Sprintf("%s is a directory; use search_files to list its contents", canonical)), nil
	}

	binary, err := IsBinary(canonical)
	if err != nil {
		return errResult(fmt.Sprintf("read %s: %v", canonical, err)), nil
	}
	if binary {
		return errResult(fmt.Sprintf("%s is a binary file; not handled by read_file", canonical)), nil
	}

	cap := r.cfg.MaxReadBytesEff()
	if p.MaxBytes > 0 && p.MaxBytes < cap {
		cap = p.MaxBytes
	}

	body, truncated, err := readUpToN(canonical, int64(cap))
	if err != nil {
		return errResult(fmt.Sprintf("read %s: %v", canonical, err)), nil
	}

	out := map[string]any{
		"path":       canonical,
		"size_bytes": stat.Size(),
		"line_count": bytes.Count(body, []byte{'\n'}) + 1,
		"content":    string(body),
		"truncated":  truncated,
	}
	b, err := json.Marshal(out)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("read_file: marshal result: %w", err)
	}
	return core.OKResult(string(b)), nil
}

// readUpToN reads at most n bytes from path. Returns (body, truncated, err)
// where truncated=true when the file is larger than n.
//
// T8 fix: the prior single f.Read(buf) call may return a
// short read (especially on slow filesystems / network mounts) so
// large files would be returned partial without the truncated flag.
// io.ReadFull loops until buf is filled OR EOF — correct semantics.
// EOF + n < len(buf) means "file smaller than n"; not an error.
func readUpToN(path string, n int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	// R5A-14: stat first and shrink n to the actual file
	// size when smaller. Avoids allocating an n-byte buffer for a 1KB
	// file just because n is set to (say) 64 MB — the prior version
	// always make([]byte, n).
	if fi, statErr := f.Stat(); statErr == nil && fi.Size() < n {
		n = fi.Size()
	}
	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, false, err
	}
	body := buf[:read]
	// Check if there's MORE. If ReadFull returned ErrUnexpectedEOF it
	// means we hit EOF before filling buf — no more to read.
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return body, false, nil
	}
	one := make([]byte, 1)
	more, _ := f.Read(one)
	return body, more > 0, nil
}

// errResult is a local alias for the structured error result. Keeps the
// many call sites readable; delegates to core.ErrResult so the shape is
// uniform across packages.
func errResult(msg string) core.ToolResult { return core.ErrResult(msg) }
