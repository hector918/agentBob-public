package accounts

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/i18n"
	"agentbob/leaf/accounts/store"
	"agentbob/leaf/claimtoken"
)

// memStore is an in-memory store.Store for binding tests: real account + handle
// bookkeeping, the rest stubbed. (The SQL impl is covered by store/40-pg_test.go.)
type memStore struct {
	accts   map[string]store.Account
	handles map[string]store.SourceHandle // key = source+"\x00"+uid
	keys    map[string]store.APIKey       // key-id → key
	keyHash map[string]string             // secret-hash → key-id
	seq     int

	bindErr    error // when set, BindHandle fails with it (mint-rollback test)
	touchCount int   // number of TouchAPIKey writes that actually ran (coalescing test)
}

func newMemStore() *memStore {
	return &memStore{accts: map[string]store.Account{}, handles: map[string]store.SourceHandle{}}
}

func hkey(source, uid string) string { return source + "\x00" + uid }

func (s *memStore) CreateAccount(_ context.Context, name, note string, now int64) (store.Account, error) {
	s.seq++
	a := store.Account{ID: "ac_" + strconv.Itoa(s.seq), DisplayName: name, Note: note, Flow: "normal", CreatedAt: now, UpdatedAt: now}
	s.accts[a.ID] = a
	return a, nil
}
func (s *memStore) CreateBareAccount(_ context.Context, name, onboardScope string, now int64) (store.Account, error) {
	s.seq++
	a := store.Account{ID: "ac_" + strconv.Itoa(s.seq), DisplayName: name, Flow: "", OnboardScope: onboardScope, CreatedAt: now, UpdatedAt: now}
	s.accts[a.ID] = a
	return a, nil
}
func (s *memStore) DeleteAccount(_ context.Context, id string) error {
	delete(s.accts, id)
	return nil
}
func (s *memStore) GetAccount(_ context.Context, id string) (store.Account, error) {
	a, ok := s.accts[id]
	if !ok {
		return store.Account{}, store.ErrAccountNotFound
	}
	return a, nil
}
func (s *memStore) ListAccounts(context.Context) ([]store.Account, error) { return nil, nil }
func (s *memStore) AccountRosterPage(context.Context, int, int) ([]store.Account, int64, error) {
	return nil, 0, nil
}
func (s *memStore) SetAccountFlow(_ context.Context, id, flow string, _ int64) error {
	a, ok := s.accts[id]
	if !ok {
		return store.ErrAccountNotFound
	}
	a.Flow = flow
	s.accts[id] = a
	return nil
}
func (s *memStore) SetAccountStatus(_ context.Context, id, status string, _ int64) error {
	a, ok := s.accts[id]
	if !ok {
		return store.ErrAccountNotFound
	}
	a.Status = status
	s.accts[id] = a
	return nil
}
func (s *memStore) BindHandle(_ context.Context, h store.SourceHandle, now int64) (store.SourceHandle, error) {
	if s.bindErr != nil {
		return store.SourceHandle{}, s.bindErr
	}
	s.seq++
	h.ID = "sh_" + strconv.Itoa(s.seq)
	h.BoundAt = now
	s.handles[hkey(h.Source, h.PlatformUID)] = h
	return h, nil
}
func (s *memStore) HandlesForAccount(_ context.Context, accountID string) ([]store.SourceHandle, error) {
	var out []store.SourceHandle
	for _, h := range s.handles {
		if h.AccountID == accountID {
			out = append(out, h)
		}
	}
	return out, nil
}
func (s *memStore) HandleCountsByAccount(context.Context) (map[string]int64, error) {
	out := make(map[string]int64)
	for _, h := range s.handles {
		out[h.AccountID]++
	}
	return out, nil
}
func (s *memStore) HandleBySourceUID(_ context.Context, source, uid string) (store.SourceHandle, bool, error) {
	h, ok := s.handles[hkey(source, uid)]
	return h, ok, nil
}
func (s *memStore) AddTurnUsage(context.Context, string, string, int64, int64, map[string]store.KindTokens, map[string]int64) error {
	return nil
}
func (s *memStore) AccountUsageBreakdown(context.Context, string) (store.AccountUsage, error) {
	return store.AccountUsage{}, nil
}
func (s *memStore) GetHandleLang(context.Context, string, string) (string, error)      { return "", nil }
func (s *memStore) SetHandleLangIfEmpty(context.Context, string, string, string) error { return nil }
func (s *memStore) CreateAPIKey(_ context.Context, k store.APIKey, secretHash string, now int64) (store.APIKey, error) {
	if s.keys == nil {
		s.keys = map[string]store.APIKey{}
		s.keyHash = map[string]string{}
	}
	s.seq++
	k.ID = "ak_" + strconv.Itoa(s.seq)
	k.Status = "active"
	k.CreatedAt = now
	s.keys[k.ID] = k
	s.keyHash[secretHash] = k.ID
	return k, nil
}
func (s *memStore) APIKeyByHash(_ context.Context, secretHash string) (store.APIKey, bool, error) {
	id, ok := s.keyHash[secretHash]
	if !ok {
		return store.APIKey{}, false, nil
	}
	k := s.keys[id]
	if k.Status != "active" {
		return store.APIKey{}, false, nil // revoked
	}
	if a, ok := s.accts[k.AccountID]; ok && a.Status == "paused" {
		return store.APIKey{}, false, nil // paused account disables its keys
	}
	return k, true, nil
}
func (s *memStore) TouchAPIKey(_ context.Context, id string, now int64) error {
	s.touchCount++
	if k, ok := s.keys[id]; ok {
		k.LastUsedAt = now
		s.keys[id] = k
	}
	return nil
}
func (s *memStore) ListAPIKeys(_ context.Context, accountID string) ([]store.APIKey, error) {
	var out []store.APIKey
	for _, k := range s.keys {
		if accountID == "" || k.AccountID == accountID {
			out = append(out, k)
		}
	}
	return out, nil
}
func (s *memStore) RevokeAPIKey(_ context.Context, id string) error {
	k, ok := s.keys[id]
	if !ok {
		return store.ErrAPIKeyNotFound
	}
	k.Status = "revoked"
	s.keys[id] = k
	return nil
}
func (s *memStore) Ping(context.Context) error { return nil }

