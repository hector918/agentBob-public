// Package email implements the email IMAP+SMTP Source (one Source struct
// per configured email account) on the trunk-rebuild clean-room
// architecture.
//
// Library choice: IMAP via github.com/emersion/go-imap/v2
// (v2 API replaces the long-deprecated v1) + MIME parsing via
// github.com/emersion/go-message (its mail subpackage handles
// transfer-encoding decoding, character-set conversion, and the
// inline/attachment split for us). SMTP send goes through stdlib
// net/smtp + crypto/tls — no new dep, and STARTTLS on submission port
// 587 is well-trodden territory there.
//
// REBUILD scope: IMAP receive -> contract.MessageEvent, SMTP send via the
// bespoke block sink, MIME parse, reply-builder, attachment capture
// (files store), quote-strip, forwarded-alias. Access control
// (allow/deny/admin) is the gate's job (the inbound screen runs above
// this layer). Per-send stats are deferred; the persistent crash-recovery
// outbound log is ported (26-outbound.go, F24), as are reply-coalescing
// (75-coalesce.go) and message_index reply-routing (the streamsink
// OnAnchor→RecordSent path).
package email

import (
	"strings"

	// Side-effect import: registers a charset decoder with go-message so
	// non-UTF-8 mail (gb2312 / ISO-2022-JP / windows-1252 / etc) is
	// transcoded during parseMIME instead of arriving as mojibake.
	_ "github.com/emersion/go-message/charset"
)

// EmailConfig is the connection config for one email account. The
// integrator wires it (reading host/port from its own config surface);
// the password is read from PassEnv at New time. There is no access
// config here — the gate owns allow/deny/admin.
type EmailConfig struct {
	// SourceID is the per-account source name (e.g. "email-support"). It is the
	// source's Name() and the scope prefix the gateway routes by (source+":dm:"
	// etc.) — no special "email" prefix is required.
	SourceID string `yaml:"source_id"`

	// Enabled is the off-switch, for parity with telegram/feishu/discord (which
	// require enabled:true). Email is historically presence-gated, so nil = enabled
	// (backward-compatible — an existing block without the key keeps working); set
	// `enabled: false` to disable an account without deleting its config.
	Enabled *bool `yaml:"enabled"`

	// Address is bob's own mailbox: the source polls it for inbound and
	// sends replies as it (the From header).
	Address string `yaml:"address"`

	// Username is the login name presented at IMAP LOGIN / SMTP AUTH when
	// it differs from Address (self-hosted / corporate servers). Empty →
	// authenticate as Address.
	Username string `yaml:"username"`

	IMAP HostPort `yaml:"imap"`
	SMTP HostPort `yaml:"smtp"`

	// PassEnv is the name of the env var holding the IMAP/SMTP password.
	// New reads os.Getenv(PassEnv); the password is never in config.
	PassEnv string `yaml:"pass_env"`

	PollIntervalSeconds int    `yaml:"poll_interval_seconds"`
	Folder              string `yaml:"folder"`

	// MarkSeen defaults to true (applyDefaults). When false bob must NOT
	// touch \Seen on ANY path so a human co-reader still sees every
	// message.
	MarkSeen           *bool `yaml:"mark_seen"`
	DeleteAfterProcess bool  `yaml:"delete_after_process"`

	MaxAttachmentBytes int64 `yaml:"max_attachment_bytes"`

	StripInboundQuotes *bool `yaml:"strip_inbound_quotes"`
	QuoteMaxLines      int   `yaml:"quote_max_lines"`

	// RequiredModelTags hard-constrains the model for turns from this mailbox: the
	// source advertises it via Caps and the flow maps it to ModelRequest.Requires, so
	// the pool serves only entries carrying all these tags (picker chooses among them)
	// or fails rather than falling back. Empty → any model. e.g. ["email"].
	RequiredModelTags []string `yaml:"required_model_tags"`

	// ForwardedDomains marks this mailbox as a forwarding target (e.g. a
	// Gmail that Cloudflare Email Routing forwards agent@/sales@/… of your
	// own domain into). When set, the source resolves the ORIGINAL
	// recipient alias from the forwarded headers and routes per-alias.
	// Empty = feature off (plain single-mailbox behaviour). Already
	// lowercased/trimmed by applyDefaults.
	ForwardedDomains []string `yaml:"forwarded_domains"`

	// BatchWindowSeconds turns on reply-coalescing: a session's per-turn
	// replies are held and combined into ONE email, sent once the session has
	// been quiet (no new turn) for this many seconds. Email-specific — one mail
	// per inbound thread instead of one per turn. 0 (default) = off: each turn
	// replies immediately. The held buffer is persisted as a file
	// ($BOB_HOME/email-batch/<source>/<sid>.json) so a restart doesn't lose
	// computed-but-unsent replies. See 75-coalesce.go.
	BatchWindowSeconds int `yaml:"batch_window_seconds"`

	// BatchMaxMessages force-flushes the held buffer once it has combined this
	// many turn-replies, even mid-processing — so a huge batch goes out as
	// progress mails instead of one giant one and the buffer stays bounded.
	// Only used when BatchWindowSeconds > 0. Default 200.
	BatchMaxMessages int `yaml:"batch_max_messages"`
}

