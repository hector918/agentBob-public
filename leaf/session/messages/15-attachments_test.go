package messages

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/pgpool"
	"agentbob/trunk"
)

// TestAttachmentsJSON: the persisted blob carries the durable metadata, drops the
// transient LocalPath (json:"-"), and is nil (→ SQL NULL) when there are none.
func TestAttachmentsJSON(t *testing.T) {
	if got := attachmentsJSON(nil); got != nil {
		t.Errorf("empty → %v, want nil (SQL NULL)", got)
	}
	raw := attachmentsJSON([]contract.Attachment{
		{Kind: "image", MIME: "image/jpeg", FileName: "photo.jpg", Path: "inbox/photo.jpg", Size: 123, LocalPath: "/staging/x-photo.jpg"},
	})
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("non-empty → %T, want JSON string", raw)
	}
	if strings.Contains(s, "staging") || strings.Contains(s, "LocalPath") {
		t.Errorf("blob must not carry transient LocalPath: %s", s)
	}
	// round-trips back to the structured form (minus LocalPath).
	var back []contract.Attachment
	if err := json.Unmarshal([]byte(s), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back) != 1 || back[0].Path != "inbox/photo.jpg" || back[0].Size != 123 || back[0].LocalPath != "" {
		t.Errorf("round-trip = %+v, want clean Path/Size and empty LocalPath", back)
	}
}

// TestDedupAttachmentsByPath: newest-first input, keep first per Path (newest wins),
// drop empty-Path entries.
func TestDedupAttachmentsByPath(t *testing.T) {
	in := []contract.Attachment{
		{Path: "inbox/a.jpg", Size: 200}, // newest a
		{Path: "inbox/b.pdf"},
		{Path: ""},                       // dropped
		{Path: "inbox/a.jpg", Size: 100}, // older a → dropped
	}
	got := dedupAttachmentsByPath(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2\n%+v", len(got), got)
	}
	if got[0].Path != "inbox/a.jpg" || got[0].Size != 200 {
		t.Errorf("first = %+v, want newest a.jpg (Size 200)", got[0])
	}
	if got[1].Path != "inbox/b.pdf" {
		t.Errorf("second = %+v, want b.pdf", got[1])
	}
}

// liveStore opens a migrated messages store against the test DB (env-gated).
func liveStore(t *testing.T) (*PG, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run messages store integration tests")
	}
	ctx := context.Background()
	reg := trunk.NewRegistry()
	m := pgpool.New(dsn)
	if err := m.Start(ctx, reg); err != nil {
		t.Fatalf("pgpool start: %v", err)
	}
	db := trunk.Require[contract.DB](reg)
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewPG(db), func() { _ = m.Stop(ctx) }
}

// TestRecentAttachmentsRoundTrip: attachments persist on the user row's JSONB and read
// back deduped newest-wins; a no-attachment row contributes nothing; LocalPath is never
// stored.
func TestRecentAttachmentsRoundTrip(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := "test_att_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	human := contract.MsgAuthor{Kind: "human", ID: "u1", Name: "U"}

	// turn 1: one image (with a transient LocalPath that must NOT persist)
	if err := st.AppendUserMsgs(ctx, sid, []contract.UserMsg{{Author: human, Text: "a", Attachments: []contract.Attachment{
		{Kind: "image", MIME: "image/jpeg", FileName: "photo.jpg", Path: "inbox/photo.jpg", Size: 100, LocalPath: "/staging/x"},
	}}}); err != nil {
		t.Fatalf("append turn1: %v", err)
	}
	// turn 2: a doc + the SAME image path re-sent newer (Size 200)
	if err := st.AppendUserMsgs(ctx, sid, []contract.UserMsg{{Author: human, Text: "b", Attachments: []contract.Attachment{
		{Kind: "document", MIME: "application/pdf", FileName: "r.pdf", Path: "inbox/r.pdf"},
		{Kind: "image", MIME: "image/jpeg", FileName: "photo.jpg", Path: "inbox/photo.jpg", Size: 200},
	}}}); err != nil {
		t.Fatalf("append turn2: %v", err)
	}
	// a no-attachment row contributes nothing
	if err := st.AppendMessage(ctx, sid, "user", "c", human); err != nil {
		t.Fatalf("append plain: %v", err)
	}

	got, err := st.RecentAttachments(ctx, sid, 0)
	if err != nil {
		t.Fatalf("RecentAttachments: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (deduped)\n%+v", len(got), got)
	}
	byPath := map[string]contract.Attachment{}
	for _, a := range got {
		byPath[a.Path] = a
		if a.LocalPath != "" {
			t.Errorf("LocalPath persisted for %s: %q", a.Path, a.LocalPath)
		}
	}
	if a, ok := byPath["inbox/photo.jpg"]; !ok || a.Size != 200 {
		t.Errorf("photo.jpg = %+v, want newest (Size 200)", a)
	}
	if _, ok := byPath["inbox/r.pdf"]; !ok {
		t.Errorf("r.pdf missing from bag: %+v", got)
	}
}
