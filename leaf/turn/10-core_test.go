package turn

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/prompt"
	"agentbob/i18n"
	"agentbob/leaf/pgpool"
	"agentbob/leaf/session/messages"
	"agentbob/trunk"
)

// sysPrompt returns a filled prompt builder whose Build() == text — the turn
// core re-Builds it each round.
func sysPrompt(text string) contract.Prompt {
	b := prompt.NewBuilder()
	b.SetLayer("system", text)
	return b
}

// TestBuildSystem renders the round's system prompt from the flow's builder and
// is nil-safe (a spec with no builder → no system message). Pure — no pg.
func TestBuildSystem(t *testing.T) {
	if got := buildSystem(nil); got != "" {
		t.Fatalf("buildSystem(nil) = %q, want empty (no system message)", got)
	}
	if got := buildSystem(sysPrompt("hello")); got != "hello" {
		t.Fatalf("buildSystem(builder) = %q, want the built layers", got)
	}
}

func liveStore(t *testing.T) (contract.MessageStore, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run turn integration tests")
	}
	ctx := context.Background()
	reg := trunk.NewRegistry()
	pg := pgpool.New(dsn)
	if err := pg.Start(ctx, reg); err != nil {
		t.Fatalf("pgpool start: %v", err)
	}
	db := trunk.Require[contract.DB](reg)
	if err := messages.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return messages.NewPG(db), func() { _ = pg.Stop(ctx) }
}

// fakePool streams a canned reply and records the prompt it was handed.
type fakePool struct {
	reply   string
	gotMsgs []contract.Message
}

func (*fakePool) ChatStream(context.Context, contract.ModelRequest, []contract.Message) (<-chan contract.StreamEvent, error) {
	return nil, nil // unused: the turn core drives the single ChatStreamWatch path
}
func (*fakePool) Chat(context.Context, contract.ModelRequest, []contract.Message) (contract.ChatResponse, error) {
	return contract.ChatResponse{}, nil
}

// ChatStreamWatch records the prompt, streams the canned reply through the
// watcher (so the sink renders deltas), and returns it as the full response.
func (p *fakePool) ChatStreamWatch(_ context.Context, _ contract.ModelRequest, msgs []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	p.gotMsgs = msgs
	usage := contract.Usage{OutputTokens: 5}
	accum := &contract.StreamAccumulator{Text: p.reply}
	_ = w(contract.StreamEvent{Text: p.reply}, accum)
	_ = w(contract.StreamEvent{Done: true, Usage: usage}, accum)
	return contract.ChatResponse{Content: p.reply, Usage: usage}, nil
}
func (*fakePool) Snapshot() contract.PoolSnapshot { return contract.PoolSnapshot{} }
func (*fakePool) FlushUsage(context.Context)      {}
func (*fakePool) FlushUsageFinal(context.Context) {}
func (*fakePool) Close()                          {}

type recSink struct {
	deltas   []string
	trace    []string
	files    []string
	last     string // returned by LastSent (zero value preserves old behavior)
	finished string
	done     bool
}

func (s *recSink) ContentDelta(t string) { s.deltas = append(s.deltas, t) }
func (s *recSink) TraceDelta(t string)   { s.trace = append(s.trace, t) }
func (s *recSink) Finish(full string) error {
	s.finished = full
	s.done = true
	return nil
}
func (s *recSink) SendFile(path, _ string) error {
	s.files = append(s.files, path)
	return nil
}
func (s *recSink) LastSent() string { return s.last }

func uscope(tag string) string { return tag + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) }

