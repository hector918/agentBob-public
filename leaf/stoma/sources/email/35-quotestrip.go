package email

import (
	"regexp"
	"strings"
	"unicode"
)

// stripInboundQuotes returns body with prior-quoted content removed.
//
// Two strategies, tried in order:
//
//  1. Chunked-substring match against the prior outbound body — the D-B5
//     enhancement. Locates the section of `current` that reproduces a
//     normalised slice of `prior`, then back-walks to a quote-header line
//     and cuts. Tolerant of line-wrap changes, punctuation tweaks, and
//     prefix re-quoting that defeat the regex-only path. Fires only when
//     prior is available (Sink.Send stashed it on the previous outbound)
//     and the chunk hit rate clears a confidence floor.
//
//  2. Regex-based strip — retained as a fallback. Always available; works
//     on the very first inbound per sender (no prior stashed yet) and on
//     any mail where chunked doesn't reach the confidence floor.
//
// Heuristic by design — mail clients don't commit to any one format and a
// perfect stripper is impossible. Goal: keep MessageEvent.Text bounded
// across N rounds, not byte-exact.
var (
	quoteLineRE = regexp.MustCompile(`^\s*>+\s?.*$`)
	// "On <date>, <sender> wrote:" — Gmail / Apple Mail, English.
	onWroteRE = regexp.MustCompile(`(?im)^On .+?wrote:\s*$`)
	// Chinese (Gmail / 网易 / QQ Mail): "<sender> 于 <date> 写道：" —
	// half-width and full-width colons both seen.
	cnWroteRE = regexp.MustCompile(`(?im)^.+?\s+于\s+.+?\s*写道\s*[:：]\s*$`)
	// Framed "original message" / "forwarded message" separator, multi-locale.
	origMsgRE = regexp.MustCompile(`(?im)^\s*-{2,}\s*(Original Message|Forwarded message|原始邮件|转发邮件|转发的邮件)\s*-{2,}\s*$`)
	// Outlook draws a long underscore rule above the quoted header block in
	// EVERY locale. A whole line of >=16 underscores is a reliable,
	// language-agnostic "quoted block starts here" marker. Cut to EOF.
	underscoreSepRE = regexp.MustCompile(`(?m)^[ \t]*_{16,}[ \t]*$`)
	// Outlook header block: From:\nSent:\nTo:\nSubject: . Match the first
	// line; drop from there to EOF.
	outlookFromRE = regexp.MustCompile(`(?im)^From:\s+.+$`)
	// 3+ consecutive blank lines → 2.
	manyBlanksRE = regexp.MustCompile(`\n[ \t]*\n[ \t]*(\n[ \t]*)+`)
)

const (
	// chunkTargetSize — splitChunks aims for ~50 chars per chunk on word
	// boundaries.
	chunkTargetSize = 50
	// chunkMaxCount — very long priors don't need finer chunking.
	chunkMaxCount = 100
	// chunkedMinHits — absolute floor: under this even a high hit rate is
	// more likely coincidence than real quotation.
	chunkedMinHits = 3
	// chunkedHitRate — chunks-matched / chunks-total floor.
	chunkedHitRate = 0.60
	// quoteHeaderLookback — how far back from the chunked-cut point to scan
	// for a quote-header line.
	quoteHeaderLookback = 200
)

// StripInboundQuotesWithPrior is the D-B5 entry-point. When priorBody is
// non-empty AND the chunked match clears the confidence floor, returns the
// chunked strip; otherwise falls back to the regex strip. Pure function.
func StripInboundQuotesWithPrior(currentBody, priorBody string) string {
	if currentBody == "" {
		return ""
	}
	if priorBody != "" {
		if stripped, ok := chunkedStrip(currentBody, priorBody); ok {
			// chunked cuts the bulk by CONTENT, but client-generated header
			// chrome above the matched body (Outlook's "________\n发件人:…")
			// isn't in priorBody, so chunked can leave it dangling. Run the
			// regex pass over the result to clip any such residual. No-op
			// when chunked already cut clean.
			return regexStrip(stripped)
		}
	}
	return regexStrip(currentBody)
}

