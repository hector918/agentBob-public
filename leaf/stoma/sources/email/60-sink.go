package email

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/i18n"
	"agentbob/leaf/stoma/sources/common"
	"agentbob/leaf/stoma/sources/streamsink"
)

// NewSink returns a block-mode sink backed by the shared streamsink core.
// Email cannot edit a sent message, so emailWire declares Caps{CanEdit:false}
// and the core runs in block discipline: ContentDelta accumulates, the final
// content is sent once at Finish as one email. TraceRender is false, so the
// core drops TraceDelta entirely (a thread of half-thought-out replies is
// worse than one finished one). The grace-ctx survival of a cancelled turn
// lives once in the core's Finish.
//
// The reply's threading/quote metadata (subject, in-reply-to, references,
// original body) is looked up from the source-side stash inside sendReply —
// the wire-format minutiae stay on the source, keeping the Sink contract
// platform-agnostic.
//
// Reply-coalescing: when the account enables it (batch_window_seconds > 0) and
// this is a turn sink (sid != ""), the reply is HELD + combined with the
// session's other turn replies into one email, flushed after the quiet window
// (see 75-coalesce.go). This includes TURN-LEVEL SYSTEM NOTICES (router deny,
// intro, agora company/member-paused): those carry a real Sid too, so on a
// coalescing account they are held with the window and may combine with the same
// session's model reply under a "combined N replies" header — deliberately, so a
// paused sender who fires five messages collects one merged rejection, not five
// (F83: coalescing IS the anti-spam intent; notices are not exempted). Only
// NON-turn sinks (slash replies, restart notices; sid == "") and accounts with
// coalescing off send immediately via the streamsink core.
func (s *Source) NewSink(ctx context.Context, t contract.Target, sessionScope, sid string, prefs contract.SinkPrefs) contract.Sink {
	if s.coalescer != nil && sid != "" {
		s.coalescer.beginTurn(sid, sessionScope, t)
		return &coalesceSink{c: s.coalescer, ctx: ctx, target: t, scope: sessionScope, sid: sid}
	}
	w := &emailWire{src: s, target: t}
	deliver := func(p, c string) error { return s.SendFile(ctx, t, p, c) }
	return streamsink.New(ctx, w, prefs, deliver, "to", CanonicalAddress(t.ChatID))
}

// emailNoSplit is emailWire.MaxChars — a budget so large the core never
// splits. Email is one message however long.
const emailNoSplit = 1 << 30

// emailWire is the email platform leaf for the streamsink core. Block-only:
// it can neither edit nor type nor render trace. The embedded
// BlockWireDefaults supplies the no-op Edit / RateLimited / BenignEdit /
// EditGone / Typing; Send routes the full reply through the source's threaded reply
// builder + async send queue.
type emailWire struct {
	streamsink.BlockWireDefaults
	src    *Source
	target contract.Target
}

var _ streamsink.Wire = (*emailWire)(nil)

func (w *emailWire) Caps() streamsink.WireCaps {
	return streamsink.WireCaps{CanEdit: false, CanType: false, TraceRender: false}
}

// Send renders + enqueues the agent's reply as one email. anchorReply is unused —
// email threads via In-Reply-To/References headers built in sendReply, not a reply
// pointer. It DOES return the sent reply's Message-ID as the anchor id: the flow's
// OnAnchor→RecordSent then indexes (scope, msgID, sid), so a user's reply (In-Reply-To
// = this id) routes back to THIS session instead of @=new forking a context-less one.
func (w *emailWire) Send(ctx context.Context, text string, _ bool) (string, error) {
	return w.src.sendReply(ctx, w.target, text)
}

func (w *emailWire) WireLen(s string) int { return len(s) }
func (w *emailWire) MaxChars() int        { return emailNoSplit }

// RedactErr scrubs the SMTP password from a sink-path error before the core
// logs it.
func (w *emailWire) RedactErr(err error) error {
	return common.RedactErr(err, w.src.password)
}

