package wordpress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbob/contract"
)

// fakeCreds is a test CredentialOpener: it hands back a fixed *Client for
// kind=wordpress, standing in for warrant's by-kind resolution. A nil client
// models "no credential authorized" (Build returns an error).
type fakeCreds struct{ c *Client }

func (f fakeCreds) Build(_ context.Context, kind string) (any, error) {
	if kind != "wordpress" {
		return nil, fmt.Errorf("unexpected kind %q", kind)
	}
	if f.c == nil {
		return nil, fmt.Errorf("没有授权给你的 wordpress 凭证")
	}
	return f.c, nil
}

func TestNamespaceClassify(t *testing.T) {
	woo := []string{"/wp-json/wc/v3/products", "/wp-json/wc/v3/orders/12", "/wp-json/wc-analytics/reports", "wp-json/wc/v3/products"}
	core := []string{"/wp-json/wp/v2/posts", "/wp-json/wp/v2/media"}
	for _, p := range woo {
		if !isWooPath(p) {
			t.Errorf("isWooPath(%q) = false, want true", p)
		}
		if isWPCorePath(p) {
			t.Errorf("isWPCorePath(%q) = true, want false", p)
		}
	}
	for _, p := range core {
		if !isWPCorePath(p) {
			t.Errorf("isWPCorePath(%q) = false, want true", p)
		}
		if isWooPath(p) {
			t.Errorf("isWooPath(%q) = true, want false", p)
		}
	}
}

// TestNamespaceLockNoBypass locks in the fix for the substring-match bypass: a
// crafted path can NOT make a Woo route classify as WP-core (or vice-versa), and
// query/fragment content can't poison classification. A wordpress_content-only
// principal must never reach the store via a "/wp/"-containing Woo path.
func TestNamespaceLockNoBypass(t *testing.T) {
	cases := []struct {
		path      string
		woo, core bool
	}{
		// deeper /wp/ segment inside a Woo resource path must NOT flip to core
		{"/wp-json/wc/v3/products/wp/123", true, false},
		// /wp/ hidden in the query must NOT flip a Woo route to core
		{"/wp-json/wc/v3/products?foo=/wp/", true, false},
		// /wc/ hidden in the query must NOT flip a WP-core route to Woo
		{"/wp-json/wp/v2/media?x=/wc/y", false, true},
		// non-/wp-json/ junk belongs to neither (both tools reject it)
		{"/foo/wc/bar", false, false},
		{"/wp-json/", false, false},
		{"https://evil.com/wc/v3", false, false},
	}
	for _, c := range cases {
		if got := isWooPath(c.path); got != c.woo {
			t.Errorf("isWooPath(%q) = %v, want %v", c.path, got, c.woo)
		}
		if got := isWPCorePath(c.path); got != c.core {
			t.Errorf("isWPCorePath(%q) = %v, want %v", c.path, got, c.core)
		}
	}
}

// TestDotSegmentTraversalRejected pins B37: a traversal path that classifies as
// one namespace (via its first segment) but a front-end normalizes into the other
// must be rejected BEFORE classification/auth — never cleaned-and-sent. Both the
// hasDotSegment primitive, the fail-closed classification, and the run() reject
// are exercised.
func TestDotSegmentTraversalRejected(t *testing.T) {
	// classic cross-namespace escape + a percent-encoded variant + a leading-dot form
	bad := []string{
		"/wp-json/wp/v2/../../wc/v3/orders",
		"/wp-json/wc/v3/../../wp/v2/users",
		"/wp-json/wp/v2/%2e%2e/wc/v3/orders",
		"/wp-json/wp/v2/./posts",
	}
	for _, p := range bad {
		if !hasDotSegment(p) {
			t.Errorf("hasDotSegment(%q) = false, want true", p)
		}
		// fail-closed classification: neither lock may claim a traversal path
		if isWooPath(p) || isWPCorePath(p) {
			t.Errorf("traversal %q classified into a namespace (woo=%v core=%v), want neither",
				p, isWooPath(p), isWPCorePath(p))
		}
	}
	// a legitimate deep path with no dot-segment must NOT be flagged
	for _, ok := range []string{"/wp-json/wc/v3/products/123", "/wp-json/wp/v2/media", "/wp-json/wc/v3/products?search=a.b"} {
		if hasDotSegment(ok) {
			t.Errorf("hasDotSegment(%q) = true, want false", ok)
		}
	}

	// run() reject: the woo tool must refuse the escape BEFORE credential resolution,
	// with the dot-segment error (not the namespace-lock error), even though the
	// first segment (/wp/) would otherwise route to the wp tool.
	ctx := context.Background()
	r := NewWooTool().Run(ctx, contract.ToolContext{}, mustArgs("GET", "/wp-json/wc/v3/../../wp/v2/users"))
	if r.OK || !strings.Contains(r.Error, "路径段") {
		t.Errorf("woo traversal reject: got OK=%v err=%q, want dot-segment rejection", r.OK, r.Error)
	}
	// symmetric: wp tool refuses a path that would reach /wc/ after normalization
	r2 := NewWPTool().Run(ctx, contract.ToolContext{}, mustArgs("GET", "/wp-json/wp/v2/../../wc/v3/orders"))
	if r2.OK || !strings.Contains(r2.Error, "路径段") {
		t.Errorf("wp traversal reject: got OK=%v err=%q, want dot-segment rejection", r2.OK, r2.Error)
	}
}

