package discord

import (
	"context"
	"fmt"

	"agentbob/contract"

	"github.com/bwmarrin/discordgo"
)

// ReactToMessage implements contract.MessageReactor for Discord. It adds a
// single emoji reaction to the user's message via MessageReactionAdd, used by
// the gateway as a "👀 seen-it" indicator at dispatch entry.
//
// chatID / messageID are MessageEvent's source-native ids — Discord channel +
// message snowflakes, passed straight through (no parse, unlike Telegram's
// int64). emoji is a unicode emoji string ("👀"); discordgo accepts that form
// directly for a standard reaction.
//
// Best-effort by contract: any failure (gateway not up, missing add-reactions
// permission, transport / API error, rate-limit) is returned (redacted so the
// bot token never leaks) for the caller to log at debug and drop — a reaction is
// a UX nicety and MUST NOT block reply delivery.
func (s *Source) ReactToMessage(ctx context.Context, chatID, messageID, emoji string) error {
	if s.dg == nil {
		return fmt.Errorf("discord: react: gateway not initialized")
	}
	if err := s.dg.MessageReactionAdd(chatID, messageID, emoji, discordgo.WithContext(ctx)); err != nil {
		return s.redactErr(err)
	}
	return nil
}

// Compile-time check: Source satisfies contract.MessageReactor. Catches a
// signature drift loudly at build time instead of at the gateway's type-assert
// site (where the wire only sees a silent capability loss — reactions just stop
// appearing).
var _ contract.MessageReactor = (*Source)(nil)
