package session

import (
	"context"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/session/store"
)

// resumeStore overrides GetSessionMode + SessionByID for the resume test (other
// store.Store methods are nil — unused on this path).
type resumeStore struct {
	store.Store
	mode   contract.SessionMode
	key    string
	active string // CurrentSession returns this as the scope's active sid ("" = none → F88 does not drop)
}

func (s resumeStore) GetSessionMode(context.Context, string) (contract.SessionMode, error) {
	return s.mode, nil
}
func (s resumeStore) SessionByID(_ context.Context, id string) (store.SessionInfo, error) {
	return store.SessionInfo{ID: id, Key: s.key}, nil
}
func (s resumeStore) CurrentSession(context.Context, string) (string, bool, error) {
	return s.active, s.active != "", nil
}

// emitSrc is a fake internal source that captures the emitted event. busy (if set)
// returns ErrBridgeBusy that many times before succeeding (exercises the resume retry).
type emitSrc struct {
	arbSrc
	got  *contract.MessageEvent
	busy *int
}

func (e emitSrc) Emit(ev contract.MessageEvent) error {
	if e.busy != nil && *e.busy > 0 {
		*e.busy--
		return contract.ErrBridgeBusy
	}
	*e.got = ev
	return nil
}

// TestResumeSession: an AUTO session is woken (emit to its scope with the note); a
// non-auto session and an empty note emit nothing.
func TestResumeSession(t *testing.T) {
	var got contract.MessageEvent
	srcByName := func(string) contract.Source { return emitSrc{got: &got} }

	// auto mode → emits to the session's scope with the note.
	m := NewManager(resumeStore{mode: contract.SessionModeAuto, key: "discord:group:1"}, stubTurnHandler{}, srcByName, Config{})
	m.ResumeSession(context.Background(), "s1", "继续")
	if got.ChatID != "discord:group:1" || got.Text != "继续" {
		t.Fatalf("auto resume emit = %+v, want {discord:group:1, 继续}", got)
	}

	// non-auto mode → no emit (left for the user's next message).
	got = contract.MessageEvent{}
	mn := NewManager(resumeStore{mode: contract.SessionModeNone, key: "telegram:dm:1"}, stubTurnHandler{}, srcByName, Config{})
	mn.ResumeSession(context.Background(), "s2", "继续")
	if got.ChatID != "" {
		t.Fatalf("non-auto session must not auto-resume, got %+v", got)
	}

	// empty note → no emit.
	got = contract.MessageEvent{}
	m.ResumeSession(context.Background(), "s1", "")
	if got.ChatID != "" {
		t.Fatalf("empty note must not emit, got %+v", got)
	}

	// F88: a SUPERSEDED session (the scope's active is a DIFFERENT sid — a force_new task
	// opened during the escalation window) must NOT resume — waking the scope would land
	// the note in the wrong (new) session.
	got = contract.MessageEvent{}
	ms := NewManager(resumeStore{mode: contract.SessionModeAuto, key: "discord:group:7", active: "s_new"}, stubTurnHandler{}, srcByName, Config{})
	ms.ResumeSession(context.Background(), "s_old", "继续")
	if got.ChatID != "" {
		t.Fatalf("superseded session must not resume, got %+v", got)
	}

	// a momentarily-busy bridge → retries until it clears (an auto worker has no human
	// to re-trigger it, so a dropped resume would wedge it).
	got = contract.MessageEvent{}
	busy := 2
	mb := NewManager(resumeStore{mode: contract.SessionModeAuto, key: "discord:group:9"}, stubTurnHandler{},
		func(string) contract.Source { return emitSrc{got: &got, busy: &busy} }, Config{})
	mb.ResumeSession(context.Background(), "s9", "继续")
	if got.ChatID != "discord:group:9" {
		t.Fatalf("busy bridge should retry until it emits, got %+v", got)
	}
}
