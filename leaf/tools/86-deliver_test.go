package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbob/contract"
)

// realFileChannel is a minimal real-fs FileChannel for the deliver test: AbsPath
// resolves under root (refusing escape) + stats, mirroring localFileChannel. Only
// the bits deliver_file touches are implemented.
type realFileChannel struct{ root string }

func (*realFileChannel) Alive() bool  { return true }
func (*realFileChannel) Close() error { return nil }
func (*realFileChannel) List(context.Context, string) ([]contract.FileEntry, error) {
	return nil, nil
}
func (c *realFileChannel) Read(_ context.Context, path string) ([]byte, error) {
	abs, err := c.AbsPath(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}
func (*realFileChannel) Write(context.Context, string, []byte) error  { return nil }
func (*realFileChannel) Mkdir(context.Context, string) error          { return nil }
func (*realFileChannel) Remove(context.Context, string) error         { return nil }
func (*realFileChannel) Rename(context.Context, string, string) error { return nil }
func (c *realFileChannel) AbsPath(path string) (string, error) {
	clean := filepath.Clean("/" + path)
	abs := filepath.Join(c.root, clean)
	if abs != c.root && !strings.HasPrefix(abs, c.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the space", path)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	return abs, nil
}

type fileOpener struct{ ch contract.FileChannel }

func (o fileOpener) OpenFile(context.Context, string) (contract.FileChannel, error) {
	return o.ch, nil
}
func (fileOpener) OpenExec(context.Context, string) (contract.ExecChannel, error) {
	return nil, fmt.Errorf("no exec")
}

// deliverSink captures the SendFile args (and the other Sink methods no-op).
type deliverSink struct {
	path, caption string
	called        bool
	err           error
}

func (*deliverSink) ContentDelta(string) {}
func (*deliverSink) TraceDelta(string)   {}
func (*deliverSink) Finish(string) error { return nil }
func (s *deliverSink) SendFile(path, caption string) error {
	s.called, s.path, s.caption = true, path, caption
	return s.err
}
func (*deliverSink) LastSent() string { return "" }

func TestDeliverFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// happy path: file resolves, sink gets the abs path + caption.
	sink := &deliverSink{}
	tc := contract.ToolContext{Channels: fileOpener{ch: &realFileChannel{root: dir}}, Sink: sink}
	r := (deliverFile{}).Run(ctx, tc, json.RawMessage(`{"path":"report.txt","caption":"看这个"}`))
	if !r.OK {
		t.Fatalf("deliver = %+v, want OK", r)
	}
	if !sink.called || sink.caption != "看这个" || !strings.HasSuffix(sink.path, "report.txt") {
		t.Fatalf("sink got path=%q caption=%q called=%v", sink.path, sink.caption, sink.called)
	}

	// missing file → error result, no send.
	sink2 := &deliverSink{}
	tc2 := contract.ToolContext{Channels: fileOpener{ch: &realFileChannel{root: dir}}, Sink: sink2}
	r = (deliverFile{}).Run(ctx, tc2, json.RawMessage(`{"path":"nope.txt"}`))
	if r.OK || sink2.called {
		t.Fatalf("missing file should error without sending: %+v called=%v", r, sink2.called)
	}

	// path escape → error.
	r = (deliverFile{}).Run(ctx, tc2, json.RawMessage(`{"path":"../secret"}`))
	if r.OK {
		t.Fatalf("escape should error, got %+v", r)
	}

	// nil sink → error.
	r = (deliverFile{}).Run(ctx, contract.ToolContext{Channels: fileOpener{ch: &realFileChannel{root: dir}}}, json.RawMessage(`{"path":"report.txt"}`))
	if r.OK {
		t.Fatalf("nil sink should error, got %+v", r)
	}

	// sink rejects → error surfaced.
	sink3 := &deliverSink{err: fmt.Errorf("platform 413")}
	tc3 := contract.ToolContext{Channels: fileOpener{ch: &realFileChannel{root: dir}}, Sink: sink3}
	r = (deliverFile{}).Run(ctx, tc3, json.RawMessage(`{"path":"report.txt"}`))
	if r.OK || !strings.Contains(r.Error, "413") {
		t.Fatalf("send failure should surface: %+v", r)
	}
}

func TestTruncateRunesEllipsis(t *testing.T) {
	if got := truncateRunesEllipsis("héllo", 3); got != "hél…" {
		t.Fatalf("truncate = %q, want hél…", got)
	}
	if got := truncateRunesEllipsis("hi", 10); got != "hi" {
		t.Fatalf("short = %q, want hi", got)
	}
	if got := truncateRunesEllipsis("hi", 0); got != "" {
		t.Fatalf("zero max = %q, want empty", got)
	}
}