// fakeGranter records allowlist writes.
type fakeGranter struct {
	allowed []string // "source\x00uid"
}

func newFakeGranter() *fakeGranter { return &fakeGranter{} }
func (g *fakeGranter) Allow(_ context.Context, source, uid string) (bool, error) {
	g.allowed = append(g.allowed, hkey(source, uid))
	return true, nil
}
func (g *fakeGranter) AllowInChat(_ context.Context, source, chatID, uid string) (bool, error) {
	g.allowed = append(g.allowed, hkey(source+":"+chatID, uid))
	return true, nil
}

func bindingManager(st store.Store, g contract.AccessGranter) *Manager {
	return &Manager{store: st, buffer: newHandleUsageBuffer(st), tokens: claimtoken.New(), ttl: codeTTL,
		granter: func() contract.AccessGranter { return g }}
}

func dmEv(source, uid, text string) contract.MessageEvent {
	return contract.MessageEvent{Source: source, UserID: uid, Text: text, ChatType: contract.ChatDM}
}

// redeemVia emulates the gate's batch redeem over the accounts slash: Verify the token →
// run each frozen "/accounts …" command through slashAccounts with the token's AsAdmin
// authority → Consume iff EVERY command succeeded. Returns the last reply + whether the
// token was consumed (ok=false / not-a-batch → "", false, no consume).
func redeemVia(m *Manager, ctx context.Context, ev contract.MessageEvent, code string) (reply string, consumed bool) {
	_, pl, ok := m.tokens.Verify(code)
	if !ok {
		return "", false
	}
	batch, isBatch := pl.(contract.BatchPayload)
	if !isBatch {
		return "", false
	}
	allOK := true
	for _, cmd := range batch.Commands {
		cev := ev
		cev.Text = cmd
		cs := &capSink{}
		args := strings.TrimSpace(strings.TrimPrefix(cmd, "/accounts"))
		if err := m.slashAccounts(ctx, contract.SlashContext{Event: cev, Args: args, Sink: cs, IsAdmin: batch.AsAdmin}); err != nil {
			allOK = false
		}
		reply = cs.out
	}
	if allOK {
		m.tokens.Consume(code)
		consumed = true
	}
	return reply, consumed
}