// HostPort is one host:port pair for IMAP / SMTP, plus the transport
// encryption mode.
//
// Security is one of:
//
//	tls      — implicit TLS from connect (IMAP 993, SMTP 465). The default.
//	starttls — connect cleartext then upgrade (IMAP 143, SMTP 587).
//	none     — NO encryption at all (cleartext on the wire, including the
//	           password). Only for servers that genuinely can't do TLS.
//
// Empty Security is derived from the well-known port (normalizeSecurity);
// aliases "ssl"→tls, "plain"→none are accepted.
type HostPort struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Security string `yaml:"security"`
}

// Security mode values for HostPort.Security (post-normalization).
const (
	secTLS      = "tls"
	secSTARTTLS = "starttls"
	secNone     = "none"
)

// ShouldMarkSeen reports whether the \Seen flag may be set on processed
// messages. MarkSeen is a *bool defaulting to true (applyDefaults); when
// the operator sets it false, bob must NOT touch \Seen on ANY path —
// success, dedup-skip, OR fetch/parse error.
func (a EmailConfig) ShouldMarkSeen() bool {
	return a.MarkSeen != nil && *a.MarkSeen
}

// LoginUser is the username presented at IMAP LOGIN / SMTP AUTH. It is the
// explicit Username when set, else the account Address — most providers
// authenticate by full email address, but self-hosted / corporate servers
// sometimes use a separate login name.
func (a EmailConfig) LoginUser() string {
	if u := strings.TrimSpace(a.Username); u != "" {
		return u
	}
	return a.Address
}

// applyDefaults fills the documented defaults on every optional field +
// normalises Security and ForwardedDomains. Run once in New before any
// network I/O.
func (a *EmailConfig) applyDefaults() {
	if a.PollIntervalSeconds <= 0 {
		a.PollIntervalSeconds = 30
	}
	if strings.TrimSpace(a.Folder) == "" {
		a.Folder = "INBOX"
	}
	if a.MarkSeen == nil {
		t := true
		a.MarkSeen = &t
	}
	if a.MaxAttachmentBytes <= 0 {
		a.MaxAttachmentBytes = 10 * 1024 * 1024
	}
	if a.StripInboundQuotes == nil {
		t := true
		a.StripInboundQuotes = &t
	}
	if a.QuoteMaxLines <= 0 {
		a.QuoteMaxLines = 5
	}
	if a.BatchMaxMessages <= 0 {
		a.BatchMaxMessages = 200
	}
	if a.IMAP.Port == 0 {
		a.IMAP.Port = 993
	}
	if a.SMTP.Port == 0 {
		a.SMTP.Port = 587
	}
	// Security after ports — empty derives from the (now-defaulted) port.
	a.IMAP.Security = normalizeSecurity(a.IMAP.Security, a.IMAP.Port)
	a.SMTP.Security = normalizeSecurity(a.SMTP.Security, a.SMTP.Port)
	// Forwarded domains: lowercase + trim, drop a leading "@", drop blanks.
	if len(a.ForwardedDomains) > 0 {
		cleaned := a.ForwardedDomains[:0]
		for _, d := range a.ForwardedDomains {
			d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@")))
			if d != "" {
				cleaned = append(cleaned, d)
			}
		}
		a.ForwardedDomains = cleaned
	}
}

// normalizeSecurity canonicalizes a Security string. Empty derives from the
// well-known port; an unrecognized value is returned unchanged so New can
// reject it loudly.
func normalizeSecurity(s string, port int) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case secTLS, "ssl":
		return secTLS
	case secSTARTTLS:
		return secSTARTTLS
	case secNone, "plain", "plaintext", "cleartext":
		return secNone
	case "":
		switch port {
		case 465, 993:
			return secTLS
		case 587, 143, 25:
			return secSTARTTLS
		default:
			return secTLS // safe default: encrypt unless told otherwise
		}
	default:
		return strings.ToLower(strings.TrimSpace(s)) // invalid → caught at New
	}
}

// validateSecurity rejects any Security value that isn't one of the three
// canonical modes (normalizeSecurity already lower-cased it).
func validateSecurity(which, sec string) string {
	switch sec {
	case secTLS, secSTARTTLS, secNone:
		return ""
	default:
		return which + ".security " + sec + " is invalid — use tls, starttls, or none"
	}
}

// CanonicalAddress lowercases + trims an email address so dedup / alias
// keys are case-insensitive. Pure helper, no I/O.
func CanonicalAddress(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}
