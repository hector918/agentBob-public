package accounts

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
)

// capSink captures the final reply text of a slash command.
type capSink struct{ out string }

func (s *capSink) ContentDelta(string)        {}
func (s *capSink) TraceDelta(string)          {}
func (s *capSink) Finish(full string) error   { s.out = full; return nil }
func (s *capSink) SendFile(_, _ string) error { return nil }
func (s *capSink) LastSent() string           { return s.out }

// TestApprovePausedHint pins F42: approving a PAUSED account grants flow but the reply
// must warn that the account is still paused (status ⊥ flow — access isn't restored
// until /accounts resume), while approving an active account carries no such warning.
func TestApprovePausedHint(t *testing.T) {
	ctx := context.Background()
	// The i18n catalog isn't loaded in unit tests, so i18n.T echoes the KEY — assert on
	// the paused-hint key's presence (the base "approved" reply never carries it).
	const hintKey = "slash.accounts.approved_still_paused"

	// paused pending account → reply carries the resume hint.
	st := newMemStore()
	a, _ := st.CreateBareAccount(ctx, "Alice", "", 1)
	_ = st.SetAccountStatus(ctx, a.ID, "paused", 2)
	m := newTestManager(st)
	sink := &capSink{}
	if err := m.cmdApprove(ctx, contract.SlashContext{IsAdmin: true, Sink: sink}, []string{a.ID}); err != nil {
		t.Fatalf("cmdApprove: %v", err)
	}
	if !strings.Contains(sink.out, hintKey) {
		t.Fatalf("approving a PAUSED account must warn to resume it, got %q", sink.out)
	}

	// active pending account → no paused warning.
	st2 := newMemStore()
	b, _ := st2.CreateBareAccount(ctx, "Bob", "", 1)
	m2 := newTestManager(st2)
	sink2 := &capSink{}
	if err := m2.cmdApprove(ctx, contract.SlashContext{IsAdmin: true, Sink: sink2}, []string{b.ID}); err != nil {
		t.Fatalf("cmdApprove active: %v", err)
	}
	if strings.Contains(sink2.out, hintKey) {
		t.Fatalf("approving an ACTIVE account must NOT carry the paused warning, got %q", sink2.out)
	}
}
