package discord

import (
	"strings"

	"github.com/bwmarrin/discordgo"
)

// mentionsBot reports whether an inbound message @-mentions THIS bot.
//
// When selfID is known (fetched at Run start), it matches precisely: only an @
// whose target user id equals this bot's counts — so two bob bots in one channel
// don't both answer one @ (the multi-bot guard). When selfID is "" (the fetch
// failed), it falls back to "any mention = me": under the mention-only intent
// the message reached bob largely because it mentions bob, so an optimistic
// match is the safe single-bot degrade. m.Mentions is populated regardless of
// the MESSAGE CONTENT intent.
func mentionsBot(mentions []*discordgo.User, selfID string) bool {
	for _, u := range mentions {
		if u == nil {
			continue
		}
		if selfID == "" {
			return true // fallback: own-id unknown, assume the mention is me
		}
		if u.ID == selfID {
			return true
		}
	}
	return false
}

// stripMentions rewrites Discord's <@id> / <@!id> mention tokens for the agent:
// the bot's own mention is removed entirely; a user mention becomes "@Name"
// (display name from the mentions slice). Returns the trimmed result. Mentions
// not in the slice (rare) are left as-is.
func stripMentions(text string, mentions []*discordgo.User, roleIDs []string, selfID string) string {
	for _, u := range mentions {
		if u == nil {
			continue
		}
		repl := ""
		// selfID=="" means we couldn't resolve our own id — we can't tell which
		// mention is the bot, so strip ALL mentions rather than risk rendering the
		// bot's own mention as @BotName (matches mentionsBot's any-mention fallback).
		if selfID != "" && u.ID != selfID {
			repl = "@" + displayName(u)
		}
		// Discord embeds a mention as <@id> or, for a nickname, <@!id>.
		text = strings.ReplaceAll(text, "<@"+u.ID+">", repl)
		text = strings.ReplaceAll(text, "<@!"+u.ID+">", repl)
	}
	// Role mentions (<@&roleid>) carry no readable name here and aren't how bob is
	// addressed — strip the raw markup so it doesn't leak into the prompt as noise.
	for _, rid := range roleIDs {
		text = strings.ReplaceAll(text, "<@&"+rid+">", "")
	}
	return strings.TrimSpace(text)
}
