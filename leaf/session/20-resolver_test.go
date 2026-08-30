package session_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/pgpool"
	"agentbob/leaf/session"
	"agentbob/leaf/session/store"
	"agentbob/trunk"
)

// failingStore errors on SessionForMessage; embedding the nil Store interface
// satisfies the rest (resolveBase only calls SessionForMessage).
type failingStore struct{ store.Store }

func (failingStore) SessionForMessage(context.Context, string, string) (string, int64, bool, error) {
	return "", 0, false, errors.New("store blip")
}

// TestResolve_StoreBlipDoesNotFork locks the load-bearing degrade: a transient
// SessionForMessage error must CONTINUE the scope's active session (Key-only),
// never fork a fresh OpenNew (which would split one conversation into duplicates).
func TestResolve_StoreBlipDoesNotFork(t *testing.T) {
	m := session.NewManager(failingStore{}, noopHandler{}, stubSrcByName, session.Config{})
	ctx := context.Background()

	grp := m.ResolveSession(ctx, contract.MessageEvent{
		Source: "telegram", ChatType: contract.ChatGroup, ChatID: "g1",
		ReplyToBot: true, ReplyToID: "x",
	})
	if grp.Key != "telegram:group:g1" || grp.OpenNew || grp.SessionID != "" {
		t.Fatalf("group reply blip = %+v, want Key-only continue (no fork)", grp)
	}

	dm := m.ResolveSession(ctx, contract.MessageEvent{
		Source: "email", ChatType: contract.ChatDM, ChatID: "a@x",
		DMReplyRouting: true, ReplyRefs: []string{"<m@x>"},
	})
	if dm.Key != "email:dm:a@x" || dm.OpenNew || dm.SessionID != "" {
		t.Fatalf("email reply blip = %+v, want Key-only continue (no fork)", dm)
	}
}

// coldHitStore resolves a reply to coldSid, but reports that sid as not-found
// (contract.ErrNoRows) from SessionByID — i.e. the session is cold-archived. The
// rest of the Store interface is the embedded nil (ensureLive only calls
// SessionByID; resolveBase only SessionForMessage here).
type coldHitStore struct {
	store.Store
	coldSid string
}

func (s coldHitStore) SessionForMessage(context.Context, string, string) (string, int64, bool, error) {
	return s.coldSid, 1, true, nil
}
func (s coldHitStore) SessionByID(context.Context, string) (store.SessionInfo, error) {
	return store.SessionInfo{}, contract.ErrNoRows // cold: no live row
}

// TestResolve_ColdReplyRestoreFallback locks the cold-rehydrate branch with no DB:
// a reply resolves to a non-live sid → ensureLive consults the restore hook. When
// restore declines (was-ended / gone), the resolution falls through to a fresh
// OpenNew in the same scope; when it accepts, the sid is kept (revived in place).
func TestResolve_ColdReplyRestoreFallback(t *testing.T) {
	ctx := context.Background()
	ev := contract.MessageEvent{
		Source: "telegram", ChatType: contract.ChatGroup, ChatID: "g9",
		ReplyToBot: true, ReplyToID: "botmsg",
	}
	scope := ev.RoutingScope()

	// restore declines (was-ended / no archive) → fresh OpenNew, sid dropped.
	declined := session.NewManager(coldHitStore{coldSid: "s_cold"}, noopHandler{}, stubSrcByName, session.Config{})
	declined.SetRestore(func(context.Context, string) bool { return false })
	if got := declined.ResolveSession(ctx, ev); got.Key != scope || !got.OpenNew || got.SessionID != "" {
		t.Fatalf("declined restore = %+v, want %s + OpenNew (no sid)", got, scope)
	}

	// no restore hook at all → same fresh-OpenNew fallback.
	none := session.NewManager(coldHitStore{coldSid: "s_cold"}, noopHandler{}, stubSrcByName, session.Config{})
	if got := none.ResolveSession(ctx, ev); got.Key != scope || !got.OpenNew || got.SessionID != "" {
		t.Fatalf("nil restore = %+v, want %s + OpenNew (no sid)", got, scope)
	}

	// restore accepts (was-alive revived in place) → keep the sid, no OpenNew.
	revived := session.NewManager(coldHitStore{coldSid: "s_cold"}, noopHandler{}, stubSrcByName, session.Config{})
	var askedFor string
	revived.SetRestore(func(_ context.Context, sid string) bool { askedFor = sid; return true })
	if got := revived.ResolveSession(ctx, ev); got.SessionID != "s_cold" || got.OpenNew {
		t.Fatalf("accepted restore = %+v, want SessionID=s_cold (revived in place)", got)
	}
	if askedFor != "s_cold" {
		t.Fatalf("restore asked for %q, want s_cold", askedFor)
	}
}

