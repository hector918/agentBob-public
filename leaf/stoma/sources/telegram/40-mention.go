package telegram

import (
	"strings"

	"github.com/go-telegram/bot/models"
)

// mentionInfo reports whether m mentions the bot — via a "@username" mention entity
// matching meUser, or a "text_mention" entity pointing at meID — and returns the
// message text with the bot's "@username" mention removed (text_mentions are left in
// place; when there's no mention the text is returned unchanged).
//
// Entities are authoritative when present (so e.g. "mail bob@hectorzhong_bot.com" is
// NOT a mention — it has no mention entity). Only when a message carries no entities
// at all does it fall back to a substring check, so detection still works.
func mentionInfo(m *models.Message, meUser string, meID int64) (mentioned bool, stripped string) {
	text, entities := m.Text, m.Entities
	if text == "" {
		text, entities = m.Caption, m.CaptionEntities
	}
	stripped = text
	if meUser == "" && meID == 0 {
		return false, text
	}

	// Byte ranges of the bot's OWN @username mentions to excise. A
	// message can carry several (e.g. "@bot @bot 干活"), and each
	// entity's offsets index the ORIGINAL text — so we collect every
	// bot-mention range here and splice them all out in one pass after
	// the loop (in reverse so earlier offsets stay valid). Stripping
	// per-entity off the original text would let later strips overwrite
	// earlier ones, leaving all but the last mention behind. Mentions of
	// OTHER users (non-matching @username) are never added, so they
	// survive untouched.
	type byteRange struct{ s, e int }
	var cuts []byteRange

	// Any entity at all makes entities authoritative — Telegram tags emails
	// and URLs (email/url entities, no mention entity), so a message like
	// "联系 bob@hectorzhong_bot.com" must NOT fall back to the substring check
	// and treat the email's "@username" prefix as a mention.
	sawEntity := len(entities) > 0
	for _, e := range entities {
		switch e.Type {
		case models.MessageEntityTypeMention:
			s0, e0, ok := utf16Range(text, e.Offset, e.Length)
			if !ok || meUser == "" {
				break
			}
			mentionText := text[s0:e0]
			botPrefix := "@" + meUser
			if strings.EqualFold(mentionText, botPrefix) {
				mentioned = true
				cuts = append(cuts, byteRange{s0, e0})
				break
			}
			// Bug fix: user typed "@botnamefoo" with no
			// whitespace. Telegram's entity greedily captures every
			// adjacent username-valid char (a-zA-Z0-9_), so a URL
			// like "@hectorzhong_bothttps://…" ends up as a single
			// entity "@hectorzhong_bothttps" — strict EqualFold
			// against "@hectorzhong_bot" misses it and the bot
			// silently ignores the message. Accept prefix match (bot
			// username + extra chars) and strip just the bot prefix,
			// leaving trailing chars in stripped text so the URL
			// makes it to the agent.
			if len(mentionText) > len(botPrefix) &&
				strings.EqualFold(mentionText[:len(botPrefix)], botPrefix) {
				mentioned = true
				cuts = append(cuts, byteRange{s0, s0 + len(botPrefix)})
			}
		case models.MessageEntityTypeTextMention:
			if e.User != nil && meID != 0 && e.User.ID == meID {
				mentioned = true // a mention-by-name — leave the visible text alone
			}
		case models.MessageEntityTypeBotCommand:
			if e.Offset != 0 {
				break // only a LEADING bot_command is an address-to-me (Bot API leading-token semantics)
			}
			// Telegram's group command form: tapping the BotCommand menu (or
			// autocomplete) sends "/whoami@botname" as ONE bot_command entity —
			// no mention entity at all. Bot API convention: "/cmd@me" must act
			// as "/cmd" (it IS an explicit address), "/cmd@otherbot" is not for
			// us. Strip our own "@botname" suffix so the group gate passes and
			// the slash registry sees a clean command name; leave other bots'
			// commands untouched (mentioned stays false → group gate ignores).
			s0, e0, ok := utf16Range(text, e.Offset, e.Length)
			if !ok || meUser == "" {
				break
			}
			cmd := text[s0:e0]
			botSuffix := "@" + meUser
			if len(cmd) > len(botSuffix) && strings.EqualFold(cmd[len(cmd)-len(botSuffix):], botSuffix) {
				mentioned = true
				cuts = append(cuts, byteRange{e0 - len(botSuffix), e0})
			}
		}
	}
	// Splice out every collected bot-mention range. Reverse order keeps
	// the still-pending (earlier) ranges valid as we shorten the string.
	for i := len(cuts) - 1; i >= 0; i-- {
		c := cuts[i]
		stripped = stripped[:c.s] + stripped[c.e:]
	}
	stripped = strings.TrimSpace(stripped)
	if sawEntity {
		return mentioned, stripped // entities are authoritative
	}

	// No entities at all — fall back to a substring check.
	if meUser != "" {
		tag := "@" + meUser
		// Case-insensitive search that returns a byte index into the ORIGINAL
		// text. strings.Index over a ToLower copy is unsafe: ToLower can change
		// byte length (İ U+0130 2B→3B, K Kelvin 3B→1B), so an offset from the
		// copy can land mid-rune or out of bounds on the original (panic).
		if i, n := caseInsensitiveIndex(text, tag); i >= 0 {
			return true, strings.TrimSpace(text[:i] + text[i+n:])
		}
	}
	return false, text
}