func TestRun_StreamsAndPersists(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := uscope("turnlite")

	pool := &fakePool{reply: "你好！"}
	c := &core{store: st, pool: pool}
	sink := &recSink{}

	c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	// reply streamed + finished.
	if !sink.done || sink.finished != "你好！" {
		t.Fatalf("sink finished=%q done=%v, want 你好！/true", sink.finished, sink.done)
	}
	if len(sink.deltas) == 0 {
		t.Fatal("reply was not streamed (no ContentDelta)")
	}
	// prompt = system + user (no history on the first turn).
	if len(pool.gotMsgs) != 2 || pool.gotMsgs[0].Role != "system" || pool.gotMsgs[1].Role != "user" || pool.gotMsgs[1].Content != "hi" {
		t.Fatalf("first prompt = %+v, want [system, user(hi)]", pool.gotMsgs)
	}
	// history persisted: user + assistant.
	msgs, err := st.GetMessages(ctx, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[0].Content != "hi" || msgs[1].Role != "assistant" || msgs[1].Content != "你好！" {
		t.Fatalf("history = %+v, want user(hi)+assistant(你好！)", msgs)
	}

	// second turn replays the prior exchange into the prompt.
	c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys"), UserText: "again", Sink: &recSink{}})
	if len(pool.gotMsgs) != 4 { // system + 2 history + new user
		t.Fatalf("second prompt len = %d, want 4 (system + 2 history + user)", len(pool.gotMsgs))
	}
}

// cancelPool streams a partial, then cancels the turn ctx mid-stream and returns
// the cancel error — simulating a /stop while content is in flight.
type cancelPool struct {
	*fakePool
	partial string
	cancel  context.CancelFunc
}

func (p *cancelPool) ChatStreamWatch(ctx context.Context, _ contract.ModelRequest, _ []contract.Message, w contract.StreamWatcher) (contract.ChatResponse, error) {
	accum := &contract.StreamAccumulator{Text: p.partial}
	_ = w(contract.StreamEvent{Text: p.partial}, accum) // user sees the partial live
	p.cancel()                                          // /stop mid-stream
	return contract.ChatResponse{}, ctx.Err()
}

// TestRun_CancelMidStreamPersistsPartial (§4.3): a cancel AFTER visible content was
// streamed persists the partial + a cut-here marker and finalizes — history matches
// what the user saw. (Contrast: a cancel with nothing shown stays silent.) No DB —
// uses the in-memory subStore as the MessageStore.
func TestRun_CancelMidStreamPersistsPartial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := newSubStore()
	c := &core{store: store, pool: &cancelPool{fakePool: &fakePool{}, partial: "半截回复", cancel: cancel}}
	sink := &recSink{}

	res := c.Run(ctx, contract.TurnSpec{Sid: "s1", Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	if res.Err == nil {
		t.Fatal("want a cancel error, got nil")
	}
	// The marker is now i18n-keyed (rendered in the turn's detected lang — "hi" → default).
	want := "半截回复" + i18n.T(cancelledMarkerKey, "default")
	if !sink.done || sink.finished != want {
		t.Fatalf("sink finished=%q done=%v, want %q/true", sink.finished, sink.done, want)
	}
	msgs, _ := store.GetMessages(ctx, "s1", 0)
	if len(msgs) < 2 || msgs[len(msgs)-1].Role != "assistant" || msgs[len(msgs)-1].Content != want {
		t.Fatalf("history last = %+v, want assistant %q persisted", msgs, want)
	}
}

// TestZeroValueTurnExitNotCancelled (F108): a forgotten-state &turnExit{} must NOT
// alias exitCancelled — the driver would release the sink silently and skip salvage.
// exitUnset holds the zero slot so such a dud falls into salvage (loud) instead.
func TestZeroValueTurnExitNotCancelled(t *testing.T) {
	var e turnExit
	if e.state == exitCancelled {
		t.Fatal("zero-value turnExit aliases exitCancelled — a forgotten state would silently cancel the turn")
	}
	if e.state != exitUnset {
		t.Fatalf("zero-value turnExit state = %d, want exitUnset (0)", e.state)
	}
}

// TestRun_ModelError: a pool failure renders the unavailable notice and persists
// no assistant row.
func TestRun_ModelError(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx := context.Background()
	sid := uscope("turnerr")

	c := &core{store: st, pool: &errPool{}}
	sink := &recSink{}
	res := c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	if res.Err == nil {
		t.Fatal("Run should report the pool error")
	}
	if sink.finished != i18n.T(modelUnavailableKey, "default") {
		t.Fatalf("finished = %q, want the unavailable notice", sink.finished)
	}
	// user persisted (before the call), assistant NOT.
	msgs, _ := st.GetMessages(ctx, sid, 0)
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("history = %+v, want only the user row", msgs)
	}
}

// errPool fails the model call with a live caller ctx (a model timeout, not a
// cancel) → the round renders the unavailable notice and surfaces the error.
type errPool struct{ fakePool }

func (errPool) ChatStreamWatch(context.Context, contract.ModelRequest, []contract.Message, contract.StreamWatcher) (contract.ChatResponse, error) {
	return contract.ChatResponse{}, context.DeadlineExceeded
}

// emptyPool always returns an empty reply → every round Continues (I5).
type emptyPool struct{ fakePool }

func (emptyPool) ChatStreamWatch(context.Context, contract.ModelRequest, []contract.Message, contract.StreamWatcher) (contract.ChatResponse, error) {
	return contract.ChatResponse{}, nil
}

// TestRun_EmptyRepliesSalvage: a model that only ever returns empty Continues
// every round until the loop cap, then salvage delivers exactly one readable
// line (I8).
func TestRun_EmptyRepliesSalvage(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	old := loopCap
	loopCap = 3
	defer func() { loopCap = old }()

	ctx := context.Background()
	sid := uscope("turnsalvage")
	c := &core{store: st, pool: &emptyPool{}}
	sink := &recSink{}

	res := c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	notice := i18n.T(salvageStockKey, "default") // i18n-keyed stock floor ("hi" → default lang)
	if !sink.done || sink.finished != notice {
		t.Fatalf("sink finished=%q done=%v, want the salvage notice", sink.finished, sink.done)
	}
	if res.Reply != notice {
		t.Fatalf("reply = %q, want the salvage notice", res.Reply)
	}
	// history: the user row + the salvage assistant row (no empty rows persisted).
	msgs, _ := st.GetMessages(ctx, sid, 0)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" || msgs[1].Content != notice {
		t.Fatalf("history = %+v, want user + salvage assistant", msgs)
	}
}

// TestRun_CancelSilent: a cancelled turn returns the cancel error and never
// finishes the sink (I8/I14 — no apology on cancel).
func TestRun_CancelSilent(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled → the driver's loop-top cancel door fires

	c := &core{store: st, pool: &fakePool{reply: "unused"}}
	sink := &recSink{}
	res := c.Run(ctx, contract.TurnSpec{Sid: uscope("turncancel"), Prompt: sysPrompt("sys"), UserText: "hi", Sink: sink})

	if sink.done {
		t.Fatal("a cancelled turn must not finish the sink")
	}
	if res.Err == nil {
		t.Fatal("a cancelled turn must surface the cancel error")
	}
}
