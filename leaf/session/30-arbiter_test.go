package session

import (
	"context"
	"sync"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/session/store"
)

// returnStore embeds store.Store (unused methods nil) and serves SessionByID from
// a fixed map, counting reads so a test can assert the per-sid cache.
type returnStore struct {
	store.Store
	rows  map[string]store.SessionInfo
	reads int
}

func (s *returnStore) SessionByID(_ context.Context, id string) (store.SessionInfo, error) {
	s.reads++
	return s.rows[id], nil
}

// arbSrc is a minimal contract.Source for the targetFor tests (replyTarget needs
// Caps()). NewSink etc. are never called here.
type arbSrc struct{}

func (arbSrc) Name() string                                            { return "telegram" }
func (arbSrc) Caps() contract.Caps                                     { return contract.Caps{} }
func (arbSrc) HealthCheck(context.Context) error                       { return nil }
func (arbSrc) Run(context.Context, chan<- contract.MessageEvent) error { return nil }
func (arbSrc) NewSink(context.Context, contract.Target, string, string, contract.SinkPrefs) contract.Sink {
	return nil
}
func (arbSrc) SendButtons(context.Context, contract.Target, string, []contract.Button) (string, error) {
	return "", nil
}
func (arbSrc) Send(context.Context, contract.Target, string) error             { return nil }
func (arbSrc) SendFile(context.Context, contract.Target, string, string) error { return nil }

func arbSrcByName(string) contract.Source { return arbSrc{} }

// A REAL chat event threads its reply to the chat (replyTarget) — no store read.
func TestTargetFor_RealChatEventThreadsReply(t *testing.T) {
	st := &returnStore{rows: map[string]store.SessionInfo{}}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	ev := contract.MessageEvent{Source: "telegram", ChatID: "9"}
	got := m.targetFor(context.Background(), "sid_X", ev, arbSrc{})
	if got.Source != "telegram" || got.ChatID != "9" {
		t.Fatalf("target = %+v, want native reply target {telegram, 9}", got)
	}
	if st.reads != 0 {
		t.Errorf("real-chat turn read the store %d times, want 0 (no归宿 lookup)", st.reads)
	}
}

// An INTERNAL wake of a worker (has a caller) returns the reply to the caller's
// scope through the internal bridge — the agent→agent round-trip.
func TestTargetFor_InternalWakeWorkerReturnsToCaller(t *testing.T) {
	st := &returnStore{rows: map[string]store.SessionInfo{
		"sid_B": {ID: "sid_B", Key: "co:acme/bob", CallerID: "sid_A"},
		"sid_A": {ID: "sid_A", Key: "telegram:dm:1"},
	}}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	ev := contract.MessageEvent{Source: contract.SourceNameInternal, ChatID: "co:acme/bob"}
	got := m.targetFor(context.Background(), "sid_B", ev, arbSrc{})
	if got.Source != contract.SourceNameInternal || got.ChatID != "telegram:dm:1" {
		t.Fatalf("target = %+v, want {internal, telegram:dm:1}", got)
	}
}

// REGRESSION (self-loop fix): an INTERNAL wake of a SOURCE-bound session (no
// caller — e.g. a top-level session resumed by a worker's return) must reply to
// its OWN source chat (reconstructed from its scope), NOT bounce back to itself.
func TestTargetFor_InternalWakeSourceSessionRepliesToItsChat(t *testing.T) {
	st := &returnStore{rows: map[string]store.SessionInfo{
		"sid_A": {ID: "sid_A", Key: "telegram:dm:1", Source: "telegram"}, // CallerID == ""
	}}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	ev := contract.MessageEvent{Source: contract.SourceNameInternal, ChatID: "telegram:dm:1"}
	got := m.targetFor(context.Background(), "sid_A", ev, arbSrc{})
	if got.Source != "telegram" || got.ChatID != "1" {
		t.Fatalf("target = %+v, want {telegram, 1} (own chat, NOT a self-addressed internal loop)", got)
	}
}

