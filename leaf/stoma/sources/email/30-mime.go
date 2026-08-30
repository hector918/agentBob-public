package email

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"net/mail"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"agentbob/contract"
	"agentbob/heartwood/files"

	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
)

// parsedEmail is the intermediate result of parseMIME — the raw material
// fetchAndEmit stamps into a MessageEvent.
//
// TextBody is the text/plain content (or "" if the message carried only
// HTML / no text part). Attachments is the list of decoded non-text parts
// already written to disk. The header fields are normalised (angle brackets
// stripped, case-folded where appropriate).
type parsedEmail struct {
	MessageID   string
	InReplyTo   string
	References  string // raw space-joined chain (NOT the parsed list)
	FromAddress string // canonical (lowercase, trimmed) sender address
	FromName    string // display name from From header, or "" if not set
	Subject     string
	Date        time.Time
	TextBody    string // text/plain after charset decode; "" if none
	Attachments []contract.Attachment

	// Recipient material for forwarded-alias resolution. ToAddrs/CcAddrs
	// are canonical addresses; XForwardedFor is the raw header (Cloudflare
	// Email Routing stamps it "<original-recipient> <forwarded-to>").
	// ForwardedAlias is filled by the IMAP loop AFTER parse.
	ToAddrs        []string
	CcAddrs        []string
	XForwardedFor  string
	ForwardedAlias string // resolved original recipient alias, or "" if none/off
}

// maxTextBodyBytes caps the TOTAL accumulated text/plain body across all
// inline parts. 1MiB total is generous for a real reply and matches the
// per-part ceiling.
const maxTextBodyBytes = 1 << 20

// maxTextPartBytes caps a SINGLE text part. Equal to the total cap.
const maxTextPartBytes = 1 << 20

// tolerableMIMEErr reports whether a go-message error is one we degrade past
// instead of failing on. Both an unknown charset and an unknown
// Content-Transfer-Encoding leave the reader usable (the body arrives
// undecoded) per the library contract; only these two are recoverable.
func tolerableMIMEErr(err error) bool {
	return gomessage.IsUnknownCharset(err) || gomessage.IsUnknownEncoding(err)
}

