package feishu

import (
	"agentbob/leaf/stoma/sources/sendgate"
	"context"
	"fmt"

	"agentbob/contract"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// ReactToMessage implements contract.MessageReactor for Feishu. It adds a single
// emoji reaction to the user's message via the IM message-reaction Create
// endpoint, used by the gateway as a "seen-it" indicator at dispatch entry and
// internally as a media-turn read-ack (onMessageReceive react on an attachment
// message). The reaction is meant to STAY: it is set once and never removed or
// swapped, so no reaction id is retained.
//
// emoji is the portable unicode glyph the cross-source contract passes (in practice
// always 👀, the only read-ack glyph any caller sends); feishuEmojiType converts it
// to Feishu's own emoji_type enum, which is what its reaction API keys on.
//
// chatID is unused (the message id alone addresses the reaction) but kept to
// satisfy the contract signature shared across platforms.
//
// Best-effort: any failure (bad message id, api client not yet initialised,
// transport / API error, permission denied) is returned through redactErr so the
// app secret never appears in the message — the caller is expected to log it and
// continue. Reaction is a UX nicety; it MUST NOT block reply delivery.
// feishuEmojiType maps a portable reaction glyph (the cross-source contract that
// telegram/discord pass as a unicode emoji) to Feishu's own emoji_type enum,
// which is what its reaction API keys on — a raw glyph is rejected. "OnIt" (在做了)
// is Feishu's seen/processing reaction, the equivalent of the 👀 read-ack. Unknown
// glyphs pass through unchanged (best-effort; the API call then fails and is logged).
func feishuEmojiType(glyph string) string {
	switch glyph {
	case "👀":
		return "OnIt"
	}
	return glyph
}

func (s *Source) ReactToMessage(ctx context.Context, _ string, messageID, emoji string) error {
	if messageID == "" {
		return fmt.Errorf("feishu: react needs a message id")
	}
	if s.api == nil {
		return fmt.Errorf("feishu: react: api client not initialized")
	}
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(feishuEmojiType(emoji)).Build()).
			Build()).
		Build()
	// A reaction is a REST call that counts against the per-bot limit, so it shares the
	// outbound serialiser + 429 cooldown with every reply send. (L-FEISHU-D1)
	// Best-effort ornament: a read-ack that queues past its useful life should be
	// dropped, not delivered late — the bounded ctx is the gate's timeliness signal.
	ctx, cancel := context.WithTimeout(ctx, sendgate.ReactionTTL)
	defer cancel()
	return s.gate.Do(ctx, func() error {
		resp, err := s.api.Im.V1.MessageReaction.Create(ctx, req)
		if err != nil {
			// The #1 cause of a persistent failure is a missing app permission
			// (im:message.reaction:write / "添加、删除消息的表情回复") — the redacted
			// error carries the api context so the operator can grant it.
			return s.redactErr(fmt.Errorf("feishu: react: %w", err))
		}
		if d, ok := feishuRespRateLimited(resp.ApiResp); ok {
			return &feishuRateLimit{retryAfter: d}
		}
		if !resp.Success() {
			return s.redactErr(fmt.Errorf("feishu: react non-OK: code=%d msg=%s (a permission-denied code means the app lacks the message-reaction write scope)", resp.Code, resp.Msg))
		}
		return nil
	})
}

// Compile-time check: Source satisfies contract.MessageReactor. Catches a
// signature drift loudly at build time instead of at the gateway's type-assert
// site (where the wire only sees a silent capability loss — reactions just stop
// appearing, with no log).
var _ contract.MessageReactor = (*Source)(nil)
