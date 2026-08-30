package accounts

import (
	"context"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/accounts/store"
)

// fakeStore is a store.Store stub: AddTurnUsage captures what the buffer flushes.
// The rest is unused by these tests.
type fakeStore struct {
	addCalls []addCall
}

type addCall struct {
	source, uid    string
	turns, success int64
	tokens         map[string]store.KindTokens
}

func (f *fakeStore) AddTurnUsage(_ context.Context, source, uid string, turns, success int64, tokens map[string]store.KindTokens, _ map[string]int64) error {
	f.addCalls = append(f.addCalls, addCall{source, uid, turns, success, tokens})
	return nil
}
func (f *fakeStore) GetHandleLang(context.Context, string, string) (string, error)      { return "", nil }
func (f *fakeStore) SetHandleLangIfEmpty(context.Context, string, string, string) error { return nil }

// unused by these tests.
func (f *fakeStore) CreateAccount(context.Context, string, string, int64) (store.Account, error) {
	return store.Account{}, nil
}
func (f *fakeStore) CreateBareAccount(context.Context, string, string, int64) (store.Account, error) {
	return store.Account{}, nil
}
func (f *fakeStore) DeleteAccount(context.Context, string) error { return nil }
func (f *fakeStore) GetAccount(context.Context, string) (store.Account, error) {
	return store.Account{}, nil
}
func (f *fakeStore) ListAccounts(context.Context) ([]store.Account, error) { return nil, nil }
func (f *fakeStore) AccountRosterPage(context.Context, int, int) ([]store.Account, int64, error) {
	return nil, 0, nil
}
func (f *fakeStore) SetAccountFlow(context.Context, string, string, int64) error {
	return nil
}
func (f *fakeStore) SetAccountStatus(context.Context, string, string, int64) error {
	return nil
}
func (f *fakeStore) BindHandle(context.Context, store.SourceHandle, int64) (store.SourceHandle, error) {
	return store.SourceHandle{}, nil
}
func (f *fakeStore) HandlesForAccount(context.Context, string) ([]store.SourceHandle, error) {
	return nil, nil
}
func (f *fakeStore) HandleCountsByAccount(context.Context) (map[string]int64, error) {
	return nil, nil
}
func (f *fakeStore) HandleBySourceUID(context.Context, string, string) (store.SourceHandle, bool, error) {
	return store.SourceHandle{}, false, nil
}
func (f *fakeStore) AccountUsageBreakdown(context.Context, string) (store.AccountUsage, error) {
	return store.AccountUsage{}, nil
}
func (f *fakeStore) CreateAPIKey(_ context.Context, k store.APIKey, _ string, _ int64) (store.APIKey, error) {
	return k, nil
}
func (f *fakeStore) APIKeyByHash(context.Context, string) (store.APIKey, bool, error) {
	return store.APIKey{}, false, nil
}
func (f *fakeStore) TouchAPIKey(context.Context, string, int64) error            { return nil }
func (f *fakeStore) ListAPIKeys(context.Context, string) ([]store.APIKey, error) { return nil, nil }
func (f *fakeStore) RevokeAPIKey(context.Context, string) error                  { return nil }
func (f *fakeStore) Ping(context.Context) error                                  { return nil }

func newTestManager(s store.Store) *Manager {
	return &Manager{store: s, buffer: newHandleUsageBuffer(s)}
}

// TestReportConsumption: tokens reported per call (handle from ctx) accumulate in
// the buffer and flush once, summed per kind, keyed on the platform-family handle.
func TestReportConsumption(t *testing.T) {
	fs := &fakeStore{}
	m := newTestManager(fs)
	// telegram2 → "telegram" family; StableUID wins over UserID.
	ev := contract.MessageEvent{Source: "telegram2", UserID: "raw", StableUID: "u42"}
	src, uid := ev.AccountHandle()
	ctx := contract.WithConsumer(context.Background(), src, uid)

	m.ReportConsumption(ctx, "llm", 100, 200)
	m.ReportConsumption(ctx, "llm", 10, 5)
	m.ReportConsumption(ctx, "ocr", 7, 0)
	m.ReportConsumption(ctx, "llm", 0, 0) // zero-token call is a no-op

	if len(fs.addCalls) != 0 {
		t.Fatalf("expected no store writes before flush, got %d", len(fs.addCalls))
	}
	m.buffer.Flush(context.Background())

	if len(fs.addCalls) != 1 {
		t.Fatalf("expected one flushed handle, got %d", len(fs.addCalls))
	}
	c := fs.addCalls[0]
	if c.source != "telegram" || c.uid != "u42" {
		t.Errorf("handle = %s/%s, want telegram/u42", c.source, c.uid)
	}
	if c.tokens["llm"] != (store.KindTokens{In: 110, Out: 205}) {
		t.Errorf("llm = %+v, want {110 205}", c.tokens["llm"])
	}
	if c.tokens["ocr"] != (store.KindTokens{In: 7, Out: 0}) {
		t.Errorf("ocr = %+v, want {7 0}", c.tokens["ocr"])
	}
	if c.turns != 0 || c.success != 0 {
		t.Errorf("turns/success = %d/%d, want 0/0 (deferred)", c.turns, c.success)
	}
}

// TestReportConsumption_NoHandle: a call with no consumer on ctx books nothing.
func TestReportConsumption_NoHandle(t *testing.T) {
	fs := &fakeStore{}
	m := newTestManager(fs)
	m.ReportConsumption(context.Background(), "llm", 100, 200)
	m.buffer.Flush(context.Background())
	if len(fs.addCalls) != 0 {
		t.Errorf("unattributed call should book nothing, got %d writes", len(fs.addCalls))
	}
}