func TestBuildURLQuery(t *testing.T) {
	c := New("https://shop.example.com/", "", "", "", "", false)
	got, err := c.buildURL("/wp-json/wc/v3/products", map[string]any{
		"per_page": float64(20), // JSON numbers decode to float64
		"search":   "T恤",
		"featured": true,
		"include":  []any{float64(1), float64(2)},
	})
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	// keys sorted: featured, include, per_page, search
	want := "https://shop.example.com/wp-json/wc/v3/products?featured=true&include=1%2C2&per_page=20&search=T%E6%81%A4"
	if got != want {
		t.Errorf("buildURL =\n  %s\nwant\n  %s", got, want)
	}
}

// TestBuildURLRestRoute locks in the plain ?rest_route= addressing for sites
// without pretty permalinks: the model still gives a /wp-json/ path (so the
// namespace lock and auth are untouched), but the wire URL targets the base with
// ?rest_route=<route>. Other query params ride alongside.
func TestBuildURLRestRoute(t *testing.T) {
	c := New("https://hygpo.com", "", "", "admin", "app pass", true)
	got, err := c.buildURL("/wp-json/wp/v2/media", map[string]any{"per_page": float64(20)})
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	// rest_route value's slashes are percent-encoded (%2F); WP urldecodes $_GET.
	want := "https://hygpo.com/?per_page=20&rest_route=%2Fwp%2Fv2%2Fmedia"
	if got != want {
		t.Errorf("buildURL =\n  %s\nwant\n  %s", got, want)
	}
	// restRoute=false keeps the pretty /wp-json/ form unchanged.
	c2 := New("https://hygpo.com", "", "", "admin", "app pass", false)
	got2, _ := c2.buildURL("/wp-json/wp/v2/media", nil)
	if want2 := "https://hygpo.com/wp-json/wp/v2/media"; got2 != want2 {
		t.Errorf("buildURL(restRoute=false) = %s, want %s", got2, want2)
	}
	// A query embedded in the path (the skill's `?search=` tag-lookup form) must
	// stay a real query param, NOT get swallowed into the rest_route value.
	got3, err := c.buildURL("/wp-json/wp/v2/tags?search=风景", nil)
	if err != nil {
		t.Fatalf("buildURL(path query): %v", err)
	}
	if want3 := "https://hygpo.com/?rest_route=%2Fwp%2Fv2%2Ftags&search=%E9%A3%8E%E6%99%AF"; got3 != want3 {
		t.Errorf("buildURL(path query) =\n  %s\nwant\n  %s", got3, want3)
	}
}

func TestBuildURLMissingPath(t *testing.T) {
	c := New("https://shop.example.com", "", "", "", "", false)
	if _, err := c.buildURL("  ", nil); err == nil {
		t.Error("buildURL(empty) = nil err, want error")
	}
}

