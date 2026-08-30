package streamsink

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agentbob/contract"
)

// secretWire's Send always fails with an error embedding a secret; RedactErr
// scrubs it. Used to pin that Finish redacts the content-flush error before
// returning it to the (cross-source) caller, which logs it raw.
type secretWire struct{ secret string }

func (secretWire) Caps() WireCaps { return WireCaps{} } // block
func (w secretWire) Send(context.Context, string, bool) (string, error) {
	return "", fmt.Errorf("POST /bot%s/sendMessage: dial tcp: timeout", w.secret)
}
func (secretWire) Edit(context.Context, string, string) error { return nil }
func (secretWire) WireLen(s string) int                       { return len([]rune(s)) }
func (secretWire) MaxChars() int                              { return 1 << 30 }
func (secretWire) RateLimited(error) (time.Duration, bool)    { return 0, false }
func (secretWire) BenignEdit(error) bool                      { return false }
func (secretWire) EditGone(error) bool                        { return false }
func (secretWire) Typing(context.Context)                     {}
func (w secretWire) RedactErr(err error) error {
	if err == nil || !strings.Contains(err.Error(), w.secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), w.secret, "<redacted>"))
}

// TestFinish_RedactsContentError pins that the error Finish hands back to its
// caller is scrubbed by the wire (the caller is cross-source and logs it raw).
// Without the RedactErr at the Finish boundary a token-bearing wire error would
// reach slog unredacted.
func TestFinish_RedactsContentError(t *testing.T) {
	w := secretWire{secret: "SECRET-TOKEN-123"}
	s := New(context.Background(), w, contract.DefaultSinkPrefs(), nil)
	s.ContentDelta("hi")
	err := s.Finish("hi") // Send fails every attempt → flush returns the error
	if err == nil {
		t.Fatal("expected a flush error from a permanently-failing Send")
	}
	if strings.Contains(err.Error(), w.secret) {
		t.Fatalf("Finish leaked the secret: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("Finish did not redact the error: %q", err.Error())
	}
}

// fakeWire is a recording Wire for white-box core tests. It counts in
// WireLen=runes so MaxChars is a rune budget.
type fakeWire struct {
	mu        sync.Mutex
	caps      WireCaps
	maxChars  int
	calls     []wireCall
	nextID    int
	failSend  int   // fail this many Sends with rl (then succeed)
	rl        error // the error RateLimited recognises
	failPlain int   // fail this many Sends with a NON-rate-limit error (then succeed)
	plainErr  error // the non-rate-limit error returned while failPlain > 0
	editErr   error // non-nil → every Edit fails with this error
	benign    error // the error BenignEdit recognises
	gone      error // the error EditGone recognises
}

type wireCall struct {
	op     string // "send" | "send-fail" | "edit"
	text   string
	msgID  string
	anchor bool
}

func (w *fakeWire) Caps() WireCaps { return w.caps }

func (w *fakeWire) Send(_ context.Context, text string, anchor bool) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failSend > 0 {
		w.failSend--
		w.calls = append(w.calls, wireCall{op: "send-fail", text: text, anchor: anchor})
		return "", w.rl
	}
	if w.failPlain > 0 {
		w.failPlain--
		w.calls = append(w.calls, wireCall{op: "send-fail", text: text, anchor: anchor})
		return "", w.plainErr
	}
	w.nextID++
	id := fmt.Sprintf("m%d", w.nextID)
	w.calls = append(w.calls, wireCall{op: "send", text: text, msgID: id, anchor: anchor})
	return id, nil
}

func (w *fakeWire) Edit(_ context.Context, msgID, text string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.editErr != nil {
		w.calls = append(w.calls, wireCall{op: "edit-fail", text: text, msgID: msgID})
		return w.editErr
	}
	w.calls = append(w.calls, wireCall{op: "edit", text: text, msgID: msgID})
	return nil
}

func (w *fakeWire) WireLen(s string) int { return len([]rune(s)) }
func (w *fakeWire) MaxChars() int {
	if w.maxChars > 0 {
		return w.maxChars
	}
	return 1 << 30
}
func (w *fakeWire) RateLimited(err error) (d time.Duration, ok bool) {
	if w.rl != nil && errors.Is(err, w.rl) {
		return time.Millisecond, true
	}
	return 0, false
}
func (w *fakeWire) BenignEdit(err error) bool { return w.benign != nil && errors.Is(err, w.benign) }
func (w *fakeWire) EditGone(err error) bool   { return w.gone != nil && errors.Is(err, w.gone) }
func (w *fakeWire) Typing(context.Context)    {}
func (w *fakeWire) RedactErr(err error) error { return err }

func (w *fakeWire) snapshot() []wireCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]wireCall(nil), w.calls...)
}

func sends(calls []wireCall) []string {
	var out []string
	for _, c := range calls {
		if c.op == "send" {
			out = append(out, c.text)
		}
	}
	return out
}

