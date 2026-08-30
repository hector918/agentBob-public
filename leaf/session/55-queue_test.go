package session

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/session/store"
)

func TestQueueContent(t *testing.T) {
	cases := []struct {
		name string
		ev   contract.MessageEvent
		want string
	}{
		{"plain caption", contract.MessageEvent{Text: " hi "}, "hi"},
		{"empty", contract.MessageEvent{Text: "   "}, ""},
		{"stand-in counts as empty", contract.MessageEvent{Text: NoCaptionStandIn}, ""},
		{"caption + filenames", contract.MessageEvent{
			Text:        "look",
			Attachments: []contract.Attachment{{FileName: "a.jpg"}, {FileName: "b.jpg"}},
		}, "look [a.jpg, b.jpg]"},
		{"caption + nameless attachment", contract.MessageEvent{
			Text:        "look",
			Attachments: []contract.Attachment{{Kind: "sticker"}},
		}, "look"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := queueContent(c.ev); got != c.want {
				t.Fatalf("queueContent = %q, want %q", got, c.want)
			}
		})
	}
}

func TestNudgeEligible(t *testing.T) {
	cases := []struct {
		name string
		ev   contract.MessageEvent
		want bool
	}{
		{"plain text", contract.MessageEvent{Text: "go"}, true},
		{"empty text", contract.MessageEvent{Text: "  "}, false},
		{"stand-in", contract.MessageEvent{Text: NoCaptionStandIn}, false},
		{"has attachment", contract.MessageEvent{Text: "look", Attachments: []contract.Attachment{{Kind: "image"}}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nudgeEligible(c.ev); got != c.want {
				t.Fatalf("nudgeEligible = %v, want %v", got, c.want)
			}
		})
	}
}

func TestQueueDedup(t *testing.T) {
	m := newQueueTestManager()
	m.pending["s"] = []contract.MessageEvent{{Text: "hello", MessageID: "m1"}}
	if !m.queueDedup("s", contract.MessageEvent{Text: "hello", MessageID: "m2"}) {
		t.Fatal("expected dedup hit for identical plain-text content")
	}
	if m.queueDedup("s", contract.MessageEvent{Text: "world", MessageID: "m3"}) {
		t.Fatal("unexpected dedup for different content")
	}
	if m.queueDedup("s", contract.MessageEvent{Text: "  "}) {
		t.Fatal("empty content must never dedup")
	}
}

// F119: a captioned album's non-photo siblings share one caption AND a static
// fallback filename ("video.mp4"), so content equality alone must NOT dedup a
// genuinely different file — only a true redelivery (same message id) dedups.
func TestQueueDedup_AlbumSiblingsNotSwallowed(t *testing.T) {
	m := newQueueTestManager()
	video := func(msgID string) contract.MessageEvent {
		return contract.MessageEvent{
			Text:         "看看这些",
			MessageID:    msgID,
			MediaGroupID: "g1",
			Attachments:  []contract.Attachment{{Kind: "video", FileName: "video.mp4"}},
		}
	}
	m.pending["s"] = []contract.MessageEvent{video("m2")}
	if m.queueDedup("s", video("m3")) {
		t.Fatal("distinct album sibling (same caption + fallback filename, different msg id) must NOT dedup")
	}
	if !m.queueDedup("s", video("m2")) {
		t.Fatal("true redelivery (same message id) must dedup")
	}
	// attachment-bearing vs queued plain text with the same caption: not a dup.
	m.pending["t"] = []contract.MessageEvent{{Text: "看看这些", MessageID: "p1"}}
	if m.queueDedup("t", video("m9")) {
		t.Fatal("attachment event must not dedup against a plain text with the same caption")
	}
}

// nudgeTextOf reads a popped nudge's text, "" when the lane was empty — keeps the
// assertions below about the QUEUE mechanics, not about nil-checking.
func nudgeTextOf(um *contract.UserMsg) string {
	if um == nil {
		return ""
	}
	return um.Text
}

