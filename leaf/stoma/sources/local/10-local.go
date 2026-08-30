// Package local is the local-terminal Source: a tiny REPL that feeds the source
// bus the same way an IM platform does, and prints the reply (streamed, when
// streaming is on).
//
// PORT NOTE (trunk-rebuild): this is "local-lite" — the streaming sink (the
// load-bearing concurrent part) is ported verbatim, but one piece waits on a
// not-yet-ported dependency:
//   - SendButtons is stubbed (the old huh TUI menu needs the callbacks
//     subsystem's CallbackResolver; New no longer takes one). It returns an
//     "unsupported" error until callbacks lands.
//
// IsAdmin is gone from contract.Source (admin authority moved to the gate); the
// terminal user's admin status rides on MessageEvent.IsAdmin=true, stamped here.
package local

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentbob/contract"
	"agentbob/i18n"
)

type chunk struct {
	text string
	done bool
}

// Source implements contract.Source over stdin/stdout.
//
// Each NewSink mints its own per-sink stream channel and appends itself to the
// Source's FIFO of pending sinks under sinkMu. printReply pops sinks in
// registration order and reads one sink's channel at a time — without per-sink
// channels, chunks from a concurrent async Send could interleave with a
// streamed reply. A FIFO (not a single slot) because sinks and their consumers
// are asynchronous: a turn stalled on the store can open its sink AFTER the
// user has already dispatched the next line, so two turns' sinks can be alive
// at once; a single slot would let the late one shadow (and lose) the real one.
//
// owed counts dispatched events whose reply has not yet been rendered (or
// written off). It pairs with the FIFO to keep late sinks and their turns
// matched up: see printReply's catch-up loop. Touched only by the Run
// goroutine — no lock.
//
// ChatID is per-process (PID-based by default, BOB_LOCAL_ID override) so
// multiple terminals sharing one BOB_HOME don't map to the same session scope.
type Source struct {
	chatID string

	sinkMu sync.Mutex
	sinks  []*sink // pending (registered, not yet consumed) sinks, oldest first

	owed int // Run-goroutine only; see struct doc

	// stdoutMu serialises concurrent out-of-band prints (Source.Send /
	// Source.SendFile) against EACH OTHER only. It does NOT cover printReply's
	// stream rendering or sink.SendFile, which write stdout unlocked — an
	// out-of-band print can still land mid-line in a streamed reply. That is a
	// cosmetic line-split in a single-user REPL, deliberately not worth a lock
	// on every streamed chunk.
	stdoutMu sync.Mutex
}

// New builds the local terminal source.
func New() *Source {
	id := os.Getenv("BOB_LOCAL_ID")
	if id == "" {
		// PID is stable for the process lifetime + unique across concurrent
		// processes on one host. Restart => new pid => new session (mirrors the
		// "new chat = new session" semantic). For stable keying across restarts
		// of one terminal, the operator sets BOB_LOCAL_ID.
		id = "default-" + strconv.Itoa(os.Getpid())
	}
	return &Source{chatID: id}
}

func (s *Source) Name() string { return "local" }

// HealthCheck is a no-op: the local source lives in the same process as bob, so
// "is the source reachable?" reduces to "is bob running?" — always true here.
func (s *Source) HealthCheck(context.Context) error { return nil }

// Caps: no ReplyTo (terminal has no reply-to primitive) and Trusted (the
// terminal user needs no sender screen).
func (s *Source) Caps() contract.Caps { return contract.Caps{Trusted: true} } // terminal/CLI user — not screened

// Send prints a one-off plain text message to stdout — used for out-of-band
// feedback. Takes stdoutMu so concurrent out-of-band prints don't interleave
// with each other (a streaming reply renders unlocked; see stdoutMu's doc).
func (s *Source) Send(_ context.Context, _ contract.Target, text string) error {
	s.stdoutMu.Lock()
	defer s.stdoutMu.Unlock()
	fmt.Println("\n" + text)
	return nil
}

// SendFile: for a terminal user the file is already on the local filesystem, so
// "delivery" is just telling them where it is.
func (s *Source) SendFile(_ context.Context, _ contract.Target, path, caption string) error {
	s.stdoutMu.Lock()
	defer s.stdoutMu.Unlock()
	line := "📎 file: " + path
	if caption != "" {
		line += " — " + caption
	}
	fmt.Println("\n" + line)
	return nil
}

// SendButtons is stubbed in local-lite: the TUI menu needs the callbacks
// subsystem (CallbackResolver), not yet ported. Returns an error so a caller
// that reaches it fails loudly rather than silently dropping an interaction.
func (s *Source) SendButtons(context.Context, contract.Target, string, []contract.Button) (string, error) {
	return "", fmt.Errorf("local: SendButtons unavailable (callbacks subsystem not yet wired)")
}

