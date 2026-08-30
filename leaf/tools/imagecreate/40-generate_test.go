package imagecreate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agentbob/contract"
)

// --- fakes ----------------------------------------------------------------

type fakeSink struct {
	mu     sync.Mutex
	files  []string
	traces []string
}

func (s *fakeSink) ContentDelta(string) {}
func (s *fakeSink) TraceDelta(t string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.traces = append(s.traces, t)
}
func (s *fakeSink) Finish(string) error { return nil }
func (s *fakeSink) LastSent() string    { return "" }
func (s *fakeSink) SendFile(path, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files = append(s.files, path)
	return nil
}

// fakeChannels is a real on-disk space rooted at a temp dir, so AbsPath returns a
// path SendFile could genuinely open.
type fakeChannels struct{ root string }

func (c *fakeChannels) OpenFile(context.Context, string) (contract.FileChannel, error) {
	return &fakeFileChannel{root: c.root}, nil
}
func (c *fakeChannels) OpenExec(context.Context, string) (contract.ExecChannel, error) {
	return nil, os.ErrInvalid
}

type fakeFileChannel struct{ root string }

func (f *fakeFileChannel) Alive() bool  { return true }
func (f *fakeFileChannel) Close() error { return nil }
func (f *fakeFileChannel) List(context.Context, string) ([]contract.FileEntry, error) {
	return nil, nil
}
func (f *fakeFileChannel) Read(_ context.Context, p string) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.root, p))
}
func (f *fakeFileChannel) Write(_ context.Context, p string, data []byte) error {
	return os.WriteFile(filepath.Join(f.root, p), data, 0o600)
}
func (f *fakeFileChannel) Mkdir(_ context.Context, p string) error {
	return os.MkdirAll(filepath.Join(f.root, p), 0o700)
}
func (f *fakeFileChannel) Remove(_ context.Context, p string) error {
	return os.Remove(filepath.Join(f.root, p))
}
func (f *fakeFileChannel) Rename(_ context.Context, a, b string) error {
	return os.Rename(filepath.Join(f.root, a), filepath.Join(f.root, b))
}
func (f *fakeFileChannel) AbsPath(p string) (string, error) {
	return filepath.Join(f.root, p), nil
}

// submitPool drives the progress hook the way the real provider does, emitting one
// submitted event per configured id — which is what a busy-retry or a failover
// looks like from the tool's side.
type submitPool struct {
	fakePool
	submitIDs []string
	err       error
}

func (p *submitPool) Chat(ctx context.Context, _ contract.ModelRequest, _ []contract.Message) (contract.ChatResponse, error) {
	report := contract.ImageProgressFrom(ctx)
	for _, id := range p.submitIDs {
		report(contract.ImageEvent{Stage: contract.ImageStageSubmitted, PromptID: id, Entry: "comfy-klein"})
	}
	report(contract.ImageEvent{Stage: contract.ImageStageProgress, Cur: 2, Max: 4})
	if p.err != nil {
		return contract.ChatResponse{}, p.err
	}
	reply, _ := json.Marshal(map[string]any{
		"image_b64": base64.StdEncoding.EncodeToString([]byte("\x89PNG-not-really")),
		"width":     768, "height": 768, "seconds": 20,
	})
	return contract.ChatResponse{Content: string(reply)}, nil
}

func generateFixture(t *testing.T, pool contract.ModelPool) (*Tool, *fakeSink, contract.ToolContext) {
	t.Helper()
	home, space := t.TempDir(), t.TempDir()
	tl := New(func() contract.ModelPool { return pool }, testCatalog, func() contract.Gateway { return nil }, home)
	sink := &fakeSink{}
	return tl, sink, contract.ToolContext{
		Sid: "sid-1", Scope: "telegram:dm:7",
		Sink: sink, Channels: &fakeChannels{root: space},
	}
}

// --- tests ----------------------------------------------------------------

func TestGenerateDeliversAndClearsWAL(t *testing.T) {
	pool := &submitPool{
		fakePool:  fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}},
		submitIDs: []string{"job-1"},
	}
	tl, sink, tc := generateFixture(t, pool)
	res := tl.Run(context.Background(), tc, json.RawMessage(`{"style":"comfyui-klein","prompt":"a car"}`))
	if !res.OK {
		t.Fatalf("res = %+v, want success", res)
	}
	if len(sink.files) != 1 {
		t.Fatalf("sent %d files, want 1", len(sink.files))
	}
	if _, err := os.Stat(sink.files[0]); err != nil {
		t.Errorf("delivered path is not a real file: %v", err)
	}
	// The receipt must not carry the image itself — a megabyte of base64 through the
	// transcript is exactly what the sink exists to avoid. The size bound is a
	// tripwire for THAT, sized with room for prose above it: a real payload is tens
	// of kilobytes, so anything in this range is wording. (It used to sit at 400
	// bytes, close enough to the then-current wording that a four-byte edit tripped
	// it — a prose budget wearing a payload guard's comment. The receipt does stay in
	// context for the whole session, being NoAutoCompress, but that cost is a
	// judgement call about what the model needs told, not something this asserts.)
	if strings.Contains(res.Data, "PNG") || len(res.Data) > 2000 {
		t.Errorf("receipt looks like it carries payload: %q", res.Data)
	}
	if n := len(tl.wal.list()); n != 0 {
		t.Errorf("%d wal records left after a delivered image", n)
	}
	if len(sink.traces) == 0 {
		t.Error("no progress lines were emitted")
	}
}

