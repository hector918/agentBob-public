package session

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/i18n"
	"agentbob/leaf/session/store"
)

// captureSink records the Finish text so a slash test can assert the user-visible reply.
type captureSink struct{ finished string }

func (*captureSink) ContentDelta(string)           {}
func (*captureSink) TraceDelta(string)             {}
func (s *captureSink) Finish(full string) error    { s.finished = full; return nil }
func (*captureSink) SendFile(string, string) error { return nil }
func (*captureSink) LastSent() string              { return "" }

// TestCloseSession_BeforeCancelRegistered locks the race fix: a /close-session that
// lands AFTER submit set busy but BEFORE runTurnAndDrain registered the per-sid
// cancel (the lock-free recordPending window) must still take effect — record the
// close intent (closing[sid], honored at cancel-registration time) and report the
// running turn as closed, NOT as "nothing running" (which silently lost the abort).
func TestCloseSession_BeforeCancelRegistered(t *testing.T) {
	m := NewManager(&returnStore{rows: map[string]store.SessionInfo{}}, stubTurnHandler{}, arbSrcByName, Config{})
	ctx := context.Background()
	ev := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, ChatID: "c1", UserID: "u"}
	scope := m.ResolveSession(ctx, ev).Key

	// Simulate the window: busy is set (submit) but no cancel is registered yet.
	m.mu.Lock()
	m.busy["sid1"] = true
	m.sidMeta["sid1"] = sidMetaRow{Scope: scope}
	m.mu.Unlock()

	sink := &captureSink{}
	if err := m.slashCloseSession(ctx, contract.SlashContext{Event: ev, Sink: sink, Lang: "en"}); err != nil {
		t.Fatalf("slashCloseSession: %v", err)
	}

	m.mu.Lock()
	closing := m.closing["sid1"]
	m.mu.Unlock()
	if !closing {
		t.Fatal("closing[sid] not set — a /close-session in the pre-registration window was lost (turn would run uncancelled)")
	}
	if got := sink.finished; got == i18n.T("slash.close.none", "en") {
		t.Fatalf("reported %q (nothing running) for a busy sid — abort silently lost", got)
	}
	// close.ok now echoes the closed sid (L-session-D1) — assert prefix + sid.
	if got, want := sink.finished, i18n.T("slash.close.ok", "en"); !strings.HasPrefix(got, want) || !strings.Contains(got, "sid1") {
		t.Fatalf("reply = %q, want close.ok %q + sid", got, want)
	}
}

// fakeGroupRouter is an internal contract.GroupRouter returning a fixed sub-scope
// (the agora virtual-group seam, faked).
type fakeGroupRouter struct{ sub string }

func (f fakeGroupRouter) RouteGroupScope(context.Context, contract.MessageEvent) (string, []string, bool) {
	return f.sub, nil, f.sub != ""
}

// TestCloseSession_MatchesMemberSubScope is the headline regression for the
// group↔member fix: in a virtual group a member turn runs under "<group>#<member>",
// so /close-session must resolve THAT sub-scope (via readScope) to match the busy
// turn. With the old bare-group scope the scan never matched and the abort was lost.
func TestCloseSession_MatchesMemberSubScope(t *testing.T) {
	m := NewManager(&returnStore{rows: map[string]store.SessionInfo{}}, stubTurnHandler{}, arbSrcByName, Config{})
	m.SetGroupRouter(func() contract.GroupRouter { return fakeGroupRouter{sub: "telegram:group:9#alice"} })
	ctx := context.Background()
	// A fresh group event (no reply) addressing a member — readScope routes it to the
	// member sub-scope via the group router.
	ev := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, ChatID: "9", UserID: "u", Text: "^alice /close-session"}

	m.mu.Lock()
	m.busy["sid_alice"] = true
	m.sidMeta["sid_alice"] = sidMetaRow{Scope: "telegram:group:9#alice"} // the scope the member turn ran under
	m.mu.Unlock()

	sink := &captureSink{}
	if err := m.slashCloseSession(ctx, contract.SlashContext{Event: ev, Sink: sink, Lang: "en"}); err != nil {
		t.Fatalf("slashCloseSession: %v", err)
	}
	m.mu.Lock()
	closing := m.closing["sid_alice"]
	m.mu.Unlock()
	if !closing {
		t.Fatal("closing[sid_alice] not set — /close-session resolved the bare group scope, missing the member turn")
	}
	if got, want := sink.finished, i18n.T("slash.close.ok", "en"); !strings.HasPrefix(got, want) || !strings.Contains(got, "sid_alice") {
		t.Fatalf("reply = %q, want close.ok %q + sid (member turn should have been found)", got, want)
	}
}

