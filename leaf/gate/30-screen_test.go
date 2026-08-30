package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbob/contract"
	"agentbob/leaf/claimtoken"
	"agentbob/trunk"
)

// fakeSlash is a contract.SlashRegistry that records the command texts a redeemed token's
// batch dispatches, notes whether each ran under a bounded ctx (F115), and can return a
// fixed error + reply — the screen drives Consume from whether the whole batch succeeded.
type fakeSlash struct {
	dispatched  []string // sc.Event.Text of each Dispatch call, in order
	sawAdmin    []bool   // sc.IsAdmin observed per call
	err         error    // returned by every Dispatch (a failed batch keeps the token)
	reply       string   // written to the command's sink (folds into the receipt)
	sawDeadline bool     // the ctx carried a deadline
}

func (f *fakeSlash) Register(contract.SlashCommand) {}
func (f *fakeSlash) List() []contract.SlashCommand  { return nil }
func (f *fakeSlash) Dispatch(ctx context.Context, sc contract.SlashContext) error {
	f.dispatched = append(f.dispatched, sc.Event.Text)
	f.sawAdmin = append(f.sawAdmin, sc.IsAdmin)
	_, f.sawDeadline = ctx.Deadline()
	if f.reply != "" {
		_ = sc.Sink.Finish(f.reply)
	}
	return f.err
}

// newScreener builds a screener over a real claim-token facility and the given slash
// registry (the batch executor). The facility is returned so a test can Mint a token.
func newScreener(p Policy, sl contract.SlashRegistry) (*screener, *claimtoken.Module) {
	f := claimtoken.New()
	return &screener{
		policies: map[string]Policy{"tg": p},
		slash:    sl,
		tokens:   f,
		rejected: NewRejectedSenders(10),
	}, f
}

func ev(user, text string) contract.MessageEvent {
	return contract.MessageEvent{Source: "tg", ChatID: "c", UserID: user, Text: text, ChatType: contract.ChatDM}
}

func batchTok(f *claimtoken.Module, ttl time.Duration, asAdmin bool, cmds ...string) string {
	return f.Mint("test-code", contract.BatchPayload{Commands: cmds, AsAdmin: asAdmin}, ttl)
}

// TestScreen_Order pins the gate order (docs/accounts.md §6.1):
// denylist > token redeem > allowlist. The screen Verifies the token once and runs its
// batch of commands; an all-success run consumes the token + returns Redeemed.
func TestScreen_Order(t *testing.T) {
	ctx := context.Background()
	p := Policy{Allowlist: IDList{"alice"}, Denylist: IDList{"blocked"}}
	sl := &fakeSlash{reply: "bound"}
	s, f := newScreener(p, sl)
	code := batchTok(f, time.Minute, true, "/gate allow tg c stranger")

	// denied wins even when the text is a live token — the redeem step is never reached.
	if v := s.Screen(ctx, ev("blocked", code)); v.Action != contract.ScreenDrop || v.Reason != contract.ReasonDenied {
		t.Fatalf("denied sender: got %+v, want Drop/Denied", v)
	}
	if len(sl.dispatched) != 0 {
		t.Fatalf("batch must not run for a denied sender (dispatched=%v)", sl.dispatched)
	}

	// a live token from an un-allowlisted sender runs its batch (bypasses allowlist) + is consumed.
	v := s.Screen(ctx, ev("stranger", code))
	if v.Action != contract.ScreenRedeemed || !strings.Contains(v.Reply, "bound") {
		t.Fatalf("live token: got %+v, want Redeemed with the command's reply in the receipt", v)
	}
	// a NON-admin redeemer must NOT see the raw admin command / internal ids (info hygiene).
	if strings.Contains(v.Reply, "/gate allow") {
		t.Fatalf("non-admin receipt must not echo the raw command: %q", v.Reply)
	}
	if len(sl.dispatched) != 1 || sl.dispatched[0] != "/gate allow tg c stranger" {
		t.Fatalf("batch dispatched = %v, want the one frozen command", sl.dispatched)
	}
	if _, _, ok := f.Verify(code); ok {
		t.Fatal("an all-success batch must consume the token")
	}

	// an allowlisted sender with non-token text passes.
	if v := s.Screen(ctx, ev("alice", "hi")); v.Action != contract.ScreenPass {
		t.Fatalf("allowlisted: got %+v, want Pass", v)
	}

	// an un-allowlisted sender with non-token text is dropped (unauthorized) and recorded.
	if v := s.Screen(ctx, ev("stranger", "hi")); v.Action != contract.ScreenDrop || v.Reason != contract.ReasonUnauthorized {
		t.Fatalf("stranger: got %+v, want Drop/Unauthorized", v)
	}
	got := s.rejected.List()
	if len(got) != 1 || got[0].UserID != "stranger" {
		t.Fatalf("rejected feed = %+v, want exactly 1 entry (stranger); denied must NOT be recorded", got)
	}
}