// NewSink returns a per-turn sink with the scope's render prefs. Local-cli
// renders one turn at a time, but sinks may be OPENED out of step with the
// render loop (a store-stalled turn opens its sink late), so each NewSink
// appends to the FIFO rather than replacing a single slot — the renderer
// consumes them in registration order (turns on one session are serialised,
// so registration order is turn order).
func (s *Source) NewSink(_ context.Context, _ contract.Target, _ string, _ string, prefs contract.SinkPrefs) contract.Sink {
	k := &sink{
		stream: make(chan chunk, 256),
		prefs:  prefs,
	}
	s.sinkMu.Lock()
	s.sinks = append(s.sinks, k)
	s.sinkMu.Unlock()
	return k
}

// takeSink pops + returns the oldest pending sink, or nil when none is
// registered.
func (s *Source) takeSink() *sink {
	s.sinkMu.Lock()
	defer s.sinkMu.Unlock()
	if len(s.sinks) == 0 {
		return nil
	}
	k := s.sinks[0]
	s.sinks = s.sinks[1:]
	return k
}

// discardSinks drops ALL pending sinks and returns how many were dropped.
// Called right before dispatching a new line: anything still registered at
// that point is a stale reply from a previous turn that the user has already
// given up waiting on — rendering it under the new prompt would misattribute
// it as the new line's answer.
func (s *Source) discardSinks() int {
	s.sinkMu.Lock()
	dropped := s.sinks
	s.sinks = nil
	s.sinkMu.Unlock()
	// F33: a discarded sink may carry an INTERNAL-woken turn's reply — a worker return /
	// cron / proactive delivery that landed in the sink while the user sat idle at `you>`
	// and printReply wasn't reading. Don't silently drop it: drain the buffered content
	// and print it with a header so the operator actually sees the background answer (it
	// is also in session history, but a terminal user shouldn't have to go dig for it).
	for _, k := range dropped {
		if text := strings.TrimSpace(k.drain()); text != "" {
			s.stdoutMu.Lock()
			fmt.Println("\n[bob（后台回复）]:\n" + text)
			s.stdoutMu.Unlock()
		}
	}
	return len(dropped)
}

type sink struct {
	stream chan chunk
	prefs  contract.SinkPrefs

	mu       sync.Mutex
	finished bool
	// contentSoFar mirrors the concatenation of ContentDeltas pushed so far
	// (trace excluded), so Finish can tell whether its canonical `full` is a
	// continuation of what already streamed or something new (e.g. an error
	// notice the turn core passes straight to Finish) that must still render.
	contentSoFar strings.Builder
	// blockBuf accumulates deltas when prefs.Stream=false, flushed in one chunk
	// at Finish. Trace deltas only land here if prefs.Trace.
	blockBuf strings.Builder
}

// drain non-blockingly collects the buffered content a discarded sink never got to
// render (F33). Reads whatever is in the channel right now (a finished turn's chunks, or
// a stalled turn's partial); a closed channel stops it. Best-effort snapshot — any later
// pushes to this now-orphaned sink still hit the bounded channel's overflow drop.
func (k *sink) drain() string {
	var b strings.Builder
	for {
		select {
		case c, ok := <-k.stream:
			if !ok {
				return b.String()
			}
			b.WriteString(c.text)
		default:
			return b.String()
		}
	}
}

// ContentDelta routes LLM content to the REPL printer.
func (k *sink) ContentDelta(text string) { k.delta(text, true) }

