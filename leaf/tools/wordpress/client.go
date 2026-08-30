// Package wordpress is a thin REST client + two namespace-locked tools for a
// WordPress site and its WooCommerce store. Both surfaces speak the WP REST API
// over HTTP Basic Auth; the tools are GENERIC PASSTHROUGHS — the model supplies
// {method, path, query, body} and the per-endpoint field knowledge lives in the
// woocommerce_operation / wordpress_content SKILLs (leaf/skills/builtin), NOT in
// Go. Decoupled by design: a field/endpoint change is a skill-doc edit, never a
// code change. (docs/wordpress.md)
//
// TWO TOOLS, not one, because warrant gates per-tool name (tool:use:<name>): the
// tool-split IS the permission boundary. Each tool is locked to its own URL
// namespace so the capability name stays honest (a `woocommerce` tool that could
// also reach /wp/v2 would make `tool:use:woocommerce` a lie):
//   - woocommerce       → only /wp-json/wc/… and /wp-json/wc-…  (store)
//   - wordpress_content → only /wp-json/wp/…                    (blog/pages/media)
//
// Finer than that (read-only, no-PII) is NOT bob's warrant — bound it at the API
// credential's own scope (a read-only WooCommerce key).
//
// CONFIG = a vault credential, NOT env (docs/tools-warrant-credentials.md). One
// kind=wordpress credential per site, named (e.g. wp-shop-a), with these fields:
//
//	kind=wordpress
//	base_url                  site base, e.g. https://shop.example.com (HTTPS)
//	woo_ck / woo_cs           WooCommerce consumer key/secret (Basic auth for /wc/ paths)
//	wp_user / wp_pass         WP application password (Basic auth for core paths)
//	rest_route                optional; "true" → address the REST API via the plain
//	                          ?rest_route= entry instead of /wp-json/ (for sites
//	                          without pretty permalinks, where /wp-json/ 404s)
//
// At use time the tool asks warrant (via the identity-bound CredentialOpener) for
// the kind=wordpress credential the caller is authorized for; warrant builds this
// *Client (BuildClient) and the secret never reaches the tool's Run code — it only
// calls c.do(). Multiple sites = multiple credentials, one per member (grant
// credential:use:<name>). No credential authorized → Run reports it, not a 401.
//
// AUTH is asymmetric (see docs/wordpress.md): a WP admin application password
// authenticates BOTH core and WooCommerce routes, but a WooCommerce ck/cs pair
// authenticates ONLY WooCommerce routes. So:
//   - a /wc/ path uses ck/cs when present, else FALLS BACK to the app password;
//   - a /wp/ path uses the app password only (ck/cs cannot authenticate it).
package wordpress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// requestTimeout caps one REST call. WP/Woo endpoints are normal JSON CRUD; 30s
// is generous headroom for a large product/order list page.
const requestTimeout = 30 * time.Second

// maxRespBytes caps a response body. A wide product/order list (per_page=100 with
// full field sets) is at most a few MB; 16 MB bounds a misbehaving endpoint while
// leaving comfortable room.
const maxRespBytes = 16 << 20

// sharedHTTP is ONE client (hence one connection pool) reused across every
// per-identity *Client wrapper. A *Client is rebuilt per tool call (credential
// resolution is per identity), but the transport must NOT be — a fresh client per
// call would drop TCP/TLS keep-alive, re-handshaking the site on every step of a
// multi-call turn. The wrapper carries only credentials; the pool lives here.
var sharedHTTP = &http.Client{Timeout: requestTimeout}

// Client talks to one WordPress site. It is stateless: every call self-carries
// method/path/query/body and the auth is chosen per-path. Holds both credential
// pairs (either may be empty); authorize() picks and reports a clear "缺哪个 env"
// when the needed pair is absent.
type Client struct {
	base   string
	wooCK  string
	wooCS  string
	wpUser string
	wpPass string
	// restRoute, when true, makes buildURL address the REST API through the plain
	// ?rest_route= entry instead of the pretty /wp-json/ path. Per-site config: a
	// site without pretty permalinks 404s on /wp-json/. The model still supplies a
	// /wp-json/ path (so restNamespace, the warrant lock and auth are untouched);
	// only the outgoing wire form changes.
	restRoute bool
}

