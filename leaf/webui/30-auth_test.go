package webui

import (
	"testing"

	"agentbob/leaf/claimtoken"
)

// TestAuth_RemintVoidsPriorToken pins the per-coverage hard reset preserved across the
// claim-token migration: re-minting a coverage VOIDS its prior pending token (so a
// leaked-but-unsubmitted old token is dead), and a fresh token still unlocks.
func TestAuth_RemintVoidsPriorToken(t *testing.T) {
	a := NewAuth(claimtoken.New())

	old := a.Mint() // admin "*"
	if old == "" {
		t.Fatal("first mint returned empty")
	}
	fresh := a.Mint() // re-mint → old must be voided
	if fresh == "" || fresh == old {
		t.Fatalf("re-mint must issue a NEW token, got old=%q fresh=%q", old, fresh)
	}
	if _, ok := a.SubmitToken(old); ok {
		t.Fatal("the prior token must be VOID after a re-mint")
	}
	cookie, ok := a.SubmitToken(fresh)
	if !ok || cookie == "" {
		t.Fatal("the fresh token must unlock")
	}
	if !a.ValidAdmin(cookie) {
		t.Fatal("the unlocked session must be admin")
	}
}

// TestAuth_SingleUseAndWrongKind: a token unlocks exactly once, and a token of another
// claim-token kind (sharing the facility) is rejected at submit.
func TestAuth_SingleUseAndWrongKind(t *testing.T) {
	f := claimtoken.New()
	a := NewAuth(f)

	tok := a.MintScoped("scope-1")
	if _, ok := a.SubmitToken(tok); !ok {
		t.Fatal("first submit must succeed")
	}
	if _, ok := a.SubmitToken(tok); ok {
		t.Fatal("a spent token must not unlock again (single-use)")
	}

	// A foreign-kind token in the SAME facility must not be accepted as a webui unlock.
	foreign := f.Mint("gate-admit", "payload", tokenTTL)
	if _, ok := a.SubmitToken(foreign); ok {
		t.Fatal("a non-webui-unlock token must be rejected")
	}
}

// TestAuth_ScopedCoverage: a scoped session authorizes only its exact scope, never
// another scope and never admin endpoints.
func TestAuth_ScopedCoverage(t *testing.T) {
	a := NewAuth(claimtoken.New())
	cookie, ok := a.SubmitToken(a.MintScoped("scope-A"))
	if !ok {
		t.Fatal("scoped unlock failed")
	}
	if !a.Authorize(cookie, "scope-A") {
		t.Fatal("scoped session must authorize its own scope")
	}
	if a.Authorize(cookie, "scope-B") {
		t.Fatal("scoped session must NOT authorize another scope")
	}
	if a.ValidAdmin(cookie) {
		t.Fatal("scoped session must NOT be admin")
	}
}