// TestDescribeMode: each SessionMode (incl. empty/unknown) maps to a distinct,
// non-empty i18n key — so no two modes collapse to the same gloss and an unrun
// session never renders blank. Catalog-state-independent: i18n.T returns the key
// itself when the catalog isn't loaded, which is still distinct per mode.
func TestDescribeMode(t *testing.T) {
	modes := []contract.SessionMode{
		contract.SessionModeManual,
		contract.SessionModeAuto,
		contract.SessionModeNone,
		contract.SessionMode(""), // pre-first-turn / unknown
	}
	seen := map[string]contract.SessionMode{}
	for _, mode := range modes {
		got := describeMode(mode, "zh")
		if strings.TrimSpace(got) == "" {
			t.Errorf("describeMode(%q) is blank", mode)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("describeMode(%q) and describeMode(%q) both = %q", mode, prev, got)
		}
		seen[got] = mode
	}
}

// uncensoredStore fakes the three store calls slashUncensored makes: the scope's
// active session, its row (sticky tags), and the tag write (applied to the row so
// a follow-up read sees it).
type uncensoredStore struct {
	store.Store
	sid  string // active sid returned for any scope ("" = none)
	rows map[string]store.SessionInfo
	sets []struct {
		sid  string
		tags []string
	}
}

func (s *uncensoredStore) CurrentSession(context.Context, string) (string, bool, error) {
	return s.sid, s.sid != "", nil
}

func (s *uncensoredStore) SessionByID(_ context.Context, id string) (store.SessionInfo, error) {
	return s.rows[id], nil
}

func (s *uncensoredStore) SetSessionPreferModelTags(_ context.Context, id string, tags []string) error {
	si := s.rows[id]
	si.PreferModelTags = tags
	s.rows[id] = si
	s.sets = append(s.sets, struct {
		sid  string
		tags []string
	}{id, tags})
	return nil
}

// /uncensored toggles: first call arms the scope (with the issuer's id), a second
// call clears the pending arm ("off"), and with a STAMPED active session it clears
// the persisted tags instead of re-arming.
func TestUncensoredToggle(t *testing.T) {
	st := &uncensoredStore{rows: map[string]store.SessionInfo{}}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	ctx := context.Background()
	ev := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, ChatID: "c1", UserID: "alice"}
	scope := m.readScope(ctx, ev)

	// 1) ON: arms the scope, remembering the issuer.
	sink := &captureSink{}
	if err := m.slashUncensored(ctx, contract.SlashContext{Event: ev, Sink: sink, Lang: "en"}); err != nil {
		t.Fatalf("slashUncensored (on): %v", err)
	}
	m.mu.Lock()
	armTags, armed := m.armedPrefer[armKey{scope: scope, userID: "alice"}]
	m.mu.Unlock()
	if !armed || len(armTags) != 1 || armTags[0] != "uncensored" {
		t.Fatalf("arm = %v (armed=%v), want tags [uncensored] for alice", armTags, armed)
	}
	if sink.finished != i18n.T("slash.uncensored.armed", "en") {
		t.Fatalf("reply = %q, want armed", sink.finished)
	}

	// 2) OFF while still armed (arm not yet consumed): clears the arm, no re-arm.
	sink = &captureSink{}
	if err := m.slashUncensored(ctx, contract.SlashContext{Event: ev, Sink: sink, Lang: "en"}); err != nil {
		t.Fatalf("slashUncensored (off, armed): %v", err)
	}
	m.mu.Lock()
	_, armed = m.armedPrefer[armKey{scope: scope, userID: "alice"}]
	m.mu.Unlock()
	if armed {
		t.Fatal("second /uncensored must clear the pending arm")
	}
	if sink.finished != i18n.T("slash.uncensored.off", "en") {
		t.Fatalf("reply = %q, want off", sink.finished)
	}

	// 3) OFF on a stamped active session: clears the persisted tags.
	st.sid = "sid1"
	st.rows["sid1"] = store.SessionInfo{ID: "sid1", PreferModelTags: []string{"uncensored"}}
	sink = &captureSink{}
	if err := m.slashUncensored(ctx, contract.SlashContext{Event: ev, Sink: sink, Lang: "en"}); err != nil {
		t.Fatalf("slashUncensored (off, stamped): %v", err)
	}
	if len(st.sets) != 1 || st.sets[0].sid != "sid1" || st.sets[0].tags != nil {
		t.Fatalf("sets = %+v, want one clearing write to sid1", st.sets)
	}
	m.mu.Lock()
	_, armed = m.armedPrefer[armKey{scope: scope, userID: "alice"}]
	m.mu.Unlock()
	if armed {
		t.Fatal("clearing a stamped session must not re-arm the scope")
	}
	if sink.finished != i18n.T("slash.uncensored.off", "en") {
		t.Fatalf("reply = %q, want off", sink.finished)
	}

	// 4) ON again from clean: arms normally.
	sink = &captureSink{}
	if err := m.slashUncensored(ctx, contract.SlashContext{Event: ev, Sink: sink, Lang: "en"}); err != nil {
		t.Fatalf("slashUncensored (re-arm): %v", err)
	}
	if sink.finished != i18n.T("slash.uncensored.armed", "en") {
		t.Fatalf("reply = %q, want armed", sink.finished)
	}
}

