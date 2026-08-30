package email

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
)

// jsonFiles lists the persisted batch files (excludes .tmp) in dir.
func jsonFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	return out
}

func testCoalesceSource() *Source {
	return &Source{
		cfg:    EmailConfig{SourceID: "email-test", Address: "bob@example.com"},
		sendCh: make(chan sendReq, 32),
	}
}

func recvSend(t *testing.T, s *Source, timeout time.Duration) (sendReq, bool) {
	t.Helper()
	select {
	case r := <-s.sendCh:
		return r, true
	case <-time.After(timeout):
		return sendReq{}, false
	}
}

// Two turn-replies in one session, force-flushed at maxSeg, arrive as ONE email
// carrying both segments.
func TestCoalesce_ForceFlushAtMax(t *testing.T) {
	s := testCoalesceSource()
	c := newBatchCoalescer(s, t.TempDir(), 10*time.Second, 2) // long window; force at 2
	tgt := contract.Target{Source: s.Name(), ChatID: "u@x.com"}

	c.beginTurn("sid1", "scope1", tgt)
	_ = c.addSegment("sid1", "scope1", tgt, "AAA")
	if _, ok := recvSend(t, s, 80*time.Millisecond); ok {
		t.Fatal("flushed after one segment — should wait for window / max")
	}
	c.beginTurn("sid1", "scope1", tgt)
	_ = c.addSegment("sid1", "scope1", tgt, "BBB")

	r, ok := recvSend(t, s, time.Second)
	if !ok {
		t.Fatal("expected one combined email at maxSeg")
	}
	body := string(r.body)
	if !strings.Contains(body, "AAA") || !strings.Contains(body, "BBB") {
		t.Fatalf("combined email missing a segment:\n%s", body)
	}
	if _, ok := recvSend(t, s, 80*time.Millisecond); ok {
		t.Fatal("two segments must produce exactly ONE email")
	}
}

// The flow's reply-routing anchor hook (coalesceSink.OnAnchor → setRecord) must
// fire at the deferred send with the combined mail's minted Message-ID, so a user
// reply threads back to the originating session instead of forking a new one
// (L-EMAIL-B1). Without this, coalescing accounts silently split every thread.
func TestCoalesce_RecordsAnchorAtDeferredSend(t *testing.T) {
	s := testCoalesceSource()
	c := newBatchCoalescer(s, t.TempDir(), 10*time.Second, 1) // force-flush at one segment
	tgt := contract.Target{Source: s.Name(), ChatID: "u@x.com"}

	got := make(chan string, 1)
	// Mirror the flow: NewSink(beginTurn) then OnAnchor before the turn runs.
	c.beginTurn("sidR", "scopeR", tgt)
	(&coalesceSink{c: c, sid: "sidR", scope: "scopeR", target: tgt}).OnAnchor(func(id string) { got <- id })

	_ = c.addSegment("sidR", "scopeR", tgt, "answer") // segCount==maxSeg → flush → sendBatch

	r, ok := recvSend(t, s, time.Second)
	if !ok {
		t.Fatal("expected the coalesced email to send")
	}
	select {
	case id := <-got:
		if id != r.messageID || id == "" {
			t.Fatalf("anchor recorded msgID %q, want the sent %q", id, r.messageID)
		}
	case <-time.After(time.Second):
		t.Fatal("anchor hook never fired — coalesced reply is not reply-routable")
	}
}

// A turn that ends SILENTLY (a /stop cancel or stay_silent → the turn core's
// releaseSink path Finishes with EMPTY content) must still balance the per-turn
// `active` counter, so a reply buffered by a PRIOR turn flushes instead of wedging
// undelivered (B11). Without the empty Finish, active stays >0 after the new turn's
// beginTurn stopped the window timer, and the held reply never sends.
func TestCoalesce_SilentEmptyFinishReleasesBufferedReply(t *testing.T) {
	s := testCoalesceSource()
	c := newBatchCoalescer(s, t.TempDir(), 100*time.Millisecond, 5) // window flush; high max (no force at 1)
	tgt := contract.Target{Source: s.Name(), ChatID: "u@x.com"}

	// Turn 1 produces a real reply → buffered + armed (active back to 0).
	c.beginTurn("sidS", "scopeS", tgt)
	_ = c.addSegment("sidS", "scopeS", tgt, "buffered reply")
	// Turn 2 begins (active++, window timer STOPPED) then ends SILENTLY: the turn
	// core's releaseSink → Finish("") → addSegment("") must decrement active back to 0
	// and re-arm, releasing turn 1's held reply.
	c.beginTurn("sidS", "scopeS", tgt)
	if st := c.bySid["sidS"]; st == nil || st.active != 1 {
		t.Fatalf("after a second beginTurn, active must be 1, got %+v", st)
	}
	_ = c.addSegment("sidS", "scopeS", tgt, "") // the empty release

	r, ok := recvSend(t, s, 2*time.Second)
	if !ok {
		t.Fatal("a silent (empty Finish) turn must release the counter so the buffered reply flushes — got nothing (wedged)")
	}
	if !strings.Contains(string(r.body), "buffered reply") {
		t.Fatalf("flushed body missing the buffered reply:\n%s", r.body)
	}
}

// One reply flushes by itself after the quiet window.
func TestCoalesce_WindowFlush(t *testing.T) {
	s := testCoalesceSource()
	c := newBatchCoalescer(s, t.TempDir(), 40*time.Millisecond, 0)
	tgt := contract.Target{Source: s.Name(), ChatID: "u@x.com"}

	c.beginTurn("sidW", "scopeW", tgt)
	_ = c.addSegment("sidW", "scopeW", tgt, "hello")

	r, ok := recvSend(t, s, time.Second)
	if !ok {
		t.Fatal("expected flush after the quiet window")
	}
	if !strings.Contains(string(r.body), "hello") {
		t.Fatalf("body missing segment:\n%s", r.body)
	}
}

