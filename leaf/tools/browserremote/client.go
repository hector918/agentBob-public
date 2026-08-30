// Package browserremote is the trunk-side thin client for the external
// browserd service (docs/browserd.md). It backs ONE model-facing `browser` tool
// — an action-dispatched tool whose `action` enum fans out to browserd's
// endpoints (was 13 separate browser_* tools). Each Run is: parse args → one HTTP
// call to browserd, which owns chromium + all pool/profile state.
//
// The `browser` tool is ALWAYS registered (leaf/tools/00-module.go) so the
// capability stays visible; when BROWSERD_URL is unset/down its Run reports the
// backend unavailable rather than vanishing. BROWSERD_URL non-empty additionally
// gates the contract.BrowserTakeover Provide (the live takeover seam below) — it
// does NOT gate the tool itself.
//
// PORTED from skeleton (tools/browserremote/client.go) and DELIBERATELY
// SELF-CONTAINED: the skeleton client imported agentbob/browserd (the wire
// request/response types), agentbob/config (BrowserConfig), agentbob/tools/browser
// (ProfileIdentity gate + ControlHolds + result types) and agentbob/tools/neterror.
// NONE of those packages exist in the trunk-rebuild tree, so this port inlines
// the MINIMAL wire surface (Common + Response + the per-action req/resp shapes)
// rather than depending on them. The HTTP wire contract toward browserd is
// unchanged — a browserd built from skeleton answers these requests verbatim.
//
// DROPPED vs skeleton (flagged in the port report, not silently lost):
//   - permission gate (browser.ProfileIdentity): trunk's warrant authorizes the
//     ToolSet up front; there is no in-Run gate seam. profile_key is derived from
//     the sid until a trunk profile-identity mechanism lands.
//   - profile backup faces (backup.go): served the housekeeping sweep, absent in trunk.
//
// The human control-hold gate (browser takeover yield) IS wired: the tool checks
// contract.BrowserControlHold before each action (leaf/tools/25-control-hold.go),
// driven by leaf/webui's control-mode toggle.
//
// RE-ADDED for the takeover port (takeover.go, docs/wip-takeover-port.md): the
// takeover proxy (screencast/input, api-key authed) + live/profiles control calls
// — contract.BrowserTakeover, keyed by sid, consumed by leaf/webui.
package browserremote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// pageTimeoutSeconds is the assumed browserd-side per-page budget. Skeleton read
// this from config.BrowserConfig.PageTimeoutSecondsEff(); trunk has no browser
// config section (mirrors the scrapling/web_search ports, which inline their own
// defaults), so it is fixed at the skeleton default (45s).
const pageTimeoutSeconds = 45

// transportTimeoutMarginSeconds is the fixed slack added to the SERVER-side
// worst-case wall time to form the HTTP client timeout. The server worst case is
// TWO page timeouts (an idle-closed instance auto-resume-navigates before the
// requested action, both inside one HTTP request), so the client must budget for
// both plus this margin, else a live-but-slow browserd is misreported as
// unreachable. Ported verbatim from skeleton.
const transportTimeoutMarginSeconds = 30

// maxRespBytes caps a browserd response body. The largest legitimate payload is
// /vision's base64 full-page PNG (a few MB); 32 MB is comfortable headroom while
// still bounding a misbehaving endpoint.
const maxRespBytes = 32 << 20

// Common is the routing envelope every action request self-carries. browserd is
// stateless toward bob; each call names its scope + profile. Inlined from
// skeleton's browserd.Common (the only fields the shells set).
type Common struct {
	Scope      string `json:"scope"`
	ProfileKey string `json:"profile_key"`
	// Mode routes the profile (browser profile identity Phase 2): "copy" = a read-only
	// per-session worker copy seeded from the principal's master vault dir (own instance,
	// concurrent, discarded on close); "" = in-place on the vault dir (the login session a
	// human takes over — the master's single writer). Worker actions send "copy";
	// the human-controlled login copy is the only writer of the master.
	Mode string `json:"mode,omitempty"`
}