// TestScreen_BatchFailKeepsToken pins the consume policy: a batch with a FAILED command
// keeps the token (original TTL) so the redeemer can retry the idempotent batch. Redeemed
// by a REAL admin so the receipt is delivered (see TestScreen_IneligibleRedeemInert for
// the non-admin fall-through).
func TestScreen_BatchFailKeepsToken(t *testing.T) {
	ctx := context.Background()
	sl := &fakeSlash{err: errors.New("boom")}
	s, f := newScreener(Policy{Admins: IDList{"root"}}, sl)
	code := batchTok(f, time.Hour, true, "/gate allow tg c stranger")
	if v := s.Screen(ctx, ev("root", code)); v.Action != contract.ScreenRedeemed {
		t.Fatalf("admin redeem should return Redeemed even on a failed batch: %+v", v)
	}
	if _, _, ok := f.Verify(code); !ok {
		t.Fatal("a failed batch must KEEP the token for retry")
	}
}

// TestScreen_IneligibleRedeemInert pins the anti-oracle: a non-admin whose batch commits
// NOTHING (e.g. an applicant pasting their own AsAdmin=false bounce code — its AdminOnly
// /gate allow is rejected) stays INERT — the screen falls through to normal gating
// (Drop/bounce), never a Redeemed receipt that would leak the token's liveness, and the
// token is kept.
func TestScreen_IneligibleRedeemInert(t *testing.T) {
	ctx := context.Background()
	sl := &fakeSlash{err: errors.New("admin only")} // command rejected for the non-admin
	s, f := newScreener(Policy{Allowlist: IDList{"alice"}}, sl)
	code := batchTok(f, time.Hour, false, "/gate allow tg c stranger")
	v := s.Screen(ctx, ev("stranger", code))
	if v.Action != contract.ScreenDrop || v.Reason != contract.ReasonUnauthorized {
		t.Fatalf("ineligible redeem must fall through to normal gating (Drop), got %+v", v)
	}
	if _, _, ok := f.Verify(code); !ok {
		t.Fatal("an inert redemption must keep the token")
	}
}

// TestScreen_AsAdminAuthority pins §3: AsAdmin=false runs the batch under the redeemer's
// REAL admin status (an applicant can't self-admit); AsAdmin=true runs it as admin for
// whoever redeems.
func TestScreen_AsAdminAuthority(t *testing.T) {
	ctx := context.Background()
	p := Policy{Admins: IDList{"root"}}

	// AsAdmin=false, non-admin redeemer → command runs with IsAdmin=false.
	sl := &fakeSlash{}
	s, f := newScreener(p, sl)
	s.Screen(ctx, ev("stranger", batchTok(f, time.Hour, false, "/gate allow tg c stranger")))
	if len(sl.sawAdmin) != 1 || sl.sawAdmin[0] {
		t.Fatalf("AsAdmin=false + non-admin redeemer: sawAdmin=%v, want [false]", sl.sawAdmin)
	}

	// AsAdmin=false, REAL admin redeemer → command runs with IsAdmin=true.
	sl2 := &fakeSlash{}
	s2, f2 := newScreener(p, sl2)
	s2.Screen(ctx, ev("root", batchTok(f2, time.Hour, false, "/gate allow tg c stranger")))
	if len(sl2.sawAdmin) != 1 || !sl2.sawAdmin[0] {
		t.Fatalf("AsAdmin=false + real admin: sawAdmin=%v, want [true]", sl2.sawAdmin)
	}

	// AsAdmin=true, non-admin redeemer → command runs with IsAdmin=true (bearer vouch).
	sl3 := &fakeSlash{}
	s3, f3 := newScreener(p, sl3)
	s3.Screen(ctx, ev("stranger", batchTok(f3, time.Hour, true, "/accounts bindto x")))
	if len(sl3.sawAdmin) != 1 || !sl3.sawAdmin[0] {
		t.Fatalf("AsAdmin=true + non-admin: sawAdmin=%v, want [true]", sl3.sawAdmin)
	}
}