// Regression: pool.Chat may submit more than once for a single tool call (busy
// retry against the same entry, or failover to a peer). Every submission writes a
// record; keeping only the last id left the earlier ones on disk, and the sweep
// would deliver them again as duplicate images minutes later.
func TestGenerateClearsEveryRecordWhenTheCallResubmits(t *testing.T) {
	pool := &submitPool{
		fakePool:  fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}},
		submitIDs: []string{"job-1", "job-2", "job-3"},
	}
	tl, _, tc := generateFixture(t, pool)
	res := tl.Run(context.Background(), tc, json.RawMessage(`{"style":"comfyui-klein","prompt":"a car"}`))
	if !res.OK {
		t.Fatalf("res = %+v, want success", res)
	}
	if left := tl.wal.list(); len(left) != 0 {
		t.Errorf("%d orphaned wal records after a re-submitting call: %+v", len(left), left)
	}
}

// A failed generation leaves nothing behind either: the job is already known to
// have failed, so a surviving record would produce a bogus "it didn't make it".
func TestGenerateClearsRecordsOnFailure(t *testing.T) {
	pool := &submitPool{
		fakePool:  fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}},
		submitIDs: []string{"job-1", "job-2"},
		err:       context.Canceled,
	}
	tl, _, tc := generateFixture(t, pool)
	if res := tl.Run(context.Background(), tc, json.RawMessage(`{"style":"comfyui-klein","prompt":"x"}`)); res.OK {
		t.Fatal("a failed generation reported success")
	}
	if left := tl.wal.list(); len(left) != 0 {
		t.Errorf("%d wal records left after a failure: %+v", len(left), left)
	}
}

// The record has to be on disk BEFORE the call returns — that is what makes it a
// write-ahead log rather than a receipt.
func TestWALRecordIsWrittenAtSubmitTime(t *testing.T) {
	home, space := t.TempDir(), t.TempDir()
	var seen []walRecord
	pool := &probePool{
		fakePool: fakePool{entries: []contract.ModelInfo{imageEntry("comfy-klein", "live", "comfyui-klein")}},
		onSubmit: func() { seen = newWAL(home).list() },
	}
	tl := New(func() contract.ModelPool { return pool }, testCatalog, func() contract.Gateway { return nil }, home)
	tc := contract.ToolContext{Sid: "sid-1", Scope: "telegram:dm:7", Sink: &fakeSink{}, Channels: &fakeChannels{root: space}}
	tl.Run(context.Background(), tc, json.RawMessage(`{"style":"comfyui-klein","prompt":"x"}`))

	if len(seen) != 1 {
		t.Fatalf("saw %d records mid-call, want the job recorded before it could finish", len(seen))
	}
	if seen[0].Entry != "comfy-klein" {
		t.Errorf("Entry = %q — recovery cannot pin the backend without it", seen[0].Entry)
	}
	if seen[0].Scope != "telegram:dm:7" {
		t.Errorf("Scope = %q — recovery cannot find the chat without it", seen[0].Scope)
	}
}

// probePool runs onSubmit right after announcing the job, i.e. at the instant the
// record must already exist.
type probePool struct {
	fakePool
	onSubmit func()
}

func (p *probePool) Chat(ctx context.Context, _ contract.ModelRequest, _ []contract.Message) (contract.ChatResponse, error) {
	contract.ImageProgressFrom(ctx)(contract.ImageEvent{
		Stage: contract.ImageStageSubmitted, PromptID: "job-1", Entry: "comfy-klein",
	})
	p.onSubmit()
	reply, _ := json.Marshal(map[string]any{"image_b64": base64.StdEncoding.EncodeToString([]byte("x")), "width": 1, "height": 1})
	return contract.ChatResponse{Content: string(reply)}, nil
}

// Tags are a general routing facility. An operational tag on an image entry is not
// a 画风, and offering it would put a meaningless word in the catalog that routes
// to no variant.
func TestOperationalTagsAreNotOfferedAsStyles(t *testing.T) {
	pool := &fakePool{entries: []contract.ModelInfo{
		imageEntry("comfy-klein", "live", "comfyui-klein", "uncensored", "fallback"),
	}}
	caps := liveCapabilities(pool, testCatalog())
	if len(caps) != 1 || caps[0].Style != "comfyui-klein" {
		t.Fatalf("caps = %+v, want only the guide-described style", caps)
	}
}
