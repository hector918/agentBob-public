package email

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// rePrefixRE matches a single subject prefix the reply builder should peel
// off before prepending a fresh "Re: ". Covers English (Re/Fwd), Chinese
// (回复/转发). Trailing colon may be ASCII ":" or full-width "：".
var rePrefixRE = regexp.MustCompile(`(?i)^\s*(re|fwd|fw|回复|转发)\s*[:：]\s*`)

// buildReplyBody renders the outbound reply body, quoting only the
// immediate previous message.
//
//   - replyText is the agent's reply (already a finished string).
//   - quoteFrom is the previous sender's display name or address.
//   - quoteDate is a human date string.
//   - quoteBody is the previous sender's RAW text/plain content as parsed
//     (PRE quote-strip — the stash deliberately keeps the original body to
//     keep the quote honest; see stashInboundContextFromMessage in
//     60-sink.go).
//   - maxLines caps the quoted block (bounding nested-quote growth from the
//     pre-strip body); lines beyond are replaced by a single "> [...]"
//     trailer so body bytes grow ~linearly per round.
//
// Pure function (no I/O).
func buildReplyBody(replyText, quoteFrom, quoteDate, quoteBody string, maxLines int) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(replyText, " \t\n"))
	b.WriteString("\n\n")
	if quoteDate != "" || quoteFrom != "" {
		// "On <date>, <sender> wrote:" — Gmail / Outlook / Apple Mail all
		// recognise this header and collapse the trailing quote in their UI.
		// Keep the wording verbatim or the recognition breaks.
		header := "On"
		if quoteDate != "" {
			header += " " + quoteDate + ","
		}
		if quoteFrom != "" {
			header += " " + quoteFrom
		}
		header += " wrote:"
		b.WriteString(header)
		b.WriteString("\n")
	}
	if quoteBody == "" {
		return strings.TrimRight(b.String(), " \t\n")
	}
	if maxLines <= 0 {
		maxLines = 5
	}
	lines := strings.Split(quoteBody, "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	for _, ln := range lines {
		// One leading "> " per quoted line. Existing >'s on a line (nested
		// quotes from earlier rounds) just get one more level.
		b.WriteString("> ")
		b.WriteString(ln)
		b.WriteString("\n")
	}
	if truncated {
		b.WriteString("> [...]\n")
	}
	return strings.TrimRight(b.String(), " \t\n")
}

// buildSubject prepends "Re: " unless the incoming subject already starts
// with one (case-insensitive). Peels off ALL stacked Re:/Fwd:/回复:/转发:
// prefixes before prepending a single "Re: ".
func buildSubject(orig string) string {
	orig = strings.TrimSpace(orig)
	if orig == "" {
		return "Re: (no subject)"
	}
	stripped := orig
	for {
		next := rePrefixRE.ReplaceAllString(stripped, "")
		if next == stripped {
			break
		}
		stripped = next
	}
	stripped = strings.TrimSpace(stripped)
	if stripped == "" {
		return "Re: (no subject)"
	}
	// RFC 5322 §2.1.1 caps a header line at 998 bytes and renderEmail writes
	// Subject on a single line. Worst-case Q-encoding expands every byte to
	// =XX inside 75-char encoded words (~3.6×), so 240 raw bytes keeps the
	// encoded line safely under the cap.
	if len(stripped) > subjectSoftCap {
		cut := subjectSoftCap
		for cut > 0 && !utf8.RuneStart(stripped[cut]) {
			cut-- // never split a multibyte rune
		}
		stripped = strings.TrimRight(stripped[:cut], " \t") + "…"
	}
	return "Re: " + stripped
}

// subjectSoftCap is the max raw bytes of subject kept when building a reply
// subject — see the budget math in buildSubject.
const subjectSoftCap = 240

// buildReferences extends the References header chain. parentRefs is the
// inbound message's References header value (a space-separated list of
// message ids, may be empty); parentMsgID is the inbound Message-ID (without
// angle brackets). RFC 5322 §3.6.4: append the parent Message-ID; the chain
// grows linearly per round.
//
// Once the joined value exceeds referencesSoftCap, trim the middle and keep
// the first ID (thread root) plus the most recent referencesTailKeep IDs
// (RFC 5256 §2 guidance), so the line stays under the 998-char limit.
func buildReferences(parentRefs, parentMsgID string) string {
	parts := []string{}
	if s := strings.TrimSpace(parentRefs); s != "" {
		for _, f := range strings.Fields(s) {
			parts = append(parts, f)
		}
	}
	if parentMsgID != "" {
		parts = append(parts, fmt.Sprintf("<%s>", strings.Trim(parentMsgID, "<>")))
	}
	joined := strings.Join(parts, " ")
	if len(joined) <= referencesSoftCap || len(parts) <= referencesTailKeep+1 {
		return joined
	}
	trimmed := make([]string, 0, referencesTailKeep+1)
	trimmed = append(trimmed, parts[0])
	trimmed = append(trimmed, parts[len(parts)-referencesTailKeep:]...)
	return strings.Join(trimmed, " ")
}

// referenceIDs parses a References header (space-separated message-ids,
// oldest→newest) into bare ids (angle brackets stripped), NEWEST-first — the
// order reply routing wants. Blank → nil. Used to set MessageEvent.ReplyRefs
// so a reply still finds its session when the client dropped In-Reply-To but
// kept References.
func referenceIDs(refs string) []string {
	fields := strings.Fields(refs)
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, 0, len(fields))
	for i := len(fields) - 1; i >= 0; i-- {
		if id := strings.Trim(fields[i], "<>"); id != "" {
			out = append(out, id)
		}
	}
	return out
}

const (
	// referencesSoftCap — total joined-value byte length above which
	// buildReferences trims the middle.
	referencesSoftCap = 800
	// referencesTailKeep — number of most-recent message-IDs kept (in
	// addition to the root) when trimming for the 998-char limit.
	referencesTailKeep = 10
)
