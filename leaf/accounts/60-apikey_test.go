package accounts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/accounts/store"
)

// TestMintVerifyRoundTrip: a minted key's plaintext verifies once, resolves the
// key's policy, binds a billing handle, and stops verifying after revocation.
func TestMintVerifyRoundTrip(t *testing.T) {
	st := newMemStore()
	m := bindingManager(st, nil)
	ctx := context.Background()
	acc, _ := st.CreateAccount(ctx, "friend", "", 1)

	token, key, err := m.MintAPIKey(ctx, acc.ID, "nas open-webui",
		APIKeyPolicy{Kinds: []string{contract.KindLLM, contract.KindTranslate}})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(token, "sk-bob-") {
		t.Errorf("token = %q, want sk-bob- prefix", token)
	}
	// The lane allowlist round-trips — modelgate reads it off the verified key, so a
	// dropped column would silently re-route (or strand) traffic.
	if len(key.Kinds) != 2 || key.Kinds[0] != contract.KindLLM || len(key.Models) != 0 {
		t.Errorf("key policy not stored: %+v", key)
	}

	// The plaintext verifies to the key's policy.
	info, ok := m.VerifyKey(ctx, token)
	if !ok {
		t.Fatal("VerifyKey should accept the freshly minted token")
	}
	if info.ID != key.ID || info.AccountID != acc.ID {
		t.Errorf("verified info mismatch: %+v", info)
	}
	if len(info.Kinds) != 2 || info.Kinds[1] != contract.KindTranslate {
		t.Errorf("verified kinds = %v, want [llm translate]", info.Kinds)
	}

	// Minting bound a billing handle (source "apikey", uid = key-id) so spend rolls up.
	h, bound, _ := st.HandleBySourceUID(ctx, apiKeyHandleSource, key.ID)
	if !bound || h.AccountID != acc.ID {
		t.Errorf("billing handle not bound to account: bound=%v handle=%+v", bound, h)
	}

	// A wrong token never verifies.
	if _, ok := m.VerifyKey(ctx, "sk-bob-deadbeef"); ok {
		t.Error("an unknown token must not verify")
	}

	// Revoke → the same token stops verifying.
	if err := m.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := m.VerifyKey(ctx, token); ok {
		t.Error("a revoked key must not verify")
	}
}

// TestMintRollsBackOnBindFailure: if the billing-handle bind fails, mint is
// atomic — it returns the error, hands out NO token, and leaves no verifiable key
// (a working-but-unbilled credential would strand spend forever).
func TestMintRollsBackOnBindFailure(t *testing.T) {
	st := newMemStore()
	st.bindErr = errors.New("boom")
	m := bindingManager(st, nil)
	ctx := context.Background()
	acc, _ := st.CreateAccount(ctx, "friend", "", 1)

	token, _, err := m.MintAPIKey(ctx, acc.ID, "", APIKeyPolicy{Kinds: []string{contract.KindLLM}})
	if err == nil {
		t.Fatal("mint should fail when the billing-handle bind fails")
	}
	if token != "" {
		t.Errorf("no token should be returned on a failed mint, got %q", token)
	}
	// Every key the mint created must be rolled back (none verifiable).
	keys, _ := st.ListAPIKeys(ctx, acc.ID)
	for _, k := range keys {
		if k.Status == "active" {
			t.Errorf("an active key survived a failed mint: %+v", k)
		}
	}
}

// TestVerifyCoalescesTouch: VerifyKey runs per request but the last_used_at write
// is coalesced — a burst of verifies touches the row at most once.
func TestVerifyCoalescesTouch(t *testing.T) {
	st := newMemStore()
	m := bindingManager(st, nil)
	ctx := context.Background()
	acc, _ := st.CreateAccount(ctx, "friend", "", 1)
	token, _, err := m.MintAPIKey(ctx, acc.ID, "", APIKeyPolicy{Kinds: []string{contract.KindLLM}})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, ok := m.VerifyKey(ctx, token); !ok {
			t.Fatal("verify should succeed")
		}
	}
	if st.touchCount != 1 {
		t.Errorf("touch writes = %d, want 1 (coalesced within the window)", st.touchCount)
	}
}

