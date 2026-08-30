package discord

import "agentbob/leaf/stoma/sources/common"

// redactErr scrubs the bot token from an error before it is logged. discordgo's
// transport / auth errors can echo request context; the token must never reach
// slog. Delegates to the shared common.RedactErr (which also runs the generic
// scrubber). A nil err or an empty token is a pass-through. Type identity is not
// preserved when something is scrubbed (the leaf already classified the raw
// error before redacting).
func (s *Source) redactErr(err error) error {
	return common.RedactErr(err, s.token)
}