// TestScreen_NoTokenSkips verifies that text which isn't a live token simply skips the
// redeem step rather than panicking.
func TestScreen_NoTokenSkips(t *testing.T) {
	s, _ := newScreener(Policy{AllowAll: true}, &fakeSlash{})
	if v := s.Screen(context.Background(), ev("anyone", "CODE")); v.Action != contract.ScreenPass {
		t.Fatalf("non-token text: got %+v, want Pass", v)
	}
}

// TestScreen_RedeemDeadline pins F115: each batched command is dispatched under a bounded
// ctx so a slow store can't wedge the single inbound consume loop.
func TestScreen_RedeemDeadline(t *testing.T) {
	ctx := context.Background()
	sl := &fakeSlash{}
	s, f := newScreener(Policy{Allowlist: IDList{"alice"}}, sl)
	tok := batchTok(f, time.Hour, true, "/gate allow tg c stranger")
	if v := s.Screen(ctx, ev("stranger", tok)); v.Action != contract.ScreenRedeemed {
		t.Fatalf("redeem should pass: %+v", v)
	}
	if !sl.sawDeadline {
		t.Fatal("each batched command must run under a bounded (deadline) ctx (F115)")
	}
}

// TestMintBounce_RetiresPreviousToken pins D7: a bounceReplyTTL re-arm mints a
// FRESH admit token and Consumes the one the feed entry recorded, so any
// (source,chat,user) holds at most one live admit token — a chatty stranger
// can't accrue a pile of live tokens. Only the latest bounce's token redeems.
func TestMintBounce_RetiresPreviousToken(t *testing.T) {
	ctx := context.Background()
	s, f := newScreener(Policy{Allowlist: IDList{"alice"}}, nil)
	base := time.Unix(1_000_000, 0)
	now := base
	s.rejected.now = func() time.Time { return now }

	if v := s.Screen(ctx, ev("stranger", "hi")); v.Action != contract.ScreenDrop || v.Reply == "" {
		t.Fatalf("first bounce: %+v, want Drop with reply", v)
	}
	tok1 := s.rejected.List()[0].Token
	if _, _, ok := f.Verify(tok1); !ok {
		t.Fatal("first admit token must be live")
	}

	now = base.Add(bounceReplyTTL) // past the cooldown → the bounce re-arms
	if v := s.Screen(ctx, ev("stranger", "hi again")); v.Action != contract.ScreenDrop || v.Reply == "" {
		t.Fatalf("re-armed bounce: %+v, want Drop with reply", v)
	}
	tok2 := s.rejected.List()[0].Token
	if tok2 == "" || tok2 == tok1 {
		t.Fatalf("re-arm must mint a fresh token (tok1=%q tok2=%q)", tok1, tok2)
	}
	if _, _, ok := f.Verify(tok1); ok {
		t.Fatal("the replaced admit token must be consumed (one live token per sender)")
	}
	if _, _, ok := f.Verify(tok2); !ok {
		t.Fatal("the replacement admit token must be live")
	}
}

// TestAuthorized_TwoTier pins the per-group override semantics.
func TestAuthorized_TwoTier(t *testing.T) {
	p := Policy{
		Allowlist: IDList{"alice", "carol"},
		Groups: map[string]Group{
			"open":   {AllowAll: true},
			"add":    {AllowlistAdd: IDList{"bob"}},
			"strict": {Allowlist: IDList{"carol"}},
			"banned": {Denylist: IDList{"alice"}},
		},
	}
	cases := []struct {
		chat, user string
		want       bool
	}{
		{"dm", "alice", true},      // source-level allowlist
		{"dm", "bob", false},       // not allowed anywhere
		{"open", "stranger", true}, // per-chat allow_all opens the room
		{"add", "bob", true},       // per-chat union grant on top of source allowlist
		{"strict", "alice", false}, // per-chat allowlist (AND) restricts alice out
		{"strict", "carol", true},  // source-allowed AND in the per-chat allowlist
		{"banned", "alice", false}, // per-chat denylist wins over source allowlist
	}
	for _, c := range cases {
		if got := p.Authorized(c.chat, c.user); got != c.want {
			t.Errorf("Authorized(%q,%q) = %v, want %v", c.chat, c.user, got, c.want)
		}
	}
}

func TestIsAdmin(t *testing.T) {
	p := Policy{Admins: IDList{"root"}, Groups: map[string]Group{"g": {Admins: IDList{"mod"}}}}
	s, _ := newScreener(p, nil)
	if !s.IsAdmin("tg", "any", "root") {
		t.Error("source-level admin not recognized")
	}
	if !s.IsAdmin("tg", "g", "mod") {
		t.Error("per-chat admin not recognized in its chat")
	}
	if s.IsAdmin("tg", "other", "mod") {
		t.Error("per-chat admin leaked outside its chat")
	}
	if s.IsAdmin("unknown", "g", "mod") {
		t.Error("unknown source must have no admins (closed default)")
	}
}