// New builds the client. baseURL is the site root (no /wp-json suffix); the
// credential args come from the vault credential and any may be "". Trailing slash
// on base is trimmed so path joining is unambiguous. restRoute selects the plain
// ?rest_route= entry (see Client.restRoute). The http transport is the
// package-shared sharedHTTP (keep-alive across calls), not per-Client.
func New(baseURL, wooCK, wooCS, wpUser, wpPass string, restRoute bool) *Client {
	return &Client{
		base:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		wooCK:     strings.TrimSpace(wooCK),
		wooCS:     strings.TrimSpace(wooCS),
		wpUser:    strings.TrimSpace(wpUser),
		wpPass:    strings.TrimSpace(wpPass),
		restRoute: restRoute,
	}
}

// BuildClient is the kind=wordpress credential factory (registered with the broker
// by the wiring layer). It builds a *Client from the vault fields so the secret
// stays inside the Client — warrant hands the tool this ready client, never the
// fields. Returns `any` to match credentials.KindFactory; the tools type-assert
// *Client. base_url is required (the one non-secret, identifying field).
func BuildClient(data map[string]string) (any, error) {
	if strings.TrimSpace(data["base_url"]) == "" {
		return nil, errors.New("wordpress credential needs base_url")
	}
	restRoute := isTruthy(data["rest_route"])
	return New(data["base_url"], data["woo_ck"], data["woo_cs"], data["wp_user"], data["wp_pass"], restRoute), nil
}

// apiResult is one REST call's raw outcome (status + body). Status classification
// (2xx vs error) is the tool's job, not the client's.
type apiResult struct {
	status int
	body   []byte
}

// do performs one REST call: build the URL (base + path + encoded query), pick
// the auth for the path's namespace, send, and read the (bounded) body. A
// returned error is a transport/config failure (URL build, missing credential,
// site unreachable); a non-2xx HTTP reply is NOT an error here — it rides back in
// apiResult.status for the tool to surface with the body (WP/Woo return a JSON
// {code,message} error the model can read and self-correct from).
func (c *Client) do(ctx context.Context, method, path string, query map[string]any, body json.RawMessage) (*apiResult, error) {
	u, err := c.buildURL(path, query)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败：%v", err)
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.authorize(req, path); err != nil {
		return nil, err
	}
	return c.send(req)
}

// send dispatches a prepared request and reads the bounded response. Shared by do
// (JSON body) and doUpload (binary body): the two differ only in how the request
// body and content headers are built — the dispatch + bounded read + apiResult
// wrap is identical, so it lives here once.
func (c *Client) send(req *http.Request) (*apiResult, error) {
	resp, err := sharedHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("WordPress 站点不可达（%v）", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败：%v", err)
	}
	return &apiResult{status: resp.StatusCode, body: raw}, nil
}

// doUpload performs a binary media upload: it sends the raw file bytes (NOT a JSON
// body) to a /wp/v2/media endpoint, naming the file via Content-Disposition — the
// header WordPress reads to set the attachment's filename/extension. Everything
// else mirrors do(): same buildURL, same per-path auth (a core media POST uses the
// app password, which authorize() already selects), same shared transport and
// bounded read. Separate from do() because the wire shape is fundamentally
// different (binary body + image content-type + disposition, not application/json),
// so the generic JSON passthrough cannot express it.
func (c *Client) doUpload(ctx context.Context, path string, data []byte, contentType, filename string) (*apiResult, error) {
	u, err := c.buildURL(path, nil)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败：%v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if err := c.authorize(req, path); err != nil {
		return nil, err
	}
	return c.send(req)
}

// buildURL joins base + path and encodes the query map. path is taken verbatim
// (the model gives the full /wp-json/… path, per the skill docs); a leading slash
// is added if missing. Query values are stringified (numbers without trailing
// zeros, bools as true/false, slices joined by comma for WP's array params); keys
// are sorted so the URL is deterministic (testable).
func (c *Client) buildURL(path string, query map[string]any) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return "", errors.New("path 不能为空（如 /wp-json/wc/v3/products）")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u, err := url.Parse(c.base + p)
	if err != nil {
		return "", fmt.Errorf("path 非法：%v", err)
	}
	// rest_route mode: a site without pretty permalinks 404s on /wp-json/, so
	// address the same endpoint via the plain ?rest_route= entry. This is a pure
	// wire-form rewrite AFTER the namespace lock has passed (restNamespace still
	// reads the /wp-json/ path the model gave), so the warrant boundary and auth
	// selection are untouched. Done on the parsed u.Path (NOT the raw path) so a
	// query the model embedded in the path — e.g. /wp-json/wp/v2/tags?search=风景 —
	// is already split into u.RawQuery and stays a real query param, instead of
	// being swallowed into the rest_route value. Only a genuine /wp-json/ path is
	// translated; anything else is left verbatim.
	var route string
	if c.restRoute && strings.HasPrefix(u.Path, "/wp-json/") {
		route = strings.TrimPrefix(u.Path, "/wp-json") // "/wp/v2/media"
		u.Path = "/"
	}
	if route != "" || len(query) > 0 {
		q := u.Query()
		if route != "" {
			q.Set("rest_route", route)
		}
		keys := make([]string, 0, len(query))
		for k := range query {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			q.Set(k, queryValue(query[k]))
		}
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// isTruthy reads a credential boolean field leniently (true/1/yes/on, any case).
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// queryValue renders one query value as a string. JSON numbers decode to float64,
// so integers are formatted without a spurious ".0"; slices become comma-joined
// (WooCommerce/WP accept `include=1,2,3`).
func queryValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers decode to float64 via plain Unmarshal; format integers
		// (product/order IDs) without a spurious ".0".
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, queryValue(e))
		}
		return strings.Join(parts, ",")
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