func liveManager(t *testing.T) (*session.Manager, *store.PG, func()) {
	t.Helper()
	dsn := os.Getenv("PGPOOL_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGPOOL_TEST_DSN to run session resolver integration tests")
	}
	ctx := context.Background()
	reg := trunk.NewRegistry()
	m := pgpool.New(dsn)
	if err := m.Start(ctx, reg); err != nil {
		t.Fatalf("pgpool start: %v", err)
	}
	db := trunk.Require[contract.DB](reg)
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := store.NewPG(db)
	return session.NewManager(st, noopHandler{}, stubSrcByName, session.Config{}), st, func() { _ = m.Stop(ctx) }
}

func uscope(tag string) string {
	return "test:" + tag + ":" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func TestResolve_KeyComputation(t *testing.T) {
	mgr, _, done := liveManager(t)
	defer done()
	ctx := context.Background()

	cases := []struct {
		name string
		ev   contract.MessageEvent
		want Resolutionish
	}{
		{"dm plain", contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, ChatID: "c1"},
			Resolutionish{Key: "telegram:dm:c1"}},
		{"dm with thread", contract.MessageEvent{Source: "email", ChatType: contract.ChatDM, ChatID: "a@x", ThreadID: "sales@x"},
			Resolutionish{Key: "email:dm:a@x#sales@x"}},
		{"group fresh @", contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, ChatID: "g1"},
			Resolutionish{Key: "telegram:group:g1", OpenNew: true}},
		{"internal", contract.MessageEvent{Source: contract.SourceNameInternal, ChatID: "company:x/inbox:main"},
			Resolutionish{Key: "company:x/inbox:main"}},
		{"internal force-new", contract.MessageEvent{Source: contract.SourceNameInternal, ChatID: "company:x/inbox:main",
			Dispatch: &contract.MessageEventDispatchMeta{ForceNewSession: true}},
			Resolutionish{Key: "company:x/inbox:main", OpenNew: true}},
		{"email fresh compose", contract.MessageEvent{Source: "email", ChatType: contract.ChatDM, ChatID: "a@x", DMReplyRouting: true},
			Resolutionish{Key: "email:dm:a@x", OpenNew: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mgr.ResolveSession(ctx, c.ev)
			if got.Key != c.want.Key || got.OpenNew != c.want.OpenNew || got.SessionID != c.want.SessionID || got.Drop {
				t.Fatalf("ResolveSession = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestResolve_GroupReplyReconnect(t *testing.T) {
	mgr, st, done := liveManager(t)
	defer done()
	ctx := context.Background()

	// The index is keyed by (session_scope, msg_id); the bot message was sent in
	// the SAME scope the reply arrives in, so derive the scope from the event.
	chat := "grp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	reply := contract.MessageEvent{
		Source: "telegram", ChatType: contract.ChatGroup, ChatID: chat,
		ReplyToBot: true, ReplyToID: "botmsg9",
	}
	scope := reply.RoutingScope()
	sid, _, err := st.EnsureSession(ctx, scope, "telegram", "u1", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSent(ctx, scope, "botmsg9", sid); err != nil {
		t.Fatal(err)
	}

	// A reply to that bot message reconnects to its session.
	got := mgr.ResolveSession(ctx, reply)
	if got.Key != scope || got.SessionID != sid || got.OpenNew {
		t.Fatalf("reply-reconnect = %+v, want Key=%s SessionID=%s", got, scope, sid)
	}

	// The message_index is the AUTHORITY: a reply whose id IS recorded reconnects
	// EVEN WHEN ReplyToBot is false. Discord cannot always inline the referenced
	// message's author, so a genuine reply-to-bob arrives with ReplyToBot=false; the
	// index hit must still reconnect (else @=new forks a context-less session).
	noFlag := mgr.ResolveSession(ctx, contract.MessageEvent{
		Source: "telegram", ChatType: contract.ChatGroup, ChatID: chat,
		ReplyToBot: false, ReplyToID: "botmsg9",
	})
	if noFlag.Key != scope || noFlag.SessionID != sid || noFlag.OpenNew {
		t.Fatalf("reply-reconnect (no ReplyToBot flag) = %+v, want Key=%s SessionID=%s", noFlag, scope, sid)
	}

	// A reply to an UNRECORDED message → miss → fresh @=new.
	miss := mgr.ResolveSession(ctx, contract.MessageEvent{
		Source: "telegram", ChatType: contract.ChatGroup, ChatID: chat,
		ReplyToBot: true, ReplyToID: "ghost",
	})
	if miss.Key != scope || !miss.OpenNew || miss.SessionID != "" {
		t.Fatalf("reply-miss = %+v, want group key + OpenNew", miss)
	}
}

func TestResolve_EmailReplyRouting(t *testing.T) {
	mgr, st, done := liveManager(t)
	defer done()
	ctx := context.Background()

	addr := "a-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@x"
	reply := contract.MessageEvent{
		Source: "email", ChatType: contract.ChatDM, ChatID: addr,
		DMReplyRouting: true, ReplyRefs: []string{"<other@x>", "<msg-1@x>"},
	}
	scope := reply.RoutingScope()
	sid, _, err := st.EnsureSession(ctx, scope, "email", addr, "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSent(ctx, scope, "<msg-1@x>", sid); err != nil {
		t.Fatal(err)
	}

	// A reply whose References chain hits the recorded message continues it.
	got := mgr.ResolveSession(ctx, reply)
	if got.Key != scope || got.SessionID != sid || got.OpenNew {
		t.Fatalf("email reply-routing = %+v, want Key=%s SessionID=%s", got, scope, sid)
	}
}

// Resolutionish mirrors session.Resolution's exported fields for table tests
// (the package-external test can't name the unexported zero-value succinctly).
type Resolutionish struct {
	Key       string
	SessionID string
	OpenNew   bool
}

// groupPicker is a fake contract.GroupRouter returning a fixed routing decision.
type groupPicker struct {
	sub     string
	members []string
}

func (p groupPicker) RouteGroupScope(context.Context, contract.MessageEvent) (string, []string, bool) {
	return p.sub, p.members, true
}

// bareStore is a store.Store whose methods are never called (fresh-group resolve path
// touches no store method: no reply id → no SessionForMessage; SessionID "" → no
// SessionByID).
type bareStore struct{ store.Store }

// TestResolve_GroupMemberRouting covers the virtual-group resolver branch (Phase 2.2):
// a picked member rewrites the key to its sub-scope; an unaddressed fanned group leaves
// the bare group scope (flow asks "找谁"); no agora seam = 1:1, unchanged.
func TestResolve_GroupMemberRouting(t *testing.T) {
	ctx := context.Background()
	grp := "telegram:group:777"
	ev := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, ChatID: "777", Text: "#alice 看下"}

	m := session.NewManager(bareStore{}, noopHandler{}, stubSrcByName, session.Config{})
	m.SetGroupRouter(func() contract.GroupRouter { return groupPicker{sub: grp + "#alice"} })
	if r := m.ResolveSession(ctx, ev); r.Key != grp+"#alice" || !r.OpenNew {
		t.Fatalf("picked: got Key=%q OpenNew=%v, want %s#alice / true", r.Key, r.OpenNew, grp)
	}

	m2 := session.NewManager(bareStore{}, noopHandler{}, stubSrcByName, session.Config{})
	m2.SetGroupRouter(func() contract.GroupRouter { return groupPicker{members: []string{"alice", "bob"}} })
	if r := m2.ResolveSession(ctx, ev); r.Key != grp {
		t.Fatalf("ask (no pick): got Key=%q, want %s unchanged", r.Key, grp)
	}

	m3 := session.NewManager(bareStore{}, noopHandler{}, stubSrcByName, session.Config{})
	if r := m3.ResolveSession(ctx, ev); r.Key != grp {
		t.Fatalf("no-agora: got Key=%q, want %s", r.Key, grp)
	}
}
