// Tests for the model pool's request-level failover — when an entry's call
// fails, the pool retries on the next-best entry instead of surfacing the
// error, so the caller "only feels it as slower":
//   - Chat (non-streaming): atomic, always retriable on failure
//   - ChatStreamWatch (streaming): retriable ONLY before the first content
//     event; a post-content failure is committed and surfaces to the caller
//   - ctx cancellation is never retried
//
// White-box (package model) so a pool can be built directly from fake
// chatters without the provider/probe machinery.
package model

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/model/providers"
)

// fakeChatter is a contract.Chatter whose behaviour is fully scripted.
type fakeChatter struct {
	mu          sync.Mutex
	chatCalls   int
	streamCalls int

	chatErr error  // Chat: returned if non-nil (overridden per-call by chatErrs when set)
	text    string // content (Chat: Content; ChatStream: one Text event)
	preErr  error  // ChatStream: delivered as the FIRST event (before any content)
	postErr error  // ChatStream: delivered AFTER the content event
	stall   bool   // ChatStream: open the stream but never produce content (until ctx ends)
	pingErr error  // Ping: returned if non-nil
	// chatErrs lets a test program per-call Chat behaviour: chatErrs[i]
	// is returned on the (i+1)-th call (nil → success with `text`).
	// Calls past len(chatErrs) fall through to the chatErr default. Lets
	// tests assert e.g. "errs on call 1, ok on call 2" for the wake-then-
	// retry path.
	chatErrs []error
}

var _ contract.Chatter = (*fakeChatter)(nil)