func TestTakeNudge(t *testing.T) {
	m := newQueueTestManager()
	m.sidMeta["s"] = sidMetaRow{Scope: "telegram:dm:1"}
	// Queue: an image (not eligible), then two plain texts.
	m.pending["s"] = []contract.MessageEvent{
		{Text: "look", MessageID: "m0", Attachments: []contract.Attachment{{Kind: "image"}}},
		{Text: "first", MessageID: "m1"},
		{Text: "second", MessageID: "m2"},
	}

	if got := nudgeTextOf(m.takeNudge("s", "telegram:dm:1", nil)); got != "first" {
		t.Fatalf("takeNudge #1 = %q, want %q", got, "first")
	}
	// "first" removed; the image and "second" remain, order preserved.
	if len(m.pending["s"]) != 2 || m.pending["s"][0].MessageID != "m0" || m.pending["s"][1].MessageID != "m2" {
		t.Fatalf("pending after #1 = %+v", m.pending["s"])
	}
	if got := nudgeTextOf(m.takeNudge("s", "telegram:dm:1", nil)); got != "second" {
		t.Fatalf("takeNudge #2 = %q, want %q", got, "second")
	}
	// Only the image (ineligible) remains → no more nudges.
	if got := m.takeNudge("s", "telegram:dm:1", nil); got != nil {
		t.Fatalf("takeNudge #3 = %+v, want nil (only ineligible image left)", got)
	}
	if len(m.pending["s"]) != 1 || m.pending["s"][0].MessageID != "m0" {
		t.Fatalf("pending after drain = %+v", m.pending["s"])
	}
}

// TestTakeNudge_Attribution: a nudge now comes back as a UserMsg carrying its AUTHOR,
// and its text is rendered the way the drain path renders content — the quoted-reply
// line in front (joinEvents), in every scope.
//
// It must NOT bake the "[名字]: " speaker prefix into the text: the turn persists this
// as a real user row, so renderSpeakers (leaf/turn/10-core.go) attributes it from
// Author at build time, including the "<2 distinct speakers → no prefix" rule a baked-in
// prefix could not honour. Baking it in as well would render "[Bob]: [Bob]: …" (F143's
// fix was only needed while the nudge bypassed renderSpeakers via the system prompt).
func TestTakeNudge_Attribution(t *testing.T) {
	m := newQueueTestManager()
	ev := contract.MessageEvent{Text: "别忘了带伞", MessageID: "m1", UserID: "u9", UserName: "Bob",
		ReplyToUser: "Alice", ReplyToText: "明天下雨吗"}
	want := "[In reply to Alice: \"明天下雨吗\"]\n别忘了带伞"

	for _, scope := range []string{"telegram:group:42", "telegram:dm:1"} {
		m.pending["s"] = []contract.MessageEvent{ev}
		got := m.takeNudge("s", scope, nil)
		if got == nil {
			t.Fatalf("%s: takeNudge returned nil", scope)
		}
		if got.Text != want {
			t.Errorf("%s: nudge text = %q, want %q", scope, got.Text, want)
		}
		if strings.Contains(got.Text, "[Bob]:") {
			t.Errorf("%s: speaker prefix must not be baked in — renderSpeakers adds it: %q", scope, got.Text)
		}
		if got.Author != (contract.MsgAuthor{Kind: "human", ID: "u9", Name: "Bob"}) {
			t.Errorf("%s: author = %+v, want the human sender", scope, got.Author)
		}
	}
}

// TestTakeNudge_SkipsPreTurn verifies the nudge fast-lane pulls only a message that
// arrived AFTER the turn started (the preTurn snapshot) — pre-queued backlog stays for
// the drain, so a text waiting behind an unprocessed image isn't yanked forward (F146).
func TestTakeNudge_SkipsPreTurn(t *testing.T) {
	m := newQueueTestManager()
	m.pending["s"] = []contract.MessageEvent{
		{Text: "old backlog", MessageID: "old"},
		{Text: "mid-turn", MessageID: "new"},
	}
	preTurn := map[string]bool{"old": true} // "old" was already queued when the turn began
	if got := nudgeTextOf(m.takeNudge("s", "telegram:dm:1", preTurn)); got != "mid-turn" {
		t.Fatalf("takeNudge = %q, want the post-turn arrival 'mid-turn'", got)
	}
	// The pre-turn message stays queued for the in-order drain.
	if len(m.pending["s"]) != 1 || m.pending["s"][0].MessageID != "old" {
		t.Fatalf("pre-turn backlog must remain for drain; pending = %+v", m.pending["s"])
	}
	// Nothing new left → the fast-lane is empty (the pre-turn message never nudges).
	if got := m.takeNudge("s", "telegram:dm:1", preTurn); got != nil {
		t.Fatalf("takeNudge #2 = %+v, want nil (only pre-turn backlog left)", got)
	}
}

