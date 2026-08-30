package bridge

import (
	"context"
	"testing"
	"time"

	"agentbob/contract"
)

// Emit pushes onto the buffer and Run drains it to the bus, stamping the
// internal source name (so the inbound flow routes it internally).
func TestEmitDrainsToOut(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan contract.MessageEvent, 1)
	go func() { _ = s.Run(ctx, out) }()

	if err := s.Emit(contract.MessageEvent{ChatID: "telegram:dm:1", Text: "hi"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	select {
	case ev := <-out:
		if ev.Source != contract.SourceNameInternal {
			t.Errorf("Source = %q, want %q", ev.Source, contract.SourceNameInternal)
		}
		if ev.ChatID != "telegram:dm:1" || ev.Text != "hi" {
			t.Errorf("event mangled: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event did not reach out")
	}
}

// F86/F91②: the return leg stamps the WORKER's identity (D7) so the caller's history
// attributes the reply to the member inbox that produced it — concurrent workers' returns
// are not indistinguishable anonymous rows.
func TestReturnSinkStampsWorkerIdentity(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan contract.MessageEvent, 1)
	go func() { _ = s.Run(ctx, out) }()

	sink := s.NewSink(ctx, contract.Target{ChatID: "co:acme/mgr"}, "co:acme/alice", "sid1", contract.SinkPrefs{})
	if err := sink.Finish("done"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	select {
	case ev := <-out:
		if ev.ChatID != "co:acme/mgr" || ev.Text != "done" {
			t.Errorf("return event mangled: %+v", ev)
		}
		if ev.UserID != "inbox:co:acme/alice" || ev.UserName != "agora worker co:acme/alice" {
			t.Errorf("worker identity not stamped: id=%q name=%q", ev.UserID, ev.UserName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("return event did not reach out")
	}
}

// A full buffer is backpressure, not a deadlock: Emit returns ErrBridgeBusy.
func TestEmitBusyWhenFull(t *testing.T) {
	s := New() // Run not started → buffer fills to cap
	for i := 0; i < bufferCap; i++ {
		if err := s.Emit(contract.MessageEvent{ChatID: "x"}); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}
	if err := s.Emit(contract.MessageEvent{ChatID: "x"}); err != contract.ErrBridgeBusy {
		t.Errorf("Emit on full = %v, want ErrBridgeBusy", err)
	}
}

// The return sink emits ONE internal event to the caller scope (t.ChatID) on
// Finish, carrying the buffered content; trace deltas are dropped.
func TestReturnSinkEmitsToCaller(t *testing.T) {
	s := New()
	sink := s.NewSink(context.Background(), contract.Target{ChatID: "caller:scope"}, "", "", contract.DefaultSinkPrefs())
	sink.ContentDelta("hi ")
	sink.TraceDelta("progress noise") // must NOT appear in the emitted reply
	sink.ContentDelta("there")
	if err := sink.Finish("hi there"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	select {
	case ev := <-s.buf:
		if ev.ChatID != "caller:scope" {
			t.Errorf("ChatID = %q, want caller:scope", ev.ChatID)
		}
		if ev.Text != "hi there" {
			t.Errorf("Text = %q, want %q", ev.Text, "hi there")
		}
		if ev.Dispatch != nil {
			t.Errorf("Dispatch = %+v, want nil (inbound reply, not a dispatch)", ev.Dispatch)
		}
	default:
		t.Fatal("return sink did not emit an event")
	}
}

// An empty reply emits nothing — never wake the caller for no content.
func TestReturnSinkEmptyEmitsNothing(t *testing.T) {
	s := New()
	sink := s.NewSink(context.Background(), contract.Target{ChatID: "caller:scope"}, "", "", contract.DefaultSinkPrefs())
	if err := sink.Finish("   "); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	select {
	case ev := <-s.buf:
		t.Fatalf("empty reply must emit nothing, got %+v", ev)
	default:
	}
}

// A missing target scope is a wiring bug: drop loud, emit nothing, no error.
func TestReturnSinkNoTargetDrops(t *testing.T) {
	s := New()
	sink := s.NewSink(context.Background(), contract.Target{}, "", "", contract.DefaultSinkPrefs())
	if err := sink.Finish("reply"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	select {
	case ev := <-s.buf:
		t.Fatalf("no-target sink must emit nothing, got %+v", ev)
	default:
	}
}