// loopingStore rides uncensoredStore's fake base and adds the turn-mode pref pair.
type loopingStore struct {
	uncensoredStore
	mode contract.TurnMode
	fail bool
}

func (s *loopingStore) GetScopeTurnMode(context.Context, string) (contract.TurnMode, error) {
	if s.fail {
		return contract.TurnModeRegular, fmt.Errorf("boom")
	}
	return s.mode, nil
}

func (s *loopingStore) SetScopeTurnMode(_ context.Context, _ string, m contract.TurnMode) error {
	if s.fail {
		return fmt.Errorf("boom")
	}
	s.mode = m
	return nil
}

// TestLoopingToggle drives /looping through every branch on a fake store:
// on / off / bare flip / bad arg / store failure.
func TestLoopingToggle(t *testing.T) {
	st := &loopingStore{uncensoredStore: uncensoredStore{rows: map[string]store.SessionInfo{}}}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	ctx := context.Background()
	ev := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, ChatID: "c1", UserID: "alice"}

	run := func(args string) string {
		sink := &captureSink{}
		if err := m.slashLooping(ctx, contract.SlashContext{Event: ev, Sink: sink, Lang: "en", Args: args}); err != nil {
			t.Fatalf("slashLooping(%q): %v", args, err)
		}
		return sink.finished
	}

	if got := run("on"); st.mode != contract.TurnModeLooping || got != i18n.T("slash.looping.on", "en") {
		t.Fatalf("on: mode=%q reply=%q", st.mode, got)
	}
	if got := run(""); st.mode != contract.TurnModeRegular || got != i18n.T("slash.looping.off", "en") {
		t.Fatalf("bare flip: mode=%q reply=%q", st.mode, got)
	}
	if got := run(""); st.mode != contract.TurnModeLooping || got != i18n.T("slash.looping.on", "en") {
		t.Fatalf("bare flip back: mode=%q reply=%q", st.mode, got)
	}
	if got := run("off"); st.mode != contract.TurnModeRegular || got != i18n.T("slash.looping.off", "en") {
		t.Fatalf("off: mode=%q reply=%q", st.mode, got)
	}
	if got := run("sideways"); st.mode != contract.TurnModeRegular || got != i18n.T("slash.looping.usage", "en") {
		t.Fatalf("bad arg: mode=%q reply=%q (mode must not change)", st.mode, got)
	}
	st.fail = true
	if got := run("on"); got != i18n.T("slash.looping.failed", "en") {
		t.Fatalf("store failure: reply=%q", got)
	}
}

// Two independent toggles coexist on one scope, and turning one off leaves the
// other standing. The pre-handler cleared the WHOLE tag column, so
// /uncensored silently switched /thinking off (and vice versa).
func TestToggleSessionTag_PerTagNotWholeColumn(t *testing.T) {
	st := &uncensoredStore{rows: map[string]store.SessionInfo{}}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	ctx := context.Background()
	ev := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, ChatID: "c1", UserID: "alice"}
	scope := m.readScope(ctx, ev)

	armTags := func() []string {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.armedPrefer[armKey{scope: scope, userID: "alice"}]
	}

	// Arm both, same issuer: the second toggle ADDS to the pending arm.
	if err := m.slashUncensored(ctx, contract.SlashContext{Event: ev, Sink: &captureSink{}, Lang: "en"}); err != nil {
		t.Fatalf("slashUncensored (on): %v", err)
	}
	if err := m.slashThinking(ctx, contract.SlashContext{Event: ev, Sink: &captureSink{}, Lang: "en"}); err != nil {
		t.Fatalf("slashThinking (on): %v", err)
	}
	if got := armTags(); len(got) != 2 || got[0] != "uncensored" || got[1] != "thinking" {
		t.Fatalf("arm tags = %v, want [uncensored thinking]", got)
	}

	// Turn ONE off: the other must survive in the pending arm.
	sink := &captureSink{}
	if err := m.slashUncensored(ctx, contract.SlashContext{Event: ev, Sink: sink, Lang: "en"}); err != nil {
		t.Fatalf("slashUncensored (off): %v", err)
	}
	if got := armTags(); len(got) != 1 || got[0] != "thinking" {
		t.Fatalf("arm tags after /uncensored off = %v, want [thinking]", got)
	}
	if sink.finished != i18n.T("slash.uncensored.off", "en") {
		t.Fatalf("reply = %q, want uncensored off", sink.finished)
	}

	// Same rule on a STAMPED session: only the named tag is removed.
	m.mu.Lock()
	delete(m.armedPrefer, armKey{scope: scope, userID: "alice"})
	m.mu.Unlock()
	st.sid = "sid1"
	st.rows["sid1"] = store.SessionInfo{ID: "sid1", PreferModelTags: []string{"uncensored", "thinking"}}
	if err := m.slashThinking(ctx, contract.SlashContext{Event: ev, Sink: &captureSink{}, Lang: "en"}); err != nil {
		t.Fatalf("slashThinking (off, stamped): %v", err)
	}
	if got := st.rows["sid1"].PreferModelTags; len(got) != 1 || got[0] != "uncensored" {
		t.Fatalf("stamped tags = %v, want [uncensored]", got)
	}
}

