package promptlib

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
)

// run invokes the tool with a JSON arg string and returns the result.
func run(t *testing.T, arg string) contract.ToolResult {
	t.Helper()
	return corpusImport{}.Run(context.Background(), contract.ToolContext{}, json.RawMessage(arg))
}

// fakeFileChannel serves one in-memory file for the file-reference path. Only Read
// is exercised; the rest satisfy the FileChannel interface.
type fakeFileChannel struct{ files map[string][]byte }

func (f fakeFileChannel) Alive() bool  { return true }
func (f fakeFileChannel) Close() error { return nil }
func (f fakeFileChannel) Read(_ context.Context, path string) ([]byte, error) {
	if b, ok := f.files[path]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}
func (f fakeFileChannel) List(context.Context, string) ([]contract.FileEntry, error) { return nil, nil }
func (f fakeFileChannel) Write(context.Context, string, []byte) error                { return nil }
func (f fakeFileChannel) Mkdir(context.Context, string) error                        { return nil }
func (f fakeFileChannel) Remove(context.Context, string) error                       { return nil }
func (f fakeFileChannel) Rename(context.Context, string, string) error               { return nil }
func (f fakeFileChannel) AbsPath(string) (string, error)                             { return "", nil }

type fakeOpener struct{ ch contract.FileChannel }

func (o fakeOpener) OpenFile(context.Context, string) (contract.FileChannel, error) { return o.ch, nil }
func (o fakeOpener) OpenExec(context.Context, string) (contract.ExecChannel, error) {
	return nil, os.ErrNotExist
}

// runWithFiles invokes the tool with a space file bag wired into ToolContext.
func runWithFiles(t *testing.T, arg string, files map[string][]byte) contract.ToolResult {
	t.Helper()
	tc := contract.ToolContext{Channels: fakeOpener{ch: fakeFileChannel{files: files}}}
	return corpusImport{}.Run(context.Background(), tc, json.RawMessage(arg))
}

// TestImportHappyPath drives a real import through an httptest server and asserts
// the wire shape: POST to /v1/contents/text, the three required headers, a body
// that round-trips to the caption+items, and a 201 body returned verbatim.
func TestImportHappyPath(t *testing.T) {
	var gotBody importReq
	var gotHeaders http.Header
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotHeaders = r.Method, r.URL.Path, r.Header
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"c_123","caption":"苏轼卷"}`))
	}))
	defer srv.Close()
	t.Setenv("PROMPTLIB_BASE", srv.URL)
	t.Setenv("PROMPTLIB_IMPORT_KEY", "sekret")

	res := run(t, `{"caption":"苏轼卷 · 来源X · 2026-07","items":[{"heading":"水调歌头 · 苏轼","body":"明月几时有"},{"body":"大江东去"}]}`)

	if !res.OK {
		t.Fatalf("want OK, got error: %s", res.Error)
	}
	if !strings.Contains(res.Data, `"id":"c_123"`) {
		t.Errorf("result should carry the content id verbatim, got: %s", res.Data)
	}
	if gotMethod != http.MethodPost || gotPath != importPath {
		t.Errorf("wire = %s %s, want POST %s", gotMethod, gotPath, importPath)
	}
	if gotHeaders.Get("X-Import-Key") != "sekret" {
		t.Errorf("X-Import-Key = %q, want sekret", gotHeaders.Get("X-Import-Key"))
	}
	if gotHeaders.Get("CF-Connecting-IP") == "" {
		t.Error("CF-Connecting-IP header missing (rate-limit middleware needs it)")
	}
	if gotHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotHeaders.Get("Content-Type"))
	}
	if gotBody.Caption != "苏轼卷 · 来源X · 2026-07" || len(gotBody.Items) != 2 {
		t.Errorf("decoded body wrong: %+v", gotBody)
	}
	if gotBody.Items[0].Heading != "水调歌头 · 苏轼" || gotBody.Items[1].Heading != "" {
		t.Errorf("item headings wrong: %+v", gotBody.Items)
	}
}

// TestImportFromFileObjectForm — a {caption,items} file is read from the space and
// shipped; the caption/items reach the server (content never rode the tool_call).
func TestImportFromFileObjectForm(t *testing.T) {
	var got importReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"c_file"}`))
	}))
	defer srv.Close()
	t.Setenv("PROMPTLIB_BASE", srv.URL)
	t.Setenv("PROMPTLIB_IMPORT_KEY", "k")

	fileJSON := `{"caption":"诗经 · 来源X","items":[{"heading":"关雎 · 国风 周南","body":"关关雎鸠"},{"body":"大江东去"}]}`
	res := runWithFiles(t, `{"file":"shijing_import.json"}`, map[string][]byte{"shijing_import.json": []byte(fileJSON)})
	if !res.OK {
		t.Fatalf("want OK, got %q", res.Error)
	}
	if got.Caption != "诗经 · 来源X" || len(got.Items) != 2 || got.Items[0].Heading != "关雎 · 国风 周南" {
		t.Errorf("server received wrong body: %+v", got)
	}
}

