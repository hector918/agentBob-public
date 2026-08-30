package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
	"time"

	"agentbob/contract"
)

// cleartextPlainAuth is AUTH PLAIN that, unlike smtp.PlainAuth, does NOT
// refuse to run over an unencrypted connection. It exists solely for the
// security:none transport, where the operator has explicitly accepted
// sending credentials in the clear (compatibility with servers that can't do
// TLS). For tls / starttls we keep stdlib smtp.PlainAuth so its cleartext
// guard still protects the normal case.
type cleartextPlainAuth struct {
	username, password string
}

func (a cleartextPlainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}

func (a cleartextPlainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge in cleartext PLAIN auth")
	}
	return nil, nil
}

// sendReq is one queued outbound email.
//
// body is already a fully-rendered RFC 2822 message (headers + body joined
// with CRLF). The send worker hands it straight to the SMTP session — no
// further mutation, no header inspection.
type sendReq struct {
	to        string // canonical recipient address (log lines + envelope primary)
	body      []byte
	queuedAt  time.Time
	messageID string // minted Message-ID of the rendered body (reply-routing anchors fire at enqueue time, not here)

	// outboundID is the outbound-log file id backing this send (durability
	// between enqueue and SMTP-accept). "" when the log is disabled (no file
	// store). Set by enqueueSend for a fresh send; carried pre-set by a boot
	// replay so enqueueSend does not re-record it. The send worker deletes the
	// file on SMTP success and keeps it (for the next boot's replay) on
	// permanent failure. See 26-outbound.go.
	outboundID string
}

// runSendWorker drains sendCh until ctx is cancelled. Uses a fresh SMTP dial
// per message (simpler than keep-alive — modern hosts are happy with that
// and reconnect handling becomes "always reconnect"). On send failure,
// retries with a short backoff up to sendRetryAttempts.
//
// sendCh is bounded (cap 50). When full, enqueueSend returns an error;
// Send/sendReply propagates that as REJECTION rather than blocking the
// gateway. A real outage gets caller back-pressure instead of unbounded
// growth.
func (s *Source) runSendWorker(ctx context.Context) {
	const (
		sendRetryAttempts = 3
		sendBackoff       = 5 * time.Second
		sendMaxBackoff    = 30 * time.Second
	)
	for {
		select {
		case <-ctx.Done():
			if remaining := len(s.sendCh); remaining > 0 {
				slog.Warn("email: shutdown — dropping queued sends",
					"source", s.Name(), "count", remaining)
			}
			return
		case req, ok := <-s.sendCh:
			if !ok {
				return
			}
			backoff := sendBackoff
			var lastErr error
			for attempt := 0; attempt < sendRetryAttempts; attempt++ {
				if ctx.Err() != nil {
					return
				}
				err := s.sendOnce(ctx, req)
				if err == nil {
					lastErr = nil
					break
				}
				lastErr = err
				slog.Warn("email: SMTP send failed, will retry",
					"source", s.Name(), "to", req.to,
					"attempt", attempt+1, "err", s.redact(err.Error()))
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				backoff *= 2
				if backoff > sendMaxBackoff {
					backoff = sendMaxBackoff
				}
			}
			if lastErr != nil {
				slog.Error("email: SMTP send permanently failed",
					"source", s.Name(), "to", req.to,
					"queued_ago", time.Since(req.queuedAt),
					"err", s.redact(lastErr.Error()))
				// KEEP the outbound-log file (a permanent failure replays on the
				// next boot within the TTL) and summon the admin — a silently
				// dropped reply is exactly what F24 was about.
				s.notifyAdmin(contract.AdminError,
					"SMTP send to %s permanently failed after %d attempts (queued %s ago); will retry on next restart: %s",
					req.to, sendRetryAttempts, time.Since(req.queuedAt).Round(time.Second), s.redact(lastErr.Error()))
			} else {
				// Delivered — the server owns the message now; drop its recovery file.
				s.outboundDelete(req.outboundID)
			}
		}
	}
}

