package session

import (
	"encoding/json"
	"testing"

	"agentbob/contract"
)

// A pre-attachments archive payload (rows serialized as bare contract.Message)
// must still unmarshal after the ArchivedMessage envelope landed — the embedded
// Message flattens in JSON, so old rows read back with Atts nil; new payloads
// round-trip the attachments bag.
func TestArchivePayload_LegacyJSONCompat(t *testing.T) {
	legacy := `{"meta":{"ID":"s1","Key":"telegram:dm:1"},"msgs":[{"Role":"user","Content":"hi","RowKind":"msg"}]}`
	var p archivePayload
	if err := json.Unmarshal([]byte(legacy), &p); err != nil {
		t.Fatalf("legacy payload unmarshal: %v", err)
	}
	if len(p.Msgs) != 1 || p.Msgs[0].Role != "user" || p.Msgs[0].Content != "hi" || p.Msgs[0].Atts != nil {
		t.Fatalf("legacy row = %+v, want a plain user row with nil Atts", p.Msgs)
	}

	p.Msgs[0].Atts = []contract.Attachment{{Kind: "image", MIME: "image/jpeg", FileName: "a.jpg", Path: "inbox/a.jpg"}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back archivePayload
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if back.Meta.ID != "s1" || len(back.Msgs) != 1 || back.Msgs[0].Content != "hi" {
		t.Fatalf("round-trip payload = %+v", back)
	}
	if len(back.Msgs[0].Atts) != 1 || back.Msgs[0].Atts[0].Path != "inbox/a.jpg" {
		t.Fatalf("attachments bag lost in round-trip: %+v", back.Msgs[0].Atts)
	}
}