// regexStrip is the heuristic fallback. Steps:
//  1. Drop standard quote-block headers + everything after them.
//  2. Drop every contiguous run of `^\s*>+\s?.*` lines.
//  3. Trim trailing whitespace + collapse runs of 3+ blank lines to 2.
func regexStrip(body string) string {
	if body == "" {
		return ""
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")

	// Step 1 (header-block drops) BEFORE step 2: an "On ... wrote:" header
	// is usually followed by a >-quoted block; stripping the quoted lines
	// first would leave the header dangling as real content.
	if loc := onWroteRE.FindStringIndex(body); loc != nil {
		body = body[:loc[0]]
	}
	if loc := cnWroteRE.FindStringIndex(body); loc != nil {
		body = body[:loc[0]]
	}
	if loc := origMsgRE.FindStringIndex(body); loc != nil {
		body = body[:loc[0]]
	}
	if loc := underscoreSepRE.FindStringIndex(body); loc != nil {
		body = body[:loc[0]]
	}
	// Outlook From:/Sent:/To:/Subject: block: only treat a `From:` header at
	// the START of a line FOLLOWED by `Sent:`/`To:`/`Subject:` on the next
	// few lines as a quote-block — a bare "From: X" in normal prose must not
	// eat the whole tail.
	if loc := outlookFromRE.FindStringIndex(body); loc != nil {
		tail := body[loc[0]:]
		nextLines := strings.SplitN(tail, "\n", 6)
		hits := 0
		for i := 1; i < len(nextLines) && i <= 4; i++ {
			l := strings.TrimSpace(nextLines[i])
			if strings.HasPrefix(l, "Sent:") || strings.HasPrefix(l, "To:") || strings.HasPrefix(l, "Subject:") {
				hits++
			}
		}
		// Require >=2 of {Sent:,To:,Subject:}.
		if hits >= 2 {
			body = body[:loc[0]]
		}
	}

	// Step 2: drop contiguous `>`-quoted line runs.
	out := make([]string, 0, 64)
	for _, line := range strings.Split(body, "\n") {
		if quoteLineRE.MatchString(line) {
			continue
		}
		out = append(out, line)
	}
	body = strings.Join(out, "\n")

	// Step 3: collapse blank-line runs + trim trailing whitespace.
	body = manyBlanksRE.ReplaceAllString(body, "\n\n")
	body = strings.TrimRight(body, " \t\n")
	return body
}

// chunkedStrip looks for an in-order, possibly-gapped run of normalised
// chunks of `prior` inside `current`. Returns (stripped, true) on a
// confident match; (_, false) when the hit rate is below the floor or
// inputs are too small.
func chunkedStrip(current, prior string) (string, bool) {
	curNorm := normaliseForMatch(current)
	priNorm := normaliseForMatch(prior)
	if curNorm == "" || priNorm == "" {
		return "", false
	}

	chunks := splitChunks(priNorm, chunkTargetSize)
	if len(chunks) < chunkedMinHits {
		return "", false
	}

	pos := 0
	hits := 0
	firstHitPos := -1
	for _, ch := range chunks {
		idx := strings.Index(curNorm[pos:], ch)
		if idx < 0 {
			continue
		}
		if firstHitPos < 0 {
			firstHitPos = pos + idx
		}
		pos = pos + idx + len(ch)
		hits++
	}

	if hits < chunkedMinHits {
		return "", false
	}
	if float64(hits)/float64(len(chunks)) < chunkedHitRate {
		return "", false
	}
	if firstHitPos < 0 {
		return "", false
	}

	origCut := mapNormToOrig(current, firstHitPos)
	if origCut < 0 || origCut > len(current) {
		return "", false
	}

	cut := backUpToQuoteHeaderOrLineStart(current, origCut, quoteHeaderLookback)
	out := strings.TrimRight(current[:cut], "\n \t")
	return out, true
}

// normaliseForMatch lowercases, strips leading `>`/`> ` runs from each line,
// collapses internal whitespace runs to a single space, trims each line's
// trailing whitespace, then re-joins with `\n`.
func normaliseForMatch(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		for {
			trimmed := strings.TrimLeft(line, " \t")
			if !strings.HasPrefix(trimmed, ">") {
				break
			}
			line = strings.TrimPrefix(trimmed, ">")
			line = strings.TrimPrefix(line, " ")
		}
		line = strings.ToLower(line)
		var b strings.Builder
		prevSpace := false
		for _, r := range line {
			if unicode.IsSpace(r) {
				if !prevSpace {
					b.WriteByte(' ')
					prevSpace = true
				}
				continue
			}
			b.WriteRune(r)
			prevSpace = false
		}
		lines[i] = strings.TrimRight(b.String(), " ")
	}
	return strings.Join(lines, "\n")
}

