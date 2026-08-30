package session_test

import (
	"context"
	"os"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/pgpool"
	"agentbob/leaf/session"
	"agentbob/leaf/session/store"
	"agentbob/trunk"
)

// TestModule_ThroughTrunk wires pgpool + session via the trunk and checks the
// session module migrates, provides contract.SessionManager, and dispatches an
// event end-to-end through the stub turn handler (no turn module registered).
func TestModule_ThroughTrunk(t *testing.T) {
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run the session module integration test")
	}
	ctx := context.Background()
	reg := trunk.NewRegistry()
	pg := pgpool.New(dsn)
	if err := pg.Start(ctx, reg); err != nil {
		t.Fatalf("pgpool start: %v", err)
	}
	defer pg.Stop(ctx)

	// The session module Needs the source bus (Gateway) for its source lookup
	// and the slash registry (to register /new).
	trunk.Provide[contract.Gateway](reg, fakeGateway{})
	trunk.Provide[contract.SlashRegistry](reg, fakeSlashRegistry{})

	sm := session.New(session.Config{})
	if sm.Optional() {
		t.Fatal("session must be critical")
	}
	if err := sm.Start(ctx, reg); err != nil {
		t.Fatalf("session start: %v", err)
	}
	if sm.Health() != trunk.StateReady {
		t.Fatalf("Health = %v, want Ready", sm.Health())
	}

	// The capability is registered and Submit reaches the stub turn without panic.
	mgr := trunk.Require[contract.SessionManager](reg)
	ev := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, ChatID: "mod-" + uscope(""), UserID: "u", Text: "hi"}
	mgr.Submit(ctx, ev)
	sm.Manager().Wait()

	if err := sm.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if sm.Health() != trunk.StateStopped {
		t.Fatalf("Health after Stop = %v, want Stopped", sm.Health())
	}
}

// fakeGateway is a minimal contract.Gateway for the module test: it resolves
// every source name to the dispatch stub and exposes no live event stream.
type fakeGateway struct{}

func (fakeGateway) Events() <-chan contract.MessageEvent { return nil }
func (fakeGateway) SourceByName(string) contract.Source  { return srcStub{} }
func (fakeGateway) SourceNames() []string                { return nil }

// fakeSlashRegistry is a no-op contract.SlashRegistry for the module test —
// session registers /new into it at Start.
type fakeSlashRegistry struct{}

func (fakeSlashRegistry) Register(contract.SlashCommand)                        {}
func (fakeSlashRegistry) Dispatch(context.Context, contract.SlashContext) error { return nil }
func (fakeSlashRegistry) List() []contract.SlashCommand                         { return nil }

func TestRecovery_ClearOnBoot(t *testing.T) {
	_, st, done := liveManager(t)
	defer done()
	ctx := context.Background()
	mgr := session.NewManager(st, noopHandler{}, stubSrcByName, session.Config{})

	scope := uscope("recovery")
	chatID := "rc-" + uscope("")
	if err := st.AddPending(ctx, scope, "telegram", chatID, "", "m1", nil); err != nil {
		t.Fatal(err)
	}

	// The store surfaces our pending chat (what boot recovery will list).
	chats, err := st.ListPendingChats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mine *store.PendingChat
	for i := range chats {
		if chats[i].ChatID == chatID {
			mine = &chats[i]
		}
	}
	if mine == nil {
		t.Fatalf("ListPendingChats missing chat %s", chatID)
	}

	// RecoverPendingOnBoot clears the pending row — no user notice is pushed.
	mgr.RecoverPendingOnBoot(ctx)

	after, _ := st.ListPendingChats(ctx)
	for _, c := range after {
		if c.ChatID == chatID {
			t.Fatalf("pending row for %s should be cleared on boot", chatID)
		}
	}
}
