package tools

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// denyEgressProxy is a tiny forward proxy that REFUSES to connect to blocked
// (loopback / RFC1918 / link-local / cloud-metadata / CGNAT…) addresses — the SSRF
// egress guard for the scrapling subprocess.
//
// Why a proxy and not an in-process dialer: scrapling runs as a subprocess driving a
// real browser (playwright) that follows redirects ITSELF, so a Go DialContext never
// sees the redirect hops. By pointing ONLY the scrapling subprocess's HTTP(S)_PROXY
// env at this proxy, EVERY request and every redirect tunnels through here, where the
// target host is resolved and checked against isBlockedIP before we dial it. It is
// scoped to the subprocess env, so bob's own llama / pg connections never use it.
//
// It is a strict ADD on top of the existing pre-launch CheckURLHost: get/post already
// disable redirects and are fully guarded by that check; this closes the
// fetch/stealthy-fetch post-redirect hole. dialChecked dials the RESOLVED IP (not the
// hostname) so a DNS rebind between check and connect can't slip a private IP through.
type denyEgressProxy struct {
	url string
}

var (
	denyProxyOnce sync.Once
	denyProxy     *denyEgressProxy
)

// egressProxyURL lazily starts the singleton proxy and returns its URL
// ("http://127.0.0.1:<port>"), or "" if it could not start (caller then runs without
// it — CheckURLHost still guards the initial host, so this only loses redirect-hop
// coverage, never fails the fetch).
func egressProxyURL() string {
	denyProxyOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			slog.Warn("scrape: deny-egress proxy failed to listen — redirect SSRF guard disabled", "err", err)
			return
		}
		denyProxy = &denyEgressProxy{url: "http://" + ln.Addr().String()}
		srv := &http.Server{Handler: http.HandlerFunc(denyProxyHandle), ReadHeaderTimeout: 15 * time.Second}
		go func() { _ = srv.Serve(ln) }()
	})
	if denyProxy == nil {
		return ""
	}
	return denyProxy.url
}

// dialChecked resolves addr ("host:port"), refuses if ANY resolved IP is blocked,
// then dials the first allowed IP directly (rebind-safe).
func dialChecked(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if blocked, reason := isBlockedIP(ip); blocked {
			return nil, fmt.Errorf("scrape: blocked egress to %s — %s", host, reason)
		}
	}
	return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

// denyProxyTransport carries the checked dialer for plain-HTTP forward proxying.
var denyProxyTransport = &http.Transport{DialContext: dialChecked}

func denyProxyHandle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		denyProxyConnect(w, r)
		return
	}
	// Plain-HTTP forward proxy (absolute-form request). The checked dialer blocks a
	// private target; RequestURI must be cleared for a client round-trip.
	r.RequestURI = ""
	resp, err := denyProxyTransport.RoundTrip(r)
	if err != nil {
		http.Error(w, "blocked or unreachable: "+err.Error(), http.StatusForbidden)
		return
	}
	defer resp.Body.Close()
	// Strip hop-by-hop headers (RFC 7230 §6.1) before relaying upstream→client — a
	// forwarded Transfer-Encoding / Connection would otherwise corrupt the client's
	// framing. Mirrors net/http/httputil.ReverseProxy.
	removeHopByHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// hopByHopHeaders are the connection-specific headers a proxy must not forward
// end-to-end (RFC 7230 §6.1).
var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// removeHopByHopHeaders deletes from h any header named in its own Connection token
// list, then the standard hop-by-hop set.
func removeHopByHopHeaders(h http.Header) {
	for _, f := range h["Connection"] {
		for _, sf := range strings.Split(f, ",") {
			if sf = strings.TrimSpace(sf); sf != "" {
				h.Del(sf)
			}
		}
	}
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
}

// denyProxyConnect handles HTTPS tunnels: check+dial the target, then splice the two
// conns (the browser does its own TLS to the original host inside the tunnel, so SNI
// is unaffected by dialing the resolved IP).
func denyProxyConnect(w http.ResponseWriter, r *http.Request) {
	target, err := dialChecked(r.Context(), "tcp", r.Host)
	if err != nil {
		http.Error(w, "blocked: "+err.Error(), http.StatusForbidden)
		return
	}
	defer target.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) { _, _ = io.Copy(dst, src); done <- struct{}{} }
	go cp(target, client)
	go cp(client, target)
	<-done // first half to close ends the tunnel; defers close the other
}