// targetFromScope: DM keeps its "#thread" grammar; a virtual-group "#member"
// sub-scope is a routing artifact and must NOT leak into Target.ThreadID (B25).
func TestTargetFromScope_GrammarSplit(t *testing.T) {
	cases := []struct {
		scope      string
		wantChat   string
		wantThread string
		wantOK     bool
	}{
		{"telegram:dm:1", "1", "", true},
		{"telegram:dm:1#42", "1", "42", true}, // DM "#thread" grammar kept
		{"telegram:group:9", "9", "", true},
		{"telegram:group:9#alice", "9", "", true}, // member suffix dropped, NOT a thread
		{"telegram:inbox:co", "", "", false},      // not a native chat scope
	}
	for _, c := range cases {
		got, ok := targetFromScope("telegram", c.scope)
		if ok != c.wantOK || got.ChatID != c.wantChat || got.ThreadID != c.wantThread {
			t.Errorf("targetFromScope(%q) = %+v,%v; want chat=%q thread=%q ok=%v",
				c.scope, got, ok, c.wantChat, c.wantThread, c.wantOK)
		}
	}
}

// peekScope carries the virtual-group member sub-scope on a reply-hit (the joined
// session's own Key) so a slash command targets the member's scope, not the bare
// group; a non-group event passes the base scope through unchanged.
func TestPeekScope_ReplyHitMemberSubScope(t *testing.T) {
	st := &returnStore{rows: map[string]store.SessionInfo{
		"sid_alice": {ID: "sid_alice", Key: "telegram:group:9#alice"},
	}}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	grp := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, ChatID: "9"}
	if got := m.peekScope(context.Background(), Resolution{SessionID: "sid_alice", Key: "telegram:group:9"}, grp); got != "telegram:group:9#alice" {
		t.Errorf("peekScope reply-hit = %q, want telegram:group:9#alice", got)
	}
	dm := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, ChatID: "1"}
	if got := m.peekScope(context.Background(), Resolution{Key: "telegram:dm:1"}, dm); got != "telegram:dm:1" {
		t.Errorf("peekScope DM = %q, want telegram:dm:1 (no member routing)", got)
	}
}

// recSrc is a recording contract.Source: it captures Send calls and reports the
// configured Caps (the F59 drain-source test needs a ReplyTo-capable source next
// to a no-op bridge).
type recSrc struct {
	name  string
	caps  contract.Caps
	mu    sync.Mutex
	sends []string
}

func (s *recSrc) Name() string                                            { return s.name }
func (s *recSrc) Caps() contract.Caps                                     { return s.caps }
func (s *recSrc) HealthCheck(context.Context) error                       { return nil }
func (s *recSrc) Run(context.Context, chan<- contract.MessageEvent) error { return nil }
func (s *recSrc) NewSink(context.Context, contract.Target, string, string, contract.SinkPrefs) contract.Sink {
	return nil
}
func (s *recSrc) SendButtons(context.Context, contract.Target, string, []contract.Button) (string, error) {
	return "", nil
}
func (s *recSrc) Send(_ context.Context, _ contract.Target, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends = append(s.sends, text)
	return nil
}
func (s *recSrc) SendFile(context.Context, contract.Target, string, string) error { return nil }
func (s *recSrc) sendCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sends)
}

// F59: a drain batch queued behind an INTERNAL-woken turn must speak through ITS
// OWN source — the reply anchor (ReplyToID via the source's Caps) and the
// cleared-ack must use the humans' source, not the bridge the first turn was
// dispatched with (bridge Caps.ReplyTo=false + Send no-op would drop both).
func TestRunTurnAndDrain_DrainBatchUsesItsOwnSource(t *testing.T) {
	tg := &recSrc{name: "telegram", caps: contract.Caps{ReplyTo: true}}
	bridge := &recSrc{name: contract.SourceNameInternal}
	byName := func(n string) contract.Source {
		switch n {
		case "telegram":
			return tg
		case contract.SourceNameInternal:
			return bridge
		}
		return nil
	}
	st := &albumStore{}
	h := &gateHandler{}
	m := NewManager(st, h, byName, Config{})
	const sid, scope = "s1", "telegram:group:9"
	m.mu.Lock()
	m.turnSeq++
	turn := m.turnSeq
	m.busy[sid] = true
	m.sidMeta[sid] = sidMetaRow{Scope: scope, turn: turn}
	m.mu.Unlock()
	// Two human messages that queued during an internal-woken turn: the drain is
	// dispatched with the bridge src (what runTurnAndDrain historically reused).
	batch := []contract.MessageEvent{
		{Source: "telegram", ChatType: contract.ChatGroup, ChatID: "9", UserID: "u", MessageID: "h1", Text: "hi"},
		{Source: "telegram", ChatType: contract.ChatGroup, ChatID: "9", UserID: "u", MessageID: "h2", Text: "again"},
	}
	m.runTurnAndDrain(context.Background(), bridge, scope, sid, batch, false, turn, true)

	runs := h.works()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if got := runs[0].Target; got.Source != "telegram" || got.ReplyToID != "h1" {
		t.Fatalf("drain target = %+v, want telegram-anchored (ReplyToID=h1)", got)
	}
	if tg.sendCount() != 1 {
		t.Fatalf("cleared-ack must go out the batch's own source; telegram sends = %d, want 1", tg.sendCount())
	}
	if bridge.sendCount() != 0 {
		t.Fatalf("cleared-ack leaked to the internal bridge (%d sends)", bridge.sendCount())
	}
}