// parseMIME reads one RFC 5322 message off r and decodes it. If fileStore is
// non-nil, attachment parts are written under subdir (the staging bucket the
// caller computed) and recorded in parsedEmail.Attachments with their
// LocalPath + sandbox-relative Path populated. Attachments >
// maxAttachmentBytes are skipped with a log line; the rest of the message
// still parses.
//
// text/html parts are dropped entirely (text/plain covers the common case;
// HTML rendering is its own complexity).
//
// Errors from go-message's unknown-charset AND unknown-transfer-encoding
// paths are tolerated: the reader stays usable in both modes (the body just
// arrives undecoded). A real parse error (malformed structure) is still
// returned.
func parseMIME(r io.Reader, fileStore *files.Store, subdir string, maxAttachmentBytes int64) (*parsedEmail, error) {
	// gomail.CreateReader tolerates only an unknown CHARSET (it returns a
	// nil reader for an unknown transfer-encoding), so call the lower-level
	// message.Read directly: it returns a usable *Entity for BOTH cases.
	ent, err := gomessage.Read(r)
	if err != nil && !tolerableMIMEErr(err) {
		return nil, fmt.Errorf("message.Read: %w", err)
	}
	mr := gomail.NewReader(ent)
	pe := &parsedEmail{}

	// htmlFallback keeps the FIRST text/html part's raw bytes (bounded). An
	// HTML-only message (no text/plain sibling) would otherwise leave TextBody
	// empty and be silently dropped at submit; after the walk we degrade it to
	// plain text ONLY when no text/plain part filled the body.
	var htmlFallback bytes.Buffer

	// Header fields: parse what we need + canonicalise.
	h := mr.Header
	if msgID, hErr := h.MessageID(); hErr == nil {
		pe.MessageID = strings.Trim(msgID, "<>")
	}
	if irList, hErr := h.MsgIDList("In-Reply-To"); hErr == nil && len(irList) > 0 {
		pe.InReplyTo = strings.Trim(irList[0], "<>")
	}
	// References is the full thread chain — keep verbatim, the reply builder
	// needs to extend it.
	pe.References = h.Get("References")
	if subj, hErr := h.Subject(); hErr == nil {
		pe.Subject = subj
	}
	if d, hErr := h.Date(); hErr == nil {
		pe.Date = d
	}
	if from, hErr := h.AddressList("From"); hErr == nil && len(from) > 0 {
		pe.FromAddress = CanonicalAddress(from[0].Address)
		pe.FromName = strings.TrimSpace(from[0].Name)
	}
	// Recipient material for forwarded-alias resolution.
	if to, hErr := h.AddressList("To"); hErr == nil {
		for _, a := range to {
			pe.ToAddrs = append(pe.ToAddrs, CanonicalAddress(a.Address))
		}
	}
	if cc, hErr := h.AddressList("Cc"); hErr == nil {
		for _, a := range cc {
			pe.CcAddrs = append(pe.CcAddrs, CanonicalAddress(a.Address))
		}
	}
	pe.XForwardedFor = h.Get("X-Forwarded-For")

	// Walk every part. Inline text/* → pe.TextBody (concat if multiple
	// parts). Anything else (attachment disposition, or inline non-text such
	// as a pasted image) → attachment. text/html → drop. Bounded read on
	// every part to defend against a runaway MIME stream.
	for {
		p, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			if tolerableMIMEErr(perr) {
				// Unknown charset: p is still valid, the body just arrives
				// undecoded. Unknown encoding: the mail.Reader contract
				// returns (nil, err), so p is nil here — skip that one bad
				// part (NextPart has discarded its residual bytes) rather
				// than nil-deref and lose the whole message.
				if p == nil {
					continue
				}
			} else {
				return pe, fmt.Errorf("mr.NextPart: %w", perr)
			}
		}
		switch ph := p.Header.(type) {
		case *gomail.InlineHeader:
			ct, _, _ := ph.ContentType()
			if strings.EqualFold(ct, "text/html") {
				// Drop HTML from the body (text/plain is the common path), but
				// stash the first html part (bounded) as a fallback for the
				// HTML-only case — degraded to plain text after the walk.
				if htmlFallback.Len() == 0 {
					n, _ := io.CopyN(&htmlFallback, p.Body, maxTextPartBytes+1)
					if n > maxTextPartBytes {
						htmlFallback.Truncate(maxTextPartBytes)
					}
				}
				continue // drop html from TextBody
			}
			// go-message classifies EVERY disp=inline part as InlineHeader,
			// including image/pdf (pasted screenshots arrive that way from
			// Gmail / Apple Mail). Binary bytes are not body text — capture
			// non-text inline parts as attachments, same as the
			// AttachmentHeader branch. Only ""-ct (malformed header) stays on
			// the text path, matching the RFC text/plain default.
			if ct != "" && !strings.HasPrefix(strings.ToLower(ct), "text/") {
				pe.Attachments = append(pe.Attachments,
					savePart(fileStore, subdir, maxAttachmentBytes, ct, inlinePartFilename(ph, ct), p.Body))
				continue
			}
			// text/plain (and any other text/* we haven't classified) lands
			// as body text. Cap each part at 1MiB.
			var buf bytes.Buffer
			// Read cap+1 to DETECT overflow: CopyN(…, cap) returns (cap, nil) for an
			// over-cap part, so the old "rerr != nil" check never fired → silent
			// truncation. n > cap ⇒ the part was larger than the cap; trim to cap and
			// warn loudly that the tail was dropped.
			n, rerr := io.CopyN(&buf, p.Body, maxTextPartBytes+1)
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				slog.Warn("email: text part read error", "err", rerr, "bytes_so_far", buf.Len())
			} else if n > maxTextPartBytes {
				buf.Truncate(maxTextPartBytes)
				slog.Warn("email: text part exceeded cap — tail dropped", "cap", maxTextPartBytes)
			}
			// Enforce the TOTAL cap at append time: trim this part to the
			// remaining budget. (A pre-append >= check let the total reach
			// ~2× the cap — one under-cap body plus one full-cap part.)
			remain := maxTextBodyBytes - len(pe.TextBody)
			switch {
			case remain <= 0:
				// at cap — drop this part's text rather than grow further
			case pe.TextBody == "":
				if buf.Len() > remain {
					buf.Truncate(remain)
					slog.Warn("email: text body exceeded total cap — tail dropped", "cap", maxTextBodyBytes)
				}
				pe.TextBody = buf.String()
			default:
				remain-- // the "\n" joiner counts against the budget
				if buf.Len() > remain {
					buf.Truncate(remain)
					slog.Warn("email: text body exceeded total cap — tail dropped", "cap", maxTextBodyBytes)
				}
				pe.TextBody += "\n" + buf.String()
			}
		case *gomail.AttachmentHeader:
			fname, _ := ph.Filename()
			ct, _, _ := ph.ContentType()
			pe.Attachments = append(pe.Attachments,
				savePart(fileStore, subdir, maxAttachmentBytes, ct, fname, p.Body))
		}
	}

	// HTML-only fallback: no text/plain part filled the body, but an html part
	// was present — degrade it to plain text so the message still carries its
	// intent into a turn instead of vanishing (empty text + no attachments →
	// silent submit drop). Naive strip by design; full HTML rendering stays out
	// of scope. The Subject is the next fallback, applied by fetchAndEmit.
	if strings.TrimSpace(pe.TextBody) == "" && htmlFallback.Len() > 0 {
		pe.TextBody = stripHTMLToText(htmlFallback.String())
	}
	return pe, nil
}

