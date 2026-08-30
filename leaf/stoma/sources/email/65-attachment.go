// sources/email/65-attachment.go — multipart/mixed renderer + the
// Source.SendFile implementation.
//
// Mirrors renderEmail (60-sink.go) but emits a multipart/mixed message with
// two parts:
//  1. text/plain (quoted-printable) — the caption (may be empty; empty
//     caption renders as a one-line "(attached: <filename>)" placeholder so
//     the recipient's mail client doesn't display a bare empty body).
//  2. application/* (base64) — the file body, encoded in 76-char lines per
//     RFC 2045.
//
// Threading: when an inbound context exists for the recipient, SendFile
// reuses the same subject / In-Reply-To / References headers sendReply uses,
// so the attached email lands in the same thread. Without inbound context
// (one-off delivery), the subject is the caption (or "File from bob" if
// empty). Reuses the existing enqueueSend + sendReq machinery — same
// delivery path, retry semantics, and ordering as text-only sends.
package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"agentbob/contract"
	"agentbob/i18n"
)

// SendFile reads the file at path, builds a multipart/mixed email (caption
// text + attached file), and enqueues it via the existing send queue.
//
// target.ChatID = recipient address. Caption may be empty.
func (s *Source) SendFile(ctx context.Context, t contract.Target, path, caption string) error {
	if s.closed.Load() {
		return fmt.Errorf("email: source closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	to := CanonicalAddress(t.ChatID)
	if to == "" {
		return fmt.Errorf("email: SendFile needs a recipient ChatID")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("email: SendFile read %s: %w", path, err)
	}
	if len(body) == 0 {
		return fmt.Errorf("email: SendFile refuses empty file %s", path)
	}
	const maxBytes = 25 * 1024 * 1024
	if len(body) > maxBytes {
		return fmt.Errorf("email: SendFile file too large (%d bytes; max %d)", len(body), maxBytes)
	}

	cfg := &s.cfg
	msgID := newMessageID(cfg.Address)

	// Reuse inbound context when available so the attachment lands in the
	// same thread the user wrote into. Keyed by (sender, alias) — same key
	// sendReply uses.
	ic := s.lookupInboundContext(inboxKey(to, t.ThreadID))
	subject := defaultAttachmentSubject(caption, filepath.Base(path))
	inReplyTo := ""
	references := ""
	replyTo := t.ThreadID
	var ccList []string
	if ic != nil {
		subject = buildSubject(ic.Subject)
		inReplyTo = ic.MessageID
		references = buildReferences(ic.References, ic.MessageID)
		if replyTo == "" {
			replyTo = ic.Alias
		}
		ccList = replyAllCc(cfg.Address, replyTo, to, cfg.ForwardedDomains, ic.ToAddrs, ic.CcAddrs)
	}

	rendered, rErr := renderEmailWithAttachment(emailRender{
		From:       cfg.Address,
		To:         to,
		Cc:         strings.Join(ccList, ", "),
		ReplyTo:    replyTo,
		Subject:    subject,
		Date:       formatRFC2822Date(time.Now()),
		MessageID:  msgID,
		InReplyTo:  inReplyTo,
		References: references,
		Body:       captionBody(caption, filepath.Base(path)),
	}, filepath.Base(path), guessMIME(path), body)
	if rErr != nil {
		return fmt.Errorf("email: SendFile render: %w", rErr)
	}
	return s.enqueueSend(ctx, sendReq{
		to:        to,
		body:      []byte(rendered),
		queuedAt:  time.Now(),
		messageID: msgID,
	})
}

// captionBody picks the body text for the attachment email: the
// caller-supplied caption if non-empty, otherwise a one-line placeholder
// naming the attachment.
func captionBody(caption, filename string) string {
	caption = strings.TrimSpace(caption)
	if caption != "" {
		return caption
	}
	return "(attached: " + filename + ")"
}

// defaultAttachmentSubject is used only when there's no inbound context to
// thread against. Picks "File from bob: <filename>" so the recipient's inbox
// doesn't show a bare "Message from bob" for what's clearly a file delivery.
// 78-char cap on the final subject; filename is truncated on a UTF-8 rune
// boundary.
func defaultAttachmentSubject(caption, filename string) string {
	if c := strings.TrimSpace(caption); c != "" && len(c) <= 78 {
		return c
	}
	// lang from the caption (empty caption → i18n default); subject must match the body's locale.
	prefix := i18n.T("email.subject.file_prefix", i18n.Detect(caption))
	const capLen = 78
	if len(prefix)+len(filename) <= capLen {
		return prefix + filename
	}
	keep := capLen - len(prefix) - 1
	if keep < 1 {
		return prefix
	}
	// keep < len(filename) here: we only reach this point when prefix+filename
	// exceeds capLen, so filename is strictly longer than keep.
	for keep > 0 && !utf8.RuneStart(filename[keep]) {
		keep--
	}
	return prefix + filename[:keep] + "…"
}

// guessMIME returns the MIME type for path by extension. Falls back to
// application/octet-stream.
func guessMIME(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if mt := mime.TypeByExtension(ext); mt != "" {
		return mt
	}
	return "application/octet-stream"
}

// buildContentTypeWithName glues a `name=<filename>` parameter onto a MIME
// type that may itself already carry parameters. Non-ASCII filenames land as
// RFC 2231 `name*=UTF-8”...` form. On ParseMediaType failure we degrade to
// `<type>; name=<RFC 2231-encoded>` rather than surfacing the error.
func buildContentTypeWithName(rawType, filename string) string {
	mt, params, err := mime.ParseMediaType(rawType)
	if err != nil {
		// Degrade: append a name param to the raw type. FormatMediaType with an
		// empty/invalid type returns "" — slicing [2:] would PANIC — so format with a
		// throwaway type and lift its "; name=..." tail; if even that fails, drop the
		// param rather than crash.
		if ft := mime.FormatMediaType("text/plain", map[string]string{"name": filename}); ft != "" {
			if k := strings.Index(ft, "; "); k >= 0 {
				return rawType + ft[k:]
			}
		}
		return rawType
	}
	if params == nil {
		params = map[string]string{}
	}
	params["name"] = filename
	return mime.FormatMediaType(mt, params)
}

// renderEmailWithAttachment builds a multipart/mixed email body matching
// renderEmail's header conventions plus a multipart Content-Type with two
// parts.
func renderEmailWithAttachment(r emailRender, attachName, attachMime string, attachBody []byte) (string, error) {
	var bodyBuf bytes.Buffer
	mw := multipart.NewWriter(&bodyBuf)
	boundary := mw.Boundary()

	// --- text/plain part ---
	textHdr := textproto.MIMEHeader{}
	textHdr.Set("Content-Type", "text/plain; charset=UTF-8")
	textHdr.Set("Content-Transfer-Encoding", "quoted-printable")
	tp, err := mw.CreatePart(textHdr)
	if err != nil {
		return "", err
	}
	normalised := strings.ReplaceAll(r.Body, "\r\n", "\n")
	normalised = strings.ReplaceAll(normalised, "\r", "\n")
	normalised = strings.ReplaceAll(normalised, "\n", "\r\n")
	var qpBuf bytes.Buffer
	qpw := quotedprintable.NewWriter(&qpBuf)
	if _, err := qpw.Write([]byte(normalised)); err != nil {
		return "", err
	}
	if err := qpw.Close(); err != nil {
		return "", err
	}
	if _, err := tp.Write(qpBuf.Bytes()); err != nil {
		return "", err
	}

	// --- application/* attachment part ---
	attHdr := textproto.MIMEHeader{}
	attHdr.Set("Content-Type", buildContentTypeWithName(attachMime, attachName))
	attHdr.Set("Content-Transfer-Encoding", "base64")
	attHdr.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": attachName}))
	ap, err := mw.CreatePart(attHdr)
	if err != nil {
		return "", err
	}
	encoded := base64.StdEncoding.EncodeToString(attachBody)
	// Chunk into 76-char lines per RFC 2045 — required for strict relays.
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		if _, err := ap.Write([]byte(encoded[i:end])); err != nil {
			return "", err
		}
		if _, err := ap.Write([]byte("\r\n")); err != nil {
			return "", err
		}
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	// --- assemble headers + body ---
	var out strings.Builder
	out.WriteString("From: ")
	out.WriteString(stripCRLF(r.From))
	out.WriteString("\r\n")
	out.WriteString("To: ")
	out.WriteString(stripCRLF(r.To))
	out.WriteString("\r\n")
	if cc := stripCRLF(r.Cc); cc != "" {
		out.WriteString("Cc: ")
		out.WriteString(cc)
		out.WriteString("\r\n")
	}
	if rt := stripCRLF(r.ReplyTo); rt != "" {
		out.WriteString("Reply-To: ")
		out.WriteString(rt)
		out.WriteString("\r\n")
	}
	out.WriteString("Subject: ")
	out.WriteString(sanitiseHeaderValue(r.Subject))
	out.WriteString("\r\n")
	out.WriteString("Date: ")
	out.WriteString(r.Date)
	out.WriteString("\r\n")
	if r.MessageID != "" {
		out.WriteString("Message-ID: <")
		out.WriteString(r.MessageID)
		out.WriteString(">\r\n")
	}
	if ir := stripCRLF(strings.Trim(r.InReplyTo, "<>")); ir != "" {
		out.WriteString("In-Reply-To: <")
		out.WriteString(ir)
		out.WriteString(">\r\n")
	}
	if rf := stripCRLF(r.References); rf != "" {
		out.WriteString("References: ")
		out.WriteString(rf)
		out.WriteString("\r\n")
	}
	out.WriteString("Auto-Submitted: auto-replied\r\n")
	out.WriteString("MIME-Version: 1.0\r\n")
	out.WriteString("Content-Type: ")
	out.WriteString(mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": boundary}))
	out.WriteString("\r\n\r\n")
	out.Write(bodyBuf.Bytes())
	return out.String(), nil
}
