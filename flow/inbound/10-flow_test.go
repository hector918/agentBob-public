package inbound

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/claimtoken"
	"agentbob/leaf/gate"
	"agentbob/leaf/slash"
	"agentbob/leaf/stoma"
	"agentbob/leaf/webui"
	"agentbob/trunk"
)

// fakeSource is a configurable contract.Source: it emits a fixed event list
// (for the bus integration test) and records what it was asked to Send (for the
// redeem-reply assertions).
type fakeSource struct {
	name       string
	caps       contract.Caps
	emit       []contract.MessageEvent
	mu         sync.Mutex // guards sent/lastTarget — dispatch runs on a goroutine
	sent       []string
	lastTarget contract.Target // the Target of the most recent NewSink (slash-thread assertions)
}

// sinkTarget returns the Target of the most recent NewSink call.
func (f *fakeSource) sinkTarget() contract.Target {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastTarget
}

// sentSnapshot returns a copy of the recorded sends (deliverRedeem runs async).
func (f *fakeSource) sentSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// resetSent clears the recorded sends under the lock.
func (f *fakeSource) resetSent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = nil
}

// waitSent polls until at least n sends are recorded (the redeem reply is delivered
// on a fire-and-forget goroutine), returning the snapshot (which may be short on timeout).
func (f *fakeSource) waitSent(t *testing.T, n int) []string {
	t.Helper()
	for i := 0; i < 500; i++ {
		if got := f.sentSnapshot(); len(got) >= n {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	return f.sentSnapshot()
}

func (f *fakeSource) Name() string                      { return f.name }
func (f *fakeSource) Caps() contract.Caps               { return f.caps }
func (f *fakeSource) HealthCheck(context.Context) error { return nil }
func (f *fakeSource) Run(ctx context.Context, out chan<- contract.MessageEvent) error {
	for _, ev := range f.emit {
		select {
		case out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeSource) NewSink(_ context.Context, t contract.Target, _, _ string, _ contract.SinkPrefs) contract.Sink {
	f.mu.Lock()
	f.lastTarget = t
	f.mu.Unlock()
	return nil
}
func (f *fakeSource) SendButtons(context.Context, contract.Target, string, []contract.Button) (string, error) {
	return "", nil
}
func (f *fakeSource) Send(_ context.Context, _ contract.Target, text string) error {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	f.mu.Unlock()
	return nil
}
func (f *fakeSource) SendFile(context.Context, contract.Target, string, string) error { return nil }

type fakeGateway struct {
	events chan contract.MessageEvent
	src    map[string]contract.Source
}

func (g *fakeGateway) Events() <-chan contract.MessageEvent     { return g.events }
func (g *fakeGateway) SourceByName(name string) contract.Source { return g.src[name] }
func (g *fakeGateway) SourceNames() []string                    { return nil }

type fakeScreener struct {
	verdict contract.Verdict
	calls   int
}

func (s *fakeScreener) Screen(context.Context, contract.MessageEvent) contract.Verdict {
	s.calls++
	return s.verdict
}
func (s *fakeScreener) IsAdmin(string, string, string) bool { return false }

// TestHandle_Triage drives the routing switch directly with fakes.
func TestHandle_Triage(t *testing.T) {
	gated := &fakeSource{name: "tg"}                                        // screened (default, not Trusted)
	plain := &fakeSource{name: "local", caps: contract.Caps{Trusted: true}} // exempt
	gw := &fakeGateway{src: map[string]contract.Source{"tg": gated, "local": plain}}
	scr := &fakeScreener{}
	var got []contract.MessageEvent
	m := &Module{gw: gw, screener: scr, submit: func(_ context.Context, ev contract.MessageEvent) {
		got = append(got, ev)
	}}
	ctx := context.Background()
	dm := func(src string) contract.MessageEvent {
		return contract.MessageEvent{Source: src, UserID: "u", ChatType: contract.ChatDM, Text: "hi"}
	}

	// Pass → submitted.
	scr.verdict = contract.Verdict{Action: contract.ScreenPass}
	m.handle(ctx, dm("tg"))
	if len(got) != 1 {
		t.Fatalf("pass: submitted %d, want 1", len(got))
	}

	// Drop → not submitted.
	got, scr.calls = nil, 0
	scr.verdict = contract.Verdict{Action: contract.ScreenDrop, Reason: contract.ReasonUnauthorized}
	m.handle(ctx, dm("tg"))
	if len(got) != 0 {
		t.Fatalf("drop: submitted %d, want 0", len(got))
	}

	// Redeemed (DM) → reply delivered (async), not submitted.
	got = nil
	gated.resetSent()
	scr.verdict = contract.Verdict{Action: contract.ScreenRedeemed, Reply: "bound"}
	m.handle(ctx, dm("tg"))
	if len(got) != 0 {
		t.Fatalf("redeemed: submitted %d, want 0", len(got))
	}
	if s := gated.waitSent(t, 1); len(s) != 1 || s[0] != "bound" {
		t.Fatalf("redeem reply: sent %v, want [bound]", s)
	}

	// A Trusted source bypasses the screen entirely and submits.
	got, scr.calls = nil, 0
	m.handle(ctx, dm("local"))
	if len(got) != 1 || scr.calls != 0 {
		t.Fatalf("trusted: submitted=%d screenCalls=%d, want 1/0", len(got), scr.calls)
	}
}

// recSlash signals (on a channel) each event handed to slash dispatch — the
// dispatch runs on its own goroutine now, so the test synchronizes via the chan.
type recSlash struct{ dispatched chan string }

func (r *recSlash) Register(contract.SlashCommand) {}
func (r *recSlash) Dispatch(_ context.Context, sc contract.SlashContext) error {
	r.dispatched <- sc.Event.Text
	return nil
}
func (r *recSlash) List() []contract.SlashCommand { return nil }

// TestHandle_SlashFork: a "/..." event is dispatched to the registry (out-of-band,
// after the gate, async), NOT submitted as a turn; plain text still submits.
func TestHandle_SlashFork(t *testing.T) {
	plain := &fakeSource{name: "local", caps: contract.Caps{Trusted: true}} // Trusted → bypasses screen, straight to the slash fork
	gw := &fakeGateway{src: map[string]contract.Source{"local": plain}}
	var submitted []contract.MessageEvent
	sl := &recSlash{dispatched: make(chan string, 4)}
	m := &Module{gw: gw, screener: &fakeScreener{}, slash: sl, submit: func(_ context.Context, ev contract.MessageEvent) {
		submitted = append(submitted, ev)
	}}
	ctx := context.Background()
	dm := func(text string) contract.MessageEvent {
		return contract.MessageEvent{Source: "local", UserID: "u", ChatType: contract.ChatDM, Text: text}
	}

	// "/new" → slash dispatch (async), not a turn.
	m.handle(ctx, dm("/new"))
	select {
	case got := <-sl.dispatched:
		if got != "/new" {
			t.Fatalf("slash dispatched %q, want /new", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slash command was never dispatched")
	}
	if len(submitted) != 0 {
		t.Fatalf("a slash command must not be submitted as a turn: %v", submitted)
	}

	// plain text → submit (synchronous), no slash dispatch.
	m.handle(ctx, dm("hello"))
	if len(submitted) != 1 {
		t.Fatalf("plain text: submitted=%d, want 1", len(submitted))
	}
	select {
	case x := <-sl.dispatched:
		t.Fatalf("plain text must not dispatch slash, got %q", x)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHandle_SlashAlbumDedup (F120): album siblings share a MediaGroupID and each
// carries the album's caption (the media-group buffer stamps it onto every sibling),
// so a "/..." caption must dispatch exactly once — not once per photo — and the
// siblings must not fall through to a turn. A different album dispatches again.
func TestHandle_SlashAlbumDedup(t *testing.T) {
	plain := &fakeSource{name: "local", caps: contract.Caps{Trusted: true}}
	gw := &fakeGateway{src: map[string]contract.Source{"local": plain}}
	sl := &recSlash{dispatched: make(chan string, 8)}
	var submitted int
	m := &Module{gw: gw, screener: &fakeScreener{}, slash: sl,
		submit: func(context.Context, contract.MessageEvent) { submitted++ }}
	ctx := context.Background()
	sib := func(gid string) contract.MessageEvent {
		return contract.MessageEvent{
			Source: "local", UserID: "u", ChatID: "c1", ChatType: contract.ChatDM,
			Text: "/new", MediaGroupID: gid,
			Attachments: []contract.Attachment{{Kind: "image", Path: "inbox/p.jpg"}},
		}
	}

	// Three siblings of one album → exactly one dispatch.
	m.handle(ctx, sib("album-1"))
	m.handle(ctx, sib("album-1"))
	m.handle(ctx, sib("album-1"))
	select {
	case <-sl.dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("album slash was never dispatched")
	}
	select {
	case x := <-sl.dispatched:
		t.Fatalf("album slash dispatched more than once (per-sibling fork): %q", x)
	case <-time.After(100 * time.Millisecond):
	}
	if submitted != 0 {
		t.Fatalf("album slash siblings must not submit turns, got %d", submitted)
	}

	// A NEW album dispatches again (dedup is per-group, not a global slash gate).
	m.handle(ctx, sib("album-2"))
	select {
	case <-sl.dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("second album's slash was never dispatched")
	}
}

// TestAlbumSlashDup pins the dedup predicate directly: first sibling passes,
// repeats dedup, non-album events never dedup, and an expired entry is pruned so
// the group can dispatch anew.
func TestAlbumSlashDup(t *testing.T) {
	m := &Module{}
	ev := contract.MessageEvent{Source: "tg", ChatID: "c", MediaGroupID: "g"}
	if m.albumSlashDup(ev) {
		t.Fatal("first album sibling must pass")
	}
	if !m.albumSlashDup(ev) {
		t.Fatal("repeat album sibling must dedup")
	}
	if m.albumSlashDup(contract.MessageEvent{Source: "tg", ChatID: "c"}) {
		t.Fatal("a non-album event (no MediaGroupID) must never dedup")
	}
	// Expired entry → pruned on the next hit, the group dispatches anew.
	m.albumSeen["tg\x00c\x00g"] = time.Now().Add(-time.Second)
	if m.albumSlashDup(ev) {
		t.Fatal("an expired dedup entry must not suppress a fresh dispatch")
	}
}

// TestHandle_SlashThreadsReply (D37): in a group on a ReplyTo-capable source, the slash
// reply sink threads to the command message (Target.ReplyToID == the command's id),
// mirroring the normal turn path — so /session etc. correlate with their command.
func TestHandle_SlashThreadsReply(t *testing.T) {
	grp := &fakeSource{name: "tg", caps: contract.Caps{Trusted: true, ReplyTo: true}}
	gw := &fakeGateway{src: map[string]contract.Source{"tg": grp}}
	sl := &recSlash{dispatched: make(chan string, 4)}
	m := &Module{gw: gw, screener: &fakeScreener{}, slash: sl, submit: func(context.Context, contract.MessageEvent) {}}

	m.handle(context.Background(), contract.MessageEvent{
		Source: "tg", UserID: "u", ChatID: "g1", ChatType: contract.ChatGroup, MessageID: "cmd-7", Text: "/session",
	})
	select {
	case <-sl.dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("slash command was never dispatched")
	}
	if got := grp.sinkTarget().ReplyToID; got != "cmd-7" {
		t.Fatalf("slash sink ReplyToID = %q, want cmd-7 (reply threaded to the command)", got)
	}
}

// TestClassify pins the flow-route marker: only ev.Source decides internal vs
// external. A reply / DM-routing flag is NOT a flow-route signal.
func TestClassify(t *testing.T) {
	if classify(contract.MessageEvent{Source: contract.SourceNameInternal}) != routeInternal {
		t.Fatal("internal source must classify as routeInternal")
	}
	if classify(contract.MessageEvent{Source: "telegram"}) != routeChat {
		t.Fatal("external source must classify as routeChat")
	}
	// a reply from an external source is still chat — continuation is downstream.
	if classify(contract.MessageEvent{Source: "telegram", ReplyToBot: true, DMReplyRouting: true}) != routeChat {
		t.Fatal("reply/DM-routing flags must not change the flow route")
	}
}

// TestHandle_InternalRoute: an internal event is submitted to its target scope
// WITHOUT going through the sender screen (internal events are trusted). When
// agora lands an InternalHandler intercepts here before this fallback; today a
// plain internal emit (send_message Stage A / cron) runs as an ordinary turn.
func TestHandle_InternalRoute(t *testing.T) {
	scr := &fakeScreener{}
	var submitted int
	m := &Module{
		gw:       &fakeGateway{src: map[string]contract.Source{}},
		screener: scr,
		submit:   func(context.Context, contract.MessageEvent) { submitted++ },
	}
	m.handle(context.Background(), contract.MessageEvent{Source: contract.SourceNameInternal, Text: "report"})
	if submitted != 1 {
		t.Fatalf("internal event submitted %d times, want 1 (woke a turn at its scope)", submitted)
	}
	if scr.calls != 0 {
		t.Fatalf("internal event was screened (%d calls), want 0 (internal is trusted)", scr.calls)
	}
}

// TestHandle_GroupRedeemPolicy: a redeem confirmation in a group is suppressed
// unless the source opted into RedeemConfirmAll.
func TestHandle_GroupRedeemPolicy(t *testing.T) {
	ctx := context.Background()
	scr := &fakeScreener{verdict: contract.Verdict{Action: contract.ScreenRedeemed, Reply: "bound"}}
	groupEv := contract.MessageEvent{Source: "tg", ChatType: contract.ChatGroup, Text: "CODE"}

	// not opted in → no send. deliverRedeem (async) runs and returns early without
	// sending; give the goroutine time to run, then assert nothing was sent.
	quiet := &fakeSource{name: "tg", caps: contract.Caps{}}
	m := &Module{gw: &fakeGateway{src: map[string]contract.Source{"tg": quiet}}, screener: scr, submit: func(context.Context, contract.MessageEvent) {}}
	m.handle(ctx, groupEv)
	time.Sleep(30 * time.Millisecond)
	if s := quiet.sentSnapshot(); len(s) != 0 {
		t.Fatalf("group redeem without RedeemConfirmAll: sent %v, want none", s)
	}

	// opted in → send (async).
	loud := &fakeSource{name: "tg", caps: contract.Caps{RedeemConfirmAll: true}}
	m2 := &Module{gw: &fakeGateway{src: map[string]contract.Source{"tg": loud}}, screener: scr, submit: func(context.Context, contract.MessageEvent) {}}
	m2.handle(ctx, groupEv)
	if s := loud.waitSent(t, 1); len(s) != 1 {
		t.Fatalf("group redeem with RedeemConfirmAll: sent %v, want [bound]", s)
	}
}

// TestFlow_ThroughTrunk wires real stoma + gate + flow and proves an allowed
// sender's event reaches the hand-off while a denied one is dropped.
func TestFlow_ThroughTrunk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tg.yaml"), []byte("allowlist: [\"alice\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{name: "tg", caps: contract.Caps{}, emit: []contract.MessageEvent{
		{Source: "tg", UserID: "alice", ChatType: contract.ChatDM, Text: "hi"},
		{Source: "tg", UserID: "bob", ChatType: contract.ChatDM, Text: "hi"},
	}}
	recorded := make(chan contract.MessageEvent, 8)
	f := New()
	f.submit = func(_ context.Context, ev contract.MessageEvent) { recorded <- ev }

	tk := trunk.New()
	tk.Register(claimtoken.New()) // gate hard-Needs ClaimTokens (gate-admit token)
	tk.Register(gate.New(dir))
	tk.Register(slash.New())
	tk.Register(webui.New("")) // provides contract.PanelRegistry (gate/stoma Need it); "" = no http
	tk.Register(stoma.New([]contract.Source{src}))
	tk.Register(f)
	if err := tk.Start(context.Background()); err != nil {
		t.Fatalf("trunk Start: %v", err)
	}
	defer tk.Stop(context.Background())

	// alice is allowlisted → her event reaches submit.
	select {
	case ev := <-recorded:
		if ev.UserID != "alice" {
			t.Fatalf("first hand-off = %q, want alice", ev.UserID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alice's event never reached the hand-off")
	}
	// bob is not allowlisted → dropped, never reaches submit.
	select {
	case ev := <-recorded:
		t.Fatalf("unexpected hand-off %+v — bob should have been dropped", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestRun_UnexpectedBusCloseMarksFailed: when the event bus closes its stream while
// the flow is NOT shutting down (ctx still live), the consume loop exits and the
// module must report StateFailed (self-detected unhealthy) — not keep claiming Ready
// while bob is silently deaf to every inbound message.
func TestRun_UnexpectedBusCloseMarksFailed(t *testing.T) {
	ch := make(chan contract.MessageEvent)
	m := &Module{gw: &fakeGateway{events: ch}, done: make(chan struct{}),
		submit: func(context.Context, contract.MessageEvent) {}}
	m.state.Store(int32(trunk.StateReady)) // mirror post-Start state

	go m.run(context.Background()) // ctx never cancelled — this is NOT a Stop
	close(ch)                      // the bus closes its stream
	<-m.done                       // run observed the close and exited

	if got := m.Health(); got != trunk.StateFailed {
		t.Fatalf("Health after unexpected bus close = %v, want StateFailed (dead pipeline must not report Ready)", got)
	}
}

// fakeLangAccounts is a minimal contract.Accounts for the resolveLang cascade test:
// LangFor returns recall (ok iff non-empty); RememberLang records the seeded langs.
type fakeLangAccounts struct {
	mu         sync.Mutex
	recall     string
	remembered []string
	lookups    int // LangFor call count (pins the langMem read-through cache)
}

func (f *fakeLangAccounts) LangFor(_ context.Context, _, _ string) (string, bool) {
	f.mu.Lock()
	f.lookups++
	f.mu.Unlock()
	if f.recall == "" {
		return "", false
	}
	return f.recall, true
}
func (f *fakeLangAccounts) RememberLang(_ context.Context, _, _, lang string) {
	f.mu.Lock()
	f.remembered = append(f.remembered, lang)
	f.mu.Unlock()
}
func (f *fakeLangAccounts) seeded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.remembered...)
}
func (f *fakeLangAccounts) AccountFor(context.Context, contract.MessageEvent) (contract.AccountInfo, bool) {
	return contract.AccountInfo{}, false
}

// fakeForkAccounts is a contract.Accounts whose AccountFor returns a fixed
// (info, ok) — the slash-fork branch discriminator (F40/F41).
type fakeForkAccounts struct {
	info contract.AccountInfo
	ok   bool
}

func (f *fakeForkAccounts) AccountFor(context.Context, contract.MessageEvent) (contract.AccountInfo, bool) {
	return f.info, f.ok
}
func (f *fakeForkAccounts) LangFor(context.Context, string, string) (string, bool) { return "", false }
func (f *fakeForkAccounts) RememberLang(context.Context, string, string, string)   {}

// TestHandle_SlashFork_AccountBranches (F40 + F41): the non-admin slash fork consults
// AccountFor once and branches honestly — a paused account is bounced (pause gates
// ACTION, not just turns); an onboarding-pass-but-entitled member's slash WORKS; a
// registered-but-unentitled sender gets an honest "pending approval" notice; a
// genuinely new sender falls through to the turn (→ intro). The i18n catalog is
// unloaded in tests, so i18n.T returns the bare key — assert against that.
func TestHandle_SlashFork_AccountBranches(t *testing.T) {
	ctx := context.Background()
	// A non-Trusted source goes through the screen; the verdict's Onboarding flag sets
	// onboardingPass. DM so deliverRedeem actually sends.
	newModule := func(pass contract.Verdict, acct *fakeForkAccounts) (*Module, *fakeSource, *recSlash, *int) {
		src := &fakeSource{name: "tg", caps: contract.Caps{}}
		gw := &fakeGateway{src: map[string]contract.Source{"tg": src}}
		sl := &recSlash{dispatched: make(chan string, 4)}
		submitted := 0
		m := &Module{gw: gw, screener: &fakeScreener{verdict: pass}, slash: sl, accounts: acct,
			submit: func(context.Context, contract.MessageEvent) { submitted++ }}
		return m, src, sl, &submitted
	}
	slash := func(text string) contract.MessageEvent {
		return contract.MessageEvent{Source: "tg", UserID: "u", ChatType: contract.ChatDM, Text: text}
	}
	noDispatch := func(t *testing.T, sl *recSlash) {
		t.Helper()
		select {
		case x := <-sl.dispatched:
			t.Fatalf("must not dispatch, got %q", x)
		case <-time.After(80 * time.Millisecond):
		}
	}

	// F41: allowlisted-then-paused (onboardingPass=false, Status=paused) → honest bounce,
	// no dispatch, no turn.
	t.Run("paused_bounces", func(t *testing.T) {
		m, src, sl, submitted := newModule(
			contract.Verdict{Action: contract.ScreenPass},
			&fakeForkAccounts{ok: true, info: contract.AccountInfo{Status: "paused", Flow: "normal", FlowKnown: true}})
		m.handle(ctx, slash("/takeover"))
		if got := src.waitSent(t, 1); len(got) != 1 || got[0] != "gate.slash.paused" {
			t.Fatalf("paused bounce = %v, want [gate.slash.paused]", got)
		}
		noDispatch(t, sl)
		if *submitted != 0 {
			t.Fatalf("paused slash must not submit a turn, got %d", *submitted)
		}
	})

	// F40: onboarding-pass but entitled (a real member un-allowlisted on THIS bot) →
	// dispatch (their slash works).
	t.Run("onboarding_entitled_dispatches", func(t *testing.T) {
		m, _, sl, submitted := newModule(
			contract.Verdict{Action: contract.ScreenPass, Onboarding: true},
			&fakeForkAccounts{ok: true, info: contract.AccountInfo{Status: "active", Flow: "normal", FlowKnown: true}})
		m.handle(ctx, slash("/new"))
		select {
		case got := <-sl.dispatched:
			if got != "/new" {
				t.Fatalf("dispatched %q, want /new", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("entitled onboarding-pass member's slash was never dispatched")
		}
		if *submitted != 0 {
			t.Fatalf("dispatched slash must not also submit, got %d", *submitted)
		}
	})

	// F40: onboarding-pass, registered but NO entitlement (bare account pending approval)
	// → honest "pending approval" notice, no dispatch.
	t.Run("onboarding_pending_notice", func(t *testing.T) {
		m, src, sl, submitted := newModule(
			contract.Verdict{Action: contract.ScreenPass, Onboarding: true},
			&fakeForkAccounts{ok: true, info: contract.AccountInfo{Status: "active", Flow: "", FlowKnown: true}})
		m.handle(ctx, slash("/new"))
		if got := src.waitSent(t, 1); len(got) != 1 || got[0] != "gate.slash.pending_approval" {
			t.Fatalf("pending notice = %v, want [gate.slash.pending_approval]", got)
		}
		noDispatch(t, sl)
		if *submitted != 0 {
			t.Fatalf("pending slash must not submit a turn, got %d", *submitted)
		}
	})

	// F40: onboarding-pass, genuinely new (unbound) → falls THROUGH to the turn (intro),
	// no dispatch, no bounce.
	t.Run("onboarding_new_falls_to_turn", func(t *testing.T) {
		m, src, sl, submitted := newModule(
			contract.Verdict{Action: contract.ScreenPass, Onboarding: true},
			&fakeForkAccounts{ok: false})
		m.handle(ctx, slash("/new"))
		noDispatch(t, sl)
		if *submitted != 1 {
			t.Fatalf("new onboarding-pass slash must fall to a turn, submitted=%d want 1", *submitted)
		}
		if got := src.sentSnapshot(); len(got) != 0 {
			t.Fatalf("new sender must get no bounce, got %v", got)
		}
	})

	// Regression: a slash-captioned ALBUM must produce exactly ONE bounce, not one per
	// sibling photo — the album guard runs once for the whole slash, above the branch, so
	// the paused/pending bounce paths dedup too (else 3 photos → 3 identical bounces).
	t.Run("paused_album_bounces_once", func(t *testing.T) {
		m, src, sl, submitted := newModule(
			contract.Verdict{Action: contract.ScreenPass},
			&fakeForkAccounts{ok: true, info: contract.AccountInfo{Status: "paused", Flow: "normal", FlowKnown: true}})
		sib := contract.MessageEvent{Source: "tg", UserID: "u", ChatID: "c1", ChatType: contract.ChatDM, Text: "/new", MediaGroupID: "alb-1"}
		m.handle(ctx, sib)
		m.handle(ctx, sib)
		m.handle(ctx, sib)
		// Give any (wrongly) duplicated async bounces time to land, then assert exactly one.
		time.Sleep(60 * time.Millisecond)
		if got := src.sentSnapshot(); len(got) != 1 || got[0] != "gate.slash.paused" {
			t.Fatalf("album slash must bounce exactly once, got %v", got)
		}
		noDispatch(t, sl)
		if *submitted != 0 {
			t.Fatalf("album slash bounce must not submit a turn, got %d", *submitted)
		}
	})

	// Regression: onboarding-pass + registered-but-unentitled in a GROUP (no
	// RedeemConfirmAll) — deliverRedeem would no-op, so instead of a silent black hole the
	// command falls THROUGH to the turn (intro posts a group nudge), matching the old gate.
	t.Run("group_pending_falls_to_turn", func(t *testing.T) {
		m, src, sl, submitted := newModule(
			contract.Verdict{Action: contract.ScreenPass, Onboarding: true},
			&fakeForkAccounts{ok: true, info: contract.AccountInfo{Status: "active", Flow: "", FlowKnown: true}})
		m.handle(ctx, contract.MessageEvent{Source: "tg", UserID: "u", ChatID: "g1", ChatType: contract.ChatGroup, Text: "/new"})
		noDispatch(t, sl)
		if *submitted != 1 {
			t.Fatalf("group pending slash must fall to a turn, submitted=%d want 1", *submitted)
		}
		if got := src.sentSnapshot(); len(got) != 0 {
			t.Fatalf("group pending (undeliverable DM) must not bounce, got %v", got)
		}
	})
}

// TestResolveLang_Cascade (D33) pins the per-message-wins + seed-once + haveAcct-guard
// cascade: a real language wins AND seeds (async); a signal-less message recalls the
// sender's remembered language; latin is English (never stored/recalled); no account →
// default.
func TestResolveLang_Cascade(t *testing.T) {
	ctx := context.Background()
	evWith := func(text string) contract.MessageEvent {
		return contract.MessageEvent{Source: "telegram", UserID: "u", Text: text}
	}

	// real language (zh) → use it AND seed (async, off the loop).
	acc := &fakeLangAccounts{}
	m := &Module{accounts: acc}
	if got := m.resolveLang(ctx, evWith("你好世界")); got != "zh" {
		t.Fatalf("real-language → %q, want zh", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(acc.seeded()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s := acc.seeded(); len(s) != 1 || s[0] != "zh" {
		t.Fatalf("a real-language message must seed the sender's lang (async), got %v", s)
	}

	// signal-less (digits) + a remembered language → recalled; a SECOND signal-less
	// message hits the langMem read-through cache (one SELECT per sender, then
	// memory-only — the store must not ride the consume loop steady-state, B16).
	recAcc := &fakeLangAccounts{recall: "ja"}
	m = &Module{accounts: recAcc}
	if got := m.resolveLang(ctx, evWith("12345")); got != "ja" {
		t.Fatalf("signal-less with recall → %q, want ja", got)
	}
	if got := m.resolveLang(ctx, evWith("67890")); got != "ja" {
		t.Fatalf("signal-less cached recall → %q, want ja", got)
	}
	recAcc.mu.Lock()
	lookups := recAcc.lookups
	recAcc.mu.Unlock()
	if lookups != 1 {
		t.Fatalf("LangFor store lookups = %d, want 1 (second read must come from langMem)", lookups)
	}

	// signal-less + NO recall → default.
	m = &Module{accounts: &fakeLangAccounts{}}
	if got := m.resolveLang(ctx, evWith("12345")); got != "default" {
		t.Fatalf("signal-less no recall → %q, want default", got)
	}

	// latin signal = English; never stored, never recalled.
	acc = &fakeLangAccounts{}
	m = &Module{accounts: acc}
	if got := m.resolveLang(ctx, evWith("hello world")); got != "default" {
		t.Fatalf("latin → %q, want default", got)
	}
	time.Sleep(20 * time.Millisecond)
	if s := acc.seeded(); len(s) != 0 {
		t.Fatalf("a latin message must NOT seed, got %v", s)
	}

	// no account wired → signal-less falls to default (haveAcct guard).
	m = &Module{accounts: nil}
	if got := m.resolveLang(ctx, evWith("12345")); got != "default" {
		t.Fatalf("no-account signal-less → %q, want default", got)
	}
}

// TestRun_CleanStopDoesNotMarkFailed (D34) pins the orderly-branch property directly: when
// the ctx is cancelled (a clean Stop) AND the bus channel then closes, run() must take the
// orderly path and NOT self-mark StateFailed — that mark is reserved for an UNEXPECTED
// close while the ctx is still live (TestRun_UnexpectedBusCloseMarksFailed). Tests run() in
// isolation (no Stop(), which would unconditionally overwrite to StateStopped) so the
// assertion genuinely guards run()'s ctx-cancelled check, not Stop's final store. The
// StateFailed branch IS reachable here (channel closes), so removing the ctx guard fails this.
func TestRun_CleanStopDoesNotMarkFailed(t *testing.T) {
	ch := make(chan contract.MessageEvent)
	m := &Module{gw: &fakeGateway{events: ch}, done: make(chan struct{}),
		submit: func(context.Context, contract.MessageEvent) {}}
	m.state.Store(int32(trunk.StateReady))

	ctx, cancel := context.WithCancel(context.Background())
	go m.run(ctx)
	cancel()  // orderly Stop: ctx cancelled FIRST
	close(ch) // then the bus closes — run sees a cancelled ctx, must exit quietly (not Failed)
	<-m.done

	if got := m.Health(); got == trunk.StateFailed {
		t.Fatalf("a clean stop (ctx cancelled) must NOT self-mark StateFailed, got %v", got)
	}
}