// TestPickNextDrainBatch_SplitsBySender verifies a text drain batch carries a SINGLE
// sender — a group's shared sid queues several people, and routing the whole batch by
// its anchor would mis-handle the non-anchor (member swallowed into intro / stranger
// billed to the member), so each sender drains in its own turn (F149).
func TestPickNextDrainBatch_SplitsBySender(t *testing.T) {
	ev := func(src, uid, id string) contract.MessageEvent {
		return contract.MessageEvent{Source: src, UserID: uid, MessageID: id, Text: id}
	}
	// A(a1) + B(b1) + A(a2): anchor A's events batch together (in order), B waits.
	next, remaining, _ := pickNextDrainBatch([]contract.MessageEvent{
		ev("telegram", "A", "a1"), ev("telegram", "B", "b1"), ev("telegram", "A", "a2"),
	}, false)
	if got := drainIDs(next); len(got) != 2 || got[0] != "a1" || got[1] != "a2" {
		t.Fatalf("next = %v, want [a1 a2] (single sender A)", got)
	}
	if got := drainIDs(remaining); len(got) != 1 || got[0] != "b1" {
		t.Fatalf("remaining = %v, want [b1]", got)
	}
	// An internal event (different Source) splits away from a human.
	next2, rem2, _ := pickNextDrainBatch([]contract.MessageEvent{
		ev(contract.SourceNameInternal, "", "i1"), ev("telegram", "A", "a1"),
	}, false)
	if got := drainIDs(next2); len(got) != 1 || got[0] != "i1" {
		t.Fatalf("internal batch = %v, want [i1]", got)
	}
	if got := drainIDs(rem2); len(got) != 1 || got[0] != "a1" {
		t.Fatalf("internal remaining = %v, want [a1]", got)
	}
}

func drainIDs(evs []contract.MessageEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.MessageID
	}
	return out
}

// panicRecordHandler records each RunTurn's work, then panics — driving the
// recoverTurn redispatch chain.
type panicRecordHandler struct{ gateHandler }

func (h *panicRecordHandler) RunTurn(_ context.Context, work contract.TurnWork) {
	h.mu.Lock()
	h.runs = append(h.runs, work)
	h.mu.Unlock()
	panic("boom")
}

// F93: when a panicked turn's queue redispatches in SPLIT batches (media-group /
// image drain), pickNextDrainBatch defers the drops flag to the LAST batch — the
// recover path must re-latch m.dropped for the follow-ups (like the normal drain
// does) instead of letting the flag vanish with its unconditional delete.
func TestRecoverTurn_SplitBatchKeepsDropsFlag(t *testing.T) {
	st := &albumStore{}
	h := &panicRecordHandler{}
	m := NewManager(st, h, arbSrcByName, Config{})
	const sid, scope = "s1", "telegram:group:9"
	img := func(id string) contract.MessageEvent {
		return contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup,
			ChatID: "9", UserID: "u", MessageID: id, Text: "看", MediaGroupID: "g",
			Attachments: []contract.Attachment{{Kind: "image", FileName: id + ".jpg"}}}
	}
	m.mu.Lock()
	m.turnSeq++
	turn := m.turnSeq
	m.busy[sid] = true
	m.sidMeta[sid] = sidMetaRow{Scope: scope, turn: turn}
	m.pending[sid] = []contract.MessageEvent{img("i1"), img("i2")} // splits one-per-turn
	m.dropped[sid] = true                                          // the queue overflowed during this turn
	m.mu.Unlock()

	first := []contract.MessageEvent{{Source: "telegram", ChatType: contract.ChatGroup,
		ChatID: "9", UserID: "u", MessageID: "h0", Text: "hi"}}
	m.runTurnAndDrain(context.Background(), arbSrc{}, scope, sid, first, false, turn, false)
	m.Wait() // the panic-redispatch chain runs each pending event once

	runs := h.works()
	if len(runs) != 3 {
		t.Fatalf("runs = %d, want 3 (first + two redispatched split batches)", len(runs))
	}
	if runs[1].HadDrops {
		t.Fatal("split batch must DEFER the drops flag, not announce early")
	}
	if !runs[2].HadDrops {
		t.Fatal("the LAST split batch lost the drops flag — the deferred announcement vanished")
	}
}