var (
	// reHTMLDropBlock removes <script>/<style> blocks whole (their bodies are
	// code/CSS, never prose). reHTMLBreak turns block-enders into newlines so
	// the stripped text keeps paragraph structure. reHTMLTag removes every
	// remaining tag. reHTMLInlineWS collapses horizontal whitespace runs.
	reHTMLDropBlock = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	reHTMLBreak     = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/h[1-6]|/li)\s*/?>`)
	reHTMLTag       = regexp.MustCompile(`(?s)<[^>]*>`)
	reHTMLInlineWS  = regexp.MustCompile(`[ \t\f\v]+`)
	reHTMLBlankRun  = regexp.MustCompile(`\n{3,}`)
)

// stripHTMLToText degrades an HTML body to plain text for the HTML-only
// fallback path. It drops script/style, maps block-enders to newlines, strips
// the remaining tags, unescapes entities, and collapses whitespace. Naive on
// purpose — it carries the sender's intent into a turn, not a faithful render.
func stripHTMLToText(s string) string {
	s = reHTMLDropBlock.ReplaceAllString(s, " ")
	s = reHTMLBreak.ReplaceAllString(s, "\n")
	s = reHTMLTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reHTMLInlineWS.ReplaceAllString(s, " ")
	// Trim each line + squeeze blank-line runs so the strip doesn't leave a
	// ladder of indentation/blank lines from the original markup.
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = reHTMLBlankRun.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(s)
}

// savePart decodes one non-text part to disk (when fileStore is set) and
// returns its Attachment record. Oversized or failed saves degrade to a
// metadata-only entry (no LocalPath/Path) with a warning, so the rest of the
// message still parses.
func savePart(fileStore *files.Store, subdir string, maxAttachmentBytes int64, ct, fname string, body io.Reader) contract.Attachment {
	att := contract.Attachment{
		Kind:     kindFromMIME(ct),
		MIME:     ct,
		FileName: sanitiseFilename(fname),
	}
	if fileStore == nil {
		return att // metadata only
	}
	if att.FileName == "" {
		att.FileName = "attachment"
	}
	abs, rel, n, sErr := fileStore.Save(subdir, att.FileName, body, maxAttachmentBytes)
	if sErr != nil {
		if errors.Is(sErr, files.ErrTooLarge) {
			slog.Warn("email: attachment skipped — over cap",
				"name", att.FileName, "cap", maxAttachmentBytes)
		} else {
			slog.Warn("email: attachment save failed",
				"name", att.FileName, "err", sErr)
		}
		return att // metadata-only entry
	}
	att.LocalPath = abs
	att.Path = rel
	att.Size = n
	return att
}

// inlinePartFilename picks a filename for a non-text inline part. Inline
// parts usually carry no filename — fall back through the Content-Disposition
// filename param, the Content-Type name param, then a MIME-extension default
// ("inline.png" for image/png).
func inlinePartFilename(ph *gomail.InlineHeader, ct string) string {
	if _, params, err := ph.ContentDisposition(); err == nil {
		if f := params["filename"]; f != "" {
			return f
		}
	}
	if _, params, err := ph.ContentType(); err == nil {
		if n := params["name"]; n != "" {
			return n
		}
	}
	if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 {
		return "inline" + exts[0]
	}
	return "inline"
}

// kindFromMIME classifies a Content-Type into the loose contract.Attachment
// Kind buckets the prompt layer's describeAtt knows about.
func kindFromMIME(ct string) string {
	c := strings.ToLower(ct)
	switch {
	case strings.HasPrefix(c, "image/"):
		return "image"
	case strings.HasPrefix(c, "video/"):
		return "video"
	case strings.HasPrefix(c, "audio/"):
		return "audio"
	default:
		return "document"
	}
}

// sanitiseFilename strips path separators + control runes from a
// MIME-supplied filename so a malicious sender can't traverse out of the
// sandbox or write to a hidden dot-file. files.Save has its own
// defence-in-depth check; this is the first line.
func sanitiseFilename(name string) string {
	if name == "" {
		return ""
	}
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		if r == 0 || unicode.IsControl(r) || r == '/' || r == '\\' {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimLeft(out, ".")
	return out
}

// formatRFC2822Date is what we hand the outbound reply's Date header.
func formatRFC2822Date(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.Format(time.RFC1123Z) // Mon, 26 May 2026 09:00:00 +0000
}

// senderDisplay returns the best-effort human label for a sender — the
// display name from From if present, else the bare address.
func senderDisplay(pe *parsedEmail) string {
	if pe.FromName != "" {
		return pe.FromName
	}
	return pe.FromAddress
}

// parseScreenHeader extracts only the Message-ID from a minimal header-only
// IMAP fetch — the stage-1 payload of the two-stage dedup. Uses stdlib
// net/mail.ReadMessage because the input is a header block with no body,
// which go-message/mail's CreateReader rejects.
func parseScreenHeader(r io.Reader) (screenHeader, error) {
	// net/mail.ReadMessage expects a header block + empty line + body; we
	// have only headers from the BODY.PEEK[HEADER.FIELDS …] fetch. Append a
	// terminating CRLF so the parser sees a zero-length body.
	buf, err := io.ReadAll(r)
	if err != nil {
		return screenHeader{}, fmt.Errorf("read header literal: %w", err)
	}
	bs := buf
	if !bytes.HasSuffix(bs, []byte("\r\n\r\n")) {
		if bytes.HasSuffix(bs, []byte("\r\n")) {
			bs = append(bs, '\r', '\n')
		} else {
			bs = append(bs, '\r', '\n', '\r', '\n')
		}
	}
	msg, err := mail.ReadMessage(bytes.NewReader(bs))
	if err != nil {
		return screenHeader{}, fmt.Errorf("parse minimal header: %w", err)
	}
	return screenHeader{
		messageID: strings.Trim(msg.Header.Get("Message-ID"), "<>"),
	}, nil
}