func (f *fakeChatter) Chat(_ context.Context, _ string, _ []contract.Message, _ []contract.ToolSpec) (contract.ChatResponse, error) {
	f.mu.Lock()
	f.chatCalls++
	idx := f.chatCalls - 1
	var perCallErr error
	usePerCall := false
	if idx < len(f.chatErrs) {
		perCallErr = f.chatErrs[idx]
		usePerCall = true
	}
	f.mu.Unlock()
	if usePerCall {
		if perCallErr != nil {
			return contract.ChatResponse{}, perCallErr
		}
		return contract.ChatResponse{Content: f.text, Usage: contract.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	}
	if f.chatErr != nil {
		return contract.ChatResponse{}, f.chatErr
	}
	return contract.ChatResponse{Content: f.text, Usage: contract.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

func (f *fakeChatter) ChatStream(ctx context.Context, _ string, _ []contract.Message, _ []contract.ToolSpec) (<-chan contract.StreamEvent, error) {
	f.mu.Lock()
	f.streamCalls++
	f.mu.Unlock()
	ch := make(chan contract.StreamEvent, 4)
	if f.stall {
		// Accept the request then wedge — never emit content. Honour ctx so
		// the pool's first-content-deadline cancellation lets us exit.
		go func() {
			defer close(ch)
			<-ctx.Done()
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		if f.preErr != nil {
			ch <- contract.StreamEvent{Err: f.preErr}
			return
		}
		ch <- contract.StreamEvent{Text: f.text}
		if f.postErr != nil {
			ch <- contract.StreamEvent{Err: f.postErr}
			return
		}
		ch <- contract.StreamEvent{Done: true, Usage: contract.Usage{InputTokens: 1, OutputTokens: 1}}
	}()
	return ch, nil
}

func (f *fakeChatter) Ping(context.Context) error { return f.pingErr }

func (f *fakeChatter) counts() (chat, stream int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatCalls, f.streamCalls
}

func entry(name string, priority int, c contract.Chatter) *entryRow {
	return &entryRow{
		info: contract.ModelInfo{
			Name: name, Kind: contract.KindLLM, Model: name + "-model",
			Priority: priority, Tags: []string{"smart"},
		},
		chatter: c,
	}
}

func newTestPool(rows ...*entryRow) *MultiPool {
	p := &MultiPool{usage: NewModelUsageRecorder()}
	st := &poolState{byName: map[string]*entryRow{}}
	for _, r := range rows {
		st.entries = append(st.entries, r)
		st.byName[r.info.Name] = r
	}
	p.state.Store(st)
	// HeartbeatRunner: needed because recordError starts the heartbeat
	// goroutine on a fresh dead-transition, and the runner reads
	// poolCtx.Done() to know when to stop. Recovery hook is a no-op in
	// tests that don't care about checkPoolLiveness.
	p.heartbeat = NewHeartbeatRunner(&p.state, func() { p.checkPoolLiveness() })
	return p
}

var smartReq = contract.ModelRequest{Requires: []string{"smart"}}

// TestPickRoutesByKind pins kind-based routing: a request is matched only
// against entries of its Kind (empty Kind → KindLLM), so a non-LLM entry
// is structurally unreachable from a default chat request.
func TestPickRoutesByKind(t *testing.T) {
	llm := entry("chat", 0, &fakeChatter{text: "hi"})
	tr := entry("translator", 0, &fakeChatter{text: "translated"})
	tr.info.Kind = contract.KindTranslate
	p := newTestPool(llm, tr)
	defer p.Close()

	// Default request (no Kind) routes only to the llm entry.
	if got, err := p.pick(contract.ModelRequest{}, nil); err != nil || got != llm {
		t.Fatalf("default request must pick the llm entry, got %v err %v", got, err)
	}
	// Kind=translate routes only to the translate entry.
	if got, err := p.pick(contract.ModelRequest{Kind: contract.KindTranslate}, nil); err != nil || got != tr {
		t.Fatalf("Kind=translate must pick the translate entry, got %v err %v", got, err)
	}
	// Kind=asr has no entry — must error, never fall back to llm/translate.
	if got, err := p.pick(contract.ModelRequest{Kind: contract.KindASR}, nil); err == nil {
		t.Fatalf("Kind=asr with no asr entry must error, got entry %q", got.info.Name)
	}
}

func TestChatFailoverRetriesNextEntry(t *testing.T) {
	a := &fakeChatter{chatErr: errors.New("a is down")}
	b := &fakeChatter{text: "ok"}
	// a has the higher priority → picked first, fails → pool retries on b.
	p := newTestPool(entry("a", 3, a), entry("b", 0, b))

	resp, err := p.Chat(context.Background(), smartReq, nil)
	if err != nil {
		t.Fatalf("Chat should have failed over to b, got err: %v", err)
	}
	if resp.Content != "ok" || resp.Model != "b" {
		t.Fatalf("expected b's response, got content=%q model=%q", resp.Content, resp.Model)
	}
	ca, _ := a.counts()
	cb, _ := b.counts()
	if ca != 1 || cb != 1 {
		t.Fatalf("expected a tried once + b tried once, got a=%d b=%d", ca, cb)
	}
}

func TestChatAllEntriesFail(t *testing.T) {
	a := &fakeChatter{chatErr: errors.New("a is down")}
	b := &fakeChatter{chatErr: errors.New("b is down")}
	p := newTestPool(entry("a", 3, a), entry("b", 0, b))

	if _, err := p.Chat(context.Background(), smartReq, nil); err == nil {
		t.Fatal("Chat must return an error when every entry fails")
	}
	ca, _ := a.counts()
	cb, _ := b.counts()
	if ca != 1 || cb != 1 {
		t.Fatalf("every entry should be tried exactly once, got a=%d b=%d", ca, cb)
	}
}

func TestChatNoRetryOnCancel(t *testing.T) {
	a := &fakeChatter{chatErr: context.Canceled}
	b := &fakeChatter{text: "ok"}
	p := newTestPool(entry("a", 3, a), entry("b", 0, b))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Chat(ctx, smartReq, nil); err == nil {
		t.Fatal("a cancelled Chat must return an error")
	}
	if cb, _ := b.counts(); cb != 0 {
		t.Fatalf("a cancelled Chat must NOT fail over — b was called %d times", cb)
	}
}

// thinkingChatter streams a reply on the THINKING channel only: reasoning deltas,
// then a content event that is empty or whitespace-only. This is what a
// reasoning-parser backend produces when the think runs into the output cap —
// billed tokens, nothing visible.
type thinkingChatter struct {
	fakeChatter
	reasoning string
	content   string // "" or whitespace — the shapes finalReply reads as empty
}

func (f *thinkingChatter) ChatStream(context.Context, string, []contract.Message, []contract.ToolSpec) (<-chan contract.StreamEvent, error) {
	f.mu.Lock()
	f.streamCalls++
	f.mu.Unlock()
	ch := make(chan contract.StreamEvent, 4)
	go func() {
		defer close(ch)
		ch <- contract.StreamEvent{Reasoning: f.reasoning}
		if f.content != "" {
			ch <- contract.StreamEvent{Text: f.content}
		}
		ch <- contract.StreamEvent{Done: true, Usage: contract.Usage{InputTokens: 1, OutputTokens: 900}}
	}()
	return ch, nil
}

// A thinking-only reply FAILS the attempt so the driver reaches a peer, instead of
// handing the round kernel an empty reply to loop on forever (leaf/turn/20-round.go
// I5). The whitespace variant matters just as much: finalReply TRIMS before testing
// emptiness, so a reply of "\n\n" is empty downstream and must classify the same way
// here — testing == "" would let that shape sail past as a clean success.
func TestChatStreamThinkingOnlyFailsOver(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"empty content", ""},
		{"whitespace-only content", "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &thinkingChatter{reasoning: "Let me work through this. Still thinking.", content: tc.content}
			b := &fakeChatter{text: "the actual answer"}
			rowA, rowB := entry("thinker", 5, a), entry("answerer", 1, b)
			p := newTestPool(rowA, rowB)
			defer p.Close()

			resp, err := p.ChatStreamWatch(context.Background(), smartReq, []contract.Message{{Role: "user", Content: "hi"}}, nil)
			if err != nil {
				t.Fatalf("ChatStreamWatch: %v", err)
			}
			if resp.Content != "the actual answer" {
				t.Fatalf("content = %q, want the peer's answer (failover did not happen)", resp.Content)
			}
			if _, streams := a.counts(); streams != 1 {
				t.Fatalf("thinking entry stream calls = %d, want 1", streams)
			}
			// Health is deliberately untouched: the backend streamed tokens, it is
			// alive. Cooling it would pull the only thinking-tagged entry out of
			// rotation over one long think.
			if !rowA.IsEligible() {
				t.Fatal("thinking-only must not cool the entry — it is a config outcome, not a health fault")
			}
		})
	}
}

// With no peer to reach, thinking-only surfaces as a real error rather than an empty
// success — the caller gets an honest "no model available" instead of a turn that
// spins to its fuse on an unchanged history.
func TestChatStreamThinkingOnlySoleCandidate(t *testing.T) {
	a := &thinkingChatter{reasoning: "thinking, and thinking.", content: ""}
	p := newTestPool(entry("thinker", 5, a))
	defer p.Close()

	_, err := p.ChatStreamWatch(context.Background(), smartReq, []contract.Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("sole thinking-only candidate must error, not return an empty success")
	}
	if !errors.Is(err, ErrNoModelAvailable) {
		t.Fatalf("err = %v, want it to wrap ErrNoModelAvailable", err)
	}
	if !strings.Contains(err.Error(), ErrThinkingOnly.Error()) {
		t.Fatalf("err = %v, want the cause to name the thinking-only sentinel", err)
	}
}

// blockThinkingChatter returns a thinking-only reply on the BLOCK path, the way a
// reasoning backend does when the think eats the whole output cap.
type blockThinkingChatter struct {
	fakeChatter
	out int
}

func (f *blockThinkingChatter) Chat(context.Context, string, []contract.Message, []contract.ToolSpec) (contract.ChatResponse, error) {
	f.mu.Lock()
	f.chatCalls++
	f.mu.Unlock()
	return contract.ChatResponse{
		Reasoning: "thinking, and thinking, and out of budget.",
		Usage:     contract.Usage{InputTokens: 10, OutputTokens: int(f.out)},
	}, providers.ErrThinkingOnly
}

// The block path (every side-LLM: compress, judge, salvage, vision) fails over on
// thinking-only rather than handing back an empty success, and it BOOKS the tokens
// that were really spent while leaving the entry's health alone. Thinking-only is
// the one failure class whose trigger guarantees a bill, so dropping it would put a
// hole in both /model's stats and the per-user ledger.
func TestChatBlockThinkingOnlyFailsOverAndBills(t *testing.T) {
	a := &blockThinkingChatter{out: 900}
	b := &fakeChatter{text: "the actual answer"}
	rowA, rowB := entry("thinker", 5, a), entry("answerer", 1, b)
	p := newTestPool(rowA, rowB)
	defer p.Close()

	resp, err := p.Chat(context.Background(), smartReq, []contract.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "the actual answer" {
		t.Fatalf("content = %q, want the peer's answer (failover did not happen)", resp.Content)
	}
	rowA.mu.Lock()
	booked, calls, errs := rowA.totalOutputTokens, rowA.totalCalls, rowA.totalErrors
	rowA.mu.Unlock()
	if booked != 900 {
		t.Errorf("thinking entry booked %d output tokens, want 900", booked)
	}
	if calls != 1 {
		t.Errorf("thinking entry totalCalls = %d, want 1", calls)
	}
	// totalErrors is the HEALTH counter recordError owns; thinking-only stays off it.
	if errs != 0 {
		t.Errorf("thinking entry totalErrors = %d, want 0 — health must be untouched", errs)
	}
	if !rowA.IsEligible() {
		t.Error("thinking-only must not cool the entry")
	}
}