func TestAuthorizeSelection(t *testing.T) {
	// Woo path with ck/cs → ck/cs Basic auth.
	c := New("https://x", "ck_1", "cs_1", "admin", "app pass", false)
	req, _ := http.NewRequest("GET", "https://x/wp-json/wc/v3/products", nil)
	if err := c.authorize(req, "/wp-json/wc/v3/products"); err != nil {
		t.Fatalf("authorize woo: %v", err)
	}
	if u, p, _ := req.BasicAuth(); u != "ck_1" || p != "cs_1" {
		t.Errorf("woo auth = %q/%q, want ck_1/cs_1", u, p)
	}

	// Woo path WITHOUT ck/cs → falls back to app password.
	c2 := New("https://x", "", "", "admin", "app pass", false)
	req2, _ := http.NewRequest("GET", "https://x/wp-json/wc/v3/products", nil)
	if err := c2.authorize(req2, "/wp-json/wc/v3/products"); err != nil {
		t.Fatalf("authorize woo fallback: %v", err)
	}
	if u, p, _ := req2.BasicAuth(); u != "admin" || p != "app pass" {
		t.Errorf("woo fallback auth = %q/%q, want admin/app pass", u, p)
	}

	// WP core path → app password only (ck/cs ignored).
	req3, _ := http.NewRequest("GET", "https://x/wp-json/wp/v2/posts", nil)
	if err := c.authorize(req3, "/wp-json/wp/v2/posts"); err != nil {
		t.Fatalf("authorize wp: %v", err)
	}
	if u, p, _ := req3.BasicAuth(); u != "admin" || p != "app pass" {
		t.Errorf("wp auth = %q/%q, want admin/app pass", u, p)
	}

	// WP core path with NO app password → clear config error.
	c3 := New("https://x", "ck_1", "cs_1", "", "", false)
	req4, _ := http.NewRequest("GET", "https://x/wp-json/wp/v2/posts", nil)
	if err := c3.authorize(req4, "/wp-json/wp/v2/posts"); err == nil {
		t.Error("authorize wp without app password = nil err, want config error")
	}
}

func TestRunValidation(t *testing.T) {
	woo := NewWooTool()
	ctx := context.Background()

	// bad JSON
	if r := woo.Run(ctx, contract.ToolContext{}, json.RawMessage(`{`)); r.OK {
		t.Error("bad JSON: OK=true, want false")
	}
	// bad method
	if r := woo.Run(ctx, contract.ToolContext{}, mustArgs("PATCH", "/wp-json/wc/v3/products")); r.OK {
		t.Error("bad method: OK=true, want false")
	}
	// namespace lock: woo tool rejects a /wp/ path (BEFORE any credential resolution)
	r := woo.Run(ctx, contract.ToolContext{}, mustArgs("GET", "/wp-json/wp/v2/posts"))
	if r.OK || !strings.Contains(r.Error, "范围") {
		t.Errorf("namespace lock: got OK=%v err=%q, want rejected", r.OK, r.Error)
	}
	// in-namespace but no credential backend wired → a credential error (not a
	// namespace error). Empty ToolContext → tc.Credentials nil.
	r2 := woo.Run(ctx, contract.ToolContext{}, mustArgs("GET", "/wp-json/wc/v3/products"))
	if r2.OK || !strings.Contains(r2.Error, "凭证") {
		t.Errorf("no credential backend: got OK=%v err=%q, want credential error", r2.OK, r2.Error)
	}
	// in-namespace, backend wired but no credential authorized → surfaces warrant's
	// by-kind-unique "none authorized" reason.
	r2b := woo.Run(ctx, contract.ToolContext{Credentials: fakeCreds{}}, mustArgs("GET", "/wp-json/wc/v3/products"))
	if r2b.OK || !strings.Contains(r2b.Error, "凭证") {
		t.Errorf("none authorized: got OK=%v err=%q, want credential error", r2b.OK, r2b.Error)
	}

	// wp tool rejects a /wc/ path symmetrically
	wp := NewWPTool()
	r3 := wp.Run(ctx, contract.ToolContext{}, mustArgs("GET", "/wp-json/wc/v3/products"))
	if r3.OK || !strings.Contains(r3.Error, "范围") {
		t.Errorf("wp namespace lock: got OK=%v err=%q, want rejected", r3.OK, r3.Error)
	}
}