// TestImportFromFileArrayForm — a bare [items] file with caption from the arg.
func TestImportFromFileArrayForm(t *testing.T) {
	var got importReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"c_arr"}`))
	}))
	defer srv.Close()
	t.Setenv("PROMPTLIB_BASE", srv.URL)
	t.Setenv("PROMPTLIB_IMPORT_KEY", "k")

	res := runWithFiles(t, `{"file":"items.json","caption":"从参数来的 caption"}`,
		map[string][]byte{"items.json": []byte(`[{"body":"一"},{"body":"二"}]`)})
	if !res.OK {
		t.Fatalf("want OK, got %q", res.Error)
	}
	if got.Caption != "从参数来的 caption" || len(got.Items) != 2 {
		t.Errorf("server received wrong body: %+v", got)
	}
}

// TestImportFileErrors — missing file, bad JSON, and file+items conflict all error
// locally without hitting the network.
func TestImportFileErrors(t *testing.T) {
	t.Setenv("PROMPTLIB_IMPORT_KEY", "k")
	files := map[string][]byte{
		"bad.json": []byte(`{"caption":"x","items":`), // truncated JSON
	}
	cases := map[string]string{
		"not found":        `{"file":"missing.json"}`,
		"bad json":         `{"file":"bad.json"}`,
		"file+items both":  `{"file":"bad.json","items":[{"body":"x"}]}`,
		"no file no items": `{"caption":"c"}`,
	}
	for name, arg := range cases {
		res := runWithFiles(t, arg, files)
		if res.OK {
			t.Errorf("%s: want error, got OK", name)
		}
	}
}

// TestMissingKeyReportsUnconfigured — no PROMPTLIB_IMPORT_KEY → the tool reports
// unconfigured and never touches the network (fail honest, not a 401).
func TestMissingKeyReportsUnconfigured(t *testing.T) {
	t.Setenv("PROMPTLIB_IMPORT_KEY", "")
	res := run(t, `{"caption":"c","items":[{"body":"x"}]}`)
	if res.OK || !strings.Contains(res.Error, "未配置") {
		t.Fatalf("want unconfigured error, got OK=%v err=%q", res.OK, res.Error)
	}
}

// TestValidationRejectsLocally — bad input is rejected before any request, with a
// targeted message the model can act on.
func TestValidationRejectsLocally(t *testing.T) {
	t.Setenv("PROMPTLIB_IMPORT_KEY", "k")
	// A server that fails the test if hit — these must all short-circuit locally.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be called for invalid input")
	}))
	defer srv.Close()
	t.Setenv("PROMPTLIB_BASE", srv.URL)

	cases := map[string]string{
		"empty caption": `{"caption":"  ","items":[{"body":"x"}]}`,
		"no items":      `{"caption":"c","items":[]}`,
		"blank body":    `{"caption":"c","items":[{"body":"x"},{"body":"   "}]}`,
		"bad json":      `{"caption":`,
	}
	for name, arg := range cases {
		res := run(t, arg)
		if res.OK {
			t.Errorf("%s: want error, got OK", name)
		}
	}
}

// TestStatusClassification maps each error status to an error result carrying the
// server body and a status-specific hint.
func TestStatusClassification(t *testing.T) {
	t.Setenv("PROMPTLIB_IMPORT_KEY", "k")
	for _, tc := range []struct {
		status   int
		wantHint string
	}{
		{http.StatusBadRequest, "检查数据"},
		{http.StatusUnauthorized, "不要重试"},
		{http.StatusRequestEntityTooLarge, "对半分包"},
		{http.StatusInternalServerError, "报告用户"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}))
		t.Setenv("PROMPTLIB_BASE", srv.URL)
		res := run(t, `{"caption":"c","items":[{"body":"x"}]}`)
		srv.Close()
		if res.OK {
			t.Errorf("status %d: want error", tc.status)
		}
		if !strings.Contains(res.Hint, tc.wantHint) {
			t.Errorf("status %d: hint = %q, want contains %q", tc.status, res.Hint, tc.wantHint)
		}
		if !strings.Contains(res.Error, "nope") {
			t.Errorf("status %d: error should carry server body, got %q", tc.status, res.Error)
		}
	}
}

// TestRetryOn429 — a 429 with Retry-After:0 backs off then retries and succeeds.
func TestRetryOn429(t *testing.T) {
	t.Setenv("PROMPTLIB_IMPORT_KEY", "k")
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer srv.Close()
	t.Setenv("PROMPTLIB_BASE", srv.URL)

	res := run(t, `{"caption":"c","items":[{"body":"x"}]}`)
	if !res.OK {
		t.Fatalf("want OK after retry, got %q", res.Error)
	}
	if calls != 2 {
		t.Errorf("want 2 calls (429 then 201), got %d", calls)
	}
}

func TestRetryAfter(t *testing.T) {
	mk := func(v string) http.Header {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		return h
	}
	if got := retryAfter(mk("3"), 15*time.Second); got != 3*time.Second {
		t.Errorf("seconds: got %v, want 3s", got)
	}
	if got := retryAfter(mk("999"), 5*time.Second); got != 5*time.Second {
		t.Errorf("clamp to cap: got %v, want 5s", got)
	}
	if got := retryAfter(mk(""), 8*time.Second); got != 8*time.Second {
		t.Errorf("missing → cap: got %v, want 8s", got)
	}
	if got := retryAfter(mk("garbage"), 8*time.Second); got != 8*time.Second {
		t.Errorf("garbage → cap: got %v, want 8s", got)
	}
}

// TestOversizeItems — more than maxItems is rejected locally with a split message.
func TestOversizeItems(t *testing.T) {
	t.Setenv("PROMPTLIB_IMPORT_KEY", "k")
	items := make([]item, maxItems+1)
	for i := range items {
		items[i] = item{Body: "x"}
	}
	body, _ := json.Marshal(importReq{Caption: "c", Items: items})
	res := run(t, string(body))
	if res.OK || !strings.Contains(res.Error, "分成多次") {
		t.Fatalf("want split error, got OK=%v err=%q", res.OK, res.Error)
	}
}