// TestBlock_ContentSentOnceAtFinish: a block channel buffers ContentDelta and
// sends the full reply exactly once at Finish.
func TestBlock_ContentSentOnceAtFinish(t *testing.T) {
	w := &fakeWire{caps: WireCaps{}} // all-false = block, no trace, no typing
	s := New(context.Background(), w, contract.DefaultSinkPrefs(), nil)
	s.ContentDelta("hel")
	s.ContentDelta("lo")
	if err := s.Finish("hello"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := sends(w.snapshot()); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("block send = %v, want one [hello]", got)
	}
}

// TestBlock_TraceDroppedWhenNoTraceRender: TraceRender=false drops TraceDelta
// even with prefs.Trace on — no trace message is ever sent.
func TestBlock_TraceDroppedWhenNoTraceRender(t *testing.T) {
	w := &fakeWire{caps: WireCaps{TraceRender: false}}
	s := New(context.Background(), w, contract.SinkPrefs{Trace: true}, nil)
	s.TraceDelta("doing a thing")
	s.ContentDelta("answer")
	if err := s.Finish("answer"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got := sends(w.snapshot())
	if len(got) != 1 || got[0] != "answer" {
		t.Fatalf("sends = %v, want only [answer] (trace dropped)", got)
	}
}

// TestBlock_TraceBeforeContent: a trace-capable block channel sends trace as a
// separate message BEFORE content (platforms order by send time).
func TestBlock_TraceBeforeContent(t *testing.T) {
	w := &fakeWire{caps: WireCaps{TraceRender: true}}
	s := New(context.Background(), w, contract.SinkPrefs{Trace: true}, nil)
	s.TraceDelta("TRACE")
	if err := s.Finish("CONTENT"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got := sends(w.snapshot())
	if len(got) != 2 || got[0] != "TRACE" || got[1] != "CONTENT" {
		t.Fatalf("sends = %v, want [TRACE CONTENT]", got)
	}
}

// TestTraceLinesUseHardBreaks pins the trace channel's line separator on a
// channel that declares the GFM hard break (WireCaps.LineBreak — telegram,
// feishu, discord).
//
// The trailing two spaces LOOK like whitespace litter and are exactly the kind
// of thing a later cleanup deletes on sight. They are load-bearing: a
// markdown-rendering channel reads a bare "\n" as a SOFT break and collapses
// every annotation into one running paragraph (which is what Telegram's rich
// messages did until). Delete them and the trace goes back to being
// one unreadable line.
func TestTraceLinesUseHardBreaks(t *testing.T) {
	w := &fakeWire{caps: WireCaps{TraceRender: true, LineBreak: "  \n"}}
	s := New(context.Background(), w, contract.SinkPrefs{Trace: true}, nil)
	s.TraceDelta("🧭 normal · s_abc")
	s.TraceDelta("🔧 web_search({\"query\": \"x\"})")
	// An annotation carrying an embedded body (rubric verdicts / advisor reviews
	// append "\n" + reason): its INTERNAL newlines are line boundaries too.
	s.TraceDelta("🧾 复核 ✓ 通过：\n理由一\n理由二")
	if err := s.Finish("CONTENT"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got := sends(w.snapshot())
	if len(got) != 2 {
		t.Fatalf("sends = %v, want [trace, content]", got)
	}
	want := "🧭 normal · s_abc  \n🔧 web_search({\"query\": \"x\"})  \n🧾 复核 ✓ 通过：  \n理由一  \n理由二"
	if got[0] != want {
		t.Errorf("trace message =\n%q\nwant\n%q", got[0], want)
	}
	// Every line boundary must be a hard break — a bare "\n" anywhere means some
	// path re-joined lines the soft way. The last line is exempt: it ends the
	// message, and flushChan's TrimSpace takes the outer edges off so nothing
	// dangles.
	lines := strings.Split(got[0], "\n")
	for _, line := range lines[:len(lines)-1] {
		if !strings.HasSuffix(line, "  ") {
			t.Errorf("trace line %q lacks the two-space hard break", line)
		}
	}
	if strings.HasSuffix(got[0], " ") || strings.HasSuffix(got[0], "\n") {
		t.Errorf("trace message has a trailing break: %q", got[0])
	}
	// Content is untouched: its newlines are the model's markdown, not ours.
	if got[1] != "CONTENT" {
		t.Errorf("content = %q, want CONTENT (never hard-wrapped)", got[1])
	}
}

// TestTraceHardBreaksSurviveSplit: a trace run that overflows MaxChars splits
// into a chain, and each chunk must KEEP its internal hard breaks while losing
// the dangling one at the cut.
//
// The split path trims " \t\r\n" off a chunk's tail (flushChanLockedDepth), which
// is what removes the trailing break — but it must not eat the ones between
// lines, or every chunk after the first collapses back into one paragraph on a
// markdown channel. Splitting is where trace formatting is most likely to rot,
// since a long tool run is exactly the case that overflows.
func TestTraceHardBreaksSurviveSplit(t *testing.T) {
	// 16 runes: two 7-rune annotations ("AAAA" + break) fit, the third overflows.
	w := &fakeWire{caps: WireCaps{CanEdit: true, TraceRender: true, LineBreak: "  \n"}, maxChars: 16}
	s := New(context.Background(), w, contract.SinkPrefs{Stream: true, Trace: true}, nil)
	s.TraceDelta("AAAA")
	s.TraceDelta("BBBB")
	s.TraceDelta("CCCC")
	if err := s.flushChan(&s.trace); err != nil {
		t.Fatalf("trace flush: %v", err)
	}
	got := sends(w.snapshot())
	if len(got) != 2 {
		t.Fatalf("sends = %q, want 2 chunks", got)
	}
	if got[0] != "AAAA  \nBBBB" {
		t.Errorf("chunk 0 = %q, want %q — the internal hard break must survive the cut", got[0], "AAAA  \nBBBB")
	}
	if got[1] != "CCCC" {
		t.Errorf("chunk 1 = %q, want %q", got[1], "CCCC")
	}
	for i, chunk := range got {
		if strings.HasSuffix(chunk, " ") || strings.HasSuffix(chunk, "\n") {
			t.Errorf("chunk %d = %q ends with a dangling break", i, chunk)
		}
		if w.WireLen(chunk) > 16 {
			t.Errorf("chunk %d = %q is %d runes, over the 16 cap", i, chunk, w.WireLen(chunk))
		}
	}
}

// TestBlock_SplitOverCap: content over MaxChars is split into a chain of
// messages, each within the rune budget.
func TestBlock_SplitOverCap(t *testing.T) {
	w := &fakeWire{caps: WireCaps{}, maxChars: 5}
	s := New(context.Background(), w, contract.DefaultSinkPrefs(), nil)
	if err := s.Finish("0123456789"); err != nil { // 10 runes, cap 5
		t.Fatalf("Finish: %v", err)
	}
	got := sends(w.snapshot())
	if len(got) != 2 || got[0] != "01234" || got[1] != "56789" {
		t.Fatalf("split sends = %v, want [01234 56789]", got)
	}
}

// TestCall_RetriesTransientNonRateLimit: a non-rate-limit send failure (network
// blip / brief 5xx) is retried within the maxSendAttempts budget and succeeds —
// the content still lands. Exercises the shared retry that benefits every block
// source (telegram/wechat/feishu).
func TestCall_RetriesTransientNonRateLimit(t *testing.T) {
	w := &fakeWire{caps: WireCaps{}, plainErr: errors.New("connection reset"), failPlain: maxSendAttempts - 1}
	s := New(context.Background(), w, contract.DefaultSinkPrefs(), nil)
	if err := s.Finish("hello"); err != nil {
		t.Fatalf("Finish despite transient failures: %v", err)
	}
	if got := sends(w.snapshot()); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("retried sends = %v, want [hello]", got)
	}
}

// TestCall_PermanentNonRateLimitFailsFast: a non-rate-limit error that never
// clears burns the attempt budget and is returned (no classification — it just
// fails after a couple tries).
func TestCall_PermanentNonRateLimitFailsFast(t *testing.T) {
	w := &fakeWire{caps: WireCaps{}, plainErr: errors.New("hard reject"), failPlain: 1 << 20}
	s := New(context.Background(), w, contract.DefaultSinkPrefs(), nil)
	if err := s.Finish("nope"); err == nil {
		t.Fatal("Finish on permanent failure: want error, got nil")
	}
	var attempts int
	for _, c := range w.snapshot() {
		if c.op == "send-fail" {
			attempts++
		}
	}
	if attempts != maxSendAttempts {
		t.Fatalf("send attempts = %d, want %d", attempts, maxSendAttempts)
	}
}

// TestEditStream_SendThenEdit: in stream mode the first flush sends, a later
// flush edits the same message in place. Driven via flushChan directly so the
// test doesn't wait on the flush ticker.
func TestEditStream_SendThenEdit(t *testing.T) {
	w := &fakeWire{caps: WireCaps{CanEdit: true}}
	s := New(context.Background(), w, contract.SinkPrefs{Stream: true}, nil)
	s.ContentDelta("hello")
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("flush 1: %v", err)
	}
	s.ContentDelta(" world")
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("flush 2: %v", err)
	}
	if err := s.Finish("hello world"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	calls := w.snapshot()
	if len(calls) != 2 || calls[0].op != "send" || calls[0].text != "hello" ||
		calls[1].op != "edit" || calls[1].text != "hello world" || calls[1].msgID != calls[0].msgID {
		t.Fatalf("calls = %+v, want send(hello) then edit(same id, hello world)", calls)
	}
}

// TestEditStream_SplitFreezesEachChunk pins the freeze fix: when an
// edit-stream channel's FIRST flush already exceeds 2× the cap (no prior
// message), each over-cap chunk is sent as a NEW message — the second chunk
// must NOT edit the first chunk's message (which would clobber chunk 1). This
// is the path the old telegram code got wrong; the core resets c.sentID after
// each split send.
func TestEditStream_SplitFreezesEachChunk(t *testing.T) {
	w := &fakeWire{caps: WireCaps{CanEdit: true}, maxChars: 4}
	s := New(context.Background(), w, contract.SinkPrefs{Stream: true}, nil)
	s.ContentDelta("aaaabbbbcccc") // 12 runes, cap 4 → three 4-rune chunks
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("flush: %v", err)
	}
	calls := w.snapshot()
	// All three chunks must be SENDs (distinct new messages), zero edits.
	for i, c := range calls {
		if c.op != "send" {
			t.Fatalf("call %d = %q (text %q), want all sends — a split chunk edited a frozen message", i, c.op, c.text)
		}
	}
	got := sends(calls)
	want := []string{"aaaa", "bbbb", "cccc"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("split sends = %v, want %v (each chunk its own message)", got, want)
	}
	if calls[0].msgID == calls[1].msgID || calls[1].msgID == calls[2].msgID {
		t.Fatalf("split chunks reused a message id: %+v", calls)
	}
}

