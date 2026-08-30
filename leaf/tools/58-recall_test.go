package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentbob/contract"
)

// fakeRetrieval captures the last query and returns a canned body.
type fakeRetrieval struct {
	last contract.RetrievalQuery
	body string
	err  error
}

func (f *fakeRetrieval) Retrieve(_ context.Context, q contract.RetrievalQuery) (json.RawMessage, error) {
	f.last = q
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(f.body), nil
}

func runRecall(t *testing.T, fr *fakeRetrieval, caller contract.CallerIdentity, args string) contract.ToolResult {
	t.Helper()
	tool := recallTool{client: func() contract.RetrievalClient { return fr }}
	return tool.Run(context.Background(), contract.ToolContext{Caller: caller}, json.RawMessage(args))
}

func TestRecallDMClampsToOwnHandles(t *testing.T) {
	fr := &fakeRetrieval{body: `{"context":[]}`}
	res := runRecall(t, fr, contract.CallerIdentity{Handles: []string{"telegram:1"}}, `{"query":"定价"}`)
	if !res.OK {
		t.Fatalf("expected OK, got %+v", res)
	}
	if fr.last.QueryType != "semantic" {
		t.Errorf("query_type = %q, want semantic", fr.last.QueryType)
	}
	if got := fr.last.Filters["company_id"]; got != contract.RetrievalPersonalCompany {
		t.Errorf("company_id = %v, want %s", got, contract.RetrievalPersonalCompany)
	}
	ids, _ := fr.last.Filters["user_ids"].([]string)
	if len(ids) != 1 || ids[0] != "telegram:1" {
		t.Errorf("user_ids = %v, want [telegram:1]", fr.last.Filters["user_ids"])
	}
	if _, ok := fr.last.Filters["channel_id"]; ok {
		t.Errorf("DM must not set channel_id, got %v", fr.last.Filters["channel_id"])
	}
}

func TestRecallDMNoIdentityFailsClosed(t *testing.T) {
	fr := &fakeRetrieval{body: `{"context":[]}`}
	res := runRecall(t, fr, contract.CallerIdentity{}, `{"query":"x"}`) // personal, no handles
	if res.OK {
		t.Fatalf("expected fail-closed refusal, got OK: %+v", res)
	}
	if fr.last.QueryType != "" {
		t.Errorf("client must NOT be called when identity is unknown (got query %q)", fr.last.QueryType)
	}
}

func TestRecallGroupClampsToRoom(t *testing.T) {
	fr := &fakeRetrieval{body: `{"context":[]}`}
	res := runRecall(t, fr, contract.CallerIdentity{Room: "telegram:group:9"}, `{"who":"Alice"}`)
	if !res.OK {
		t.Fatalf("expected OK, got %+v", res)
	}
	if fr.last.QueryType != "entity_mentions" {
		t.Errorf("query_type = %q, want entity_mentions", fr.last.QueryType)
	}
	if got := fr.last.Params["who_text"]; got != "Alice" {
		t.Errorf("who_text = %v, want Alice", got)
	}
	if got := fr.last.Params["mode"]; got != "both" {
		t.Errorf("mode = %v, want both (default)", got)
	}
	if got := fr.last.Filters["channel_id"]; got != "telegram:group:9" {
		t.Errorf("channel_id = %v, want telegram:group:9", got)
	}
	if _, ok := fr.last.Filters["user_ids"]; ok {
		t.Errorf("group must not clamp user_ids, got %v", fr.last.Filters["user_ids"])
	}
}

func TestRecallBadArgs(t *testing.T) {
	fr := &fakeRetrieval{body: `{"context":[]}`}
	if res := runRecall(t, fr, contract.CallerIdentity{Handles: []string{"telegram:1"}}, `{}`); res.OK {
		t.Errorf("empty query+who should error, got OK")
	}
}

func TestRecallDisabled(t *testing.T) {
	tool := recallTool{client: func() contract.RetrievalClient { return nil }}
	res := tool.Run(context.Background(), contract.ToolContext{Caller: contract.CallerIdentity{Handles: []string{"telegram:1"}}}, json.RawMessage(`{"query":"x"}`))
	if res.OK {
		t.Errorf("nil client (disabled) should error, got OK")
	}
}

func TestRecallFailOpenOnLeafError(t *testing.T) {
	fr := &fakeRetrieval{err: context.DeadlineExceeded}
	res := runRecall(t, fr, contract.CallerIdentity{Handles: []string{"telegram:1"}}, `{"query":"x"}`)
	if !res.OK {
		t.Errorf("a down leaf must fail-open (OK with skip notice), got %+v", res)
	}
}

func TestShapeRecall(t *testing.T) {
	// valid context → compact results (role surfaced, raw handle NOT leaked)
	out := shapeRecall(json.RawMessage(`{"context":[{"content":"hi","metadata":{"user_id":"telegram:1","role":"user","timestamp":"2026-01-01T00:00:00Z"}}]}`))
	var parsed struct {
		Count   int `json:"count"`
		Results []struct {
			When, Role, Content string
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("shape output not JSON: %v (%s)", err, out)
	}
	if parsed.Count != 1 || parsed.Results[0].Content != "hi" || parsed.Results[0].Role != "user" {
		t.Errorf("unexpected shape: %s", out)
	}
	if strings.Contains(out, "telegram:1") {
		t.Errorf("raw handle leaked into model output: %s", out)
	}
	// empty context → friendly message
	if got := shapeRecall(json.RawMessage(`{"context":[]}`)); got != "（没有召回到相关记忆。）" {
		t.Errorf("empty shape = %q", got)
	}
	// non-context body → raw passthrough
	raw := `{"error":"boom"}`
	if got := shapeRecall(json.RawMessage(raw)); got != raw {
		t.Errorf("unexpected body should pass through raw, got %q", got)
	}
}
