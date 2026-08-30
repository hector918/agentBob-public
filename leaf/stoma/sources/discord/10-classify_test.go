package discord

import (
	"testing"

	"agentbob/contract"

	"github.com/bwmarrin/discordgo"
)

// classifyChannel must fail CLOSED: a guild-less channel whose type lookup fails
// (unknown) is gated as a group, NOT treated as an ungated 1:1 DM — so a transient
// lookup failure never makes bob answer an un-@'d group-DM message (L-D4). A confirmed
// 1:1 DM stays ChatDM; a confirmed group DM and a guild channel are ChatGroup.
func TestClassifyChannel_FailsClosedOnUnknownDM(t *testing.T) {
	s := &Source{} // nil dg → lookupChannel returns nil → type unknown

	// Unknown guild-less channel (lookup fails, uncached) → gated as group, not ChatDM.
	if ct, _ := s.classifyChannel(&discordgo.Message{ChannelID: "c_unknown"}); ct != contract.ChatGroup {
		t.Fatalf("unknown guild-less channel = %v, want ChatGroup (fail-closed)", ct)
	}

	// Confirmed types come from the cache (a real lookup populates it).
	s.dmTypeCache = map[string]bool{"c_dm": false, "c_group": true}

	if ct, _ := s.classifyChannel(&discordgo.Message{ChannelID: "c_dm"}); ct != contract.ChatDM {
		t.Fatalf("confirmed 1:1 DM = %v, want ChatDM", ct)
	}
	if ct, _ := s.classifyChannel(&discordgo.Message{ChannelID: "c_group"}); ct != contract.ChatGroup {
		t.Fatalf("confirmed group DM = %v, want ChatGroup", ct)
	}
	if ct, _ := s.classifyChannel(&discordgo.Message{ChannelID: "c_guild", GuildID: "g1"}); ct != contract.ChatGroup {
		t.Fatalf("guild channel = %v, want ChatGroup", ct)
	}
}