func TestLoadAll(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("telegram.yaml", "allow_all: false\nallowlist: [\"alice\"]\nadmins: [\"root\"]\n")
	write("feishu.yaml", "allow_all: true\n")

	got, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d policies, want 2", len(got))
	}
	if !got["telegram"].Allowlist.Contains("alice") || !got["telegram"].Admins.Contains("root") {
		t.Errorf("telegram policy not parsed: %+v", got["telegram"])
	}
	if !got["feishu"].AllowAll {
		t.Errorf("feishu allow_all not parsed: %+v", got["feishu"])
	}

	// missing dir → empty map, no error.
	empty, err := LoadAll(filepath.Join(dir, "nope"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("missing dir: got (%v, %v), want (empty, nil)", empty, err)
	}
}

// TestModule_ThroughTrunk drives the gate module via the trunk and exercises
// the registered contract.Screener + hot reload.
// panelStub is a no-op contract.PanelRegistry so the gate can Start in isolation
// (in production the webui module provides the real one, which the gate Needs).
type panelStub struct{}

func (panelStub) RegisterPanel(contract.Panel) {}
func (panelStub) Panels() []contract.Panel     { return nil }

// slashStub is a no-op contract.SlashRegistry so the gate can Start in isolation
// (it registers /gate at Start).
type slashStub struct{}

func (slashStub) Register(contract.SlashCommand)                        {}
func (slashStub) Dispatch(context.Context, contract.SlashContext) error { return nil }
func (slashStub) List() []contract.SlashCommand                         { return nil }

// tokenStub is a minimal contract.ClaimTokens for the gate's hard Need.
type tokenStub struct{}

func (tokenStub) Mint(string, any, time.Duration) string { return "stub-admit-token" }
func (tokenStub) Verify(string) (string, any, bool)      { return "", nil, false }
func (tokenStub) Consume(string)                         {}

func regWithPanels() *trunk.Registry {
	reg := trunk.NewRegistry()
	trunk.Provide[contract.PanelRegistry](reg, panelStub{})
	trunk.Provide[contract.SlashRegistry](reg, slashStub{})
	trunk.Provide[contract.ClaimTokens](reg, tokenStub{})
	return reg
}