// TestEditStream_SplitThenFinish pins the Finish double-count fix: when an
// edit-stream channel splits mid-stream (freezing an earlier chunk and
// advancing msgStartOffset past it) and Finish is then called with the
// WHOLE canonical reply, the prefix already-sent bytes must NOT be
// re-appended. Before the fix, Finish wrote prefix+full and re-rendered
// the entire reply into the last window, duplicating the prefix and
// clobbering the second chunk.
func TestEditStream_SplitThenFinish(t *testing.T) {
	w := &fakeWire{caps: WireCaps{CanEdit: true}, maxChars: 4}
	s := New(context.Background(), w, contract.SinkPrefs{Stream: true}, nil)

	s.ContentDelta("aaaa") // fits cap exactly
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("flush 1: %v", err)
	}
	s.ContentDelta("bbbb") // buf now "aaaabbbb", over cap → freeze m1, m2="bbbb"
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("flush 2: %v", err)
	}
	if err := s.Finish("aaaabbbb"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	calls := w.snapshot()
	// Reconstruct each message's final visible text (last write wins per id).
	final := map[string]string{}
	var order []string
	for _, c := range calls {
		if _, seen := final[c.msgID]; !seen && c.msgID != "" {
			order = append(order, c.msgID)
		}
		if c.msgID != "" {
			final[c.msgID] = c.text
		}
	}
	if len(order) != 2 {
		t.Fatalf("got %d messages %v, want exactly 2 (no spurious extra)", len(order), calls)
	}
	if final[order[0]] != "aaaa" || final[order[1]] != "bbbb" {
		t.Fatalf("final messages = [%q %q], want [aaaa bbbb] (no duplication/clobber); calls=%+v",
			final[order[0]], final[order[1]], calls)
	}
}

