package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentbob/contract"
)

// stubSkillSet is a minimal contract.SkillSet for skill_view tests.
type stubSkillSet struct {
	bodies map[string]string
	files  map[string][]contract.SkillFile
}

func (stubSkillSet) List() []contract.SkillInfo { return nil }
func (stubSkillSet) Get(string) (contract.Skill, bool) {
	return contract.Skill{}, false
}
func (s stubSkillSet) Read(name string) (string, bool) { b, ok := s.bodies[name]; return b, ok }
func (s stubSkillSet) Files(name string) ([]contract.SkillFile, error) {
	return s.files[name], nil
}

// fakeSVChannel / fakeSVOpener: an in-memory file channel for materialization tests.
type fakeSVChannel struct{ files map[string][]byte }

func (c *fakeSVChannel) Alive() bool  { return true }
func (c *fakeSVChannel) Close() error { return nil }
func (c *fakeSVChannel) List(context.Context, string) ([]contract.FileEntry, error) {
	return nil, nil
}
func (c *fakeSVChannel) Read(_ context.Context, p string) ([]byte, error) { return c.files[p], nil }
func (c *fakeSVChannel) Write(_ context.Context, p string, d []byte) error {
	c.files[p] = append([]byte(nil), d...)
	return nil
}
func (c *fakeSVChannel) Mkdir(context.Context, string) error          { return nil }
func (c *fakeSVChannel) Remove(context.Context, string) error         { return nil }
func (c *fakeSVChannel) Rename(context.Context, string, string) error { return nil }
func (c *fakeSVChannel) AbsPath(p string) (string, error)             { return p, nil }

type fakeSVOpener struct{ ch *fakeSVChannel }

func (o fakeSVOpener) OpenFile(context.Context, string) (contract.FileChannel, error) {
	return o.ch, nil
}
func (o fakeSVOpener) OpenExec(context.Context, string) (contract.ExecChannel, error) {
	return nil, nil
}

func TestSkillView(t *testing.T) {
	tool := skillView{}
	skills := stubSkillSet{bodies: map[string]string{"summarize": "## 摘要\n按要点压缩"}}
	tc := contract.ToolContext{Skills: skills}

	// Authorized skill → its body.
	r := tool.Run(context.Background(), tc, json.RawMessage(`{"name":"summarize"}`))
	if !r.OK || r.Data != "## 摘要\n按要点压缩" {
		t.Fatalf("view = %+v, want the body", r)
	}
	// Unknown/unauthorized name (not in the projected set) → error, never the body.
	if r := tool.Run(context.Background(), tc, json.RawMessage(`{"name":"secret"}`)); r.OK {
		t.Error("unauthorized skill must not return a body")
	}
	// No skills wired → graceful error.
	if r := tool.Run(context.Background(), contract.ToolContext{}, json.RawMessage(`{"name":"summarize"}`)); r.OK {
		t.Error("nil skills should error, not panic")
	}
	// Missing name → error.
	if r := tool.Run(context.Background(), tc, json.RawMessage(`{}`)); r.OK {
		t.Error("empty name should error")
	}
}

// TestSkillViewMaterializesFiles: viewing a script-bearing skill writes its files
// into the space under skills/<name>/ and notes it in the returned body.
func TestSkillViewMaterializesFiles(t *testing.T) {
	ch := &fakeSVChannel{files: map[string][]byte{}}
	skills := stubSkillSet{
		bodies: map[string]string{"arxiv": "# arxiv"},
		files: map[string][]contract.SkillFile{
			"arxiv": {{Rel: "scripts/search_arxiv.py", Data: []byte("print('hi')")}},
		},
	}
	tc := contract.ToolContext{Skills: skills, Channels: fakeSVOpener{ch: ch}}
	r := skillView{}.Run(context.Background(), tc, json.RawMessage(`{"name":"arxiv"}`))
	if !r.OK {
		t.Fatalf("view failed: %s", r.Error)
	}
	if got := string(ch.files["skills/arxiv/scripts/search_arxiv.py"]); got != "print('hi')" {
		t.Fatalf("script not materialized into the space: %q", got)
	}
	if !strings.Contains(r.Data, "skills/arxiv/") {
		t.Errorf("body should note where the scripts landed: %q", r.Data)
	}
}
