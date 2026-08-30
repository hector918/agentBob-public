package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// replyMsg builds a Discord message with a reference of refType to parentID, authored
// by authorID (the inlined referenced message). parentID=="" → not a reply at all.
func replyMsg(refType discordgo.MessageReferenceType, parentID, authorID string) *discordgo.Message {
	m := &discordgo.Message{}
	if parentID == "" {
		return m
	}
	m.MessageReference = &discordgo.MessageReference{Type: refType, MessageID: parentID}
	m.ReferencedMessage = &discordgo.Message{ID: parentID}
	if authorID != "" {
		m.ReferencedMessage.Author = &discordgo.User{ID: authorID}
	}
	return m
}

// TestReplyToBotMessage pins the reply-to-bob signal that routes a guild reply back to
// the replied-to message's session (instead of @=new). It must fire ONLY for a genuine
// reply to one of bob's OWN messages.
func TestReplyToBotMessage(t *testing.T) {
	const bot = "botID"
	cases := []struct {
		name string
		m    *discordgo.Message
		self string
		want bool
	}{
		{"reply to bob's message", replyMsg(discordgo.MessageReferenceTypeDefault, "p1", bot), bot, true},
		{"reply to another user", replyMsg(discordgo.MessageReferenceTypeDefault, "p1", "someoneElse"), bot, false},
		{"not a reply", &discordgo.Message{}, bot, false},
		{"forward of bob's message", replyMsg(discordgo.MessageReferenceTypeForward, "p1", bot), bot, false},
		{"referenced author missing", replyMsg(discordgo.MessageReferenceTypeDefault, "p1", ""), bot, false},
		{"unknown self id", replyMsg(discordgo.MessageReferenceTypeDefault, "p1", bot), "", false},
	}
	for _, c := range cases {
		if got := replyToBotMessage(c.m, c.self); got != c.want {
			t.Errorf("%s: replyToBotMessage = %v, want %v", c.name, got, c.want)
		}
	}
}