// TestSplit_RuneWiderThanCap pins the no-progress guard: a cap smaller than a
// single rune's width must still make progress (emit the rune alone) rather
// than send "" forever until the depth cap drops the tail.
func TestSplit_RuneWiderThanCap(t *testing.T) {
	// A wire whose MaxChars()==0 forces wireCutoff→0 on any non-empty text —
	// the pathological "single rune wider than the cap" case.
	tiny := &tinyCapWire{}
	s := New(context.Background(), tiny, contract.DefaultSinkPrefs(), nil)
	if err := s.Finish("ab"); err != nil { // 2 runes, cap 0 → each rune alone
		t.Fatalf("Finish: %v", err)
	}
	got := tiny.sent
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("rune-wider-than-cap sends = %v, want [a b] (progress, no empty loop)", got)
	}
}

// tinyCapWire is a block wire with MaxChars()==0 — every non-empty text
// overflows and wireCutoff returns 0, exercising the splitAt==0 guard.
type tinyCapWire struct{ sent []string }

func (tinyCapWire) Caps() WireCaps { return WireCaps{} }
func (w *tinyCapWire) Send(_ context.Context, text string, _ bool) (string, error) {
	w.sent = append(w.sent, text)
	return "", nil
}
func (tinyCapWire) Edit(context.Context, string, string) error { return nil }
func (tinyCapWire) WireLen(s string) int                       { return len([]rune(s)) }
func (tinyCapWire) MaxChars() int                              { return 0 }
func (tinyCapWire) RateLimited(error) (time.Duration, bool)    { return 0, false }
func (tinyCapWire) BenignEdit(error) bool                      { return false }
func (tinyCapWire) EditGone(error) bool                        { return false }
func (tinyCapWire) Typing(context.Context)                     {}
func (tinyCapWire) RedactErr(err error) error                  { return err }

