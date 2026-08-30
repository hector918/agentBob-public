package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agentbob/contract"
)

// memFile is an in-memory FileChannel for testing the file tool's verb dispatch.
type memFile struct{ files map[string][]byte }

func (*memFile) Alive() bool  { return true }
func (*memFile) Close() error { return nil }
func (m *memFile) List(context.Context, string) ([]contract.FileEntry, error) {
	out := make([]contract.FileEntry, 0, len(m.files))
	for k := range m.files {
		out = append(out, contract.FileEntry{Name: k})
	}
	return out, nil
}
func (m *memFile) Read(_ context.Context, path string) ([]byte, error) {
	d, ok := m.files[path]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return d, nil
}
func (m *memFile) Write(_ context.Context, path string, data []byte) error {
	m.files[path] = data
	return nil
}
func (*memFile) Mkdir(context.Context, string) error { return nil }
func (m *memFile) Remove(_ context.Context, path string) error {
	delete(m.files, path)
	return nil
}
func (m *memFile) Rename(_ context.Context, o, n string) error {
	m.files[n] = m.files[o]
	delete(m.files, o)
	return nil
}

// AbsPath: the in-memory channel has no real fs path; return the path unchanged
// (errors if the "file" is absent) — enough for tests that exercise the deliver
// path's wiring rather than real attachment delivery.
func (m *memFile) AbsPath(path string) (string, error) {
	if _, ok := m.files[path]; !ok {
		return "", fmt.Errorf("memFile: no such file %q", path)
	}
	return path, nil
}

type memOpener struct{ ch contract.FileChannel }

func (o memOpener) OpenFile(context.Context, string) (contract.FileChannel, error) { return o.ch, nil }
func (memOpener) OpenExec(context.Context, string) (contract.ExecChannel, error) {
	return nil, fmt.Errorf("no exec in this test")
}

func TestFileTool(t *testing.T) {
	mf := &memFile{files: map[string][]byte{}}
	tc := contract.ToolContext{Channels: memOpener{ch: mf}}
	ctx := context.Background()
	run := func(args string) contract.ToolResult {
		return (fsTool{}).Run(ctx, tc, json.RawMessage(args))
	}

	// coreutils-named verbs: write / cat / mv.
	if r := run(`{"cmd":"write","path":"x.txt","content":"hi"}`); !r.OK {
		t.Fatalf("write: %+v", r)
	}
	if r := run(`{"cmd":"cat","path":"x.txt"}`); !r.OK || r.Data != "hi" {
		t.Fatalf("cat: %+v", r)
	}
	if r := run(`{"cmd":"mv","path":"x.txt","dst":"y.txt"}`); !r.OK {
		t.Fatalf("mv: %+v", r)
	}
	if r := run(`{"cmd":"cat","path":"y.txt"}`); !r.OK || r.Data != "hi" {
		t.Errorf("cat after mv: %+v", r)
	}
	if r := run(`{"cmd":"mv","path":"y.txt"}`); r.OK {
		t.Error("mv without dst should error")
	}

	// find: glob + substring on basenames.
	_ = run(`{"cmd":"write","path":"notes.md","content":"TODO: ship\nignore me"}`)
	if r := run(`{"cmd":"find","name":"*.md"}`); !r.OK || !contains(r.Data, "notes.md") || contains(r.Data, "y.txt") {
		t.Errorf("find *.md: %+v", r)
	}
	// grep: content match → path:line:text; ignore_case.
	if r := run(`{"cmd":"grep","pattern":"TODO"}`); !r.OK || !contains(r.Data, "notes.md:1:TODO: ship") {
		t.Errorf("grep TODO: %+v", r)
	}
	if r := run(`{"cmd":"grep","pattern":"todo","ignore_case":true}`); !r.OK || !contains(r.Data, "notes.md") {
		t.Errorf("grep -i todo: %+v", r)
	}
	if r := run(`{"cmd":"grep","pattern":"nomatch"}`); !r.OK || r.Data != "(no matches)" {
		t.Errorf("grep no match: %+v", r)
	}
	if r := run(`{"cmd":"grep"}`); r.OK {
		t.Error("grep without pattern should error")
	}

	if r := run(`{"cmd":"zap"}`); r.OK {
		t.Error("unknown cmd should error")
	}
	if r := (fsTool{}).Run(ctx, contract.ToolContext{}, json.RawMessage(`{"cmd":"cat","path":"x"}`)); r.OK {
		t.Error("nil channels should error")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