// TestSelfNewThenResolve: a user self-creates an account, the handle binds + auto-
// allowlists, and the flow resolves to it.
func TestSelfNewThenResolve(t *testing.T) {
	st, g := newMemStore(), newFakeGranter()
	m := bindingManager(st, g)
	ctx := context.Background()
	ev := dmEv("telegram", "u1", "")

	a, err := m.CreateAndBindSelf(ctx, ev, "Alice")
	if err != nil {
		t.Fatalf("CreateAndBindSelf: %v", err)
	}
	if id, ok, _ := m.AccountForHandle(ctx, "telegram", "u1"); !ok || id != a.ID {
		t.Errorf("AccountForHandle = %q/%v, want %s/true", id, ok, a.ID)
	}
	// Self-new is now BARE: no auto-allowlist (access pending an admin grant) and a
	// flow-less account — otherwise an onboarding-open source would let a stranger
	// /accounts new themselves into full access.
	if len(g.allowed) != 0 {
		t.Errorf("self-new must NOT auto-allowlist (access pending admin grant): %v", g.allowed)
	}
	if acct, _ := st.GetAccount(ctx, a.ID); acct.Flow != "" {
		t.Errorf("self-new account flow = %q, want \"\" (bare)", acct.Flow)
	}
	// Second self-new is refused (already bound).
	if _, err := m.CreateAndBindSelf(ctx, ev, "Again"); err != ErrHandleBoundElsewhere {
		t.Errorf("second self-new err = %v, want ErrHandleBoundElsewhere", err)
	}
}

// TestRedeemAdminCode: an admin-minted code binds a first-contact sender via the
// gate's chat-redeem dispatch, and auto-allowlists.
func TestRedeemAdminCode(t *testing.T) {
	st, g := newMemStore(), newFakeGranter()
	m := bindingManager(st, g)
	ctx := context.Background()
	a, _ := st.CreateAccount(ctx, "Team", "", nowUnix())

	code, err := m.MintForAdmin(ctx, a.ID)
	if err != nil {
		t.Fatalf("MintForAdmin: %v", err)
	}
	reply, consumed := redeemVia(m, ctx, dmEv("telegram", "newcomer", ""), code)
	if !consumed || reply == "" {
		t.Fatalf("redeem: consumed=%v reply=%q, want consumed with a reply", consumed, reply)
	}
	if id, ok, _ := m.AccountForHandle(ctx, "telegram", "newcomer"); !ok || id != a.ID {
		t.Errorf("after redeem, handle resolves to %q/%v, want %s", id, ok, a.ID)
	}
	if len(g.allowed) != 1 {
		t.Errorf("redeem should auto-allowlist once, got %v", g.allowed)
	}
	// A non-token message is not redeemed.
	if _, consumed := redeemVia(m, ctx, dmEv("telegram", "x", ""), "hello there"); consumed {
		t.Errorf("plain text should not redeem")
	}
	// The code is single-use: replaying it does not re-bind (consumed at the gate).
	if _, consumed := redeemVia(m, ctx, dmEv("telegram", "other", ""), code); consumed {
		t.Errorf("a spent code should not redeem again")
	}
}

// TestSelfCodeCannotRepoint: a self-minted code refuses to move a handle already
// bound to a different account; an admin code may.
func TestSelfCodeCannotRepoint(t *testing.T) {
	st, g := newMemStore(), newFakeGranter()
	m := bindingManager(st, g)
	ctx := context.Background()
	acA, _ := st.CreateAccount(ctx, "A", "", nowUnix())
	acB, _ := st.CreateAccount(ctx, "B", "", nowUnix())
	// u1 is bound to A.
	if _, _, err := m.bindAndAllowlist(ctx, dmEv("telegram", "u1", ""), acA.ID, "test"); err != nil {
		t.Fatalf("seed bind: %v", err)
	}

	selfCode, _ := m.MintForSelf(ctx, acB.ID)
	reply, consumed := redeemVia(m, ctx, dmEv("telegram", "u1", ""), selfCode)
	// A self-code (no "repoint") refuses to move a bound handle: the command errors, so the
	// batch is NOT all-success → the token is KEPT (idempotent retry), not consumed.
	if consumed {
		t.Fatalf("self-code repoint refusal must keep the token (batch failed), reply=%q", reply)
	}
	if id, _, _ := m.AccountForHandle(ctx, "telegram", "u1"); id != acA.ID {
		t.Errorf("self-code must not repoint: handle now %s, want %s (reply=%q)", id, acA.ID, reply)
	}

	adminCode, _ := m.MintForAdmin(ctx, acB.ID)
	if _, consumed := redeemVia(m, ctx, dmEv("telegram", "u1", ""), adminCode); !consumed {
		t.Fatalf("admin-code (repoint) redeem should succeed + consume")
	}
	if id, _, _ := m.AccountForHandle(ctx, "telegram", "u1"); id != acB.ID {
		t.Errorf("admin-code should repoint: handle now %s, want %s", id, acB.ID)
	}
}

// TestMintUnknownAccount: minting against a missing account errors.
func TestMintUnknownAccount(t *testing.T) {
	m := bindingManager(newMemStore(), newFakeGranter())
	if _, err := m.MintForAdmin(context.Background(), "ac_nope"); err == nil {
		t.Errorf("mint for unknown account should error")
	}
}