// TestEditStream_DegradeToBlock: a sustained throttle (every attempt fails with
// the rate-limit error) flips the sink to block — subsequent intermediate
// flushes are skipped; Finish still delivers.
func TestEditStream_DegradeToBlock(t *testing.T) {
	rl := errors.New("429 too many requests")
	// ONE 429 is enough: the source-level send gate already relayed it, so the
	// core treats a surfacing 429 as sustained — no re-dials, straight to degrade.
	w := &fakeWire{caps: WireCaps{CanEdit: true}, rl: rl, failSend: 1}
	s := New(context.Background(), w, contract.SinkPrefs{Stream: true}, nil)
	s.ContentDelta("a")
	// First flush: Send 429s → call returns the rl error without retrying → degrade.
	_ = s.flushChan(&s.content)
	// Intermediate flush after degrade must NOT send.
	s.ContentDelta("b")
	beforeFinish := len(sends(w.snapshot()))
	_ = s.flushChan(&s.content)
	if afterSkip := len(sends(w.snapshot())); afterSkip != beforeFinish {
		t.Fatalf("degraded intermediate flush sent %d new messages, want 0", afterSkip-beforeFinish)
	}
	// Finish delivers despite degrade (the throttle cleared; Send now succeeds).
	if err := s.Finish("ab"); err != nil {
		t.Fatalf("Finish after degrade: %v", err)
	}
	if got := sends(w.snapshot()); len(got) == 0 || got[len(got)-1] != "ab" {
		t.Fatalf("post-degrade Finish sends = %v, want last == ab", got)
	}
}

