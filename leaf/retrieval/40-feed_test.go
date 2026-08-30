package retrieval

import (
	"testing"
	"time"

	"agentbob/contract"
)

func ev(source, uid, name, text, msgID string, ct contract.ChatType) contract.MessageEvent {
	return contract.MessageEvent{
		Source: source, UserID: uid, UserName: name, Text: text,
		MessageID: msgID, ChatType: ct, ReceivedAt: time.Unix(1000, 0),
	}
}

func TestBuildRowsDM(t *testing.T) {
	rows := buildRows(contract.RetrievalTurn{
		Sid:      "s1",
		Events:   []contract.MessageEvent{ev("telegram", "1", "Alice", "hello", "m1", contract.ChatDM)},
		Reply:    "hi there",
		AnchorID: "m1",
	})
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (user+assistant), got %d", len(rows))
	}
	u := rows[0]
	if u.MessageID != "s1:m1" {
		t.Errorf("user message_id = %q, want s1:m1", u.MessageID)
	}
	if u.UserID != "telegram:1" {
		t.Errorf("user_id = %q, want telegram:1", u.UserID)
	}
	if u.CompanyID != contract.RetrievalPersonalCompany {
		t.Errorf("company = %q, want %s", u.CompanyID, contract.RetrievalPersonalCompany)
	}
	if u.Content != "hello" {
		t.Errorf("DM content must NOT be name-prefixed, got %q", u.Content)
	}
	if u.ChannelID != "" {
		t.Errorf("DM channel_id must be empty, got %q", u.ChannelID)
	}
	a := rows[1]
	if a.MessageID != "s1:m1:a" || a.UserID != contract.RetrievalBotHandle || a.Role != "assistant" {
		t.Errorf("assistant row wrong: %+v", a)
	}
}

func TestBuildRowsGroupPrefixesName(t *testing.T) {
	rows := buildRows(contract.RetrievalTurn{
		Sid:    "s2",
		Room:   "telegram:group:9",
		Events: []contract.MessageEvent{ev("telegram", "1", "Alice", "delayed", "m7", contract.ChatGroup)},
	})
	if len(rows) != 1 {
		t.Fatalf("want 1 row (no reply), got %d", len(rows))
	}
	r := rows[0]
	if r.Content != "[Alice]: delayed" {
		t.Errorf("group content must be name-prefixed, got %q", r.Content)
	}
	if r.ChannelID != "telegram:group:9" {
		t.Errorf("group channel_id = %q, want the room", r.ChannelID)
	}
	if r.UserID != "telegram:1" {
		t.Errorf("group user_id = %q, want the speaker handle", r.UserID)
	}
}

func TestBuildRowsSkipsEmpty(t *testing.T) {
	rows := buildRows(contract.RetrievalTurn{
		Sid:    "s3",
		Events: []contract.MessageEvent{ev("telegram", "1", "Alice", "   ", "m1", contract.ChatDM)},
		Reply:  "",
	})
	if len(rows) != 0 {
		t.Fatalf("empty text + empty reply → 0 rows, got %d", len(rows))
	}
}

// TestBuildRowsAnchorlessReplySkipped: a proactive/resume turn (no user event → empty
// AnchorID) with a reply emits NO assistant row. Every such turn in one sid would share
// message_id "<sid>::a", so the leaf's message_id dedup would silently drop all but the
// first — these anchor-less bot-initiated replies are simply not fed (B).
func TestBuildRowsAnchorlessReplySkipped(t *testing.T) {
	rows := buildRows(contract.RetrievalTurn{
		Sid:   "s5",
		Reply: "proactive ping", // no Events, no AnchorID
	})
	if len(rows) != 0 {
		t.Fatalf("anchor-less reply must feed 0 rows (collision-prone assistant row skipped), got %d: %+v", len(rows), rows)
	}
}

func TestBuildRowsCompanyOverride(t *testing.T) {
	rows := buildRows(contract.RetrievalTurn{
		Sid:     "s4",
		Company: "acme",
		Events:  []contract.MessageEvent{ev("telegram", "1", "Alice", "x", "m1", contract.ChatGroup)},
	})
	if rows[0].CompanyID != "acme" {
		t.Errorf("company = %q, want acme", rows[0].CompanyID)
	}
}