// TestMintUnknownAccountRejected: minting for a missing account fails, no key.
func TestMintUnknownAccountRejected(t *testing.T) {
	st := newMemStore()
	m := bindingManager(st, nil)
	if _, _, err := m.MintAPIKey(context.Background(), "ac_nope", "", APIKeyPolicy{Kinds: []string{contract.KindLLM}}); err != store.ErrAccountNotFound {
		t.Errorf("mint for unknown account: err = %v, want ErrAccountNotFound", err)
	}
}

// TestPausedAccountDisablesKeys: a paused account's key stops verifying.
func TestPausedAccountDisablesKeys(t *testing.T) {
	st := newMemStore()
	m := bindingManager(st, nil)
	ctx := context.Background()
	acc, _ := st.CreateAccount(ctx, "friend", "", 1)
	token, _, err := m.MintAPIKey(ctx, acc.ID, "", APIKeyPolicy{Models: []string{"big"}})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := m.VerifyKey(ctx, token); !ok {
		t.Fatal("active account's key should verify")
	}
	_ = st.SetAccountStatus(ctx, acc.ID, "paused", 2)
	if _, ok := m.VerifyKey(ctx, token); ok {
		t.Error("a paused account's key must not verify")
	}
}

// TestAPIKeyMintPolicyGuards pins the mint-time guards a wrong policy would otherwise
// turn into a key that looks fine and can never route: a written-but-empty flag must
// NOT be read as "take the default", the two forms are exclusive, and a typo'd lane
// name would match no entry ever.
func TestAPIKeyMintPolicyGuards(t *testing.T) {
	ctx := context.Background()
	newCmd := func(args ...string) (*capSink, error) {
		st := newMemStore()
		acc, _ := st.CreateAccount(ctx, "friend", "", 1)
		m := bindingManager(st, nil)
		sink := &capSink{}
		sc := contract.SlashContext{IsAdmin: true, Sink: sink, Event: contract.MessageEvent{ChatType: contract.ChatDM}}
		return sink, m.cmdAPIKeyNew(ctx, sc, append([]string{acc.ID}, args...))
	}

	// No policy at all → the documented default (kinds=llm), minted fine.
	if sink, err := newCmd("nas key"); err != nil {
		t.Fatalf("bare mint should take the default: %v (%s)", err, sink.out)
	} else if !strings.Contains(sink.out, "slash.accounts.apikey_minted") {
		t.Errorf("bare mint reply = %q, want the minted receipt", sink.out)
	}
	// kinds= written but empty (an unset shell variable) is a mistake, not a default.
	if sink, err := newCmd("kinds=", "nas key"); err == nil {
		t.Errorf("empty kinds= should be refused, got reply %q", sink.out)
	} else if sink, _ := newCmd("models="); !strings.Contains(sink.out, "apikey_empty_allow") {
		t.Errorf("empty models= reply = %q, want the empty-policy message", sink.out)
	}
	// A multi-lane key is the whole point of v8 — it must mint.
	if sink, err := newCmd("kinds=llm,translate", "promptlib"); err != nil {
		t.Fatalf("multi-lane mint should succeed: %v (%s)", err, sink.out)
	}
	// The image lane mints like any other. It admits the /v1/images/* endpoints
	// rather than chat, and while it was missing from the valid set those endpoints
	// were unreachable: every request 403'd and no key could be made that wouldn't.
	if sink, err := newCmd("kinds=image", "promptlib draw"); err != nil {
		t.Fatalf("image-lane mint should succeed: %v (%s)", err, sink.out)
	}
	// An unknown lane never mints (it could never match an entry) — even alongside
	// a valid one, so a typo can't hide behind a good neighbour.
	if sink, err := newCmd("kinds=llm,embedding"); err == nil {
		t.Errorf("unknown lane should be refused, got %q", sink.out)
	} else if !strings.Contains(sink.out, "apikey_bad_kind") {
		t.Errorf("bad lane reply = %q", sink.out)
	}
	// The two forms are exclusive.
	if sink, err := newCmd("kinds=llm", "models=big"); err == nil {
		t.Errorf("kinds= with models= should be refused, got %q", sink.out)
	} else if !strings.Contains(sink.out, "apikey_new_conflict") {
		t.Errorf("conflict reply = %q", sink.out)
	}
}
