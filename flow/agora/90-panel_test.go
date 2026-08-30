package agora

import (
	"context"
	"testing"

	"agentbob/contract"
	"agentbob/trunk"
)

// fakeChatHistory is a minimal contract.ChatHistory keyed by scope; it records the
// scopes it was asked for so the test can assert the inbox→scopes resolution.
type fakeChatHistory struct {
	byScope   map[string][]contract.Message
	gotScopes []string
}

func (f *fakeChatHistory) MessagesForScopes(_ context.Context, scopes []string, limit, offset int) ([]contract.Message, int64, error) {
	f.gotScopes = scopes
	var all []contract.Message
	for _, s := range scopes {
		all = append(all, f.byScope[s]...)
	}
	total := int64(len(all))
	if offset >= len(all) {
		return nil, total, nil
	}
	all = all[offset:]
	if limit > 0 && limit < len(all) {
		all = all[:limit]
	}
	return all, total, nil
}

func newChatFlow(ag contract.Agora, ch contract.ChatHistory) *flow {
	reg := trunk.NewRegistry()
	if ag != nil {
		trunk.Provide[contract.Agora](reg, ag)
	}
	if ch != nil {
		trunk.Provide[contract.ChatHistory](reg, ch)
	}
	return &flow{reg: reg}
}

// TestChatPanel_ViewAndPage pins the chat-log panel: View("inbox:<id>") resolves the
// inbox to its scopes (via Agora) and returns a Paged table of the merged history;
// Page("chat:<id>") pages the same; bad ids error; an unknown inbox shows empty.
func TestChatPanel_ViewAndPage(t *testing.T) {
	ctx := context.Background()
	ag := &fakeAgora{scope: "telegram:group:9", inboxID: "ib_x"}
	ch := &fakeChatHistory{byScope: map[string][]contract.Message{
		"telegram:group:9": {{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
	}}
	p := newChatFlow(ag, ch).chatPanel()

	fields, err := p.View(ctx, "inbox:ib_x")
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if len(fields) != 1 || fields[0].Kind != "table" {
		t.Fatalf("view fields = %+v, want one table", fields)
	}
	tbl := fields[0]
	if tbl.ID != "chat:ib_x" || !tbl.Paged || tbl.Total != 2 || len(tbl.Rows) != 2 {
		t.Fatalf("table = %+v", tbl)
	}
	if len(ch.gotScopes) != 1 || ch.gotScopes[0] != "telegram:group:9" {
		t.Fatalf("resolved scopes = %v, want [telegram:group:9]", ch.gotScopes)
	}

	tp, err := p.Page(ctx, "chat:ib_x", 1, 0)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if tp.Total != 2 || len(tp.Rows) != 1 {
		t.Fatalf("page = %+v, want total 2 / 1 row", tp)
	}

	if _, err := p.View(ctx, "nope"); err == nil {
		t.Fatal("View with no inbox: prefix must error")
	}
	if _, err := p.Page(ctx, "nope", 10, 0); err == nil {
		t.Fatal("Page with no chat: prefix must error")
	}

	// unknown inbox (agora returns no scopes) → an empty-state text field, no error.
	fs, err := p.View(ctx, "inbox:ib_unknown")
	if err != nil {
		t.Fatalf("view unknown: %v", err)
	}
	if len(fs) != 1 || fs[0].Kind != "text" {
		t.Fatalf("unknown-inbox view = %+v, want one text field", fs)
	}
}