// A new sid in the SAME scope (/new) force-flushes the prior session's buffer
// instead of merging across the session boundary.
func TestCoalesce_NewSessionFlushesPrior(t *testing.T) {
	s := testCoalesceSource()
	c := newBatchCoalescer(s, t.TempDir(), 10*time.Second, 0)
	tgt := contract.Target{Source: s.Name(), ChatID: "u@x.com"}

	c.beginTurn("sidA", "scopeS", tgt)
	_ = c.addSegment("sidA", "scopeS", tgt, "first")
	c.beginTurn("sidB", "scopeS", tgt) // /new → flush sidA

	r, ok := recvSend(t, s, time.Second)
	if !ok {
		t.Fatal("expected prior session flushed when a new sid opens in the same scope")
	}
	if !strings.Contains(string(r.body), "first") {
		t.Fatalf("flushed wrong buffer:\n%s", r.body)
	}
}

// A persisted buffer survives a restart: a fresh coalescer over the same dir
// rehydrates the held reply and flushes it after the window.
func TestCoalesce_Rehydrate(t *testing.T) {
	dir := t.TempDir()
	s := testCoalesceSource()
	c := newBatchCoalescer(s, dir, 10*time.Second, 0)
	tgt := contract.Target{Source: s.Name(), ChatID: "u@x.com"}
	c.beginTurn("sidR", "scopeR", tgt)
	_ = c.addSegment("sidR", "scopeR", tgt, "persisted") // writes the file

	// Fresh source + coalescer over the same dir = a restart.
	s2 := testCoalesceSource()
	c2 := newBatchCoalescer(s2, dir, 40*time.Millisecond, 0)
	c2.rehydrate(context.Background())

	c2.mu.Lock()
	st := c2.bySid["sidR"]
	c2.mu.Unlock()
	if st == nil || !strings.Contains(st.combined, "persisted") {
		t.Fatal("rehydrate lost the persisted buffer")
	}
	r, ok := recvSend(t, s2, time.Second)
	if !ok {
		t.Fatal("rehydrated buffer should flush after the window")
	}
	if !strings.Contains(string(r.body), "persisted") {
		t.Fatalf("rehydrated flush body wrong:\n%s", r.body)
	}
}

// The durability contract: a successful flush DELETES the persisted file (so
// rehydrate won't re-send a delivered reply); the buffer is persisted mid-batch.
func TestCoalesce_FlushDeletesFile(t *testing.T) {
	dir := t.TempDir()
	s := testCoalesceSource()
	c := newBatchCoalescer(s, dir, 10*time.Second, 2)
	tgt := contract.Target{Source: s.Name(), ChatID: "u@x.com"}

	c.beginTurn("sidF", "scopeF", tgt)
	_ = c.addSegment("sidF", "scopeF", tgt, "one")
	if f := jsonFiles(t, dir); len(f) != 1 {
		t.Fatalf("expected a persisted file mid-batch, got %v", f)
	}
	c.beginTurn("sidF", "scopeF", tgt)
	_ = c.addSegment("sidF", "scopeF", tgt, "two") // force-flush at maxSeg=2
	if _, ok := recvSend(t, s, time.Second); !ok {
		t.Fatal("expected a flushed email")
	}
	if f := jsonFiles(t, dir); len(f) != 0 {
		t.Fatalf("file must be deleted after a successful flush, got %v", f)
	}
}

// The other half of the contract: when the flush can't enqueue (send queue
// full), the file is KEPT so the next boot's rehydrate replays the reply.
func TestCoalesce_EnqueueFailKeepsFile(t *testing.T) {
	dir := t.TempDir()
	s := &Source{cfg: EmailConfig{SourceID: "email-test", Address: "bob@example.com"}, sendCh: make(chan sendReq, 1)}
	s.sendCh <- sendReq{} // pre-fill so the flush's enqueueSend hits queue-full
	c := newBatchCoalescer(s, dir, 10*time.Second, 2)
	tgt := contract.Target{Source: s.Name(), ChatID: "u@x.com"}

	c.beginTurn("sidK", "scopeK", tgt)
	_ = c.addSegment("sidK", "scopeK", tgt, "a")
	c.beginTurn("sidK", "scopeK", tgt)
	_ = c.addSegment("sidK", "scopeK", tgt, "b") // force-flush → enqueue fails
	if f := jsonFiles(t, dir); len(f) != 1 {
		t.Fatalf("file must be KEPT when the flush can't enqueue, got %v", f)
	}
}

// NewSink coalesces a turn sink but sends a non-turn sink (sid=="") immediately.
func TestNewSink_NonTurnBypassesCoalescing(t *testing.T) {
	s := testCoalesceSource()
	s.coalescer = newBatchCoalescer(s, t.TempDir(), time.Second, 0)
	tgt := contract.Target{Source: s.Name(), ChatID: "u@x.com"}

	notice := s.NewSink(context.Background(), tgt, "scope", "", contract.SinkPrefs{})
	if _, ok := notice.(*coalesceSink); ok {
		t.Fatal(`sid=="" (notice/slash) must NOT be coalesced`)
	}
	turn := s.NewSink(context.Background(), tgt, "scope", "sidX", contract.SinkPrefs{})
	if _, ok := turn.(*coalesceSink); !ok {
		t.Fatal("a turn sink must be coalesced when batching is on")
	}
}