// Send posts a one-off text message — an unsolicited reply (e.g. admin-line
// forward). Queues a plain-text email with a generic subject and no
// In-Reply-To. The target.ChatID is the recipient address.
func (s *Source) Send(ctx context.Context, t contract.Target, text string) error {
	if s.closed.Load() {
		return fmt.Errorf("email: source closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	to := CanonicalAddress(t.ChatID)
	if to == "" {
		return fmt.Errorf("email: Send needs a recipient ChatID")
	}
	cfg := &s.cfg
	msgID := newMessageID(cfg.Address)
	// Stash the outbound text so the next inbound from this recipient
	// (+alias, if any) can be chunked-stripped against it. (D-B5)
	s.recordPriorBody(inboxKey(to, t.ThreadID), text)
	body := renderEmail(emailRender{
		From:      cfg.Address,
		To:        to,
		Subject:   i18n.T("email.subject.default", i18n.Detect(text)),
		Date:      formatRFC2822Date(time.Now()),
		MessageID: msgID,
		Body:      text,
	})
	return s.enqueueSend(ctx, sendReq{
		to:        to,
		body:      []byte(body),
		queuedAt:  time.Now(),
		messageID: msgID,
	})
}

// sendReply renders + queues the agent's reply, populated with the minimal
// quote header + the inbound message's threading headers so the recipient's
// mail client groups the conversation correctly.
func (s *Source) sendReply(ctx context.Context, t contract.Target, agentReply string) (string, error) {
	if s.closed.Load() {
		return "", fmt.Errorf("email: source closed")
	}
	cfg := &s.cfg
	to := CanonicalAddress(t.ChatID)
	if to == "" {
		return "", fmt.Errorf("email: reply needs a recipient address (target.ChatID)")
	}
	// t.ThreadID carries THIS turn's forwarded alias (ReplyTarget copies
	// ev.ThreadID), so the cache key is per-alias and Reply-To is taken
	// straight from it — no dependence on which alias was stashed last.
	key := inboxKey(to, t.ThreadID)

	// Pull the inbound context the source stashed during fetchAndEmit. When
	// absent, degrade to a bare reply — no threading headers, generic
	// subject.
	ic := s.lookupInboundContext(key)
	subject := i18n.T("email.subject.default", i18n.Detect(agentReply)) // degraded path (no inbound ctx); real subject set below
	inReplyTo := ""
	references := ""
	replyTo := t.ThreadID // Reply-To = this turn's forwarded alias ("" if none)
	quoteFrom := to
	quoteDate := ""
	quoteBody := ""
	var ccList []string
	if ic != nil {
		subject = buildSubject(ic.Subject)
		if replyTo == "" {
			replyTo = ic.Alias // fallback if ThreadID was lost (e.g. across a restart)
		}
		// Reply-all: every other recipient of the inbound (minus bob's own
		// identities + the primary To) rides the Cc. ForwardedDomains drops
		// every owned-domain alias so bob never CCs an address it receives at.
		ccList = replyAllCc(cfg.Address, replyTo, to, cfg.ForwardedDomains, ic.ToAddrs, ic.CcAddrs)
		inReplyTo = ic.MessageID
		references = buildReferences(ic.References, ic.MessageID)
		if ic.FromName != "" {
			quoteFrom = ic.FromName + " <" + ic.FromAddress + ">"
		}
		quoteDate = formatRFC2822Date(ic.Date)
		quoteBody = ic.Body
	}
	body := buildReplyBody(agentReply, quoteFrom, quoteDate, quoteBody, cfg.QuoteMaxLines)
	// Stash the user-visible reply body so the NEXT inbound from this sender
	// can be chunked-stripped against it. (D-B5)
	s.recordPriorBody(key, body)
	msgID := newMessageID(cfg.Address)
	rendered := renderEmail(emailRender{
		From:       cfg.Address,
		To:         to,
		Cc:         strings.Join(ccList, ", "),
		ReplyTo:    replyTo,
		Subject:    subject,
		Date:       formatRFC2822Date(time.Now()),
		MessageID:  msgID,
		InReplyTo:  inReplyTo,
		References: references,
		Body:       body,
	})
	// Return msgID as the anchor (see emailWire.Send) even if the enqueue errors: the
	// id is final at render time, and the flow only indexes a non-empty id.
	return msgID, s.enqueueSend(ctx, sendReq{
		to:        to,
		body:      []byte(rendered),
		queuedAt:  time.Now(),
		messageID: msgID,
	})
}

// inboundContext is the per-conversation reply-thread material cached by the
// IMAP loop and read by the sink. One entry per (sender, alias) — we always
// reply to the LATEST message in a thread, which is correct for the
// In-Reply-To / References chain.
type inboundContext struct {
	Subject     string
	MessageID   string
	References  string
	FromAddress string
	FromName    string
	Date        time.Time
	Body        string
	Alias       string   // forwarded alias this inbound was delivered to (Reply-To on the way out)
	ToAddrs     []string // canonical recipients from the inbound To header (for reply-all Cc)
	CcAddrs     []string // canonical recipients from the inbound Cc header (for reply-all Cc)
}

// inboxKey scopes the per-conversation caches (inbound-context stash +
// prior-outbound body) by (sender, forwarded-alias). A forwarding mailbox
// splits ONE sender into MANY independent sessions (agent@ vs sales@), so
// keying by sender alone would let the two aliases clobber each other's
// threading / Reply-To / quote-strip context. alias "" → just the sender.
func inboxKey(sender, alias string) string {
	s := CanonicalAddress(sender)
	if alias == "" {
		return s
	}
	return s + "#" + alias
}

func (s *Source) stashInboundContext(key string, ic inboundContext) {
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	if s.inboxCtx == nil {
		s.inboxCtx = make(map[string]inboundContext, 64)
	}
	// Bound the map: 1024 distinct senders is plenty for a single source.
	// Beyond that, drop a random entry — eviction policy doesn't matter much
	// because the data is best-effort.
	if len(s.inboxCtx) >= 1024 {
		for k := range s.inboxCtx {
			delete(s.inboxCtx, k)
			break
		}
	}
	s.inboxCtx[key] = ic
}

func (s *Source) lookupInboundContext(key string) *inboundContext {
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	if s.inboxCtx == nil {
		return nil
	}
	if ic, ok := s.inboxCtx[key]; ok {
		return &ic
	}
	return nil
}

// replyAllCc computes the reply-all Cc set for a reply to one inbound: every
// address the inbound carried in To + Cc, MINUS bob's own identities and the
// primary recipient (already our To), de-duplicated and canonicalised. To
// addresses come before Cc; order is otherwise stable. Empty when the
// inbound had no other recipients.
//
// Dropping bob's own identities is mandatory loop-prevention — if the reply
// CC'd an address bob receives at, the forwarder routes it back into bob's
// own inbox as a fresh inbound and bob replies to itself. "bob's own" is
// THREE things: selfAddr (the account mailbox), any address in ownedDomains
// (a mailbox can have several forwarded aliases), and alias (kept explicit;
// harmless belt-and-suspenders in the non-forwarded case).
//
// Being on this list grants NO authorization: it only widens who RECEIVES
// the reply. An address here that later SENDS its own mail is gated upstream
// like any other sender.
func replyAllCc(selfAddr, alias, primaryTo string, ownedDomains, toAddrs, ccAddrs []string) []string {
	exclude := map[string]bool{
		"":                          true,
		CanonicalAddress(selfAddr):  true,
		CanonicalAddress(alias):     true,
		CanonicalAddress(primaryTo): true,
	}
	seen := map[string]bool{}
	var out []string
	keep := func(a string) {
		ca := CanonicalAddress(a)
		if exclude[ca] || seen[ca] || addrInOwnedDomain(ca, ownedDomains) {
			return
		}
		seen[ca] = true
		out = append(out, ca)
	}
	for _, a := range toAddrs {
		keep(a)
	}
	for _, a := range ccAddrs {
		keep(a)
	}
	return out
}

// emailRender is the small shape renderEmail consumes — keeps the renderer
// free of inbound-context coupling so it's easy to test.
type emailRender struct {
	From       string
	To         string
	Cc         string // reply-all recipients, comma-joined ("" = no Cc header). Also drives the SMTP envelope.
	ReplyTo    string // optional; the forwarded alias so replies thread back to it
	Subject    string
	Date       string
	MessageID  string
	InReplyTo  string
	References string
	Body       string
}

// renderEmail joins the headers + body with CRLF (RFC 5322 line ending).
// Subject is folded to a single line — we don't wrap.
func renderEmail(r emailRender) string {
	var b strings.Builder
	// stripCRLF on To/From too: symmetric with Subject/In-Reply-To/
	// References below — defends against a CR/LF in the address injecting a
	// Bcc header. (defense-in-depth)
	b.WriteString("From: ")
	b.WriteString(stripCRLF(r.From))
	b.WriteString("\r\n")
	b.WriteString("To: ")
	b.WriteString(stripCRLF(r.To))
	b.WriteString("\r\n")
	// Cc = the reply-all recipient list. The same addresses must be in the
	// SMTP envelope (RCPT TO) for them to actually receive the mail —
	// envelopeRecipients re-derives them from this header.
	if cc := stripCRLF(r.Cc); cc != "" {
		b.WriteString("Cc: ")
		b.WriteString(cc)
		b.WriteString("\r\n")
	}
	// Reply-To = the forwarded alias when set, so the recipient's reply goes
	// back to the alias → forwarder → bob, keeping the thread on your domain
	// even though From is the mailbox address.
	if rt := stripCRLF(r.ReplyTo); rt != "" {
		b.WriteString("Reply-To: ")
		b.WriteString(rt)
		b.WriteString("\r\n")
	}
	b.WriteString("Subject: ")
	b.WriteString(sanitiseHeaderValue(r.Subject))
	b.WriteString("\r\n")
	b.WriteString("Date: ")
	b.WriteString(r.Date)
	b.WriteString("\r\n")
	if r.MessageID != "" {
		b.WriteString("Message-ID: <")
		b.WriteString(r.MessageID)
		b.WriteString(">\r\n")
	}
	// sanitise every inbound-derived header to defeat injection: an inbound
	// Message-ID like `foo@x>\r\nBcc: attacker@evil` would otherwise inject
	// a Bcc on our reply. stripCRLF (not sanitiseHeaderValue) so an empty
	// value drops the header instead of writing the "(no subject)" fallback.
	if ir := stripCRLF(strings.Trim(r.InReplyTo, "<>")); ir != "" {
		b.WriteString("In-Reply-To: <")
		b.WriteString(ir)
		b.WriteString(">\r\n")
	}
	if rf := stripCRLF(r.References); rf != "" {
		b.WriteString("References: ")
		b.WriteString(rf)
		b.WriteString("\r\n")
	}
	// Auto-Submitted: auto-replied — RFC 3834 §5 marker. Always on. Strict
	// relays score auto-replies less harshly when present, and broken
	// auto-responders know not to bounce back.
	b.WriteString("Auto-Submitted: auto-replied\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	// quoted-printable is universally accepted; declaring 8bit without the
	// server having advertised 8BITMIME would get the message rejected by
	// strict relays.
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	b.WriteString("\r\n")
	// Normalise line endings to CRLF + drop bare \r so dot-stuffing on the
	// wire works as expected.
	body := strings.ReplaceAll(r.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	var qpBuf strings.Builder
	qpw := quotedprintable.NewWriter(&qpBuf)
	_, _ = qpw.Write([]byte(body))
	_ = qpw.Close()
	encoded := qpBuf.String()
	b.WriteString(encoded)
	if !strings.HasSuffix(encoded, "\r\n") {
		b.WriteString("\r\n")
	}
	return b.String()
}

// sanitiseHeaderValue scrubs CR / LF from a header value to defeat
// header-injection. Empty after stripping → "(no subject)" — Subject-specific
// fallback. Non-ASCII is RFC 2047 Q-encoded (`=?UTF-8?q?…?=`): RFC 5322
// headers are 7-bit and we don't negotiate SMTPUTF8, so a raw UTF-8 subject
// would be rejected by strict relays or arrive as mojibake.
func sanitiseHeaderValue(v string) string {
	v = stripCRLF(v)
	if v == "" {
		v = "(no subject)"
	}
	return mime.QEncoding.Encode("UTF-8", v)
}

// stripCRLF removes any CR / LF in a header value without adding a fallback
// string. Used for In-Reply-To / References interpolation, where the empty
// case is handled by the caller.
func stripCRLF(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	return strings.TrimSpace(v)
}

// newMessageID mints an RFC 5322 Message-ID. The right-hand-side uses the
// bob mailbox's domain so it's at least plausible. The left side is a
// unix-nano + 8 random hex bytes — sortable and unique.
func newMessageID(fromAddr string) string {
	domain := "agentbob.local"
	if i := strings.LastIndex(fromAddr, "@"); i >= 0 && i+1 < len(fromAddr) {
		domain = fromAddr[i+1:]
	}
	var rb [8]byte
	_, _ = rand.Read(rb[:])
	return fmt.Sprintf("%d.%s@%s", time.Now().UnixNano(), hex.EncodeToString(rb[:]), domain)
}

// stashInboundContextFromMessage is the IMAP loop's stash entry — called
// from fetchAndEmit just after the MessageEvent is emitted. Keeps the parsed
// metadata in-memory so the next agent reply can build a proper minimal-quote
// response. Best-effort: if the agent doesn't reply, the entry sits until
// evicted.
func (s *Source) stashInboundContextFromMessage(pe *parsedEmail) {
	s.stashInboundContext(inboxKey(pe.FromAddress, pe.ForwardedAlias), inboundContext{
		Subject:     pe.Subject,
		MessageID:   pe.MessageID,
		References:  pe.References,
		FromAddress: pe.FromAddress,
		FromName:    pe.FromName,
		Date:        pe.Date,
		Body:        pe.TextBody, // pre-strip body — keeps the quote builder honest
		Alias:       pe.ForwardedAlias,
		ToAddrs:     pe.ToAddrs,
		CcAddrs:     pe.CcAddrs,
	})
	slog.Debug("email: stashed inbound context for reply",
		"source", s.Name(), "from", pe.FromAddress, "message_id", pe.MessageID)
}