// newQueueTestManager builds a Manager backed by a no-op store (only RemovePending
// is exercised) for the in-package queue-logic tests — no DB needed.
func newQueueTestManager() *Manager {
	return NewManager(&nudgeStubStore{}, stubTurnHandler{}, func(string) contract.Source { return nil }, Config{})
}

// nudgeStubStore embeds store.Store so unused methods are nil; it records the ids
// passed to RemovePending so a test can assert WHEN the WAL row is dropped.
type nudgeStubStore struct {
	store.Store
	removed [][]string
}

func (s *nudgeStubStore) RemovePending(_ context.Context, _ string, ids []string) error {
	s.removed = append(s.removed, ids)
	return nil
}

// AddPending / GetScopePrefs / SessionByID / SetSessionPreferModelTags no-ops let
// runTurnAndDrain run end-to-end on the stub (the turn build reads the session's
// sticky model tags every turn).
func (s *nudgeStubStore) AddPending(context.Context, string, string, string, string, string, map[string]any) error {
	return nil
}

func (s *nudgeStubStore) GetScopePrefs(context.Context, string) (contract.SinkPrefs, error) {
	return contract.DefaultSinkPrefs(), nil
}

func (s *nudgeStubStore) SessionByID(context.Context, string) (store.SessionInfo, error) {
	return store.SessionInfo{}, nil
}

func (s *nudgeStubStore) SetSessionPreferModelTags(context.Context, string, []string) error {
	return nil
}

// takeNudge must DEFER the WAL-row removal (record the id, don't drop it) so a
// mid-turn crash leaves the row for recovery; removeServedPending drops it only
// on turn success.
func TestTakeNudge_DefersWALRemoval(t *testing.T) {
	st := &nudgeStubStore{}
	m := NewManager(st, stubTurnHandler{}, func(string) contract.Source { return nil }, Config{})
	m.sidMeta["s"] = sidMetaRow{Scope: "telegram:dm:1"}
	m.pending["s"] = []contract.MessageEvent{{Text: "hi", MessageID: "m1"}}

	if got := nudgeTextOf(m.takeNudge("s", "telegram:dm:1", nil)); got != "hi" {
		t.Fatalf("takeNudge = %q, want %q", got, "hi")
	}
	if len(st.removed) != 0 {
		t.Fatalf("takeNudge must NOT drop the WAL row yet; RemovePending called %d times", len(st.removed))
	}
	if ids := m.nudged["s"]; len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("nudged[s] = %v, want [m1]", m.nudged["s"])
	}
	// Turn success → the deferred row is removed now (with the served batch).
	m.removeServedPending("telegram:dm:1", "s", nil)
	if len(st.removed) != 1 || len(st.removed[0]) != 1 || st.removed[0][0] != "m1" {
		t.Fatalf("removeServedPending should drop the nudged id m1, got %v", st.removed)
	}
	if len(m.nudged["s"]) != 0 {
		t.Fatalf("nudged[s] must be cleared after removal, got %v", m.nudged["s"])
	}
}

// REGRESSION (B14): a NORMALLY completed turn must drain its ingress-WAL rows.
// runTurnAndDrain releases the child ctx itself after RunTurn returns — the abort
// verdict must be captured BEFORE that cancel, or every successful turn reads as
// aborted: removeServedPending never runs and the WAL rows + nudged ids accumulate.
func TestRunTurnAndDrain_NormalTurnDrainsWAL(t *testing.T) {
	st := &nudgeStubStore{}
	m := NewManager(st, stubTurnHandler{}, func(string) contract.Source { return arbSrc{} }, Config{})
	const sid, scope = "s", "telegram:dm:1"
	m.mu.Lock()
	m.turnSeq++
	turn := m.turnSeq
	m.busy[sid] = true
	m.sidMeta[sid] = sidMetaRow{Scope: scope, turn: turn}
	m.mu.Unlock()
	batch := []contract.MessageEvent{{Source: "telegram", ChatID: "1", MessageID: "m1", Text: "hi"}}
	m.runTurnAndDrain(context.Background(), arbSrc{}, scope, sid, batch, false, turn, false)
	if len(st.removed) != 1 || len(st.removed[0]) != 1 || st.removed[0][0] != "m1" {
		t.Fatalf("normal completed turn must clear its WAL rows; RemovePending calls = %v", st.removed)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy[sid] {
		t.Fatal("busy must be released after the drain")
	}
}
