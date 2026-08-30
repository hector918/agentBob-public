package session_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/session"
)

// noopHandler is a TurnHandler that does nothing — used where a turn is never
// expected to matter (resolver tests).
type noopHandler struct{}

func (noopHandler) RunTurn(context.Context, contract.TurnWork) {}
func (noopHandler) Blocked(context.Context, string) bool       { return false }
func (noopHandler) NotePending(context.Context, string, contract.MessageEvent, contract.PendingDisposition) {
}
func (noopHandler) DumpPrompt(context.Context, contract.TurnWork) string { return "" }
func (noopHandler) FlowInfo(context.Context, contract.TurnWork) (string, contract.SessionMode, bool) {
	return "", "", false
}

// recordingHandler records RunTurn/NotePending calls and can gate RunTurn to
// simulate a long-running turn.
type recordingHandler struct {
	mu      sync.Mutex
	runs    []contract.TurnWork
	notes   []contract.PendingDisposition
	blocked bool
	started chan struct{} // signalled when a RunTurn begins (buffered)
	gate    chan struct{} // RunTurn blocks on receive until closed (nil = no gate)
}

func (h *recordingHandler) RunTurn(_ context.Context, work contract.TurnWork) {
	if h.started != nil {
		h.started <- struct{}{}
	}
	if h.gate != nil {
		<-h.gate
	}
	h.mu.Lock()
	h.runs = append(h.runs, work)
	h.mu.Unlock()
}
func (h *recordingHandler) Blocked(context.Context, string) bool { return h.blocked }
func (h *recordingHandler) NotePending(_ context.Context, _ string, _ contract.MessageEvent, d contract.PendingDisposition) {
	h.mu.Lock()
	h.notes = append(h.notes, d)
	h.mu.Unlock()
}
func (h *recordingHandler) DumpPrompt(context.Context, contract.TurnWork) string { return "" }
func (h *recordingHandler) FlowInfo(context.Context, contract.TurnWork) (string, contract.SessionMode, bool) {
	return "", "", false
}
func (h *recordingHandler) runCount() int  { h.mu.Lock(); defer h.mu.Unlock(); return len(h.runs) }
func (h *recordingHandler) noteCount() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.notes) }

// stubSrcByName is the source lookup the manager holds in tests — every name
// resolves to the dispatch stub.
func stubSrcByName(string) contract.Source { return srcStub{} }

// srcStub is a minimal contract.Source for dispatch tests.
type srcStub struct{}

func (srcStub) Name() string                                            { return "telegram" }
func (srcStub) Caps() contract.Caps                                     { return contract.Caps{} }
func (srcStub) HealthCheck(context.Context) error                       { return nil }
func (srcStub) Run(context.Context, chan<- contract.MessageEvent) error { return nil }
func (srcStub) NewSink(context.Context, contract.Target, string, string, contract.SinkPrefs) contract.Sink {
	return sinkStub{}
}
func (srcStub) SendButtons(context.Context, contract.Target, string, []contract.Button) (string, error) {
	return "", nil
}
func (srcStub) Send(context.Context, contract.Target, string) error             { return nil }
func (srcStub) SendFile(context.Context, contract.Target, string, string) error { return nil }

type sinkStub struct{}

func (sinkStub) ContentDelta(string)           {}
func (sinkStub) TraceDelta(string)             {}
func (sinkStub) Finish(string) error           { return nil }
func (sinkStub) SendFile(string, string) error { return nil }
func (sinkStub) LastSent() string              { return "" }

func dm(text string) contract.MessageEvent {
	return contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, ChatID: "dispatch-c", UserID: "u", Text: text}
}

func TestSubmit_PlainDispatch(t *testing.T) {
	_, st, done := liveManager(t) // liveManager gives us a live store
	defer done()
	h := &recordingHandler{}
	m := session.NewManager(st, h, stubSrcByName, session.Config{MaxBatch: 10})
	ev := dm("hello")
	ev.ChatID = "plain-" + uscope("")

	m.Submit(context.Background(), ev)
	m.Wait()
	if h.runCount() != 1 {
		t.Fatalf("runs = %d, want 1", h.runCount())
	}
	if h.runs[0].Events[0].Text != "hello" || h.runs[0].Sid == "" || h.runs[0].Scope == "" {
		t.Fatalf("turn work = %+v", h.runs[0])
	}
}

func TestSubmit_BusyQueuesThenDrains(t *testing.T) {
	_, st, done := liveManager(t)
	defer done()
	h := &recordingHandler{started: make(chan struct{}, 4), gate: make(chan struct{})}
	m := session.NewManager(st, h, stubSrcByName, session.Config{MaxBatch: 10})
	ctx := context.Background()
	chat := "busy-" + uscope("")
	ev1 := dm("first")
	ev1.ChatID = chat
	ev2 := dm("second")
	ev2.ChatID = chat

	m.Submit(ctx, ev1)
	<-h.started // turn 1 is running (busy set)

	// Second message for the same sid queues behind the busy turn.
	m.Submit(ctx, ev2)
	deadline := time.Now().Add(2 * time.Second)
	for h.noteCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("second message was never queued (NotePending)")
		}
		time.Sleep(2 * time.Millisecond)
	}

	close(h.gate) // release turn 1 → drain spawns the follow-up turn
	<-h.started   // follow-up turn running
	m.Wait()

	if h.runCount() != 2 {
		t.Fatalf("runs = %d, want 2 (initial + drained follow-up)", h.runCount())
	}
	if h.notes[0] != contract.PendingQueued {
		t.Fatalf("note = %v, want PendingQueued", h.notes[0])
	}
	// follow-up batch carries the queued second message.
	if got := h.runs[1].Events[0].Text; got != "second" {
		t.Fatalf("drained turn text = %q, want second", got)
	}
}

