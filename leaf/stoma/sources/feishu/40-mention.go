package feishu

import (
	"strings"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// mentionsBot reports whether an inbound group message @-mentions THIS bot.
//
// When selfOpenID is known (best-effort fetched at Run start), it matches
// precisely: only an @ whose target open_id equals this bot's counts — so two
// bob bots in one group don't both answer one @ (the multi-bot guard). When
// selfOpenID is "" (the fetch failed, or a single-bot deploy), it falls back to
// the original "any bot mention = me" test, which is correct for one bot/app.
func mentionsBot(mentions []*larkim.MentionEvent, selfOpenID string) bool {
	for _, m := range mentions {
		if m == nil || m.MentionedType == nil || *m.MentionedType != "bot" {
			continue
		}
		if selfOpenID == "" {
			return true // fallback: can't tell bots apart, assume it's me
		}
		// Precise mode (selfOpenID known = a careful multi-bot setup): require an
		// EXACT open_id match. A bot mention without an open_id is intentionally
		// NOT matched — answering an ambiguous mention would make two bobs in one
		// group both reply (the exact false-positive precise mode guards).
		// (Single-bot deploys hit the selfOpenID=="" branch.)
		if m.Id != nil && m.Id.OpenId != nil && *m.Id.OpenId == selfOpenID {
			return true
		}
	}
	return false
}

// stripMentions rewrites the @-mention placeholder tokens Feishu embeds in a
// message's text into clean text for the agent: a bot mention ("@_bot_1") is
// removed entirely; a user mention ("@_user_1") becomes "@Name". The mentions
// slice maps each placeholder Key to its type / name. Returns the trimmed
// result.
func stripMentions(text string, mentions []*larkim.MentionEvent) string {
	for _, m := range mentions {
		if m == nil || m.Key == nil {
			continue
		}
		repl := ""
		if m.MentionedType != nil && *m.MentionedType == "user" && m.Name != nil {
			repl = "@" + *m.Name
		}
		text = strings.ReplaceAll(text, *m.Key, repl)
	}
	return strings.TrimSpace(text)
}
