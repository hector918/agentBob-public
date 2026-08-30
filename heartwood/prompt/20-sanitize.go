package prompt

import (
	"strings"
	"unicode"

	"agentbob/contract"
)

// This file holds the prompt-text hygiene primitives shared by every layer that
// renders untrusted display names / quoted text into the STRUCTURED prompt (the
// turn core's speaker prefixing, flow/compose's per-event rendering, and the
// session nudge fast-lane). They live here — direct-imported like clock/files —
// because the same "[name]: " / "[In reply to …]" meta structure must be defended
// identically wherever it is produced; a per-module copy silently drifts.

// SanitizeSpeaker strips characters from an untrusted display name that could spoof
// the "[name]: " prompt structure (brackets, newlines) and bounds the length, so a
// member can't fake an extra speaker / role boundary via their chat name.
func SanitizeSpeaker(s string) string {
	s = strings.NewReplacer("[", "", "]", "", "\n", " ", "\r", " ").Replace(s)
	s = strings.TrimSpace(StripControl(s))
	if r := []rune(s); len(r) > 48 {
		s = strings.TrimSpace(string(r[:48]))
	}
	return s
}

// SanitizeQuoted neutralises untrusted text for the one-line bracketed reply block:
// brackets and double quotes could spoof the meta structure, control chars could
// inject extra lines (StripControl). The rune cap keeps a long quoted message from
// bloating the prompt; the quote is context, not a resolvable handle, so truncation
// is safe.
func SanitizeQuoted(s string, max int) string {
	s = strings.NewReplacer("[", "", "]", "", `"`, "'", "\n", " ", "\r", " ").Replace(s)
	s = strings.TrimSpace(StripControl(s))
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max])) + "…"
	}
	return s
}

// quotedKindMax bounds the kind noun inside a media note. A note's whole value is its
// tail clause, and the note shares ReplyLine's QuotedMax budget with the parent's prose
// — an unbounded kind (sources build it from an ATTACKER-CHOSEN filename) would push
// that clause past the cut. 40 runes leaves the note well under QuotedMax whatever the
// filename.
const quotedKindMax = 40

// QuotedMediaNote renders the note a source puts in ReplyToText when the replied-to
// message carried media. kind is the bare noun ("一张图片", "一个文件：report.pdf");
// inAttachments says whether that media is being ingested into THIS turn's attachment
// list. It lives beside SanitizeQuoted for the same reason the rest of this file does:
// the note must survive that sanitiser identically in every source.
//
// The wording is load-bearing and was measured against agents-a2 on the
// incident prompt (a plain comment replying to a picture bob itself had sent):
//   - No brackets. A bracketed "[photo]" is stripped to the bare word `photo` inside
//     the quote's own quotes, where it reads as the parent's TEXT — the model then
//     hunts for a file by that name. Leaked a vision call in 11/36 runs.
//   - Naming the kind alone ("（一张图片）") is WORSE than the bare word: it asserts a
//     picture exists and sends the model looking harder — 16/22 runs. Saying it is out
//     of reach ends it: 0/18, and that held under all three tool-selection rubrics
//     tried, i.e. the fix is here and does not depend on the rubric.
//
// Hence inAttachments, which decides whether the out-of-reach clause is TRUE. Telegram
// downloads a user-authored parent's media into this turn's inbox, where flow/compose
// lists it as 「（来自被回复的消息）」; claiming out-of-reach there would both contradict
// the attachment list and — being the stronger signal — suppress the legitimate reach
// for a picture the user is plainly asking about.
func QuotedMediaNote(kind string, inAttachments bool) string {
	if kind == "" {
		return ""
	}
	if r := []rune(kind); len(r) > quotedKindMax {
		kind = strings.TrimSpace(string(r[:quotedKindMax])) + "…"
	}
	if inAttachments {
		return "（" + kind + "）"
	}
	return "（" + kind + "，不是本轮附件）"
}

// StripControl removes control chars — all C0 (<0x20, incl \n \r \t), DEL, and C1
// (0x80–0x9F, incl U+0085 NEL), plus the U+2028/U+2029 line/paragraph separators —
// that would break a one-line prompt entry or inject a fake list line. unicode.IsControl
// covers C0+DEL+C1; the two Unicode separators are not "control" but break the one-line
// contract just the same. Unlike a rune-capping cleaner it does NOT truncate: callers such
// as the attachment path renderer need the value echoed back verbatim to resolve, so
// clipping it would make it unusable; bounding is the caller's job when needed.
func StripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, s)
}

// QuotedMax is the rune cap ReplyLine puts on a quoted reply. Exported because it is
// a BUDGET a source has to plan around, not just an internal bound: a source that
// concatenates prose and a QuotedMediaNote must order them so the cut lands in the
// prose (note first), since a half-eaten note is worse than none — see there.
const QuotedMax = 120

// ReplyLine renders the quoted-reply context (`[In reply to Carol: "..."]`) when the
// source carried the replied-to message's text — so the model sees WHAT the user is
// answering, not just their answer. Empty when the event is not a reply (or the
// source couldn't fetch the parent text).
func ReplyLine(ev contract.MessageEvent) string {
	quote := SanitizeQuoted(ev.ReplyToText, QuotedMax)
	if quote == "" {
		return ""
	}
	if who := SanitizeQuoted(ev.ReplyToUser, 48); who != "" {
		return "[In reply to " + who + ": \"" + quote + "\"]"
	}
	return "[In reply to: \"" + quote + "\"]"
}
