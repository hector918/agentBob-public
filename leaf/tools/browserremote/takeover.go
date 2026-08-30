package browserremote

// Takeover proxy (docs/browserd.md §4, docs/wip-takeover-port.md): bob's webui
// relays /api/browser/screencast + /input onto browserd's takeover face. The human
// only ever talks to bob's single origin. Implements contract.BrowserTakeover, keyed
// by SID (trunk's browser axis — see browser.go's `common`).
//
// Auth: the takeover face authenticates with the SAME control-plane API key
// ($BROWSERD_API_KEY) as every other browserd endpoint (Authorization: Bearer
// <apiKey>). browserd is a dumb service — it only knows bob via the api key and
// knows nothing about user business. The per-takeover AUTHORIZATION (who may take
// over which browser) stays entirely on bob's side: leaf/webui runs
// Authorize(cookie, scope) BEFORE calling Screencast/Input; only after that does
// bob reach browserd here, server-to-server, with the api key. The key is never
// exposed to the human's browser (bob's webui proxies the stream — the browser
// connects to bob, never to browserd). The per-takeover ticket browserd used to
// verify was retired (it encoded user business into a dumb service); see the §4
// removal note in docs/browserd.md.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agentbob/contract"
)

const (
	takeoverDefaultPort   = "8378" // browserd --takeover-listen default (zero-config: control host + this port)
	takeoverInputTimeout  = 5 * time.Second
	takeoverHeaderTimeout = 15 * time.Second
	maxFrameLineBytes     = 16 << 20 // one SSE frame line (base64 JPEG) ceiling
)

var _ contract.BrowserTakeover = (*Client)(nil)

// takeoverBaseURL derives the takeover-face base from the control-plane URL: same
// scheme + host, port swapped to the takeover default. "" on an unparseable URL.
func takeoverBaseURL(controlURL string) string {
	u, err := url.Parse(controlURL)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return ""
	}
	return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), takeoverDefaultPort)
}

func (c *Client) takeoverURL(path, scope string) string {
	return c.takeoverBase + path + "?scope=" + url.QueryEscape(scope)
}

// setTakeoverAuth attaches the control-plane bearer key to a takeover-face request
// (the takeover face is authed by the same $BROWSERD_API_KEY as the control plane).
// A no-op when no key is configured (browserd then relies on network isolation).
func (c *Client) setTakeoverAuth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

func (c *Client) faceUnreachable(err error) error {
	if ue, ok := err.(*url.Error); ok {
		err = ue.Err // strip the URL before this reaches an HTTP error body
	}
	return fmt.Errorf("browser takeover face %s unreachable: %v", c.takeoverBase, err)
}

// Screencast implements contract.BrowserTakeover: opens browserd's takeover SSE for
// sid and forwards every base64 JPEG frame to `frame`. stop severs the upstream;
// done closes when the upstream ends for any reason.
func (c *Client) Screencast(sid, quality string, frame func(jpegB64 string)) (stop func(), done <-chan struct{}, err error) {
	if c.takeoverBase == "" {
		return nil, nil, fmt.Errorf("browser service: cannot derive takeover address from %q", c.base)
	}
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := c.takeoverConnect(ctx, sid, quality)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	d := make(chan struct{})
	go func() {
		defer close(d)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), maxFrameLineBytes)
		for sc.Scan() {
			// `data: <b64>` frames interleaved with `:keepalive` comments (dropped).
			if b64, ok := strings.CutPrefix(sc.Text(), "data: "); ok && b64 != "" {
				frame(b64)
			}
		}
	}()
	return cancel, d, nil
}

// takeoverConnect performs the screencast GET authed by the control-plane api key.
// On success the response body is the live SSE stream (caller closes it).
func (c *Client) takeoverConnect(ctx context.Context, scope, quality string) (*http.Response, error) {
	u := c.takeoverURL("/takeover/screencast", scope)
	if quality != "" {
		u += "&q=" + url.QueryEscape(quality) // requested tier (backend may clamp)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("browser takeover: build request: %v", err)
	}
	c.setTakeoverAuth(req)
	resp, err := c.tkc.Do(req)
	if err != nil {
		return nil, c.faceUnreachable(err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	return nil, fmt.Errorf("browser takeover: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}

// takeoverPost POSTs one JSON body to a takeover-face endpoint authed by the
// control-plane api key. Bounded by takeoverInputTimeout.
func (c *Client) takeoverPost(scope, path string, body []byte) (status int, raw []byte, err error) {
	if c.takeoverBase == "" {
		return 0, nil, fmt.Errorf("browser service: cannot derive takeover address from %q", c.base)
	}
	ctx, cancel := context.WithTimeout(context.Background(), takeoverInputTimeout)
	defer cancel()
	req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, c.takeoverURL(path, scope), bytes.NewReader(body))
	if rerr != nil {
		return 0, nil, fmt.Errorf("browser takeover: build request: %v", rerr)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setTakeoverAuth(req)
	resp, derr := c.tkc.Do(req)
	if derr != nil {
		return 0, nil, c.faceUnreachable(derr)
	}
	raw, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// Input implements contract.BrowserTakeover.
func (c *Client) Input(sid string, ev contract.BrowserInput) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("browser takeover: encode input: %v", err)
	}
	status, raw, err := c.takeoverPost(sid, "/takeover/input", body)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("browser takeover: input rejected (HTTP %d): %s", status, strings.TrimSpace(string(raw)))
	}
	return nil
}

// SaveLogin implements contract.BrowserTakeover: posts /save so browserd captures the worker
// copy's live login (cookies incl session cookies) into its member master sidecar. The copy
// stays live — the worker resumes on the SAME now-logged-in copy.
func (c *Client) SaveLogin(ctx context.Context, scope string) error {
	return c.post(ctx, "/save", saveReq{Scope: scope}, nil)
}

// LiveForSid implements contract.BrowserTakeover: control-plane /live read (never
// spawns chromium). Conservative — a transport error or undecidable answer → false.
func (c *Client) LiveForSid(ctx context.Context, sid string) (bool, error) {
	var data liveData
	if err := c.post(ctx, "/live", liveReq{Scope: sid}, &data); err != nil {
		return false, err
	}
	return data.Live, nil
}

// LiveSids implements contract.BrowserTakeover: the custodian's live profile
// checkouts; in trunk a profile key IS the sid, so the keys are the live sids.
func (c *Client) LiveSids(ctx context.Context) ([]string, error) {
	var data profilesData
	if err := c.post(ctx, "/profiles", profilesReq{}, &data); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(data.Profiles))
	for _, p := range data.Profiles {
		if p.Key != "" {
			out = append(out, p.Key)
		}
	}
	return out, nil
}
