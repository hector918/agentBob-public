package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

// mentionEntity builds a "@username" mention entity over the UTF-16
// range [off, off+length). For the ASCII test strings here UTF-16
// offsets equal byte/rune offsets.
func mentionEntity(off, length int) models.MessageEntity {
	return models.MessageEntity{
		Type:   models.MessageEntityTypeMention,
		Offset: off,
		Length: length,
	}
}

// TestMentionInfo_MultipleBotMentionsAllStripped pins that a message
// carrying several of the bot's OWN @mentions has every one removed
// (the prior code rebuilt `stripped` from the original text each pass
// and so kept all but the last). An @otheruser mention in the same
// message MUST survive untouched.
func TestMentionInfo_MultipleBotMentionsAllStripped(t *testing.T) {
	const me = "hectorzhong_bot"

	t.Run("two_bot_mentions", func(t *testing.T) {
		// "@hectorzhong_bot @hectorzhong_bot do work"
		text := "@" + me + " @" + me + " do work"
		botLen := len("@" + me)
		ents := []models.MessageEntity{
			mentionEntity(0, botLen),
			mentionEntity(botLen+1, botLen), // +1 for the space
		}
		m := &models.Message{Text: text, Entities: ents}
		mentioned, stripped := mentionInfo(m, me, 0)
		if !mentioned {
			t.Fatalf("expected mentioned=true")
		}
		if stripped != "do work" {
			t.Errorf("both bot mentions should be stripped: got %q want %q", stripped, "do work")
		}
	})

	t.Run("bot_mention_with_other_user_preserved", func(t *testing.T) {
		// "@hectorzhong_bot @alice ping" — strip the bot, keep @alice.
		botTag := "@" + me
		text := botTag + " @alice ping"
		ents := []models.MessageEntity{
			mentionEntity(0, len(botTag)),
			mentionEntity(len(botTag)+1, len("@alice")),
		}
		m := &models.Message{Text: text, Entities: ents}
		mentioned, stripped := mentionInfo(m, me, 0)
		if !mentioned {
			t.Fatalf("expected mentioned=true")
		}
		if stripped != "@alice ping" {
			t.Errorf("other-user mention must be preserved: got %q want %q", stripped, "@alice ping")
		}
	})

	t.Run("single_bot_mention_still_works", func(t *testing.T) {
		botTag := "@" + me
		text := botTag + " hello"
		ents := []models.MessageEntity{mentionEntity(0, len(botTag))}
		m := &models.Message{Text: text, Entities: ents}
		mentioned, stripped := mentionInfo(m, me, 0)
		if !mentioned {
			t.Fatalf("expected mentioned=true")
		}
		if stripped != "hello" {
			t.Errorf("got %q want %q", stripped, "hello")
		}
	})
}

// TestMentionInfo_NonMentionEntityIsNotAMention pins that a message which
// carries a NON-mention entity (e.g. Telegram's auto-tagged email/url) does
// NOT fall back to the substring check: "联系 bob@hectorzhong_bot.com" has an
// email entity but no mention entity, so the bot is NOT @'d and the text is
// returned unchanged. Entities present ⇒ authoritative.
func TestMentionInfo_NonMentionEntityIsNotAMention(t *testing.T) {
	const me = "hectorzhong_bot"
	text := "联系 bob@hectorzhong_bot.com"
	ents := []models.MessageEntity{{Type: models.MessageEntityTypeEmail, Offset: 0, Length: 1}}
	m := &models.Message{Text: text, Entities: ents}
	mentioned, stripped := mentionInfo(m, me, 0)
	if mentioned {
		t.Errorf("email is not a mention: expected mentioned=false")
	}
	if stripped != text {
		t.Errorf("text must be returned unchanged: got %q want %q", stripped, text)
	}
}

// TestMentionInfo_SubstringFallbackLengthChangingCase pins the no-entity
// substring fallback against case folds that change byte length. The prior
// code indexed the ORIGINAL text with an offset computed over a ToLower copy,
// which could slice mid-rune or panic out of bounds. "İİİİ@hectorzhong_bot hi"
// (İ = U+0130, 2 bytes, lowercases to 3 bytes) must detect the mention and
// strip it cleanly without panicking.
func TestMentionInfo_SubstringFallbackLengthChangingCase(t *testing.T) {
	const me = "hectorzhong_bot"
	text := "İİİİ@" + me + " hi"
	m := &models.Message{Text: text} // no entities → substring fallback
	mentioned, stripped := mentionInfo(m, me, 0)
	if !mentioned {
		t.Fatalf("expected mentioned=true")
	}
	if stripped != "İİİİ hi" {
		t.Errorf("got %q want %q", stripped, "İİİİ hi")
	}
}