// TestGraceCtxDelivers: a cancelled turn ctx does not drop the reply — Finish
// derives a grace ctx and still sends.
func TestGraceCtxDelivers(t *testing.T) {
	w := &fakeWire{caps: WireCaps{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead before Finish
	s := New(ctx, w, contract.DefaultSinkPrefs(), nil)
	if err := s.Finish("salvage"); err != nil {
		t.Fatalf("Finish on dead ctx: %v", err)
	}
	if got := sends(w.snapshot()); len(got) != 1 || got[0] != "salvage" {
		t.Fatalf("grace-ctx sends = %v, want [salvage]", got)
	}
}

// TestOnAnchor_FiresPerNewSendNotOnEdit pins P1's send-time indexing hook after
// F135: OnAnchor fires for EVERY new message the sink sends — the first content
// send, the first trace send, AND each split-overflow chunk — but NEVER for an
// Edit of an existing message. So a reply to ANY delivered message (incl. a MIDDLE
// split chunk) enters the reply-routing index; without per-chunk anchoring a reply
// to a middle chunk clean-misses resolveBase and forks a context-less session.
func TestOnAnchor_FiresPerNewSendNotOnEdit(t *testing.T) {
	w := &fakeWire{caps: WireCaps{CanEdit: true, TraceRender: true}, maxChars: 4}
	s := New(context.Background(), w, contract.SinkPrefs{Stream: true, Trace: true}, nil)

	var mu sync.Mutex
	var anchors []string
	s.OnAnchor(func(id string) {
		mu.Lock()
		anchors = append(anchors, id)
		mu.Unlock()
	})

	// Trace channel: a long annotation over the cap → splits into a chain of sends.
	s.TraceDelta("doing a whole lot of things")
	if err := s.flushChan(&s.trace); err != nil {
		t.Fatalf("trace flush: %v", err)
	}
	// Content channel: first flush sends (new message)...
	s.ContentDelta("hi")
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("content flush 1: %v", err)
	}
	// ...second flush EDITS the same message (still fits cap 4) — no new anchor...
	s.ContentDelta("!")
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("content flush 2: %v", err)
	}
	// ...then an over-cap burst splits into a CHAIN of NEW sends — each one anchors.
	s.ContentDelta("aaaabbbbcccc")
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("content flush 3 (split): %v", err)
	}
	if err := s.Finish("hi!aaaabbbbcccc"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// The anchor stream must equal EXACTLY the ids the wire actually SENT — every
	// successful Send (one per new message, incl. every split chunk), and nothing
	// from an Edit. That set-equality IS the F135 invariant: every delivered message
	// is indexed exactly once.
	var wantSends []string
	for _, c := range w.snapshot() {
		if c.op == "send" && c.msgID != "" {
			wantSends = append(wantSends, c.msgID)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(anchors, ",") != strings.Join(wantSends, ",") {
		t.Fatalf("anchors = %v, want exactly the sent ids %v (one per new send, none from edits)", anchors, wantSends)
	}
	// Guard the guard: the scenario must actually produce a multi-message reply
	// (trace split + content + content split), or the equality above is vacuous.
	if len(wantSends) < 3 {
		t.Fatalf("expected a multi-message reply, got only %d sends: %v", len(wantSends), wantSends)
	}
}

// TestOnAnchor_NotFiredOnEmptyID: a block channel whose Send returns "" (no
// platform id, e.g. email/wechat) never anchors — there is nothing to index.
func TestOnAnchor_NotFiredOnEmptyID(t *testing.T) {
	w := &tinyCapWire{} // Send returns "" always; MaxChars 0 but "hi" splits to runes
	s := New(context.Background(), w, contract.DefaultSinkPrefs(), nil)
	fired := 0
	s.OnAnchor(func(string) { fired++ })
	if err := s.Finish("hi"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if fired != 0 {
		t.Fatalf("onAnchor fired %d times on empty-id sends, want 0", fired)
	}
}

// TestFinishIdempotent: a second Finish returns nil without re-sending.
func TestFinishIdempotent(t *testing.T) {
	w := &fakeWire{caps: WireCaps{}}
	s := New(context.Background(), w, contract.DefaultSinkPrefs(), nil)
	if err := s.Finish("once"); err != nil {
		t.Fatalf("first Finish: %v", err)
	}
	if err := s.Finish("twice"); err != nil {
		t.Fatalf("second Finish: %v", err)
	}
	if got := sends(w.snapshot()); len(got) != 1 || got[0] != "once" {
		t.Fatalf("idempotent sends = %v, want one [once]", got)
	}
}

// TestEditStream_EditGoneRecoversWithFreshSend pins the dead-anchor recovery:
// when the streamed message is deleted mid-turn (every Edit fails with an
// EditGone-classified error), the sink must NOT re-edit the dead id every tick
// and fail Finish — it drops the id and delivers the window as a brand-new
// Send. Also pins the retry-loop break: a deterministic edit-state error burns
// exactly one attempt, not the full maxSendAttempts budget.
func TestEditStream_EditGoneRecoversWithFreshSend(t *testing.T) {
	gone := errors.New("message to edit not found")
	w := &fakeWire{caps: WireCaps{CanEdit: true}, editErr: gone, gone: gone}
	s := New(context.Background(), w, contract.SinkPrefs{Stream: true}, nil)

	s.ContentDelta("hello")
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("flush 1: %v", err)
	}
	s.ContentDelta(" world") // user deleted the streamed message in between
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("flush 2 must recover from a gone edit target, got: %v", err)
	}
	if err := s.Finish("hello world"); err != nil {
		t.Fatalf("Finish after recovery: %v", err)
	}

	calls := w.snapshot()
	var editFails int
	for _, c := range calls {
		if c.op == "edit-fail" {
			editFails++
		}
	}
	if editFails != 1 {
		t.Fatalf("edit attempts on the dead id = %d, want exactly 1 (terminal, no retry burn)", editFails)
	}
	got := sends(calls)
	if len(got) != 2 || got[0] != "hello" || got[1] != "hello world" {
		t.Fatalf("sends = %v, want [hello, hello world] (fresh message replaces the dead one)", got)
	}
	if id := s.LastSent(); id == "" {
		t.Fatal("LastSent is empty after recovery send — reply routing lost its anchor")
	}
}

// TestEditStream_BenignEditIsSuccess pins the "message is not modified"
// semantics: a benign no-op edit means the content is ALREADY on screen, so the
// flush (and Finish) must report success — propagating it used to roll back
// lastSent and re-fire the same no-op edit on every tick, then fail Finish on a
// reply the user in fact received.
func TestEditStream_BenignEditIsSuccess(t *testing.T) {
	benign := errors.New("message is not modified")
	w := &fakeWire{caps: WireCaps{CanEdit: true}, editErr: benign, benign: benign}
	s := New(context.Background(), w, contract.SinkPrefs{Stream: true}, nil)

	s.ContentDelta("ab")
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("flush 1: %v", err)
	}
	s.ContentDelta("c")
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("benign no-op edit must be success, got: %v", err)
	}
	if err := s.Finish("abc"); err != nil {
		t.Fatalf("Finish after a benign edit: %v", err)
	}
	calls := w.snapshot()
	var editFails int
	for _, c := range calls {
		if c.op == "edit-fail" {
			editFails++
		}
	}
	// One benign edit at flush 2; Finish's flush must dedup on lastSent (which
	// a rollback would have cleared) and fire no further edit.
	if editFails != 1 {
		t.Fatalf("edit attempts = %d, want exactly 1 (no per-tick re-edit, no retry burn)", editFails)
	}
	if got := sends(calls); len(got) != 1 || got[0] != "ab" {
		t.Fatalf("sends = %v, want just [ab] (no duplicate delivery)", got)
	}
}

