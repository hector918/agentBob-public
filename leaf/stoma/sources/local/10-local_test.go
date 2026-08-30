package local

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agentbob/contract"
)

func newSink(t *testing.T, prefs contract.SinkPrefs) *sink {
	t.Helper()
	s := New()
	k, ok := s.NewSink(context.Background(), contract.Target{}, "scope", "sid", prefs).(*sink)
	if !ok {
		t.Fatal("NewSink did not return *sink")
	}
	return k
}

// drain collects chunks until the channel closes, returning the concatenated
// text and whether a done chunk was seen.
func drain(k *sink) (string, bool) {
	var b []byte
	var sawDone bool
	for c := range k.stream {
		b = append(b, c.text...)
		if c.done {
			sawDone = true
		}
	}
	return string(b), sawDone
}

// TestSink_DrainMethod pins F33's helper: sink.drain() non-blockingly collects buffered
// content a discarded sink never rendered (an internal-woken reply), and returns "" once
// empty/closed — so discardSinks can print it instead of silently dropping it.
func TestSink_DrainMethod(t *testing.T) {
	k := &sink{stream: make(chan chunk, 8)}
	k.stream <- chunk{text: "backg"}
	k.stream <- chunk{text: "round reply"}
	if got := k.drain(); got != "background reply" {
		t.Fatalf("drain = %q, want the buffered content", got)
	}
	if got := k.drain(); got != "" {
		t.Fatalf("drain of an empty sink = %q, want \"\"", got)
	}
	// A closed stream with a leftover chunk still drains it, then stops.
	k2 := &sink{stream: make(chan chunk, 8)}
	k2.stream <- chunk{text: "final"}
	close(k2.stream)
	if got := k2.drain(); got != "final" {
		t.Fatalf("drain of a closed stream = %q, want \"final\"", got)
	}
}