// A toggle whose tag is NOT currently set arms rather than reporting "off", even
// when the session carries other tags — "on" is per-tag, not "is the column empty".
func TestToggleSessionTag_ArmsWhenOtherTagsStamped(t *testing.T) {
	st := &uncensoredStore{sid: "sid1", rows: map[string]store.SessionInfo{
		"sid1": {ID: "sid1", PreferModelTags: []string{"uncensored"}},
	}}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	ctx := context.Background()
	ev := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, ChatID: "c1", UserID: "alice"}

	sink := &captureSink{}
	if err := m.slashThinking(ctx, contract.SlashContext{Event: ev, Sink: sink, Lang: "en"}); err != nil {
		t.Fatalf("slashThinking: %v", err)
	}
	if sink.finished != i18n.T("slash.thinking.armed", "en") {
		t.Fatalf("reply = %q, want thinking armed", sink.finished)
	}
	if len(st.sets) != 0 {
		t.Fatalf("arming must not write the session row yet: %+v", st.sets)
	}
}

// A pending arm belonging to SOMEBODY ELSE is replaced, not merged: takeArmedPrefer
// Arms are PER-ISSUER: two members of the same scope can each hold their own, and
// neither one's toggle touches the other's. With a single slot per scope, bob's
// /thinking silently replaced alice's pending /uncensored, and bob's /uncensored
// disarmed alice's arm while replying "off" to bob — who had never turned it on.
func TestToggleSessionTag_ArmsArePerIssuer(t *testing.T) {
	st := &uncensoredStore{rows: map[string]store.SessionInfo{}}
	m := NewManager(st, stubTurnHandler{}, arbSrcByName, Config{})
	ctx := context.Background()
	alice := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, ChatID: "g1", UserID: "alice"}
	bob := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, ChatID: "g1", UserID: "bob"}
	scope := m.readScope(ctx, alice)
	armOf := func(user string) []string {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.armedPrefer[armKey{scope: scope, userID: user}]
	}

	if err := m.slashUncensored(ctx, contract.SlashContext{Event: alice, Sink: &captureSink{}, Lang: "en"}); err != nil {
		t.Fatalf("alice /uncensored: %v", err)
	}
	if err := m.slashThinking(ctx, contract.SlashContext{Event: bob, Sink: &captureSink{}, Lang: "en"}); err != nil {
		t.Fatalf("bob /thinking: %v", err)
	}
	if got := armOf("alice"); len(got) != 1 || got[0] != "uncensored" {
		t.Fatalf("alice's arm = %v, want [uncensored] untouched by bob", got)
	}
	if got := armOf("bob"); len(got) != 1 || got[0] != "thinking" {
		t.Fatalf("bob's arm = %v, want [thinking]", got)
	}

	// bob turning "off" a tag he never armed must arm HIS OWN, not disarm alice's.
	sink := &captureSink{}
	if err := m.slashUncensored(ctx, contract.SlashContext{Event: bob, Sink: sink, Lang: "en"}); err != nil {
		t.Fatalf("bob /uncensored: %v", err)
	}
	if sink.finished != i18n.T("slash.uncensored.armed", "en") {
		t.Fatalf("reply = %q, want armed — bob never turned it on", sink.finished)
	}
	if got := armOf("alice"); len(got) != 1 || got[0] != "uncensored" {
		t.Fatalf("alice's arm = %v, want it to survive bob's toggle", got)
	}

	// Only the issuer's own turn consumes their own arm.
	got := m.takeArmedPrefer(scope, []contract.MessageEvent{{Source: "telegram", UserID: "alice"}})
	if len(got) != 1 || got[0] != "uncensored" {
		t.Fatalf("alice's turn consumed %v, want [uncensored]", got)
	}
	if len(armOf("bob")) != 2 {
		t.Fatalf("bob's arm = %v, want it still pending after alice's turn", armOf("bob"))
	}
}