// TestEditStream_OverflowSplitAtStreamedEndSkipsNoOpEdit pins the overflow
// dedup: when the split point lands exactly at the end of the already-streamed
// text (previous fits-flush ended at a newline right under the cap), the first
// chunk is ALREADY on screen — the sink must freeze the window and move on to
// the tail without any wire call, instead of re-editing the same text every
// tick ("message is not modified" wedge, losing everything after the split).
func TestEditStream_OverflowSplitAtStreamedEndSkipsNoOpEdit(t *testing.T) {
	w := &fakeWire{caps: WireCaps{CanEdit: true}, maxChars: 4}
	s := New(context.Background(), w, contract.SinkPrefs{Stream: true}, nil)

	s.ContentDelta("abc\n") // fits: renders as "abc" (trimmed), lastSent="abc"
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("flush 1: %v", err)
	}
	s.ContentDelta("defgh") // over cap; split lands on the newline → firstPart=="abc"==lastSent
	if err := s.flushChan(&s.content); err != nil {
		t.Fatalf("flush 2: %v", err)
	}
	calls := w.snapshot()
	for i, c := range calls {
		if c.op != "send" {
			t.Fatalf("call %d = %q (text %q), want sends only — the already-rendered chunk was re-edited", i, c.op, c.text)
		}
	}
	got := sends(calls)
	want := []string{"abc", "defg", "h"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("sends = %v, want %v (tail delivered past the frozen chunk)", got, want)
	}
}