// TestSubmit_ServedDrainsPendingWAL pins Layer 1 (a): once RunTurn returns (the
// batch got a reply) its ingress-WAL rows are gone, so a restart would not
// over-notify this already-answered chat.
func TestSubmit_ServedDrainsPendingWAL(t *testing.T) {
	_, st, done := liveManager(t)
	defer done()
	h := &recordingHandler{} // no gate → RunTurn returns at once = served
	m := session.NewManager(st, h, stubSrcByName, session.Config{MaxBatch: 10})
	ctx := context.Background()
	ev := dm("hello")
	ev.ChatID = "served-" + uscope("")
	ev.MessageID = "m-served-1"

	m.Submit(ctx, ev)
	m.Wait()
	if h.runCount() != 1 {
		t.Fatalf("runs = %d, want 1", h.runCount())
	}
	chats, err := st.ListPendingChats(ctx)
	if err != nil {
		t.Fatalf("ListPendingChats: %v", err)
	}
	for _, c := range chats {
		if c.ChatID == ev.ChatID {
			t.Fatalf("served message still has a pending WAL row: %+v", c)
		}
	}
}

// TestSubmit_BlockedWritesNoPendingWAL pins the narrowed recovery: a blocked session
// is held DELIBERATELY (a verification gate), not interrupted in-flight work, so it
// writes NO ingress-WAL row — restart re-send-prompts only the message a turn was
// actively running. (Was TestSubmit_UnservedKeepsPendingWAL, which pinned the old
// notify-everything-unserved behavior.)
func TestSubmit_BlockedWritesNoPendingWAL(t *testing.T) {
	_, st, done := liveManager(t)
	defer done()
	h := &recordingHandler{blocked: true}
	m := session.NewManager(st, h, stubSrcByName, session.Config{MaxBatch: 10})
	ctx := context.Background()
	ev := dm("hi")
	ev.ChatID = "blocked-nowal-" + uscope("")
	ev.MessageID = "m-blocked-nowal-1"

	m.Submit(ctx, ev)
	m.Wait()
	chats, err := st.ListPendingChats(ctx)
	if err != nil {
		t.Fatalf("ListPendingChats: %v", err)
	}
	for _, c := range chats {
		if c.ChatID == ev.ChatID {
			t.Fatal("blocked message wrote a pending WAL row — it would over-notify 'please re-send' on restart; only in-flight messages should")
		}
	}
}

func TestSubmit_BlockedQueuesNoTurn(t *testing.T) {
	_, st, done := liveManager(t)
	defer done()
	h := &recordingHandler{blocked: true}
	m := session.NewManager(st, h, stubSrcByName, session.Config{MaxBatch: 10})
	ev := dm("hi")
	ev.ChatID = "blocked-" + uscope("")

	m.Submit(context.Background(), ev)
	m.Wait()
	if h.runCount() != 0 {
		t.Fatalf("blocked session ran a turn (%d), want 0", h.runCount())
	}
	if h.noteCount() != 1 || h.notes[0] != contract.PendingBlocked {
		t.Fatalf("notes = %v, want [PendingBlocked]", h.notes)
	}
}

// TestSubmit_InternalReturnBypassesBatchCap (④b-3-2): an internal return event to
// a BUSY caller bypasses the MaxBatch cap — a real chat message over the cap is
// dropped, but the worker's return is queued (a system reply must not be lost).
func TestSubmit_InternalReturnBypassesBatchCap(t *testing.T) {
	_, st, done := liveManager(t)
	defer done()
	h := &recordingHandler{started: make(chan struct{}, 8), gate: make(chan struct{})}
	m := session.NewManager(st, h, stubSrcByName, session.Config{MaxBatch: 1})
	ctx := context.Background()
	chat := "bypass-" + uscope("")
	scope := "telegram:dm:" + chat // the dm scope ev1-3 resolve to (resolver: src:dm:chat)

	waitNotes := func(n int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for h.noteCount() < n {
			if time.Now().After(deadline) {
				t.Fatalf("expected >= %d notes, got %d", n, h.noteCount())
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	ev1 := dm("first")
	ev1.ChatID = chat
	m.Submit(ctx, ev1)
	<-h.started // turn 1 running (busy)

	ev2 := dm("queued")
	ev2.ChatID = chat
	m.Submit(ctx, ev2)
	waitNotes(1) // pending=1=MaxBatch → PendingQueued

	ev3 := dm("dropped")
	ev3.ChatID = chat
	m.Submit(ctx, ev3)
	waitNotes(2) // batch full + real → PendingDropped

	ev4 := contract.MessageEvent{Source: contract.SourceNameInternal, ChatID: scope, Text: "worker-return"}
	m.Submit(ctx, ev4)
	waitNotes(3) // internal → bypasses the cap → PendingQueued

	close(h.gate)
	<-h.started // a drain turn began
	m.Wait()

	if h.notes[1] != contract.PendingDropped {
		t.Fatalf("note[1] = %v, want PendingDropped (real msg over cap)", h.notes[1])
	}
	if h.notes[2] != contract.PendingQueued {
		t.Fatalf("note[2] = %v, want PendingQueued (internal bypass)", h.notes[2])
	}
	served := false
	for _, w := range h.runs {
		for _, e := range w.Events {
			if e.Text == "worker-return" {
				served = true
			}
		}
	}
	if !served {
		t.Fatal("internal return was not served — lost despite the cap bypass")
	}
}
