package turn

import (
	"testing"

	"agentbob/contract"
)

// TestRenderSpeakers pins the conditional speaker-prefix: prefix human rows ONLY in a
// group with ≥2 distinct human speakers (else the lone name is noise a weak model may
// treat as content); never in a DM; never on agent/tool rows.
func TestRenderSpeakers(t *testing.T) {
	human := func(name, text string) contract.Message {
		return contract.Message{Role: "user", Content: text, Author: contract.MsgAuthor{Kind: "human", Name: name}}
	}
	const grp = "telegram:group:42"

	// single speaker → NOT prefixed (the reported "name-as-prompt" case).
	one := renderSpeakers([]contract.Message{human("至死方休", "hello"), human("至死方休", "在吗")}, grp)
	if one[0].Content != "hello" || one[1].Content != "在吗" {
		t.Fatalf("single speaker must not be prefixed: %q / %q", one[0].Content, one[1].Content)
	}

	// two distinct speakers → all human rows prefixed; the agent row left alone.
	two := renderSpeakers([]contract.Message{
		human("alice", "翻译这段"),
		human("bob", "好的"),
		{Role: "assistant", Content: "ok", Author: contract.MsgAuthor{Kind: "agent", Name: "bb"}},
	}, grp)
	if two[0].Content != "[alice]: 翻译这段" || two[1].Content != "[bob]: 好的" {
		t.Fatalf("two speakers must be prefixed: %q / %q", two[0].Content, two[1].Content)
	}
	if two[2].Content != "ok" {
		t.Fatalf("agent row must not be prefixed: %q", two[2].Content)
	}

	// DM scope → never prefixed, even with two speakers.
	dm := renderSpeakers([]contract.Message{human("alice", "x"), human("bob", "y")}, "telegram:dm:1")
	if dm[0].Content != "x" || dm[1].Content != "y" {
		t.Fatalf("DM must never be prefixed: %q / %q", dm[0].Content, dm[1].Content)
	}
}

// TestRenderSpeakers_SameNameDistinctIDs pins the id-based speaker count: two senders
// sharing a display name (distinct ids) still count as ≥2 and get attributed.
func TestRenderSpeakers_SameNameDistinctIDs(t *testing.T) {
	withID := func(id, name, text string) contract.Message {
		return contract.Message{Role: "user", Content: text, Author: contract.MsgAuthor{Kind: "human", ID: id, Name: name}}
	}
	got := renderSpeakers([]contract.Message{withID("1", "Alex", "a"), withID("2", "Alex", "b")}, "telegram:group:9")
	if got[0].Content != "[Alex]: a" || got[1].Content != "[Alex]: b" {
		t.Fatalf("same-name distinct-id should be prefixed: %q / %q", got[0].Content, got[1].Content)
	}
}