// Response is browserd's uniform reply envelope: {ok, error?, data?}. Inlined
// from skeleton's browserd.Response.
type Response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Client talks to one browserd instance. It is stateless toward browserd (every
// request self-carries {scope, profile_key}). Unlike skeleton it holds NO
// per-scope navigated marker (no SpecForSession gating in trunk) and NO control-
// hold registry.
type Client struct {
	base         string
	takeoverBase string // browserd takeover face (control host + :8378); "" if base unparseable
	apiKey       string // browserd control-plane bearer secret (A1); also auths the takeover face; empty → no header
	hc           *http.Client
	tkc          *http.Client // takeover face: no overall timeout (SSE outlives it), only a header wait
}

// New builds the client against baseURL (e.g. http://browserd:8377). apiKey is the
// optional control-plane bearer secret ($BROWSERD_API_KEY): when non-empty every
// request carries `Authorization: Bearer <apiKey>` (browserd's A1 gate, 401 without
// it — docs/browserd.md). Empty matches a browserd with no key configured. The HTTP
// timeout budgets two page timeouts plus the transport margin.
func New(baseURL, apiKey string) *Client {
	timeout := time.Duration(2*pageTimeoutSeconds+transportTimeoutMarginSeconds) * time.Second
	// Fail-loud on the dangerous keyless config: a configured browserd whose api key
	// is empty leaves the takeover face (screencast/input of a possibly-logged-in
	// browser) and control plane reachable with no Authorization header — guarded only
	// by network isolation. Keyless is supported (matches a keyless browserd) but must
	// never ship silently. (L-IO-D1)
	if strings.TrimSpace(baseURL) != "" && strings.TrimSpace(apiKey) == "" {
		slog.Warn("browserremote: BROWSERD_URL set but BROWSERD_API_KEY empty — browserd takeover face + control plane are UNAUTHENTICATED (relying on network isolation only)",
			"url", baseURL)
	}
	return &Client{
		base:         strings.TrimRight(baseURL, "/"),
		takeoverBase: takeoverBaseURL(baseURL),
		apiKey:       strings.TrimSpace(apiKey),
		hc:           &http.Client{Timeout: timeout},
		tkc:          &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: takeoverHeaderTimeout}},
	}
}

// Configured reports whether a browserd base URL was set (BROWSERD_URL). When
// false the tool is still registered (visible) but every Run reports the backend
// unavailable instead of attempting a connection.
func (c *Client) Configured() bool { return c.base != "" }

// transportError marks failures where browserd never delivered an action verdict:
// unreachable service, ctx timeout, unreadable / non-JSON reply. The distinction
// matters for browser_navigate's DNS-hint classifier — transport net-errors
// ("connection refused", "no such host") must NOT be hinted as "the TARGET domain
// doesn't exist" when the target was never touched. Action-level ok:false errors
// stay plain errors. Ported verbatim from skeleton.
type transportError struct{ msg string }

func (e *transportError) Error() string { return e.msg }

func transportErrf(format string, a ...any) error {
	return &transportError{msg: fmt.Sprintf(format, a...)}
}

// isTransportError reports whether err came from the transport/protocol layer
// rather than from browserd's action handler.
func isTransportError(err error) bool {
	var te *transportError
	return errors.As(err, &te)
}

// post sends one control-plane call and decodes the Data payload into out
// (skipped when out is nil). Errors are either *transportError (browserd
// unreachable / non-JSON) or a plain error carrying browserd's ok:false text.
func (c *Client) post(ctx context.Context, path string, in any, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return transportErrf("browser service: encode request: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return transportErrf("browser service: build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" { // browserd A1 control-plane gate — every endpoint requires this
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// browserd is configured but unreachable (down / restarting). Lead with a clean
		// model-facing message — the symmetric counterpart to the unconfigured guard —
		// keeping the net cause in parens for ops. Still a *transportError, so the
		// DNS-hint classifier keeps skipping it.
		return transportErrf("浏览器后端（browserd）暂时不可用，可能未启动或正在重启，请稍后再试或让管理员检查（%v）", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return transportErrf("browser service: read response: %v", err)
	}
	var env Response
	if err := json.Unmarshal(raw, &env); err != nil {
		return transportErrf("browser service: unexpected response (HTTP %d)", resp.StatusCode)
	}
	if !env.OK {
		// A browserd-SIDE fault (auth / 5xx) is a transport problem, not a page-level
		// action failure — wrap as *transportError so the DNS-hint classifier skips it and
		// callers treat it distinctly from a real action failure.
		if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode >= 500 {
			if env.Error != "" {
				return transportErrf("browser service error (HTTP %d): %s", resp.StatusCode, env.Error)
			}
			return transportErrf("browser service error (HTTP %d)", resp.StatusCode)
		}
		if env.Error == "" {
			return fmt.Errorf("browser service error (HTTP %d)", resp.StatusCode)
		}
		return errors.New(env.Error)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return transportErrf("browser service: malformed payload: %v", err)
		}
	}
	return nil
}
