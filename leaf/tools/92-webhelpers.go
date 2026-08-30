package tools

// Web helpers ported from skeleton (tools/web/05-ssrf.go, tools/neterror,
// tools/textcap) — only the parts the scrapling tool uses. Kept package-private
// to leaf/tools; not a re-export of the old skeleton packages.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// --- SSRF guard (ported from tools/web/05-ssrf.go) ----------------------
//
// CheckURLHost is a pre-flight check the caller runs on a URL host before
// launching the scrapling subprocess. Scrapling runs as a subprocess with its
// own network stack, so an in-process DialContext check doesn't reach it; this
// pre-check refuses a URL whose host resolves to a blocked range (loopback,
// RFC1918, link-local / cloud-metadata, etc) before the subprocess launches.
// Race against DNS rebinding is accepted v1 (short TTL resolver caches + the
// agent acts on behalf of an admin, not arbitrary remote users).
//
// Returns nil if the host resolves entirely to publicly routable addresses, or
// an error describing the first blocked address.
func CheckURLHost(host string) error {
	if host == "" {
		return fmt.Errorf("ssrf-guard: empty host")
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil {
		if blocked, reason := isBlockedIP(ip); blocked {
			return fmt.Errorf("ssrf-guard: %s is blocked (%s)", ip, reason)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("ssrf-guard: resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if blocked, reason := isBlockedIP(ip); blocked {
			return fmt.Errorf("ssrf-guard: %s resolves to %s (%s)", host, ip, reason)
		}
	}
	return nil
}

// hostFromTarget extracts the bare host from a URL string. Defensive — returns
// "" on parse failure (the CheckURLHost empty-host branch then refuses).
func hostFromTarget(target string) string {
	i := strings.Index(target, "://")
	if i < 0 {
		return ""
	}
	rest := target[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	// Strip any userinfo ("user:pass@") so the SSRF guard checks the real host, not
	// a host smuggled into the credential component. Userinfo always precedes the
	// host, so the LAST '@' bounds it (a bare host/IPv4/[IPv6] carries no '@').
	if at := strings.LastIndexByte(rest, '@'); at >= 0 {
		rest = rest[at+1:]
	}
	if h, _, err := net.SplitHostPort(rest); err == nil {
		return h
	}
	// No port: a bracketed IPv6 literal (e.g. "[::1]") keeps its brackets here,
	// which net.LookupIP can't parse → strip them so the IP check still runs (and
	// still blocks loopback/private). A bare host / IPv4 is unaffected by the trim.
	return strings.Trim(rest, "[]")
}

// SafeHTTPClient returns an http.Client wired with a safe dial + redirect cap +
// the supplied timeout. Unlike scrapling (a subprocess with its own network
// stack, pre-checked via CheckURLHost), web_search fetches SERPs in-process — so
// it installs this DialContext-level guard that re-resolves the target host AND
// verifies every resolved IP is publicly routable BEFORE the TCP connect.
// Multi-A records, IPv4/IPv6, and redirects all go through the same check, which
// also defeats DNS rebinding by pinning the connection to the vetted IP.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: safeTransport(),
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("ssrf-guard: too many redirects (%d)", len(via))
			}
			// The redirected request goes through the same safeTransport, so the
			// DialContext host check fires again; this stop is just the count cap.
			return nil
		},
	}
}

func safeTransport() http.RoundTripper {
	base := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.LookupIP(host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("ssrf-guard: no addresses for %s", host)
		}
		for _, ip := range ips {
			if blocked, reason := isBlockedIP(ip); blocked {
				return nil, fmt.Errorf("ssrf-guard: %s resolves to %s (%s)", host, ip, reason)
			}
		}
		// Pin the connection to a vetted IP to defeat DNS rebinding (dialing by
		// host:port would re-resolve and a hostile server could answer 127.0.0.1
		// the second time). The Host header stays on the Request so TLS SNI /
		// vhost routing still work.
		newAddr := net.JoinHostPort(ips[0].String(), port)
		return dialer.DialContext(ctx, network, newAddr)
	}
	return base
}