func TestSink_StreamMode(t *testing.T) {
	k := newSink(t, contract.SinkPrefs{Stream: true, Trace: true})
	k.ContentDelta("a")
	k.ContentDelta("b")
	if err := k.Finish("ab"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, done := drain(k)
	// stream mode pushed "a","b" live; the done chunk carries no text (already emitted).
	if got != "ab" || !done {
		t.Fatalf("stream: got %q done=%v, want \"ab\"/true", got, done)
	}
}

func TestSink_BlockModeTrace(t *testing.T) {
	k := newSink(t, contract.SinkPrefs{Stream: false, Trace: true})
	k.ContentDelta("a")
	k.TraceDelta("[tool]")
	k.ContentDelta("b")
	if err := k.Finish("ab"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	// block + trace: the interleaved buffer (content + trace) is rendered. Each
	// trace annotation is newline-terminated so it sits on its own line instead of
	// concatenating with surrounding content/other annotations.
	got, done := drain(k)
	if got != "a[tool]\nb" || !done {
		t.Fatalf("block+trace: got %q done=%v, want \"a[tool]\\nb\"/true", got, done)
	}
}

func TestSink_BlockModeNoTrace(t *testing.T) {
	k := newSink(t, contract.SinkPrefs{Stream: false, Trace: false})
	k.ContentDelta("x")
	k.TraceDelta("[dropped]") // dropped at the sink boundary
	if err := k.Finish("FINAL"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	// block + trace-off: `full` is the canonical content; trace never buffered.
	got, done := drain(k)
	if got != "FINAL" || !done {
		t.Fatalf("block-no-trace: got %q done=%v, want \"FINAL\"/true", got, done)
	}
}

func TestSink_FinishIdempotent(t *testing.T) {
	k := newSink(t, contract.SinkPrefs{Stream: true, Trace: true})
	if err := k.Finish("a"); err != nil {
		t.Fatalf("first Finish: %v", err)
	}
	if err := k.Finish("a"); err != nil {
		t.Fatalf("second Finish must be a no-op nil, got %v", err)
	}
	// deltas after Finish are dropped, not a panic.
	k.ContentDelta("late")
}

// TestSink_ConcurrentDeltaFinish exercises the delta-sends-vs-Finish-closes race
// under -race: many deltas spamming while Finish closes must never panic.
func TestSink_ConcurrentDeltaFinish(t *testing.T) {
	k := newSink(t, contract.SinkPrefs{Stream: true, Trace: true})
	go func() {
		for c := range k.stream { // drain so the buffer doesn't pin senders
			_ = c
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k.ContentDelta("x")
		}()
	}
	k.Finish("done")
	wg.Wait()
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it printed. printReply writes straight to stdout, so the render tests need
// this to observe (and keep out of the test log) its output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	collected := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		collected <- string(b)
	}()
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-collected
}

func openSink(s *Source) *sink {
	return s.NewSink(context.Background(), contract.Target{}, "scope", "sid", contract.SinkPrefs{Stream: true}).(*sink)
}

// TestTakeSink_FIFO pins the pending-sink registry semantics: NewSink appends,
// takeSink pops oldest-first, discardSinks drops everything and reports the
// count (each dropped sink settles one owed reply in Run's bookkeeping).
func TestTakeSink_FIFO(t *testing.T) {
	s := New()
	k1, k2 := openSink(s), openSink(s)
	if got := s.takeSink(); got != k1 {
		t.Fatalf("takeSink #1 = %p, want k1 %p", got, k1)
	}
	if got := s.takeSink(); got != k2 {
		t.Fatalf("takeSink #2 = %p, want k2 %p", got, k2)
	}
	if got := s.takeSink(); got != nil {
		t.Fatalf("takeSink on empty registry = %p, want nil", got)
	}
	openSink(s)
	openSink(s)
	if n := s.discardSinks(); n != 2 {
		t.Fatalf("discardSinks = %d, want 2", n)
	}
	if got := s.takeSink(); got != nil {
		t.Fatalf("takeSink after discard = %p, want nil", got)
	}
}

// TestPrintReply_LateSinkCatchUp reproduces the stale-sink race (F32):
//
//	msg1 dispatched → its turn stalls (e.g. on the store) before NewSink →
//	printReply times out → user sends msg2 → the store recovers → turn1's
//	late sink lands AFTER msg2's dispatch, then turn2 (queued behind turn1)
//	opens its own sink.
//
// Before the fix printReply took turn1's late sink as msg2's answer and
// returned when it closed; turn2's real sink sat unread until the next
// dispatch discarded it — the reply "REPLY-TWO" never reached the terminal.
// After the fix the owed-reply catch-up loop renders BOTH sinks in turn
// order, so this test fails on the old code (REPLY-TWO absent) and passes on
// the new.
func TestPrintReply_LateSinkCatchUp(t *testing.T) {
	s := New()
	// Run's bookkeeping for the scenario: msg1 dispatched (owed=1), its
	// printReply timed out, msg2 dispatched (owed=2).
	s.owed = 2

	// Turn1's late sink lands only now — after msg2's dispatch.
	k1 := openSink(s)
	if err := k1.Finish("REPLY-ONE"); err != nil {
		t.Fatalf("Finish k1: %v", err)
	}
	// Turn2 runs after turn1 completes (turns on one session are serialised):
	// its sink registers a beat later, well inside staleSinkGrace.
	go func() {
		time.Sleep(50 * time.Millisecond)
		k2 := openSink(s)
		_ = k2.Finish("REPLY-TWO")
	}()

	out := captureStdout(t, func() {
		if !s.printReply(context.Background(), startSpinner()) {
			t.Error("printReply = false, want true")
		}
	})
	i1, i2 := strings.Index(out, "REPLY-ONE"), strings.Index(out, "REPLY-TWO")
	if i1 < 0 {
		t.Fatalf("late turn1 reply not rendered; output %q", out)
	}
	if i2 < 0 {
		t.Fatalf("turn2 reply lost (the F32 race); output %q", out)
	}
	if i2 < i1 {
		t.Fatalf("replies rendered out of turn order; output %q", out)
	}
	if s.owed != 0 {
		t.Fatalf("owed = %d after catch-up, want 0", s.owed)
	}
}

// TestPrintReply_NormalTurnNoGraceWait pins that the catch-up loop costs the
// ordinary dispatch→reply path nothing: with the debt settled by the one
// rendered sink, printReply returns without waiting out staleSinkGrace.
func TestPrintReply_NormalTurnNoGraceWait(t *testing.T) {
	s := New()
	s.owed = 1
	k := openSink(s)
	if err := k.Finish("HI"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	start := time.Now()
	out := captureStdout(t, func() {
		if !s.printReply(context.Background(), startSpinner()) {
			t.Error("printReply = false, want true")
		}
	})
	if elapsed := time.Since(start); elapsed >= staleSinkGrace {
		t.Fatalf("normal turn waited %v (>= staleSinkGrace %v)", elapsed, staleSinkGrace)
	}
	if !strings.Contains(out, "HI") {
		t.Fatalf("reply not rendered; output %q", out)
	}
	if s.owed != 0 {
		t.Fatalf("owed = %d, want 0", s.owed)
	}
}

// TestPrintReply_WritesOffNoReplyDebt pins the write-off: when older owed
// turns never open a sink (no-reply paths), the catch-up loop waits one grace
// window, zeroes the debt, and returns — it must not hang or go negative.
func TestPrintReply_WritesOffNoReplyDebt(t *testing.T) {
	s := New()
	s.owed = 2 // one real reply below + one no-reply turn
	k := openSink(s)
	if err := k.Finish("ONLY"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	out := captureStdout(t, func() {
		if !s.printReply(context.Background(), startSpinner()) {
			t.Error("printReply = false, want true")
		}
	})
	if !strings.Contains(out, "ONLY") {
		t.Fatalf("reply not rendered; output %q", out)
	}
	if s.owed != 0 {
		t.Fatalf("owed = %d after write-off, want 0", s.owed)
	}
}

func TestSource_Contract(t *testing.T) {
	s := New()
	if s.Name() != "local" {
		t.Fatalf("Name = %q", s.Name())
	}
	if !s.Caps().Trusted {
		t.Fatal("local must be Trusted (exempt from the central screen)")
	}
	if err := s.HealthCheck(context.Background()); err != nil {
		t.Fatalf("HealthCheck should be nil: %v", err)
	}
	// SendButtons is stubbed → loud error, not a silent drop.
	if _, err := s.SendButtons(context.Background(), contract.Target{}, "pick", []contract.Button{{Text: "ok"}}); err == nil {
		t.Fatal("stubbed SendButtons must return an error")
	}
}