// TestOverflow_AllWhitespaceChunkDropped pins the whitespace guard: an over-cap
// run of pure whitespace must be dropped (offset advanced) rather than sent
// verbatim — platforms reject whitespace-only messages (telegram 400 "message
// text is empty") and the window would wedge retrying it forever.
func TestOverflow_AllWhitespaceChunkDropped(t *testing.T) {
	w := &fakeWire{caps: WireCaps{}, maxChars: 4}
	s := New(context.Background(), w, contract.DefaultSinkPrefs(), nil)
	// 8 spaces (2× the cap of whitespace-only chunks) then real content over cap.
	if err := s.Finish("        hello world"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got := sends(w.snapshot())
	for i, txt := range got {
		if strings.TrimSpace(txt) == "" {
			t.Fatalf("send %d is whitespace-only (%q) — the guard sent instead of dropping", i, txt)
		}
	}
	want := []string{"hell", "o wo", "rld"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("sends = %v, want %v (whitespace run dropped, content intact)", got, want)
	}
}

// gateWire blocks its FIRST Send until release closes; that send and every
// retry of the same stale chunk (text prefixed "a") fail hard, so sendOrEdit's
// internal retry budget can't rescue it. Other texts succeed and record. It
// lets a test hold an overflow send in flight while a concurrent Finish
// rewrites the buffer.
type gateWire struct {
	entered chan struct{} // closed when the first Send is entered
	release chan struct{} // the first Send returns after this closes
	mu      sync.Mutex
	first   bool
	sent    []string
	nextID  int
}

func (w *gateWire) Caps() WireCaps { return WireCaps{CanEdit: true} }
func (w *gateWire) Send(_ context.Context, text string, _ bool) (string, error) {
	w.mu.Lock()
	isFirst := !w.first
	w.first = true
	w.mu.Unlock()
	if isFirst {
		close(w.entered)
		<-w.release
	}
	if strings.HasPrefix(text, "a") { // the stale chunk — fail all attempts
		return "", errors.New("hard send failure")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sent = append(w.sent, text)
	w.nextID++
	return fmt.Sprintf("m%d", w.nextID), nil
}
func (*gateWire) Edit(context.Context, string, string) error { return nil }
func (*gateWire) WireLen(s string) int                       { return len([]rune(s)) }
func (*gateWire) MaxChars() int                              { return 10 }
func (*gateWire) RateLimited(error) (time.Duration, bool)    { return 0, false }
func (*gateWire) BenignEdit(error) bool                      { return false }
func (*gateWire) EditGone(error) bool                        { return false }
func (*gateWire) Typing(context.Context)                     {}
func (*gateWire) RedactErr(err error) error                  { return err }

// TestFlush_RollbackSkippedAfterFinishRebuild pins the epoch guard in
// flushChanLockedDepth: a failed overflow send must NOT roll back its offset
// advance when a concurrent Finish rebuilt the buffer (divergence branch:
// offset=0, epoch++) while the send was in flight — the old-frame subtraction
// would drive the offset negative. The canonical `full` Finish installed
// covers the chunk's content instead.
func TestFlush_RollbackSkippedAfterFinishRebuild(t *testing.T) {
	w := &gateWire{entered: make(chan struct{}), release: make(chan struct{})}
	s := New(context.Background(), w, contract.SinkPrefs{Stream: true}, nil)
	s.ContentDelta(strings.Repeat("a", 25)) // > MaxChars(10) → overflow split path

	flushErr := make(chan error, 1)
	go func() { flushErr <- s.flushChan(&s.content) }()
	<-w.entered // overflow advanced msgStartOffset and is now blocked in Send

	finErr := make(chan error, 1)
	go func() { finErr <- s.Finish("ok") }() // "ok" ≠ streamed prefix → divergence rebuild
	// Finish rewrites the buffer under s.mu BEFORE blocking on flushMu; wait
	// for the epoch bump so the release below races nothing.
	for {
		s.mu.Lock()
		bumped := s.content.epoch > 0
		s.mu.Unlock()
		if bumped {
			break
		}
		time.Sleep(time.Millisecond)
	}

	close(w.release) // in-flight send now fails; rollback must be skipped
	if err := <-flushErr; err == nil {
		t.Fatal("overflow flush should surface the hard send failure")
	}
	if err := <-finErr; err != nil {
		t.Fatalf("Finish: %v", err)
	}
	s.mu.Lock()
	off := s.content.msgStartOffset
	s.mu.Unlock()
	if off != 0 {
		t.Fatalf("msgStartOffset = %d, want 0 (rollback skipped after rebuild)", off)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.sent) != 1 || w.sent[0] != "ok" {
		t.Fatalf("delivered = %v, want exactly [ok] (canonical full, no stale re-render)", w.sent)
	}
}

// TestTracePlainChannelKeepsBareNewline is the other half of the line-break
// contract: the hard break belongs to a MARKDOWN channel, not to trace. A
// channel that declares no WireCaps.LineBreak is plain text — a bare "\n" is
// already a line boundary there, and two trailing spaces would be litter bob
// invented for a rendering quirk of a different platform.
func TestTracePlainChannelKeepsBareNewline(t *testing.T) {
	w := &fakeWire{caps: WireCaps{TraceRender: true}} // declares nothing → "\n"
	s := New(context.Background(), w, contract.SinkPrefs{Trace: true}, nil)
	s.TraceDelta("🧭 normal · s_abc")
	s.TraceDelta("✓ web_search")
	if err := s.Finish("CONTENT"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got := sends(w.snapshot())
	if len(got) != 2 {
		t.Fatalf("sends = %v, want [trace, content]", got)
	}
	if want := "🧭 normal · s_abc\n✓ web_search"; got[0] != want {
		t.Errorf("trace message = %q, want %q", got[0], want)
	}
}

// TestLineBreak_DefaultAndDeclared pins the accessor the slash dispatcher reads
// through contract.LineBreakSink.
func TestLineBreak_DefaultAndDeclared(t *testing.T) {
	plain := New(context.Background(), &fakeWire{caps: WireCaps{}}, contract.SinkPrefs{}, nil)
	if got := plain.LineBreak(); got != "\n" {
		t.Errorf("undeclared channel LineBreak = %q, want %q", got, "\n")
	}
	md := New(context.Background(), &fakeWire{caps: WireCaps{LineBreak: "  \n"}}, contract.SinkPrefs{}, nil)
	if got := md.LineBreak(); got != "  \n" {
		t.Errorf("declared channel LineBreak = %q, want %q", got, "  \n")
	}
	var _ contract.LineBreakSink = plain
}

// A channel with no separate picture primitive must still ACCEPT a picture — it
// just arrives as an attachment, which on those platforms is what a picture looked
// like anyway. The degradation lives in the sink because every streamsink sink
// satisfies contract.SinkPhotoSender, so a caller's type assertion cannot tell the
// difference and must not have to (contract.PhotoSender).
func TestSendPhotoFallsBackToTheAttachmentSend(t *testing.T) {
	w := &fakeWire{caps: WireCaps{}}
	var asFile, asPhoto []string
	s := New(context.Background(), w, contract.DefaultSinkPrefs(),
		func(p, _ string) error { asFile = append(asFile, p); return nil })

	if err := s.SendPhoto("/tmp/pic.png", ""); err != nil {
		t.Fatalf("SendPhoto without a photo deliverer: %v", err)
	}
	if len(asFile) != 1 || len(asPhoto) != 0 {
		t.Fatalf("file sends = %v, photo sends = %v — want the picture delivered as a file", asFile, asPhoto)
	}

	// With one wired, the picture takes the picture path and the attachment path is
	// left alone.
	s.WithPhotoDeliverer(func(p, _ string) error { asPhoto = append(asPhoto, p); return nil })
	if err := s.SendPhoto("/tmp/pic2.png", ""); err != nil {
		t.Fatalf("SendPhoto: %v", err)
	}
	if len(asPhoto) != 1 || len(asFile) != 1 {
		t.Errorf("file sends = %v, photo sends = %v — want the second one on the photo path only", asFile, asPhoto)
	}
	// And SendFile is untouched by any of it.
	if err := s.SendFile("/tmp/doc.pdf", ""); err != nil || len(asFile) != 2 {
		t.Errorf("SendFile diverted: err=%v file sends=%v", err, asFile)
	}
}

// A sink built with no file sender at all still refuses both, rather than
// pretending a picture went out.
func TestSendPhotoOnAChannelThatCannotDeliverFiles(t *testing.T) {
	s := New(context.Background(), &fakeWire{caps: WireCaps{}}, contract.DefaultSinkPrefs(), nil)
	if err := s.SendPhoto("/tmp/pic.png", ""); err == nil {
		t.Error("SendPhoto reported success on a channel with no delivery primitive")
	}
}
