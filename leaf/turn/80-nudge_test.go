package turn

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentbob/contract"
)

// TestFoldUserNudge is the whole contract of the fast-lane after it stopped being a
// prompt layer: a busy-arrived message is PERSISTED as a user row, joins the round's
// replay as the newest user message, reaches the acceptance gates via UserText, and
// is pulled at most once per turn.
func TestFoldUserNudge(t *testing.T) {
	const msg = "把澳大利亚的伤停也算进去"
	store := &rubricStore{}
	c := &core{store: store}
	st := newTurnState()
	pr := sysPrompt("sys")
	calls := 0
	spec := contract.TurnSpec{Sid: "s1", Prompt: pr, UserText: "查一下欧洲杯赛程", NextNudge: func() *contract.UserMsg {
		calls++
		if calls == 1 {
			return &contract.UserMsg{Author: contract.MsgAuthor{Kind: "human", ID: "u1", Name: "Bob"}, Text: msg}
		}
		return nil // queue empty after the first pull
	}}

	replay := []contract.Message{{Role: "user", Content: "查一下欧洲杯赛程"}}
	replay = c.foldUserNudge(context.Background(), &spec, st, replay) // round 0: pull

	// Durable: the message survives in history whether or not the model answers it.
	// This is the whole point — the old layer left NO record anywhere.
	if len(store.appended) != 1 || !strings.Contains(store.appended[0], msg) {
		t.Fatalf("nudge not persisted as a user row; appended = %q", store.appended)
	}
	// In the conversation, as the newest user message — not as prompt background.
	last := replay[len(replay)-1]
	if last.Role != "user" || !strings.Contains(last.Content, msg) {
		t.Fatalf("nudge not spliced onto the replay as a user row; last = %+v", last)
	}
	if last.Author.Name != "Bob" || last.Author.Kind != "human" {
		t.Errorf("nudge row lost its speaker attribution: %+v", last.Author)
	}
	if !strings.Contains(last.Content, userNudgeMark) {
		t.Errorf("nudge row should be marked as a mid-work addition: %q", last.Content)
	}
	// The acceptance gates render UserText as 「用户的请求」 — a nudge missing from it is
	// invisible to the reviewer, which would grade an answer to it as off-topic.
	if !strings.Contains(spec.UserText, msg) || !strings.Contains(spec.UserText, "查一下欧洲杯赛程") {
		t.Errorf("UserText must carry BOTH the original ask and the nudge; got %q", spec.UserText)
	}
	// Regression guard: it must never go back to riding the system prompt.
	if strings.Contains(pr.Build(), msg) {
		t.Error("nudge leaked into the system prompt — that is the position this fix moved it out of")
	}
	// Recorded on turn state so the salvage path can re-apply it to its own spec copy.
	if !strings.Contains(st.userNudge, msg) {
		t.Errorf("st.userNudge = %q, want the folded message", st.userNudge)
	}

	// One-shot: later rounds don't re-pull, and don't re-persist.
	replay = c.foldUserNudge(context.Background(), &spec, st, replay) // round 1 (a tool round)
	replay = c.foldUserNudge(context.Background(), &spec, st, replay) // round 2 (the final reply)
	if calls != 1 {
		t.Fatalf("NextNudge pulled %d times, want 1 (one-shot per turn)", calls)
	}
	if len(store.appended) != 1 {
		t.Fatalf("nudge persisted %d times, want 1", len(store.appended))
	}
	if len(replay) != 2 {
		t.Fatalf("replay grew to %d rows; the nudge must be spliced once", len(replay))
	}
}

// TestFoldUserNudge_NilReplay: with no pre-read replay the round reads history fresh,
// so the row we just wrote is picked up by that read — return nil, don't fabricate a
// one-message replay that would REPLACE the real history.
func TestFoldUserNudge_NilReplay(t *testing.T) {
	store := &rubricStore{}
	c := &core{store: store}
	spec := contract.TurnSpec{Sid: "s1", NextNudge: func() *contract.UserMsg {
		return &contract.UserMsg{Text: "再加一句"}
	}}
	if got := c.foldUserNudge(context.Background(), &spec, newTurnState(), nil); got != nil {
		t.Fatalf("nil replay must stay nil, got %+v", got)
	}
	if len(store.appended) != 1 {
		t.Fatalf("the row must still be persisted; appended = %q", store.appended)
	}
}

// TestFoldUserNudge_PersistFailure: a store error must not ALSO lose the message from
// the running turn — the replay splice is what the model reads this round.
func TestFoldUserNudge_PersistFailure(t *testing.T) {
	c := &core{store: &failingAppendStore{}}
	spec := contract.TurnSpec{Sid: "s1", NextNudge: func() *contract.UserMsg {
		return &contract.UserMsg{Text: "顺便看看中国区"}
	}}
	replay := c.foldUserNudge(context.Background(), &spec, newTurnState(), []contract.Message{{Role: "user", Content: "原任务"}})
	if len(replay) != 2 || !strings.Contains(replay[1].Content, "顺便看看中国区") {
		t.Fatalf("a failed persist must still deliver the nudge to this turn; replay = %+v", replay)
	}
}

// failingAppendStore fails the user-row write and nothing else.
type failingAppendStore struct{ rubricStore }

func (s *failingAppendStore) AppendUserMsgs(context.Context, string, []contract.UserMsg) error {
	return errors.New("store down")
}