// TestMentionInfo_BotCommandSuffix pins the Telegram group command form: the
// BotCommand menu / autocomplete sends "/whoami@botname" as ONE bot_command
// entity (no mention entity). Our own "@botname" suffix counts as an explicit
// address (mentioned=true) and is stripped so the slash registry sees a clean
// "/whoami"; another bot's command is left alone and is NOT a mention.
func TestMentionInfo_BotCommandSuffix(t *testing.T) {
	const me = "hectorzhong_bot"
	cmdEntity := func(off, length int) models.MessageEntity {
		return models.MessageEntity{Type: models.MessageEntityTypeBotCommand, Offset: off, Length: length}
	}

	t.Run("own_suffix_stripped", func(t *testing.T) {
		text := "/whoami@" + me
		m := &models.Message{Text: text, Entities: []models.MessageEntity{cmdEntity(0, len(text))}}
		mentioned, stripped := mentionInfo(m, me, 0)
		if !mentioned {
			t.Fatalf("/cmd@me must count as a mention")
		}
		if stripped != "/whoami" {
			t.Errorf("own suffix should be stripped: got %q want %q", stripped, "/whoami")
		}
	})

	t.Run("own_suffix_with_args", func(t *testing.T) {
		cmd := "/agora@" + me
		text := cmd + " overview"
		m := &models.Message{Text: text, Entities: []models.MessageEntity{cmdEntity(0, len(cmd))}}
		mentioned, stripped := mentionInfo(m, me, 0)
		if !mentioned || stripped != "/agora overview" {
			t.Errorf("got (%v, %q) want (true, %q)", mentioned, stripped, "/agora overview")
		}
	})

	t.Run("other_bots_command_ignored", func(t *testing.T) {
		text := "/whoami@some_other_bot"
		m := &models.Message{Text: text, Entities: []models.MessageEntity{cmdEntity(0, len(text))}}
		mentioned, stripped := mentionInfo(m, me, 0)
		if mentioned {
			t.Fatalf("/cmd@otherbot must NOT count as a mention")
		}
		if stripped != text {
			t.Errorf("other bot's command must be untouched: got %q", stripped)
		}
	})

	t.Run("bare_command_unchanged", func(t *testing.T) {
		text := "/whoami"
		m := &models.Message{Text: text, Entities: []models.MessageEntity{cmdEntity(0, len(text))}}
		mentioned, stripped := mentionInfo(m, me, 0)
		if mentioned || stripped != "/whoami" {
			t.Errorf("got (%v, %q) want (false, %q)", mentioned, stripped, "/whoami")
		}
	})
}

// TestCommandForOtherBot pins the multi-bot fix: a "/cmd@otherbot" must be recognised
// as not-ours (so the source drops it even when the message also replies to us), while
// "/cmd@me" and a bare "/cmd" are not "for another bot".
func TestCommandForOtherBot(t *testing.T) {
	const me = "hectorzhong_bot"
	cmdEntity := func(text string) []models.MessageEntity {
		return []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: len([]rune(text))}}
	}
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"other_bot", "/session@binserv00Bot", true},
		{"self", "/session@hectorzhong_bot", false},
		{"self_case_insensitive", "/session@HectorZhong_Bot", false},
		{"bare_command", "/session", false},
		{"not_a_command", "just chatting", false},
		{"other_bot_with_args", "/kick@binserv00Bot now", true},
	}
	for _, c := range cases {
		ents := cmdEntity(c.text)
		if c.text == "just chatting" {
			ents = nil
		} else {
			// length = the leading "/cmd@bot" token (up to first space), in runes
			tok := c.text
			for i, r := range c.text {
				if r == ' ' {
					tok = c.text[:i]
					break
				}
			}
			ents = []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: len([]rune(tok))}}
		}
		m := &models.Message{Text: c.text, Entities: ents}
		if got := commandForOtherBot(m, me); got != c.want {
			t.Errorf("%s: commandForOtherBot(%q) = %v, want %v", c.name, c.text, got, c.want)
		}
	}
}

// TestCommandForOtherBot_NoEntity covers the fallback for messages that arrive WITHOUT a
// bot_command entity (forwarded/edited): a "/cmd@otherbot" is still recognised, while a
// stray "/path@host" (target not ending in "bot") is not.
func TestCommandForOtherBot_NoEntity(t *testing.T) {
	const me = "hectorzhong_bot"
	cases := []struct {
		text string
		want bool
	}{
		{"/session@binserv00Bot", true},
		{"/session@hectorzhong_bot", false}, // for us
		{"/session", false},                 // no @target
		{"/path@host", false},               // not a bot address
		{"just text", false},
	}
	for _, c := range cases {
		m := &models.Message{Text: c.text} // no entities
		if got := commandForOtherBot(m, me); got != c.want {
			t.Errorf("no-entity %q: got %v want %v", c.text, got, c.want)
		}
	}
}

// stripLeadingForeignCommand removes the WHOLE leading "/cmd@otherbot" word so the
// leftover never reaches the slash fork as a command for us; the rest becomes turn
// input (empty when the command was the whole message) (L-D2).
func TestStripLeadingForeignCommand(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/export@reportbot pdf", "pdf"},       // word stripped, remainder kept
		{"/export@reportbot 导出  报告", "导出  报告"}, // multi-word remainder, trimmed
		{"/export@reportbot", ""},              // command was the whole message
		{"  /export@reportbot  hi ", "hi"},     // leading/trailing space tolerated
		{"just a reply", "just a reply"},       // no leading "/" → unchanged
		{"/help@reportbot\nmore", "more"},      // newline counts as the word boundary
	}
	for _, c := range cases {
		if got := stripLeadingForeignCommand(c.in); got != c.want {
			t.Errorf("strip %q: got %q want %q", c.in, got, c.want)
		}
	}
}