func TestRunHappyAndError(t *testing.T) {
	var gotMethod, gotPath, gotAuthUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuthUser, _, _ = r.BasicAuth()
		switch r.URL.Path {
		case "/wp-json/wc/v3/products":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":1,"name":"T恤"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"rest_no_route","message":"No route"}`))
		}
	}))
	defer srv.Close()

	woo := NewWooTool()
	tc := contract.ToolContext{Credentials: fakeCreds{c: New(srv.URL, "ck_1", "cs_1", "", "", false)}}
	ctx := context.Background()

	r := woo.Run(ctx, tc, mustArgs("GET", "/wp-json/wc/v3/products"))
	if !r.OK {
		t.Fatalf("happy path: OK=false err=%q", r.Error)
	}
	if !strings.Contains(r.Data, "T恤") {
		t.Errorf("happy path data = %q, want product body", r.Data)
	}
	if gotMethod != "GET" || gotPath != "/wp-json/wc/v3/products" || gotAuthUser != "ck_1" {
		t.Errorf("server saw method=%q path=%q user=%q", gotMethod, gotPath, gotAuthUser)
	}

	// non-2xx → error result carrying the body
	r2 := woo.Run(ctx, tc, mustArgs("GET", "/wp-json/wc/v3/orders/999"))
	if r2.OK || !strings.Contains(r2.Error, "404") || !strings.Contains(r2.Error, "rest_no_route") {
		t.Errorf("error path: OK=%v err=%q, want 404 + body", r2.OK, r2.Error)
	}
}

func mustArgs(method, path string) json.RawMessage {
	b, _ := json.Marshal(args{Method: method, Path: path})
	return b
}

// pngBytes is a minimal valid PNG header so http.DetectContentType / size checks
// see a real image (the upload path rejects non-image content types).
var pngBytes = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}

// fakeFileChannel is a FileChannel backed by a real temp dir, so runUpload's
// AbsPath → os.Stat → Read sequence works without a warrant/localfs stack.
type fakeFileChannel struct{ root string }

func (f fakeFileChannel) Alive() bool  { return true }
func (f fakeFileChannel) Close() error { return nil }
func (f fakeFileChannel) List(context.Context, string) ([]contract.FileEntry, error) {
	return nil, nil
}
func (f fakeFileChannel) Read(_ context.Context, p string) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.root, p))
}
func (f fakeFileChannel) Write(context.Context, string, []byte) error  { return nil }
func (f fakeFileChannel) Mkdir(context.Context, string) error          { return nil }
func (f fakeFileChannel) Remove(context.Context, string) error         { return nil }
func (f fakeFileChannel) Rename(context.Context, string, string) error { return nil }
func (f fakeFileChannel) AbsPath(p string) (string, error)             { return filepath.Join(f.root, p), nil }

// fakeChannels is a ChannelOpener vending the temp-dir-backed file channel.
type fakeChannels struct{ root string }

func (f fakeChannels) OpenFile(context.Context, string) (contract.FileChannel, error) {
	return fakeFileChannel{root: f.root}, nil
}
func (f fakeChannels) OpenExec(context.Context, string) (contract.ExecChannel, error) {
	return nil, fmt.Errorf("no exec")
}