// TraceDelta routes "what bob is doing" lines. Dropped when prefs.Trace=false.
// Each annotation is one complete line, so terminate it with a newline (content
// deltas, which are token fragments, go through delta() directly and are left
// alone).
func (k *sink) TraceDelta(text string) {
	if !k.prefs.Trace || text == "" {
		return
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	k.delta(text, false)
}

// delta is the shared push/accumulate path. isContent marks LLM content (vs
// trace), which counts toward contentSoFar for Finish's divergence check.
func (k *sink) delta(text string, isContent bool) {
	if text == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.finished {
		return
	}
	if isContent {
		k.contentSoFar.WriteString(text)
	}
	if !k.prefs.Stream {
		k.blockBuf.WriteString(text)
		return
	}
	// Stream mode: push live. The send stays UNDER k.mu so the finished-check
	// and the channel send are atomic w.r.t. Finish (which closes k.stream while
	// also holding k.mu) — without the lock a send could race the close →
	// "send on closed channel" panic. The send is non-blocking, so holding the
	// lock can't deadlock against a stalled reader.
	select {
	case k.stream <- chunk{text: text}:
	default:
		// 256-deep buffer full — drop rather than block. Terminal output isn't
		// load-bearing; better to drop than pin the agent goroutine.
	}
}

func (k *sink) Finish(full string) error {
	// Whole body under k.mu — including the channel send + close — so it's
	// mutually exclusive with delta's send (also under k.mu). delta either
	// completes its send before Finish closes, or sees finished=true and
	// returns. Sends are non-blocking, so holding the lock can't deadlock.
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.finished {
		return nil
	}
	k.finished = true
	// `full` is the canonical content reply, which is NOT always the
	// concatenation of prior ContentDeltas: the turn core calls Finish directly
	// with error notices (internal error / tool-persist failure / model
	// unavailable) that never streamed. Mirror the streamsink core: render the
	// un-streamed suffix of `full`, or — if `full` diverges from what streamed —
	// render it whole on a fresh line (printed terminal bytes can't be edited
	// away). HasPrefix("", …) covers the nothing-streamed case: tail == full.
	sofar := k.contentSoFar.String()
	var tail string
	if strings.HasPrefix(full, sofar) {
		tail = full[len(sofar):]
	} else if full != "" {
		tail = "\n" + full
	}
	if !k.prefs.Stream {
		blockText := full
		if k.prefs.Trace && k.blockBuf.Len() > 0 {
			// blockBuf holds the interleaved trace + content; append whatever
			// part of the canonical reply it doesn't already carry.
			blockText = k.blockBuf.String() + tail
		}
		select {
		case k.stream <- chunk{text: blockText, done: true}:
		default:
		}
		close(k.stream)
		return nil
	}

	select {
	case k.stream <- chunk{text: tail, done: true}:
	default:
	}
	close(k.stream)
	return nil
}

// SendFile delivers a file to the terminal user. The terminal has no attachment
// primitive — the file is already on the local filesystem — so "delivery" is
// just printing where it landed. It does NOT go through the stream channel (a
// one-off side-band print, like Source.Send) — the print can interleave with
// the reply renderer's terminal output mid-line, but never corrupts the stream
// channel itself; the line-split is cosmetic (see stdoutMu's doc). (sink
// carries no *Source ref, so it prints directly rather than reusing
// Source.SendFile; identical line.)
func (k *sink) SendFile(path, caption string) error {
	line := "📎 delivered: " + path
	if caption != "" {
		line += " (" + caption + ")"
	}
	fmt.Println("\n" + line)
	return nil
}

// LastSent returns "" — the local terminal has no platform message id to anchor
// reply routing on.
func (k *sink) LastSent() string { return "" }

// scanResult is what the stdin reader goroutine pushes per line.
type scanResult struct {
	text string
	err  error // sc.Err() (rare) — scanner buffer overflow
	eof  bool  // Scan() returned false with no error → EOF (Ctrl-D)
}

// Run reads lines from stdin, turns each into a MessageEvent, and prints the
// reply. The stdin scan runs in a dedicated goroutine pushing to a channel; the
// main loop selects on (channel, ctx.Done) so shutdown can break out without
// waiting for the user to hit Enter. The scanner is admitted one line at a time
// (scanGo) so it never reads ahead mid-turn.
func (s *Source) Run(ctx context.Context, out chan<- contract.MessageEvent) error {
	fmt.Println(i18n.T("system.local_banner", "default"))

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	lineCh := make(chan scanResult)
	scanGo := make(chan struct{})
	go func() {
		defer close(lineCh)
		for {
			select {
			case <-ctx.Done():
				return
			case <-scanGo:
			}
			ok := sc.Scan()
			var res scanResult
			switch {
			case ok:
				res = scanResult{text: sc.Text()}
			case sc.Err() != nil:
				res = scanResult{err: sc.Err()}
			default:
				res = scanResult{eof: true}
			}
			select {
			case lineCh <- res:
			case <-ctx.Done():
				return
			}
			if !ok {
				return
			}
		}
	}()

	for {
		fmt.Print("\nyou> ")
		select {
		case scanGo <- struct{}{}:
		case <-ctx.Done():
			fmt.Println("\n" + i18n.T("system.bye", "default"))
			return ctx.Err()
		}
		var res scanResult
		select {
		case <-ctx.Done():
			fmt.Println("\n" + i18n.T("system.bye", "default"))
			return ctx.Err()
		case r, ok := <-lineCh:
			if !ok {
				fmt.Println("\n" + i18n.T("system.bye", "default"))
				return io.EOF
			}
			res = r
		}
		if res.err != nil {
			return res.err
		}
		if res.eof {
			fmt.Println("\n" + i18n.T("system.bye", "default"))
			// Return io.EOF (not nil) so the bus distinguishes "user pressed
			// Ctrl-D — graceful self-exit" from a source nil-returning for no
			// reason; the bus treats EOF as intentional, not a crash.
			return io.EOF
		}
		text := strings.TrimRight(res.text, "\r\n")
		if strings.TrimSpace(text) == "" {
			continue
		}

		ev := contract.MessageEvent{
			Source:     "local",
			ChatType:   contract.ChatDM,
			ChatID:     s.chatID,
			MessageID:  strconv.FormatInt(time.Now().UnixNano(), 10),
			UserID:     "local",
			UserName:   "you",
			IsAdmin:    true,
			Text:       text,
			ReceivedAt: time.Now(),
		}
		// Discard any sinks left registered by PREVIOUS turns whose printReply
		// timed out (slow NewSinks that landed after we gave up). This event
		// hasn't been dispatched yet, so anything registered here is necessarily
		// stale — dropping it now stops a late sink from being rendered as THIS
		// turn's reply. Each discarded sink settles one owed reply.
		s.owed -= s.discardSinks()
		if s.owed < 0 {
			s.owed = 0
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
		s.owed++

		fmt.Print("\nbob> ")
		spin := startSpinner()
		if !s.printReply(ctx, spin) {
			return ctx.Err()
		}
	}
}

// sinkWaitTimeout bounds how long printReply waits for the turn to open its
// sink. The turn's NewSink runs shortly (tens of ms) after the event is
// dispatched, asynchronously — so we must wait for it rather than racing it to
// nil. A turn that never opens a sink (an error before NewSink, a no-reply
// path) is bounded by this so the REPL never hangs.
const sinkWaitTimeout = 5 * time.Second

// staleSinkGrace bounds each round of printReply's catch-up loop: once one
// owed reply has rendered, the NEXT owed turn (queued behind it, drained
// synchronously when the previous turn finishes) opens its sink within
// in-process pipeline time — unless it is a no-reply path, which this window
// writes off.
const staleSinkGrace = 2 * time.Second

// printReply renders the replies owed for the just-dispatched line.
//
// Normally that is exactly one sink: dispatch → the turn registers its sink →
// render to completion. But when a PREVIOUS printReply timed out (a turn
// stalled, e.g. on the store, before opening its sink) more than one reply is
// owed, and the FIFO's oldest sink belongs to the PREVIOUS turn, not this one.
// Rendering just the first sink would misattribute the late reply as this
// line's answer AND silently lose this line's real reply (its sink would sit
// unread until the next dispatch discards it). So after the first sink, keep
// consuming owed sinks — each under its own fresh "bob> " header so late
// replies read as late replies — until the debt is settled or a grace window
// expires with no sink (the remaining owed turns are presumed no-reply paths
// and written off; a write-off guessed wrong self-heals at the next dispatch's
// discard).
func (s *Source) printReply(ctx context.Context, spin *spinner) bool {
	k := s.awaitSink(ctx, sinkWaitTimeout)
	if k == nil {
		// No sink within the window (no-reply turn, stalled turn, or shutdown) —
		// finish the line. s.owed stays as is: if the turn opens its sink late,
		// the next printReply's catch-up loop (or the next dispatch's discard)
		// settles the debt.
		spin.erase()
		fmt.Println()
		return ctx.Err() == nil
	}
	if !s.renderSink(ctx, spin, k) {
		return false
	}
	s.owed--
	for s.owed > 0 {
		k := s.awaitSink(ctx, staleSinkGrace)
		if k == nil {
			if ctx.Err() != nil {
				return false
			}
			s.owed = 0 // write off: presumed no-reply turns (see doc above)
			break
		}
		fmt.Print("\nbob> ")
		if !s.renderSink(ctx, nil, k) {
			return false
		}
		s.owed--
	}
	if s.owed < 0 {
		s.owed = 0
	}
	return true
}

// renderSink drains one sink's stream to the terminal until its done chunk (or
// channel close, or ctx cancellation — the only false return). spin may be nil
// (catch-up rounds run without a spinner).
func (s *Source) renderSink(ctx context.Context, spin *spinner, k *sink) bool {
	for {
		select {
		case c, ok := <-k.stream:
			if !ok {
				spin.erase()
				fmt.Println()
				return true
			}
			spin.erase()
			fmt.Print(c.text)
			if c.done {
				fmt.Println()
				return true
			}
		case <-ctx.Done():
			spin.erase()
			fmt.Println()
			return false
		}
	}
}

// awaitSink waits (bounded) for a turn to register a sink, returning the
// oldest pending one. Returns nil on timeout or ctx cancellation.
func (s *Source) awaitSink(ctx context.Context, timeout time.Duration) *sink {
	deadline := time.Now().Add(timeout)
	for {
		if k := s.takeSink(); k != nil {
			return k
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Millisecond):
		}
	}
}

var _ contract.Source = (*Source)(nil)
