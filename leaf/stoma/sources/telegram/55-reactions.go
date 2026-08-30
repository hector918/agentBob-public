package telegram

import (
	"context"
	"fmt"
	"strconv"

	"agentbob/contract"
	"agentbob/leaf/stoma/sources/sendgate"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// ReactToMessage implements contract.MessageReactor for Telegram. It sets a
// single emoji reaction on the user's message via the Telegram Bot API
// 7.0+ setMessageReaction endpoint, used by the gateway as a "👀
// seen-it" indicator at dispatch entry.
//
// chatID / messageID are MessageEvent's source-native string ids:
// chatID is parsed back to int64, messageID to int (Telegram's native
// types). emoji must be a member of Telegram's standard reaction set
// (docs/bots/api#reactiontypeemoji; "👀" / "✅" / "👍" etc).
//
// Best-effort:any failure (bad-id parse, bot client not yet
// initialised, transport / API error, rate-limit) is returned through
// redactTokenErr so the bot token never appears in the message — the
// caller (gateway dispatch) is expected to slog.Debug it and continue.
// Reaction is a UX nicety; it MUST NOT block reply delivery.
func (s *Source) ReactToMessage(ctx context.Context, chatID, messageID, emoji string) error {
	chatIDInt, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("react: bad chatID %q: %w", chatID, err)
	}
	msgIDInt, err := strconv.Atoi(messageID)
	if err != nil {
		return fmt.Errorf("react: bad messageID %q: %w", messageID, err)
	}
	// s.b is set once in Run before any update fires, and is read by
	// every other method here without a lock (HealthCheck, typing,
	// SendMessage in the send queue). Mirror that pattern instead of
	// inventing a lock just for this call — the contract is "don't
	// call any Source method before Run has started", same as the
	// other s.b consumers rely on.
	if s.b == nil {
		return fmt.Errorf("react: bot not initialized")
	}

	// Through the bot's single outbound serialiser — a reaction is a real API call
	// that counts against the per-bot limit, so it shares the 429 cooldown too.
	// Best-effort ornament: a read-ack that queues past its useful life should be
	// dropped, not delivered late — the bounded ctx is the gate's timeliness signal.
	ctx, cancel := context.WithTimeout(ctx, sendgate.ReactionTTL)
	defer cancel()
	err = s.gate.Do(ctx, func() error {
		_, e := s.b.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
			ChatID:    chatIDInt,
			MessageID: msgIDInt,
			Reaction: []models.ReactionType{{
				Type:              models.ReactionTypeTypeEmoji,
				ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: emoji},
			}},
		})
		return e
	})
	if err != nil {
		return redactTokenErr(err, s.token)
	}
	return nil
}

// Compile-time check: Source satisfies contract.MessageReactor. Catches a
// signature drift loudly at build time instead of at the gateway's
// type-assert site (where the wire only sees a silent capability loss
// — reactions just stop appearing, with no log).
var _ contract.MessageReactor = (*Source)(nil)