// smtpSessionTimeout bounds the ENTIRE SMTP conversation once dialled.
// dialer.Timeout / HandshakeContext only cover dial + handshake; after that
// the blocking reads/writes have no deadline. A server that accepts the TCP
// connection but never replies would otherwise wedge the single send worker
// forever. A one-shot conn.SetDeadline caps the whole session.
const smtpSessionTimeout = 60 * time.Second

// smtpMinThroughput is the worst-case uplink assumed when growing the
// session deadline for a large body (attachment sends). A flat timeout would
// make a 25 MiB attachment send physically impossible on slow uplinks.
const smtpMinThroughput = 64 * 1024 // bytes/second

// smtpDeadline returns the one-shot session deadline for a body of n bytes.
func smtpDeadline(n int) time.Duration {
	return smtpSessionTimeout + time.Duration(n/smtpMinThroughput)*time.Second
}

// sendOnce dials SMTP, STARTTLS, AUTHs, sends, QUITs. Returns the first
// error. The dial+handshake are bounded by the net.Dialer / HandshakeContext;
// the post-dial conversation is bounded by conn.SetDeadline.
func (s *Source) sendOnce(parent context.Context, req sendReq) error {
	cfg := &s.cfg
	addr := fmt.Sprintf("%s:%d", cfg.SMTP.Host, cfg.SMTP.Port)
	if err := parent.Err(); err != nil {
		return err
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tlsConfig := &tls.Config{ServerName: cfg.SMTP.Host}

	// Dial per configured Security. tls = implicit TLS from connect (465):
	// handshake before NewClient. starttls / none = plain TCP first; the
	// upgrade (or lack of it) happens after EHLO below.
	var conn net.Conn
	if cfg.SMTP.Security == secTLS {
		tcp, derr := dialer.DialContext(parent, "tcp", addr)
		if derr != nil {
			return fmt.Errorf("smtp.Dial: %w", derr)
		}
		tlsConn := tls.Client(tcp, tlsConfig)
		if herr := tlsConn.HandshakeContext(parent); herr != nil {
			_ = tcp.Close()
			return fmt.Errorf("smtp TLS handshake: %w", herr)
		}
		conn = tlsConn
	} else {
		tcp, derr := dialer.DialContext(parent, "tcp", addr)
		if derr != nil {
			return fmt.Errorf("smtp.Dial: %w", derr)
		}
		conn = tcp
	}
	// Bound the whole post-dial conversation. Scaled by body size so a large
	// attachment body still fits the budget.
	if derr := conn.SetDeadline(time.Now().Add(smtpDeadline(len(req.body)))); derr != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp set deadline: %w", derr)
	}
	c, err := smtp.NewClient(conn, cfg.SMTP.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp.NewClient: %w", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Hello(localHostnameOr("localhost")); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}
	// STARTTLS upgrade on the submission port. If the server doesn't
	// advertise it, refuse — sending an App Password over cleartext would
	// leak it. (Skipped for tls: already encrypted; and for none: the
	// operator explicitly opted out of encryption.)
	if cfg.SMTP.Security == secSTARTTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("server at %s does not advertise STARTTLS; refusing cleartext auth", addr)
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	// Auth. On an encrypted link use the stdlib PlainAuth (which refuses to
	// send credentials over cleartext). For security:none the operator
	// accepted cleartext, so use a wrapper that skips that guard.
	var auth smtp.Auth
	if cfg.SMTP.Security == secNone {
		auth = cleartextPlainAuth{username: cfg.LoginUser(), password: s.password}
	} else {
		auth = smtp.PlainAuth("", cfg.LoginUser(), s.password, cfg.SMTP.Host)
	}
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("AUTH: %w", err)
	}
	if err := c.Mail(cfg.Address); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	// RCPT TO = the primary recipient + every address in the message's own
	// To/Cc headers (reply-all). Deriving the envelope from the body means a
	// future crash-recovery replay (which would persist only the rendered
	// body) still reaches the full reply-all set.
	rcpts := envelopeRecipients(req.body, req.to)
	if len(rcpts) == 0 {
		return fmt.Errorf("RCPT TO: no recipients")
	}
	for _, rcpt := range rcpts {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", rcpt, err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := wc.Write(req.body); err != nil {
		_ = wc.Close()
		return fmt.Errorf("DATA write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("DATA close: %w", err)
	}
	// DATA close above read the server's 2xx acceptance — the message is
	// COMMITTED, the server owns it now. QUIT is just a politeness teardown;
	// its failure says nothing about delivery. Returning the QUIT error here
	// would make runSendWorker's retry loop re-send the SAME body (same
	// Message-ID) → the recipient gets a DUPLICATE. Downgrade QUIT to
	// best-effort: log at debug, return nil.
	if err := c.Quit(); err != nil {
		slog.Debug("email: SMTP QUIT failed after message accepted — delivery already committed, not retrying",
			"source", s.Name(), "to", req.to, "err", s.redact(err.Error()))
	}
	return nil
}

// envelopeRecipients computes the SMTP RCPT TO set for an outbound message:
// the primary recipient plus every address in the rendered message's own
// To + Cc headers (reply-all), canonicalised and de-duplicated with the
// primary first. A parse failure, or a body with no extra recipients,
// degrades to just the primary, so even a malformed body still reaches the
// original sender.
func envelopeRecipients(body []byte, primary string) []string {
	seen := map[string]bool{}
	var out []string
	if p := CanonicalAddress(primary); p != "" {
		seen[p] = true
		out = append(out, p)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil {
		return out
	}
	for _, hdr := range []string{"To", "Cc"} {
		addrs, perr := msg.Header.AddressList(hdr)
		if perr != nil {
			continue
		}
		for _, a := range addrs {
			ca := CanonicalAddress(a.Address)
			if ca == "" || seen[ca] {
				continue
			}
			seen[ca] = true
			out = append(out, ca)
		}
	}
	return out
}

// enqueueSend is the non-blocking enqueue helper. Returns nil on successful
// queueing; non-nil if the queue is full or ctx is done. Caller treats
// non-nil as REJECTION (not delivery failure).
//
// Durability (F24): before enqueue the rendered message is persisted to the
// outbound log (when a file store is present), so a crash between here and
// SMTP-accept replays it on the next boot instead of losing it. The file is
// deleted by the send worker on SMTP success. A FRESH send that is REJECTED
// here (queue full / ctx done) deletes its just-written file — the caller is
// told it failed, so a replay must not resurrect it. A REPLAY (outboundID
// pre-set) keeps its file on rejection for the next boot to retry.
func (s *Source) enqueueSend(ctx context.Context, req sendReq) error {
	recorded := false
	if s.outbound != nil && req.outboundID == "" {
		if id, err := s.outbound.record(req); err == nil {
			req.outboundID, recorded = id, true
		}
	}
	select {
	case s.sendCh <- req:
		return nil
	case <-ctx.Done():
		if recorded {
			s.outboundDelete(req.outboundID)
		}
		return ctx.Err()
	default:
		if recorded {
			s.outboundDelete(req.outboundID)
		}
		slog.Error("email: send queue FULL — message dropped",
			"source", s.Name(), "to", req.to, "queue_size", cap(s.sendCh),
			"hint", "SMTP worker likely wedged on a slow upstream — check connectivity")
		s.notifyAdmin(contract.AdminError,
			"send queue full (cap %d) — a reply to %s was DROPPED; the SMTP worker is likely wedged on a slow upstream",
			cap(s.sendCh), req.to)
		return fmt.Errorf("email: send queue full (cap %d)", cap(s.sendCh))
	}
}

// localHostnameOr returns the system hostname or the fallback if the lookup
// fails. Used as the EHLO name.
func localHostnameOr(fallback string) string {
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		return h
	}
	return fallback
}