// splitChunks splits text into ~target-byte chunks on word (whitespace)
// boundaries; drops empty chunks; caps the total at chunkMaxCount.
func splitChunks(text string, target int) []string {
	if target <= 0 {
		target = chunkTargetSize
	}
	var chunks []string
	for len(text) > 0 {
		if len(chunks) >= chunkMaxCount {
			break
		}
		start := 0
		for start < len(text) && isASCIIWhitespace(text[start]) {
			start++
		}
		text = text[start:]
		if len(text) == 0 {
			break
		}
		if len(text) <= target {
			t := strings.TrimSpace(text)
			if t != "" {
				chunks = append(chunks, t)
			}
			break
		}
		cut := target
		searchEnd := target + 8
		if searchEnd > len(text) {
			searchEnd = len(text)
		}
		for i := searchEnd - 1; i >= target/2; i-- {
			if isASCIIWhitespace(text[i]) {
				cut = i
				break
			}
		}
		chunk := strings.TrimSpace(text[:cut])
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		text = text[cut:]
	}
	return chunks
}

func isASCIIWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// mapNormToOrig walks `orig` line-by-line, advancing a parallel norm cursor
// that follows the same prefix-collapse rules as normaliseForMatch. Returns
// the byte position in `orig` whose normalised prefix is at least normTarget
// chars long. The mapping is approximate but always returns a position at or
// before the true match.
func mapNormToOrig(orig string, normTarget int) int {
	if normTarget <= 0 {
		return 0
	}
	normCount := 0
	pos := 0
	for pos < len(orig) {
		lineEnd := strings.IndexByte(orig[pos:], '\n')
		var line string
		if lineEnd < 0 {
			line = orig[pos:]
			lineEnd = len(orig) - pos
		} else {
			line = orig[pos : pos+lineEnd]
		}
		norm := normaliseForMatch(line)
		if normCount+len(norm) >= normTarget {
			return pos
		}
		normCount += len(norm) + 1 // +1 for the `\n` separator
		if lineEnd >= len(orig)-pos {
			break
		}
		pos += lineEnd + 1
	}
	return pos
}

// backUpToQuoteHeaderOrLineStart walks backward from origCut up to
// `lookback` bytes. If a quote-header regex matches a line in that window,
// returns the start of THAT line. Otherwise returns the start of the line
// containing origCut. Never returns negative; always within `current` bounds.
func backUpToQuoteHeaderOrLineStart(current string, origCut, lookback int) int {
	if origCut <= 0 {
		return 0
	}
	if origCut > len(current) {
		origCut = len(current)
	}
	start := origCut - lookback
	if start < 0 {
		start = 0
	}
	window := current[start:origCut]
	candidates := []*regexp.Regexp{onWroteRE, cnWroteRE, origMsgRE}
	lastIdx := -1
	for _, re := range candidates {
		if loc := re.FindStringIndex(window); loc != nil && loc[0] > lastIdx {
			lastIdx = loc[0]
		}
	}
	if lastIdx >= 0 {
		abs := start + lastIdx
		for abs > 0 && current[abs-1] != '\n' {
			abs--
		}
		return abs
	}
	abs := origCut
	for abs > 0 && current[abs-1] != '\n' {
		abs--
	}
	return abs
}

// recordPriorBody stashes the last outbound body for a chat scope so the
// NEXT inbound from that chat can be chunked-stripped against it. Bounded
// by random eviction at cap priorBodyCap (an arbitrary entry is dropped
// when full); lost on restart — first inbound per chat post-restart falls
// back to regex strip. (D-B5)
func (s *Source) recordPriorBody(chatID, body string) {
	chatID = CanonicalAddress(chatID)
	if chatID == "" || body == "" {
		return
	}
	s.priorMu.Lock()
	defer s.priorMu.Unlock()
	if s.priorBody == nil {
		s.priorBody = make(map[string]string, 64)
	}
	if len(s.priorBody) >= priorBodyCap {
		// Evict one arbitrary entry — best-effort, eviction policy doesn't
		// matter much because the data is best-effort scaffolding.
		for k := range s.priorBody {
			delete(s.priorBody, k)
			break
		}
	}
	s.priorBody[chatID] = body
}

// lookupPriorBody fetches the last-stashed outbound body for chatID, or ""
// if none. (The persistent crash-recovery fallback is deferred — the
// outbound log store is not ported in the rebuild yet; a restart loses the
// in-memory stash and the first inbound per sender falls back to regex.)
func (s *Source) lookupPriorBody(chatID string) string {
	chatID = CanonicalAddress(chatID)
	if chatID == "" {
		return ""
	}
	s.priorMu.Lock()
	defer s.priorMu.Unlock()
	if s.priorBody == nil {
		return ""
	}
	return s.priorBody[chatID]
}