// TestLoopTopNudge drives the pure-staleness ladder at both watermark
// configurations through one table: the never-progressed rows pin the historical
// first-fire points (the 9/11 calibration), and the progressed-then-stalled rows
// pin that the ladder measures rounds-since-progress, never raw iter (the old
// `iter≥8/10` gates fired give-up at stale 3 there).
func TestLoopTopNudge(t *testing.T) {
	c := &core{}
	cases := []struct {
		name       string
		progressAt int // lastProgressIter; -1 = never progressed (stale = iter+1)
		iter       int
		wantNudge  string // substring, "" = no nudge
		wantExit   bool
	}{
		{"fresh under switch", -1, 7, "", false},      // stale 8 < switchFamilyStale (9)
		{"fresh switch-family", -1, 8, "换个思路", false}, // stale 9
		{"fresh give-up", -1, 10, "禁止再调用工具", false},   // stale 11 (higher tier)
		{"fresh menu", -1, 11, "没有取得新进展", false},      // stale 12
		{"fresh exit", -1, 13, "", true},              // stale 14 → budget dry
		{"late stale 3", 20, 23, "", false},           // old gates fired give-up here
		{"late under switch", 20, 28, "", false},      // stale 8
		{"late switch-family", 20, 29, "换个思路", false}, // stale 9
		{"late give-up", 20, 31, "禁止再调用工具", false},    // stale 11
		{"late menu", 20, 32, "没有取得新进展", false},       // stale 12
		{"late exit", 20, 34, "", true},               // stale 14
	}
	for _, tc := range cases {
		p := sysPrompt("base")
		st := newTurnState()
		st.lastProgressIter = tc.progressAt
		ex := c.loopTopNudge(contract.TurnSpec{Prompt: p}, st, tc.iter)
		if (ex != nil) != tc.wantExit {
			t.Errorf("%s (iter %d): exit=%v, want %v", tc.name, tc.iter, ex != nil, tc.wantExit)
		}
		got := nudgeLayer(p)
		if tc.wantNudge == "" && got != "" {
			t.Errorf("%s (iter %d): expected no nudge, got %q", tc.name, tc.iter, got)
		}
		if tc.wantNudge != "" && !strings.Contains(got, tc.wantNudge) {
			t.Errorf("%s (iter %d): nudge %q missing %q", tc.name, tc.iter, got, tc.wantNudge)
		}
	}
}

// TestLoopTopNudge_ProgressingLongTurnNeverNudged is the pace-invariance proof the
// driver split rests on (docs/turn-driver-split.md §2): a turn that keeps producing
// new tool info trips NOTHING however deep it runs — so a convergence-driven
// looping driver shares this guard family unchanged, no mode knob anywhere.
func TestLoopTopNudge_ProgressingLongTurnNeverNudged(t *testing.T) {
	c := &core{}
	for iter := 0; iter < 200; iter++ {
		p := sysPrompt("base")
		st := newTurnState()
		st.lastProgressIter = iter - 1 // progressed on the previous round
		if ex := c.loopTopNudge(contract.TurnSpec{Prompt: p}, st, iter); ex != nil {
			t.Fatalf("iter %d: progressing turn budget-exited", iter)
		}
		if got := nudgeLayer(p); got != "" {
			t.Fatalf("iter %d: progressing turn nudged: %q", iter, got)
		}
	}
}

// TestSalvageClearsStaleNudge (dev-pg) is the regression for the stale-nudge leak:
// a no-progress turn sets a loop nudge at iter ≥ 8, then exits to salvage — the
// nudge layer must be cleared so it doesn't ride into salvage's clean prompt.
func TestSalvageClearsStaleNudge(t *testing.T) {
	st, done := liveStore(t)
	defer done()
	old := loopCap
	loopCap = 10 // long enough that loopTopNudge sets a nudge at iter 8/9
	defer func() { loopCap = old }()

	ctx := context.Background()
	sid := uscope("turnnudgeclear")
	p := sysPrompt("sys")
	c := &core{store: st, pool: &emptyPool{}} // always empty → no progress → nudged then iter-cap
	c.Run(ctx, contract.TurnSpec{Sid: sid, Prompt: p, UserText: "hi", Sink: &recSink{}})

	if got := nudgeLayer(p); got != "" {
		t.Fatalf("stale nudge leaked into salvage prompt: %q", got)
	}
}

// nudgeLayer reads the "nudge" layer via the concrete builder's Layer accessor
// (Layer is not part of contract.Prompt — a test-only peek).
func nudgeLayer(p contract.Prompt) string {
	return p.(interface{ Layer(string) string }).Layer("nudge")
}

func TestRouteToolRound(t *testing.T) {
	st := newTurnState()
	got := routeToolRound(contract.ModelRequest{Prefer: []string{"x"}}, st)
	if len(got.Prefer) != 2 || got.Prefer[0] != "x" || got.Prefer[1] != "toolcall" {
		t.Errorf("prefer = %v, want [x toolcall] (join, not replace)", got.Prefer)
	}
	// After enough failures since the last progress, smart is joined too.
	st.failedSinceProgress = smartEscalation
	got = routeToolRound(contract.ModelRequest{}, st)
	if len(got.Prefer) != 2 || got.Prefer[0] != "toolcall" || got.Prefer[1] != "smart" {
		t.Errorf("escalated prefer = %v, want [toolcall smart]", got.Prefer)
	}
	// No duplicate when the tag is already present.
	if out := appendPrefer([]string{"toolcall"}, "toolcall"); len(out) != 1 {
		t.Errorf("appendPrefer duplicated a tag: %v", out)
	}
}