// authorize attaches Basic auth for the path's namespace. WooCommerce routes
// prefer ck/cs and fall back to the app password (a WP admin app password also
// authenticates Woo); core routes use the app password only. Returns a clear
// "缺哪个 env" error when the usable credential is absent — the tool surfaces it
// to the model as a config error, not a silent 401.
func (c *Client) authorize(req *http.Request, path string) error {
	if isWooPath(path) {
		switch {
		case c.wooCK != "" && c.wooCS != "":
			req.SetBasicAuth(c.wooCK, c.wooCS)
		case c.wpUser != "" && c.wpPass != "":
			req.SetBasicAuth(c.wpUser, c.wpPass) // admin app password also authenticates Woo
		default:
			return errors.New("WooCommerce 凭证未配置，请联系管理员配置")
		}
		return nil
	}
	if c.wpUser == "" || c.wpPass == "" {
		return errors.New("WordPress 凭证未配置，请联系管理员配置")
	}
	req.SetBasicAuth(c.wpUser, c.wpPass)
	return nil
}

// hasDotSegment reports whether any path segment is a relative dot-segment ("."
// or ".."), decoded — the traversal primitive that can defeat the namespace lock.
// restNamespace classifies a path by its FIRST segment after /wp-json/, but Go
// transmits "/wp-json/wp/v2/../../wc/v3/orders" verbatim on the wire and a
// front-end (nginx/apache) normalizes the dot-segments to reach /wc/ — so a path
// that classifies (and authenticates) as one namespace lands in another. No
// legitimate WP REST path contains a dot-segment, so this is a pure reject:
// query/fragment are stripped, then each slash-delimited segment is percent-
// decoded (catching %2e%2e as well as literal ..) and compared to "."/"..".
func hasDotSegment(path string) bool {
	p := strings.TrimSpace(path)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
		if dec, err := url.PathUnescape(seg); err == nil && (dec == "." || dec == "..") {
			return true
		}
	}
	return false
}

// restNamespace returns the REST namespace — the path segment immediately after
// /wp-json/ (lowercased: "wc", "wc-analytics", "wp", …) — or "" if path is not a
// /wp-json/ route. This is the LOAD-BEARING classification behind the per-tool
// namespace lock (the warrant boundary), so it must be exact, not a loose
// substring test: query/fragment are stripped first (a value containing "/wc/"
// must NOT poison classification), and only the FIRST segment after the /wp-json/
// prefix counts (a deeper "/wp/" or "/wc/" in the resource path can't flip it).
// The result is single-valued, so isWooPath and isWPCorePath are mutually
// exclusive — one crafted path can never satisfy both tools' locks. A path
// carrying a traversal dot-segment classifies as "" (fail-closed) so neither
// tool's lock can pass it — the primary reject is in run(), this is defense-in-depth.
func restNamespace(path string) string {
	if hasDotSegment(path) {
		return ""
	}
	p := strings.TrimSpace(path)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.ToLower(p)
	const marker = "/wp-json/"
	if !strings.HasPrefix(p, marker) {
		return ""
	}
	seg := p[len(marker):]
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	return seg
}

// isWooPath reports whether a REST path belongs to the WooCommerce namespace
// (/wp-json/wc/… and the wc-* siblings like wc-analytics). Used both for auth
// selection (here) and for the woocommerce tool's namespace lock.
func isWooPath(path string) bool {
	ns := restNamespace(path)
	return ns == "wc" || strings.HasPrefix(ns, "wc-")
}

// isWPCorePath reports whether a REST path belongs to the WordPress core
// namespace (/wp-json/wp/…). The wordpress_content tool locks to this.
func isWPCorePath(path string) bool {
	return restNamespace(path) == "wp"
}