// extraBlockedV4Nets — ranges IsLoopback/IsPrivate/IsLinkLocalUnicast don't
// catch but which an LLM-driven web tool still must not reach (CGNAT, benchmark,
// TEST-NET-1/2/3). IPv4-mapped IPv6 unwraps via To4() so the v4 checks catch it.
var extraBlockedV4Nets = func() []*net.IPNet {
	cidrs := []string{
		"100.64.0.0/10",
		"198.18.0.0/15",
		"192.0.2.0/24",
		"198.51.100.0/24",
		"203.0.113.0/24",
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// isBlockedIP returns (true, reason) for every IP an LLM-driven web tool must
// NOT reach. Public IPv4/IPv6 return (false, "").
func isBlockedIP(ip net.IP) (bool, string) {
	if ip == nil {
		return true, "nil ip"
	}
	if ip.IsLoopback() {
		return true, "loopback"
	}
	if ip.IsUnspecified() {
		return true, "unspecified (0.0.0.0/::) "
	}
	if ip.IsLinkLocalUnicast() {
		return true, "link-local (covers cloud metadata 169.254.169.254)"
	}
	if ip.IsLinkLocalMulticast() {
		return true, "link-local multicast"
	}
	if ip.IsInterfaceLocalMulticast() {
		return true, "interface-local multicast"
	}
	if ip.IsMulticast() {
		return true, "multicast"
	}
	if ip.IsPrivate() {
		return true, "private (RFC1918 / IPv6 ULA)"
	}
	if v4 := ip.To4(); v4 != nil {
		for _, n := range extraBlockedV4Nets {
			if n.Contains(v4) {
				return true, "non-routable v4 (" + n.String() + ")"
			}
		}
	}
	return false, ""
}

// --- network-error classifier (ported from tools/neterror) --------------

// dnsHint is the canned redirection text for DNS / unreachable-host failures.
// Prescriptive: names a concrete next step instead of abstract "search
// instead" — small models follow concrete instructions far better.
const dnsHint = "If you were guessing this URL from memory, stop. " +
	"Search first: call browser with action=browser_navigate and " +
	"\"https://search.brave.com/search?q=YOUR+QUERY+HERE\" then action=browser_snapshot " +
	"to read the result list and pick a real URL. " +
	"Do NOT try other variations of the same brand name — DNS errors mean " +
	"the domain doesn't exist, not that you should retry with a different path."

var dnsPatterns = []string{
	"ERR_NAME_NOT_RESOLVED",
	"ERR_NAME_RESOLUTION_FAILED",
	"ERR_INTERNET_DISCONNECTED",
	"ERR_CONNECTION_REFUSED",
	"ERR_ADDRESS_UNREACHABLE",
	"no such host",
	"server misbehaving",
	"connection refused",
	"network is unreachable",
	"ConnectError",
	"Cannot connect to host",
	"NameResolutionError",
	"getaddrinfo failed",
}

// isDNSClass reports whether the message indicates a DNS / connection failure
// the user should fix by searching for a real URL rather than retrying.
func isDNSClass(msg string) bool {
	if msg == "" {
		return false
	}
	for _, p := range dnsPatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// --- text cap (ported from tools/textcap) -------------------------------

// defaultMaxChars is the cap floor — 16 KiB ≈ 4K tokens. Stops a fat response
// from blowing the context window before it ever lands in chat history.
const defaultMaxChars = 16 * 1024

// applyTextCap returns (capped, truncated, totalBytes). The cap is measured in
// BYTES (len), not runes; when the input fits it's returned unchanged with
// truncated=false, otherwise the byte-prefix that fits inside (max - markerLen) is
// kept — snapped back to a rune boundary — and a truncation marker appended.
func applyTextCap(s string, max int) (out string, truncated bool, total int) {
	total = len(s)
	if max <= 0 {
		max = defaultMaxChars
	}
	if total <= max {
		return s, false, total
	}
	marker := fmt.Sprintf(
		"\n\n... [truncated by tool — kept the first %d of %d bytes. Use a CSS selector, a more specific URL, or paginate to get the rest.]",
		0, total,
	)
	keep := max - len(marker)
	if keep < 0 {
		keep = 0
		if max < len(marker) {
			return marker[:max], true, total
		}
	}
	marker = fmt.Sprintf(
		"\n\n... [truncated by tool — kept the first %d of %d bytes. Use a CSS selector, a more specific URL, or paginate to get the rest.]",
		keep, total,
	)
	keep = max - len(marker)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && keep < len(s) && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + marker, true, total
}

// --- small text utilities (ported from core.TruncateRunes / web helpers) -

// truncateRunes clips s to at most maxBytes bytes without splitting a rune.
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	keep := maxBytes
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep]
}

// snippet — short preview of a response body for error logs.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "(empty body)"
	}
	if len(s) > 400 {
		return truncateRunes(s, 400) + "…"
	}
	return s
}
