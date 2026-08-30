package discord

import (
	"agentbob/contract"

	"github.com/bwmarrin/discordgo"
)

// replyParentID returns the parent message id to treat as a reply target, or ""
// when the message is not a genuine reply. A Discord FORWARD also carries a
// MessageReference (Type Forward) pointing at the source message — it must NOT
// count as a reply, since forwarding one of bob's own past messages would make
// the reply quote falsely report content the user merely forwarded. Only a real
// reply (Type Default) yields a parent id.
func replyParentID(m *discordgo.Message) string {
	if m == nil || m.MessageReference == nil || m.MessageReference.Type != discordgo.MessageReferenceTypeDefault {
		return ""
	}
	return m.MessageReference.MessageID
}

// replyToBotMessage reports whether m is a genuine reply whose referenced message bob
// itself sent (referenced author == selfID). This routes a guild reply back to the
// replied-to message's session (continue) instead of @=new forking a fresh, context-
// less one. Discord inlines the referenced message, so its AUTHOR is available without
// the MessageContent intent (only the referenced content would need it). Forwards are
// excluded via replyParentID (Type Default only).
func replyToBotMessage(m *discordgo.Message, selfID string) bool {
	return selfID != "" && replyParentID(m) != "" && m.ReferencedMessage != nil &&
		m.ReferencedMessage.Author != nil && m.ReferencedMessage.Author.ID == selfID
}

// shouldDropAsGroupNoise decides whether a multi-party inbound message is noise
// the source should ignore. Such a message reaches bob only when the bot was
// @-mentioned OR it replies to one of bob's own messages (reply-to-bot). Only a
// DM always passes; every group chat (guild channel, thread, group DM — all
// mapped to ChatGroup by classifyChannel) is gated.
//
// Pure so the gate decision is testable without a live gateway. The caller passes
// replyToBot=false here ON PURPOSE: without the MessageContent intent a non-@ reply
// has empty content and is dropped downstream anyway, so admitting it gains nothing.
// reply-to-bob still drives SESSION ROUTING (replyToBotMessage → MessageEvent.ReplyToBot)
// for the @-mentioned replies that DO pass this gate.
func shouldDropAsGroupNoise(ct contract.ChatType, mentionsBot, replyToBot bool) bool {
	return ct != contract.ChatDM && !mentionsBot && !replyToBot
}