// commandForOtherBot reports whether m is a slash command explicitly addressed to a
// DIFFERENT bot ("/cmd@otherbot"). In a multi-bot group this bot must ignore it entirely:
// otherwise, when the same message ALSO replies to THIS bot (which opens the group gate),
// the unstripped "@otherbot" suffix reaches the slash registry as an unknown command and
// the wrong bot answers "no such command". Only a LEADING bot_command entity counts
// (Bot API leading-token semantics); "/cmd" with no "@" targets every bot, not "another".
func commandForOtherBot(m *models.Message, meUser string) bool {
	if meUser == "" {
		return false
	}
	text, entities := m.Text, m.Entities
	if text == "" {
		text, entities = m.Caption, m.CaptionEntities
	}
	for _, e := range entities {
		if e.Type != models.MessageEntityTypeBotCommand || e.Offset != 0 {
			continue
		}
		s0, e0, ok := utf16Range(text, e.Offset, e.Length)
		if !ok {
			return false
		}
		cmd := text[s0:e0] // e.g. "/session@binserv00Bot"
		at := strings.LastIndexByte(cmd, '@')
		if at < 0 {
			return false // "/cmd" — no @target → for all bots, not "another"
		}
		target := cmd[at+1:]
		return target != "" && !strings.EqualFold(target, meUser)
	}
	// No leading bot_command entity (forwarded / edited messages can arrive without
	// entities). Fall back to the raw leading "/cmd@X" token — but only treat X as a bot
	// address when it ends in "bot" (telegram bot usernames always do), so a stray
	// "/path@host" is not mistaken for a command to another bot.
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return false
	}
	tok := t
	if i := strings.IndexAny(t, " \t\n"); i >= 0 {
		tok = t[:i]
	}
	at := strings.LastIndexByte(tok, '@')
	if at < 0 {
		return false
	}
	target := tok[at+1:]
	return target != "" && strings.HasSuffix(strings.ToLower(target), "bot") && !strings.EqualFold(target, meUser)
}

// stripLeadingForeignCommand removes the LEADING "/cmd@otherbot" command word from text
// (the whole word, up to the first whitespace — leaving a bare "/cmd" would make the
// inbound slash fork mis-handle it as a command for US). The caller has already confirmed
// via commandForOtherBot that the message leads with a foreign command; this just excises
// that first word and returns the trimmed remainder ("" when the command was the whole
// message). A non-"/" leading text (defensive) is returned unchanged. (L-D2)
func stripLeadingForeignCommand(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return text
	}
	if i := strings.IndexAny(t, " \t\n"); i >= 0 {
		return strings.TrimSpace(t[i+1:])
	}
	return "" // the whole message was just the foreign command
}

// caseInsensitiveIndex finds the first case-insensitive occurrence of sub in s
// and returns its starting byte offset in s together with the byte length n of
// the matched span (which may differ from len(sub) when a length-changing
// case fold is involved). Returns (-1, 0) when not found. Offsets index s
// itself, so slicing s[start:start+n] is always valid.
func caseInsensitiveIndex(s, sub string) (int, int) {
	if sub == "" {
		return 0, 0
	}
	for i := range s {
		if rest := s[i:]; len(rest) >= len(sub) {
			// Walk both runewise via EqualFold over the candidate prefix.
			for n := len(sub); i+n <= len(s); n++ {
				cand := s[i : i+n]
				if strings.EqualFold(cand, sub) {
					return i, n
				}
				// Stop growing once the candidate is clearly longer than any
				// plausible fold of sub (case folds change length by at most a
				// few bytes per rune; cap the slack generously).
				if n > len(sub)+8 {
					break
				}
			}
		}
	}
	return -1, 0
}

// utf16Range converts a (UTF-16 offset, UTF-16 length) — the units Telegram message
// entities use — into a byte range [start, end) in s. ok is false if the offset is
// out of bounds.
func utf16Range(s string, off, length int) (start, end int, ok bool) {
	start, end = -1, -1
	u16 := 0
	for i, r := range s {
		if u16 == off {
			start = i
		}
		if u16 == off+length {
			end = i
			break
		}
		if r >= 0x10000 {
			u16 += 2
		} else {
			u16++
		}
	}
	if start < 0 {
		return 0, 0, false
	}
	if end < 0 {
		end = len(s)
	}
	return start, end, true
}