func TestModule_ThroughTrunk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tg.yaml"), []byte("allowlist: [\"alice\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := regWithPanels()
	m := New(dir)
	if err := m.Start(context.Background(), reg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.Health() != trunk.StateReady {
		t.Fatalf("Health = %v, want Ready", m.Health())
	}

	scr := trunk.Require[contract.Screener](reg)
	if v := scr.Screen(context.Background(), ev("alice", "hi")); v.Action != contract.ScreenPass {
		t.Fatalf("alice before reload: %+v, want Pass", v)
	}
	if v := scr.Screen(context.Background(), ev("bob", "hi")); v.Action != contract.ScreenDrop {
		t.Fatalf("bob before reload: %+v, want Drop", v)
	}

	// hot reload: add bob to the allowlist on disk, reload, bob now passes.
	if err := os.WriteFile(filepath.Join(dir, "tg.yaml"), []byte("allowlist: [\"alice\", \"bob\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if v := scr.Screen(context.Background(), ev("bob", "hi")); v.Action != contract.ScreenPass {
		t.Fatalf("bob after reload: %+v, want Pass", v)
	}
}

// TestModule_BadPolicyFailsStart pins the criticality the gate buys with
// Optional()==false: a malformed policy file must FAIL Start (→ StateFailed),
// so the trunk aborts rather than running a Gated source with no screen
// (auth-bypass). Without this, flipping Optional to true would fail open
// silently.
func TestModule_BadPolicyFailsStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tg.yaml"), []byte("allowlist: [unterminated"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(dir)
	if m.Optional() {
		t.Fatal("gate must be critical (Optional=false)")
	}
	if err := m.Start(context.Background(), trunk.NewRegistry()); err == nil {
		t.Fatal("a malformed policy file must fail Start")
	}
	if m.Health() != trunk.StateFailed {
		t.Fatalf("Health after failed Start = %v, want StateFailed", m.Health())
	}
}

// TestRejectedSenders_EvictAndForget locks the ported feed's bounded capacity,
// dedup, and forget semantics — the non-trivial logic beyond Record/List.
func TestRejectedSenders_EvictAndForget(t *testing.T) {
	r := NewRejectedSenders(2) // cap 2 distinct (source,chat,user)
	r.Record("tg", "c1", "u1", "U1", contract.ChatDM)
	r.Record("tg", "c1", "u1", "U1", contract.ChatDM) // dedup → bumps count, no new entry
	if got := r.List(); len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("after dedup: %+v, want 1 entry count=2", got)
	}
	r.Record("tg", "c2", "u2", "U2", contract.ChatDM) // now 2 distinct
	r.Record("tg", "c3", "u3", "U3", contract.ChatDM) // over cap → evict oldest (u1)
	got := r.List()
	if len(got) != 2 {
		t.Fatalf("over cap: %d entries, want 2 (eviction)", len(got))
	}
	for _, e := range got {
		if e.UserID == "u1" {
			t.Fatalf("oldest entry u1 should have been evicted: %+v", got)
		}
	}

	// empty userID is ignored (the id is the whole point).
	r2 := NewRejectedSenders(5)
	r2.Record("tg", "c", "", "anon", contract.ChatDM)
	if len(r2.List()) != 0 {
		t.Fatal("empty userID must not be recorded")
	}

	// ForgetUser drops every chat for (source,user); ForgetUserInChat drops only the
	// one (source,chat,user) — the per-chat grant's scope (L-gate-D1).
	r3 := NewRejectedSenders(10)
	r3.Record("tg", "cA", "u1", "U1", contract.ChatGroup)
	r3.Record("tg", "cB", "u1", "U1", contract.ChatGroup)
	r3.Record("tg", "cA", "u2", "U2", contract.ChatGroup)
	// Per-chat grant: u1 admitted only in cA → cA/u1 forgotten, cB/u1 + cA/u2 remain.
	r3.ForgetUserInChat("tg", "cA", "u1")
	if got := r3.List(); len(got) != 2 {
		t.Fatalf("after ForgetUserInChat(cA,u1): %+v, want cB/u1 + cA/u2 (2)", got)
	}
	// ForgetUser drops u1's remaining chat too, leaving only u2.
	r3.ForgetUser("tg", "u1")
	if got := r3.List(); len(got) != 1 || got[0].UserID != "u2" {
		t.Fatalf("after ForgetUser(u1): %+v, want only u2", got)
	}
}

// TestRejectedSenders_ForgetEmailCaseInsensitive pins F25×F116: allowlist
// matching went idEqual (email addresses compare case-insensitively), so the
// feed's forget must follow. The email source records lowercase From; an admin
// allowlisting the mixed-case spelling must still clear the feed row, or the
// ghost row F116 killed comes back. Platform ids (no "@") stay exact-match.
func TestRejectedSenders_ForgetEmailCaseInsensitive(t *testing.T) {
	// ForgetUser: mixed-case grant clears the lowercase row across all chats.
	r := NewRejectedSenders(10)
	r.Record("email", "alice@corp.example", "alice@corp.example", "Alice", contract.ChatDM)
	r.Record("email", "list@corp.example", "alice@corp.example", "Alice", contract.ChatGroup)
	r.ForgetUser("email", "Alice@Corp.Example")
	if got := r.List(); len(got) != 0 {
		t.Fatalf("ForgetUser(mixed-case email) left ghost rows: %+v", got)
	}

	// ForgetUserInChat: mixed-case grant clears exactly the one chat's row.
	r2 := NewRejectedSenders(10)
	r2.Record("email", "cA", "alice@corp.example", "Alice", contract.ChatGroup)
	r2.Record("email", "cB", "alice@corp.example", "Alice", contract.ChatGroup)
	r2.ForgetUserInChat("email", "cA", "ALICE@corp.example")
	got := r2.List()
	if len(got) != 1 || got[0].ChatID != "cB" {
		t.Fatalf("ForgetUserInChat(mixed-case email) = %+v, want only cB row", got)
	}

	// Platform ids never contain "@": case still distinguishes (exact match).
	r3 := NewRejectedSenders(10)
	r3.Record("slack", "c1", "Uabc", "U", contract.ChatDM)
	r3.ForgetUser("slack", "uABC")
	if got := r3.List(); len(got) != 1 {
		t.Fatalf("platform id forget must stay case-sensitive, got %+v", got)
	}
	r3.ForgetUserInChat("slack", "c1", "uABC")
	if got := r3.List(); len(got) != 1 {
		t.Fatalf("platform id per-chat forget must stay case-sensitive, got %+v", got)
	}
}