// TestRunUpload exercises the binary upload fork: a `file` key in body makes the
// tool read the space file and POST it as raw bytes (Content-Disposition naming
// it), and the new media id rides back in the OK result.
func TestRunUpload(t *testing.T) {
	root := t.TempDir()
	if err := mkInbox(root, "inbox/logo.png", pngBytes); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	var gotMethod, gotPath, gotDisp, gotCType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotDisp = r.Header.Get("Content-Disposition")
		gotCType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"source_url":"https://x/wp-content/logo.png"}`))
	}))
	defer srv.Close()

	wp := NewWPTool()
	tc := contract.ToolContext{
		Credentials: fakeCreds{c: New(srv.URL, "", "", "admin", "app pass", false)},
		Channels:    fakeChannels{root: root},
	}
	raw := mustUploadArgs("POST", "/wp-json/wp/v2/media", "inbox/logo.png", "")

	r := wp.Run(context.Background(), tc, raw)
	if !r.OK {
		t.Fatalf("upload: OK=false err=%q", r.Error)
	}
	if !strings.Contains(r.Data, `"id":42`) {
		t.Errorf("upload data = %q, want new media id", r.Data)
	}
	if gotMethod != "POST" || gotPath != "/wp-json/wp/v2/media" {
		t.Errorf("server saw method=%q path=%q", gotMethod, gotPath)
	}
	if !strings.Contains(gotDisp, `filename="logo.png"`) {
		t.Errorf("Content-Disposition = %q, want filename=logo.png", gotDisp)
	}
	if gotCType != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", gotCType)
	}
	if string(gotBody) != string(pngBytes) {
		t.Errorf("server got %d body bytes, want the file's %d", len(gotBody), len(pngBytes))
	}
}

// TestRunUploadRejects covers the upload fork's guards: a file on a non-media path,
// a non-POST method, and a missing file each fail before any HTTP call.
func TestRunUploadRejects(t *testing.T) {
	root := t.TempDir()
	wp := NewWPTool()
	tc := contract.ToolContext{
		Credentials: fakeCreds{c: New("https://x", "", "", "admin", "app pass", false)},
		Channels:    fakeChannels{root: root},
	}
	ctx := context.Background()

	// file on a metadata path (…/media/123) must be rejected, not hijacked.
	if r := wp.Run(ctx, tc, mustUploadArgs("POST", "/wp-json/wp/v2/media/123", "inbox/x.png", "")); r.OK {
		t.Error("upload to /media/<id>: OK=true, want rejected")
	}
	// file with a non-POST method.
	if r := wp.Run(ctx, tc, mustUploadArgs("PUT", "/wp-json/wp/v2/media", "inbox/x.png", "")); r.OK {
		t.Error("upload with PUT: OK=true, want rejected")
	}
	// file that doesn't exist in the space.
	if r := wp.Run(ctx, tc, mustUploadArgs("POST", "/wp-json/wp/v2/media", "inbox/missing.png", "")); r.OK {
		t.Error("upload missing file: OK=true, want rejected")
	}
	// content-type is sniffed from the BYTES, not the name: a .png that is really
	// HTML/text must be rejected, not sent up as image/png.
	if err := mkInbox(root, "inbox/fake.png", []byte("<html>not an image</html>")); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if r := wp.Run(ctx, tc, mustUploadArgs("POST", "/wp-json/wp/v2/media", "inbox/fake.png", "")); r.OK {
		t.Error("upload non-image bytes named .png: OK=true, want rejected")
	}
}

// TestUploadFilenameSanitized locks in that a model-supplied filename carrying a
// double-quote / control char is sanitized before reaching the Content-Disposition
// header (a raw quote would corrupt the filename param WP parses).
func TestUploadFilenameSanitized(t *testing.T) {
	root := t.TempDir()
	if err := mkInbox(root, "inbox/logo.png", pngBytes); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var gotDisp string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDisp = r.Header.Get("Content-Disposition")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7}`))
	}))
	defer srv.Close()

	wp := NewWPTool()
	tc := contract.ToolContext{
		Credentials: fakeCreds{c: New(srv.URL, "", "", "admin", "app pass", false)},
		Channels:    fakeChannels{root: root},
	}
	// filename with an embedded quote + backslash; the file itself is logo.png.
	raw := mustUploadArgs("POST", "/wp-json/wp/v2/media", "inbox/logo.png", `a"b\c.png`)
	r := wp.Run(context.Background(), tc, raw)
	if !r.OK {
		t.Fatalf("upload: OK=false err=%q", r.Error)
	}
	// sanitized: a"b\c.png → abc.png, so the header is exactly filename="abc.png"
	// with no stray quote/backslash breaking the param.
	if want := `attachment; filename="abc.png"`; gotDisp != want {
		t.Errorf("Content-Disposition = %q, want %q", gotDisp, want)
	}
}

func mustUploadArgs(method, path, file, filename string) json.RawMessage {
	body, _ := json.Marshal(uploadArgs{File: file, Filename: filename})
	b, _ := json.Marshal(args{Method: method, Path: path, Body: body})
	return b
}

// mkInbox seeds a space-relative file under root (creating parent dirs).
func mkInbox(root, rel string, data []byte) error {
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, data, 0o644)
}