// REGRESSION (self-loop fix): an INTERNAL wake with no resolvable归宿 (worker whose
// caller scope is gone) DROPS — Target{} so the flow's nil-source guard renders
// nothing — rather than self-addressing.
func TestTargetFor_InternalWakeUnresolvableDrops(t *testing.T) {
	st := &returnStore{rows: map[string]store.SessionInfo{
		"sid_B": {ID: "sid_B", Key: "co:acme/bob", CallerID: "sid_gone"}, // caller missing → Key ""
	}}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	ev := contract.MessageEvent{Source: contract.SourceNameInternal, ChatID: "co:acme/bob"}
	got := m.targetFor(context.Background(), "sid_B", ev, arbSrc{})
	if got.Source != "" {
		t.Fatalf("target = %+v, want Target{} (drop, no self-loop)", got)
	}
}

// stickyStore lets runTurnAndDrain run end-to-end (WAL / prefs no-ops, like
// nudgeStubStore) while serving SessionByID from live rows and applying
// SetSessionPreferModelTags to them — so a test can watch the sticky /uncensored
// tags get stamped and re-read across turns.
type stickyStore struct {
	store.Store
	mu   sync.Mutex
	rows map[string]store.SessionInfo
}

func (s *stickyStore) SessionByID(_ context.Context, id string) (store.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[id], nil
}

func (s *stickyStore) SetSessionPreferModelTags(_ context.Context, id string, tags []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	si := s.rows[id]
	si.PreferModelTags = tags
	s.rows[id] = si
	return nil
}

func (s *stickyStore) AddPending(context.Context, string, string, string, string, string, map[string]any) error {
	return nil
}

func (s *stickyStore) GetScopePrefs(context.Context, string) (contract.SinkPrefs, error) {
	return contract.DefaultSinkPrefs(), nil
}

func (s *stickyStore) RemovePending(context.Context, string, []string) error { return nil }

// captureTurnHandler records the TurnWork each turn ran with (stubTurnHandler
// supplies the rest of the TurnHandler surface).
type captureTurnHandler struct {
	stubTurnHandler
	mu   sync.Mutex
	runs []contract.TurnWork
}

func (h *captureTurnHandler) RunTurn(_ context.Context, work contract.TurnWork) {
	h.mu.Lock()
	h.runs = append(h.runs, work)
	h.mu.Unlock()
}

// takeArmedPrefer must be consumed ONLY by a turn carrying a human event from the
// arming user: a bystander's turn (or an internal wake echoing the issuer's id)
// leaves the arm pending, so a stranger's session is never stamped by someone
// else's /uncensored.
func TestTakeArmedPrefer_IssuerOnly(t *testing.T) {
	m := NewManager(&returnStore{rows: map[string]store.SessionInfo{}}, stubTurnHandler{}, arbSrcByName, Config{})
	const scope = "telegram:group:-77"
	m.mu.Lock()
	m.armedPrefer[armKey{scope: scope, userID: "alice"}] = []string{"uncensored"}
	m.mu.Unlock()

	if got := m.takeArmedPrefer(scope, []contract.MessageEvent{{Source: "telegram", UserID: "bob"}}); got != nil {
		t.Fatalf("bystander turn consumed the arm: %v", got)
	}
	if got := m.takeArmedPrefer(scope, []contract.MessageEvent{{Source: contract.SourceNameInternal, UserID: "alice"}}); got != nil {
		t.Fatalf("internal event consumed the arm: %v", got)
	}
	m.mu.Lock()
	_, stillArmed := m.armedPrefer[armKey{scope: scope, userID: "alice"}]
	m.mu.Unlock()
	if !stillArmed {
		t.Fatal("arm must survive non-issuer turns")
	}
	got := m.takeArmedPrefer(scope, []contract.MessageEvent{{Source: "telegram", UserID: "bob"}, {Source: "telegram", UserID: "alice"}})
	if len(got) != 1 || got[0] != "uncensored" {
		t.Fatalf("issuer turn got %v, want [uncensored]", got)
	}
	m.mu.Lock()
	_, stillArmed = m.armedPrefer[armKey{scope: scope, userID: "alice"}]
	m.mu.Unlock()
	if stillArmed {
		t.Fatal("arm must be consumed by the issuer's turn")
	}
}

