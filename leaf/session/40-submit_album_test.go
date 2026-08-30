package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/session/store"
)

// albumStore is a no-DB store.Store stub for the dispatch-level tests: it counts
// session opens and no-ops the WAL / prefs / mode calls the submit path touches.
type albumStore struct {
	store.Store
	mu    sync.Mutex
	opens int
}

func (s *albumStore) OpenSession(context.Context, string, string, string, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	return fmt.Sprintf("sid-%d", s.opens), nil
}
func (s *albumStore) SetCursor(context.Context, string) error { return nil }
func (s *albumStore) AddPending(context.Context, string, string, string, string, string, map[string]any) error {
	return nil
}
func (s *albumStore) RemovePending(context.Context, string, []string) error { return nil }
func (s *albumStore) GetScopePrefs(context.Context, string) (contract.SinkPrefs, error) {
	return contract.DefaultSinkPrefs(), nil
}
func (s *albumStore) GetSessionMode(context.Context, string) (contract.SessionMode, error) {
	return contract.SessionModeNone, nil
}
func (s *albumStore) SessionByID(context.Context, string) (store.SessionInfo, error) {
	return store.SessionInfo{}, nil
}
func (s *albumStore) openCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.opens }

// gateHandler is an in-package TurnHandler that reports its flow served + delivered (OnMode + OnOutput),
// records the works it ran, and can gate RunTurn to keep a turn in flight.
type gateHandler struct {
	mu      sync.Mutex
	runs    []contract.TurnWork
	started chan struct{} // signalled when a RunTurn begins (buffered)
	gate    chan struct{} // RunTurn blocks on receive until closed (nil = no gate)
}

func (h *gateHandler) RunTurn(_ context.Context, work contract.TurnWork) {
	if work.OnMode != nil {
		work.OnMode(contract.SessionModeNone)
	}
	if work.OnOutput != nil {
		work.OnOutput() // simulate a delivered turn — the drain cleared-ack gates on it (F150)
	}
	if h.started != nil {
		h.started <- struct{}{}
	}
	if h.gate != nil {
		<-h.gate
	}
	h.mu.Lock()
	h.runs = append(h.runs, work)
	h.mu.Unlock()
}
func (h *gateHandler) Blocked(context.Context, string) bool { return false }
func (h *gateHandler) NotePending(context.Context, string, contract.MessageEvent, contract.PendingDisposition) {
}
func (h *gateHandler) DumpPrompt(context.Context, contract.TurnWork) string { return "" }
func (h *gateHandler) FlowInfo(context.Context, contract.TurnWork) (string, contract.SessionMode, bool) {
	return "", "", false
}
func (h *gateHandler) works() []contract.TurnWork {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]contract.TurnWork(nil), h.runs...)
}

// F5: N album siblings each resolving OpenNew must converge on ONE fresh session
// — concurrently (singleflight) and late (TTL map); a different album or a bare
// @=new still opens its own.
func TestOpenNewSid_AlbumSiblingsShareOneSession(t *testing.T) {
	st := &albumStore{}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	ctx := context.Background()
	const scope = "telegram:group:9"
	sib := func(msgID, gid string) contract.MessageEvent {
		return contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup,
			ChatID: "9", UserID: "u", MessageID: msgID, MediaGroupID: gid}
	}

	var wg sync.WaitGroup
	sids := make([]string, 5)
	for i := range sids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid, err := m.openNewSid(ctx, scope, sib(fmt.Sprintf("m%d", i), "g1"))
			if err != nil {
				t.Errorf("openNewSid: %v", err)
			}
			sids[i] = sid
		}(i)
	}
	wg.Wait()
	for i, sid := range sids {
		if sid == "" || sid != sids[0] {
			t.Fatalf("sibling %d got sid %q, want all == %q", i, sid, sids[0])
		}
	}
	if st.openCount() != 1 {
		t.Fatalf("album opened %d sessions, want 1", st.openCount())
	}

	// A late sibling (after the flight completed) joins via the TTL map.
	if sid, err := m.openNewSid(ctx, scope, sib("late", "g1")); err != nil || sid != sids[0] {
		t.Fatalf("late sibling sid = %q (%v), want %q", sid, err, sids[0])
	}
	// A different album gets its own session.
	if sid, _ := m.openNewSid(ctx, scope, sib("x", "g2")); sid == sids[0] {
		t.Fatal("a different album must not reuse another album's session")
	}
	// Bare @=new (no media group) never converges.
	s1, _ := m.openNewSid(ctx, scope, sib("y", ""))
	s2, _ := m.openNewSid(ctx, scope, sib("z", ""))
	if s1 == s2 {
		t.Fatal("bare @=new opens must stay distinct")
	}
	// An expired mapping is not reused. (Key mirrors openNewSid's namespaced album key.)
	m.mu.Lock()
	m.albumSids["album\x00"+scope+"\x00"+"g1"] = albumSidEntry{sid: sids[0], at: time.Now().Add(-albumSidTTL - time.Minute)}
	m.mu.Unlock()
	if sid, _ := m.openNewSid(ctx, scope, sib("stale", "g1")); sid == sids[0] {
		t.Fatal("an expired album mapping must not be reused")
	}
}