// TestRedeem_GroupGrantsPerChat (D9): a code redeemed in a GROUP auto-allowlists at
// THAT chat's scope (AllowInChat), so a per-group allowlist doesn't still block them.
func TestRedeem_GroupGrantsPerChat(t *testing.T) {
	st, g := newMemStore(), newFakeGranter()
	m := bindingManager(st, g)
	ctx := context.Background()
	a, _ := st.CreateAccount(ctx, "Team", "", nowUnix())
	code, _ := m.MintForAdmin(ctx, a.ID)
	ev := contract.MessageEvent{Source: "telegram", UserID: "newcomer", ChatID: "g9", ChatType: contract.ChatGroup}
	reply, consumed := redeemVia(m, ctx, ev, code)
	if !consumed || reply != i18n.T("slash.accounts.redeem_bound_ok", "") {
		t.Fatalf("group redeem: consumed=%v reply=%q, want consumed/bound_ok", consumed, reply)
	}
	if len(g.allowed) != 1 || g.allowed[0] != hkey("telegram:g9", "newcomer") {
		t.Fatalf("group redeem must grant per-chat (AllowInChat), got %v", g.allowed)
	}
}

// blipHandleStore wraps memStore to fail the handle read (a transient store blip).
type blipHandleStore struct {
	*memStore
	handleErr error
}

func (s *blipHandleStore) HandleBySourceUID(ctx context.Context, source, uid string) (store.SourceHandle, bool, error) {
	if s.handleErr != nil {
		return store.SourceHandle{}, false, s.handleErr
	}
	return s.memStore.HandleBySourceUID(ctx, source, uid)
}

// TestAccountFor_HandleReadBlip (F43): a store error on the HANDLE read must NOT be
// folded into "unbound" — the router would send an entitled member to intro (a false
// "pending admin approval" notice + a swallowed turn). A blip reports bound with
// FlowKnown=false (the account-row blip's D27 posture), while a genuinely unbound
// handle (no error) still reports ok=false.
func TestAccountFor_HandleReadBlip(t *testing.T) {
	st := &blipHandleStore{memStore: newMemStore()}
	m := bindingManager(st, newFakeGranter())
	ctx := context.Background()
	ev := dmEv("telegram", "u1", "")

	// Genuinely unbound, no store error → ok=false (intro is correct).
	if _, ok := m.AccountFor(ctx, ev); ok {
		t.Fatal("unbound handle must report ok=false")
	}

	// Bind to an entitled account, then blip the handle read.
	a, _ := st.CreateAccount(ctx, "Alice", "", nowUnix())
	if _, _, err := m.bindAndAllowlist(ctx, ev, a.ID, "test"); err != nil {
		t.Fatalf("seed bind: %v", err)
	}
	st.handleErr = errors.New("store blip")
	info, ok := m.AccountFor(ctx, ev)
	if !ok {
		t.Fatal("handle-read blip must not be reported as unbound (would route to intro)")
	}
	if info.FlowKnown {
		t.Error("handle-read blip must leave FlowKnown=false (flow UNREAD, not empty)")
	}
	if info.Status == "paused" {
		t.Errorf("handle-read blip must not report paused, got %+v", info)
	}

	// Blip clears → full info again (self-correcting next turn).
	st.handleErr = nil
	if info, ok := m.AccountFor(ctx, ev); !ok || !info.FlowKnown || info.ID != a.ID || info.Flow != "normal" {
		t.Errorf("after the blip clears, AccountFor = %+v/%v, want %s bound with Flow known", info, ok, a.ID)
	}
}

// TestRedeem_NoGranterReportsPending (D8): with no granter the bind succeeds but the
// reply is honest that access is pending — not a false "bound ok".
func TestRedeem_NoGranterReportsPending(t *testing.T) {
	st := newMemStore()
	m := &Manager{store: st, buffer: newHandleUsageBuffer(st), tokens: claimtoken.New(), ttl: codeTTL,
		granter: func() contract.AccessGranter { return nil }}
	ctx := context.Background()
	a, _ := st.CreateAccount(ctx, "Team", "", nowUnix())
	code, _ := m.MintForAdmin(ctx, a.ID)
	reply, consumed := redeemVia(m, ctx, dmEv("telegram", "newcomer", ""), code)
	if !consumed {
		t.Fatalf("redeem must succeed (bind committed) + consume")
	}
	if reply != i18n.T("slash.accounts.redeem_bound_pending", "") {
		t.Fatalf("no-granter redeem must report PENDING, got %q", reply)
	}
	if _, ok, _ := m.AccountForHandle(ctx, "telegram", "newcomer"); !ok {
		t.Fatal("handle must still bind even when access is pending")
	}
}
