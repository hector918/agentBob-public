package browserremote

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbob/contract"
)

// --- fakes ---------------------------------------------------------------

// capChannel is a fake FileChannel that captures Write calls in-memory so a test
// can assert what browser_vision persisted (and where) without touching disk.
type capChannel struct{ writes map[string][]byte }

func (c *capChannel) Alive() bool  { return true }
func (c *capChannel) Close() error { return nil }
func (c *capChannel) List(context.Context, string) ([]contract.FileEntry, error) {
	return nil, nil
}
func (c *capChannel) Read(context.Context, string) ([]byte, error) { return nil, nil }
func (c *capChannel) Write(_ context.Context, path string, data []byte) error {
	if c.writes == nil {
		c.writes = map[string][]byte{}
	}
	c.writes[path] = data
	return nil
}
func (c *capChannel) Mkdir(context.Context, string) error          { return nil }
func (c *capChannel) Remove(context.Context, string) error         { return nil }
func (c *capChannel) Rename(context.Context, string, string) error { return nil }
func (c *capChannel) AbsPath(path string) (string, error)          { return "/space/" + path, nil }

type capOpener struct{ ch *capChannel }

func (o *capOpener) OpenFile(context.Context, string) (contract.FileChannel, error) {
	return o.ch, nil
}
func (o *capOpener) OpenExec(context.Context, string) (contract.ExecChannel, error) {
	return nil, nil
}

// visionStub is a browserd stub that answers /vision with a base64 PNG.
func visionStub(t *testing.T, png []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/vision") {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		data, _ := json.Marshal(visionData{ImageB64: base64.StdEncoding.EncodeToString(png), ByteCount: len(png)})
		_ = json.NewEncoder(w).Encode(Response{OK: true, Data: data})
	}))
}

// --- tests ---------------------------------------------------------------

// TestVision_AnswerAlsoSavesShot: on the model-answer success path, browser_vision
// must ALSO persist the screenshot and return its path — this is the fix for the
// dead loop where the model kept re-calling browser_vision waiting for a file path
// it never got. The path lets the caller hand the shot to deliver_file/read_file.
func TestVision_AnswerAlsoSavesShot(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nFAKE-PIXELS")
	srv := visionStub(t, png)
	defer srv.Close()

	vf := func(context.Context, string, []byte, string) (string, error) {
		return "页面上有一排原神周边商品。", nil
	}
	cap := &capChannel{}
	tool := NewTool(New(srv.URL, ""), nil, nil, vf)
	tool.(*browserTool).markNavigated("s1")

	res := tool.Run(context.Background(),
		contract.ToolContext{Sid: "s1", Channels: &capOpener{ch: cap}},
		json.RawMessage(`{"action":"browser_vision","question":"页面上有什么"}`))
	if !res.OK {
		t.Fatalf("vision should succeed, got %+v", res)
	}
	var out struct {
		Answer string `json:"answer"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal([]byte(res.Data), &out); err != nil {
		t.Fatalf("unmarshal result: %v (content=%q)", err, res.Data)
	}
	if out.Answer == "" {
		t.Errorf("answer must be present, got %q", res.Data)
	}
	if !strings.HasPrefix(out.Path, "screenshots/") {
		t.Fatalf("path must be under screenshots/, got %q", out.Path)
	}
	if got, ok := cap.writes[out.Path]; !ok {
		t.Fatalf("no write captured at returned path %q; writes=%v", out.Path, keys(cap.writes))
	} else if string(got) != string(png) {
		t.Errorf("persisted bytes != screenshot bytes")
	}
}

// TestVision_AnswerNoSpace_StillAnswers: with the answer path but NO file space
// wired, saving is best-effort — the answer must still return (path omitted), not
// fail the call.
func TestVision_AnswerNoSpace_StillAnswers(t *testing.T) {
	png := []byte("PNGBYTES")
	srv := visionStub(t, png)
	defer srv.Close()

	vf := func(context.Context, string, []byte, string) (string, error) { return "答案", nil }
	tool := NewTool(New(srv.URL, ""), nil, nil, vf)
	tool.(*browserTool).markNavigated("s2")

	res := tool.Run(context.Background(),
		contract.ToolContext{Sid: "s2"}, // Channels nil
		json.RawMessage(`{"action":"browser_vision"}`))
	if !res.OK {
		t.Fatalf("answer must survive a missing file space, got %+v", res)
	}
	if strings.Contains(res.Data, "screenshots/") {
		t.Errorf("no path should be returned without a file space, got %q", res.Data)
	}
}

// TestVision_NoModel_FallbackSavesShot: with no vision model wired, the shot save
// is the ONLY output — a missing file space is fatal (nothing else to return).
func TestVision_NoModel_FallbackSavesShot(t *testing.T) {
	png := []byte("PNGBYTES2")
	srv := visionStub(t, png)
	defer srv.Close()

	cap := &capChannel{}
	tool := NewTool(New(srv.URL, ""), nil, nil, nil) // nil vision → fallback
	tool.(*browserTool).markNavigated("s3")

	res := tool.Run(context.Background(),
		contract.ToolContext{Sid: "s3", Channels: &capOpener{ch: cap}},
		json.RawMessage(`{"action":"browser_vision"}`))
	if !res.OK {
		t.Fatalf("fallback save should succeed, got %+v", res)
	}
	if len(cap.writes) != 1 {
		t.Fatalf("expected exactly one screenshot write, got %d", len(cap.writes))
	}
}

// TestVision_EmptyPNG_NotPersisted: browserd returning an empty PNG (screenshot
// failed but HTTP 200) must NOT leave a 0-byte file nor hand back a path. On the
// answer path the save is best-effort so the answer still returns (no path); on the
// no-model fallback it's a hard error.
func TestVision_EmptyPNG_NotPersisted(t *testing.T) {
	srv := visionStub(t, nil) // empty PNG
	defer srv.Close()

	// answer path: answer returns, no path, nothing written
	vf := func(context.Context, string, []byte, string) (string, error) { return "答案", nil }
	cap := &capChannel{}
	tool := NewTool(New(srv.URL, ""), nil, nil, vf)
	tool.(*browserTool).markNavigated("s4")
	res := tool.Run(context.Background(),
		contract.ToolContext{Sid: "s4", Channels: &capOpener{ch: cap}},
		json.RawMessage(`{"action":"browser_vision"}`))
	if !res.OK {
		t.Fatalf("answer must still return on empty png, got %+v", res)
	}
	if strings.Contains(res.Data, "screenshots/") {
		t.Errorf("no path for empty png, got %q", res.Data)
	}
	if len(cap.writes) != 0 {
		t.Errorf("empty png must not be written, got %d writes", len(cap.writes))
	}

	// no-model fallback: empty png is a hard error, nothing written
	cap2 := &capChannel{}
	tool2 := NewTool(New(srv.URL, ""), nil, nil, nil)
	tool2.(*browserTool).markNavigated("s5")
	res2 := tool2.Run(context.Background(),
		contract.ToolContext{Sid: "s5", Channels: &capOpener{ch: cap2}},
		json.RawMessage(`{"action":"browser_vision"}`))
	if res2.OK {
		t.Fatalf("empty png with no model must error, got %+v", res2)
	}
	if len(cap2.writes) != 0 {
		t.Errorf("empty png must not be written, got %d writes", len(cap2.writes))
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