// F152(b): an onboarding-pass sender's repeated group @s (each resolving OpenNew, all
// bouncing off intro) converge on ONE session per (scope, sender) instead of a fresh row
// per message — while distinct onboarding senders, and a NON-onboarding @=new, stay apart.
func TestOpenNewSid_OnboardingSenderShareOneSession(t *testing.T) {
	st := &albumStore{}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	ctx := context.Background()
	const scope = "telegram:group:9"
	msg := func(uid, msgID string, onboarding bool) contract.MessageEvent {
		return contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup,
			ChatID: "9", UserID: uid, MessageID: msgID, Onboarding: onboarding}
	}

	// Same onboarding sender, three @s → one session.
	a1, _ := m.openNewSid(ctx, scope, msg("stranger", "m1", true))
	a2, _ := m.openNewSid(ctx, scope, msg("stranger", "m2", true))
	a3, _ := m.openNewSid(ctx, scope, msg("stranger", "m3", true))
	if a1 == "" || a1 != a2 || a2 != a3 {
		t.Fatalf("one onboarding sender must reuse one session, got %q/%q/%q", a1, a2, a3)
	}

	// A DIFFERENT onboarding sender gets their own session.
	if b1, _ := m.openNewSid(ctx, scope, msg("other", "m4", true)); b1 == a1 {
		t.Fatal("distinct onboarding senders must not share a session")
	}

	// A NON-onboarding (approved) sender still @=new — no reuse.
	c1, _ := m.openNewSid(ctx, scope, msg("member", "m5", false))
	c2, _ := m.openNewSid(ctx, scope, msg("member", "m6", false))
	if c1 == c2 {
		t.Fatal("a non-onboarding sender must keep @=new (distinct sessions)")
	}
}

// F5 end-to-end at the submit level: the first sibling runs a turn, the second
// (same album, group @=new path) joins the SAME sid's busy queue and drains as a
// follow-up turn — no session fork, no concurrent turns.
func TestSubmit_AlbumSiblingsQueueOnOneSession(t *testing.T) {
	st := &albumStore{}
	h := &gateHandler{started: make(chan struct{}, 4), gate: make(chan struct{})}
	m := NewManager(st, h, arbSrcByName, Config{MaxBatch: 10})
	ctx := context.Background()
	sib := func(msgID string) contract.MessageEvent {
		return contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup,
			ChatID: "alb", UserID: "u", Text: "看看", MessageID: msgID, MediaGroupID: "g1",
			Attachments: []contract.Attachment{{Kind: "image", FileName: "photo-" + msgID + ".jpg"}}}
	}

	m.Submit(ctx, sib("m1"))
	<-h.started // first sibling's turn is in flight (busy)

	m.Submit(ctx, sib("m2"))
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		queued := 0
		for _, p := range m.pending {
			queued += len(p)
		}
		m.mu.Unlock()
		if queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second sibling never joined the busy queue")
		}
		time.Sleep(2 * time.Millisecond)
	}

	close(h.gate) // release turn 1 → the queued sibling drains
	<-h.started
	m.Wait()

	if st.openCount() != 1 {
		t.Fatalf("album siblings opened %d sessions, want 1", st.openCount())
	}
	runs := h.works()
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2 (first sibling + drained follow-up)", len(runs))
	}
	if runs[0].Sid != runs[1].Sid {
		t.Fatalf("siblings ran on different sids: %q vs %q", runs[0].Sid, runs[1].Sid)
	}
	if runs[1].Events[0].MessageID != "m2" {
		t.Fatalf("drained turn served %q, want m2", runs[1].Events[0].MessageID)
	}
}
