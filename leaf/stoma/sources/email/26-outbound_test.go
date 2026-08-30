package email

import (
	"context"
	"os"
	"testing"
	"time"
)

// newTestSource builds a bare Source wired with an outbound log rooted at a
// temp dir — enough to exercise the outbound-log path without IMAP/SMTP.
func newTestSource(t *testing.T) *Source {
	t.Helper()
	dir := t.TempDir()
	s := &Source{sendCh: make(chan sendReq, sendQueueSize)}
	s.cfg = EmailConfig{SourceID: "test"}
	s.outbound = newOutboundLog(s, dir)
	return s
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// A fresh send persists a row, replay re-enqueues it with the SAME file id, and
// a delivered-marker delete drops the file (F24 round-trip).
func TestOutbound_RecordReplayDelete(t *testing.T) {
	s := newTestSource(t)
	req := sendReq{to: "alice@example.com", body: []byte("From: bob\r\n\r\nhi"), queuedAt: time.Now(), messageID: "m1@bob"}

	id, err := s.outbound.record(req)
	if err != nil || id == "" {
		t.Fatalf("record: id=%q err=%v", id, err)
	}
	if got := dirEntries(t, s.outbound.dir); len(got) != 1 {
		t.Fatalf("expected 1 persisted row, got %v", got)
	}

	// Replay must re-enqueue exactly the persisted message, tagged with its file id.
	s.outbound.replay(context.Background())
	select {
	case r := <-s.sendCh:
		if r.to != req.to || string(r.body) != string(req.body) {
			t.Fatalf("replayed req mismatch: %+v", r)
		}
		if r.outboundID != id {
			t.Fatalf("replayed req must carry the file id %q, got %q", id, r.outboundID)
		}
	default:
		t.Fatal("replay did not enqueue the persisted row")
	}

	// Delivered → delete drops the file.
	s.outboundDelete(id)
	if got := dirEntries(t, s.outbound.dir); len(got) != 0 {
		t.Fatalf("delivered row should be deleted, dir still has %v", got)
	}
}

// A row older than the TTL is pruned at replay, not resurrected.
func TestOutbound_StaleRowPruned(t *testing.T) {
	s := newTestSource(t)
	old := sendReq{to: "alice@example.com", body: []byte("stale"), queuedAt: time.Now().Add(-outboundTTL - time.Hour), messageID: "old@bob"}
	if _, err := s.outbound.record(old); err != nil {
		t.Fatalf("record: %v", err)
	}
	s.outbound.replay(context.Background())
	select {
	case r := <-s.sendCh:
		t.Fatalf("stale row must not replay, got %+v", r)
	default:
	}
	if got := dirEntries(t, s.outbound.dir); len(got) != 0 {
		t.Fatalf("stale row should be pruned, dir still has %v", got)
	}
}

// enqueueSend rejection semantics: a FRESH send whose queue is full deletes its
// just-written row (the caller is told it failed); a REPLAY keeps its row.
func TestOutbound_RejectDeletesFreshKeepsReplay(t *testing.T) {
	s := newTestSource(t)
	// Saturate the queue so the next enqueue takes the default (rejection) arm.
	for i := 0; i < cap(s.sendCh); i++ {
		s.sendCh <- sendReq{}
	}

	// Fresh send (no outboundID) → recorded then rejected → file removed.
	if err := s.enqueueSend(context.Background(), sendReq{to: "a@x", body: []byte("b"), queuedAt: time.Now(), messageID: "fresh@bob"}); err == nil {
		t.Fatal("expected queue-full rejection")
	}
	if got := dirEntries(t, s.outbound.dir); len(got) != 0 {
		t.Fatalf("rejected fresh send must leave no row, got %v", got)
	}

	// Replay (pre-set outboundID) → rejected → file KEPT for the next boot.
	replayReq := sendReq{to: "a@x", body: []byte("b"), queuedAt: time.Now(), messageID: "replay@bob"}
	id, err := s.outbound.record(replayReq)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	replayReq.outboundID = id
	if err := s.enqueueSend(context.Background(), replayReq); err == nil {
		t.Fatal("expected queue-full rejection for replay too")
	}
	if got := dirEntries(t, s.outbound.dir); len(got) != 1 {
		t.Fatalf("rejected replay must keep its row, got %v", got)
	}
}