// The sticky lifecycle across turns: the issuer's turn consumes the arm, STAMPS
// the session (persisted), and later turns on the session — with no arm pending —
// re-read the stamped tags into TurnWork.PreferredModelTags.
func TestRunTurnAndDrain_StickyPreferTags(t *testing.T) {
	st := &stickyStore{rows: map[string]store.SessionInfo{}}
	h := &captureTurnHandler{}
	m := NewManager(st, h, func(string) contract.Source { return arbSrc{} }, Config{})
	const sid, scope = "s_sticky", "telegram:group:-77"
	m.mu.Lock()
	m.armedPrefer[armKey{scope: scope, userID: "alice"}] = []string{"uncensored"}
	m.mu.Unlock()

	runTurn := func(userID string) {
		m.mu.Lock()
		m.turnSeq++
		turn := m.turnSeq
		m.busy[sid] = true
		m.sidMeta[sid] = sidMetaRow{Scope: scope, turn: turn}
		m.mu.Unlock()
		batch := []contract.MessageEvent{{Source: "telegram", ChatID: "-77", MessageID: "m" + userID, UserID: userID, Text: "hi"}}
		m.runTurnAndDrain(context.Background(), arbSrc{}, scope, sid, batch, false, turn, false)
	}

	// Turn 1: the issuer — consumes the arm, stamps the session.
	runTurn("alice")
	h.mu.Lock()
	got1 := h.runs[0].PreferredModelTags
	h.mu.Unlock()
	if len(got1) != 1 || got1[0] != "uncensored" {
		t.Fatalf("issuer turn PreferredModelTags = %v, want [uncensored]", got1)
	}
	st.mu.Lock()
	stamped := st.rows[sid].PreferModelTags
	st.mu.Unlock()
	if len(stamped) != 1 || stamped[0] != "uncensored" {
		t.Fatalf("session not stamped: rows[%s].PreferModelTags = %v", sid, stamped)
	}

	// Turn 2: no arm pending — the persisted tags must carry the preference.
	runTurn("alice")
	h.mu.Lock()
	got2 := h.runs[1].PreferredModelTags
	h.mu.Unlock()
	if len(got2) != 1 || got2[0] != "uncensored" {
		t.Fatalf("later turn PreferredModelTags = %v, want [uncensored] (sticky)", got2)
	}
}

// A consumed arm MERGES into the session's existing stamp instead of replacing it.
// The arm carries only the delta (80-slash.go::toggleSessionTag), and the merge runs
// against the session the turn actually lands on — so arming a second toggle can
// never drop the first, and a set computed at /command time against a since-replaced
// session can never be stamped onto this one.
func TestRunTurnAndDrain_ArmedTagsMergeIntoStamp(t *testing.T) {
	st := &stickyStore{rows: map[string]store.SessionInfo{}}
	h := &captureTurnHandler{}
	m := NewManager(st, h, func(string) contract.Source { return arbSrc{} }, Config{})
	const sid, scope = "s_merge", "telegram:group:-77"

	// The session already carries one sticky tag from an earlier toggle.
	st.mu.Lock()
	st.rows[sid] = store.SessionInfo{ID: sid, PreferModelTags: []string{"uncensored"}}
	st.mu.Unlock()

	m.mu.Lock()
	m.armedPrefer[armKey{scope: scope, userID: "alice"}] = []string{"thinking"}
	m.turnSeq++
	turn := m.turnSeq
	m.busy[sid] = true
	m.sidMeta[sid] = sidMetaRow{Scope: scope, turn: turn}
	m.mu.Unlock()

	batch := []contract.MessageEvent{{Source: "telegram", ChatID: "-77", MessageID: "m1", UserID: "alice", Text: "hi"}}
	m.runTurnAndDrain(context.Background(), arbSrc{}, scope, sid, batch, false, turn, false)

	h.mu.Lock()
	got := h.runs[0].PreferredModelTags
	h.mu.Unlock()
	if len(got) != 2 || got[0] != "uncensored" || got[1] != "thinking" {
		t.Fatalf("PreferredModelTags = %v, want [uncensored thinking] (merge, not replace)", got)
	}
	st.mu.Lock()
	stamped := st.rows[sid].PreferModelTags
	st.mu.Unlock()
	if len(stamped) != 2 || stamped[0] != "uncensored" || stamped[1] != "thinking" {
		t.Fatalf("persisted stamp = %v, want [uncensored thinking]", stamped)
	}
}
