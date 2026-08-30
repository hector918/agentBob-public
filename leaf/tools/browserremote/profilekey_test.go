package browserremote

import (
	"context"
	"testing"

	"agentbob/contract"
)

// TestMasterKey pins the LOGIN-STORE axis: the flow-stamped MEMBER wins, then the
// PRINCIPAL (per-role fallback), then the scope, then the sid. This is which master
// profile a worker's copy seeds its login from.
func TestMasterKey(t *testing.T) {
	tc := contract.ToolContext{Scope: "discord:group:1", Sid: "s_xyz"}

	// member (finest) wins
	ctx := contract.WithMember(contract.WithIdentity(context.Background(), "company:acme/role:shopper"), "member:alice")
	if got := masterKey(ctx, tc); got != "member:alice" {
		t.Fatalf("member should win, got %q", got)
	}
	// no member → principal (per-role)
	ctx = contract.WithIdentity(context.Background(), "company:acme/role:shopper")
	if got := masterKey(ctx, tc); got != "company:acme/role:shopper" {
		t.Fatalf("principal fallback, got %q", got)
	}
	// neither → scope, then sid
	if got := masterKey(context.Background(), tc); got != "discord:group:1" {
		t.Fatalf("scope fallback, got %q", got)
	}
	if got := masterKey(context.Background(), contract.ToolContext{Sid: "s_xyz"}); got != "s_xyz" {
		t.Fatalf("sid fallback, got %q", got)
	}
}

// TestCopyScope pins the per-COPY instance axis: the worker's scope (concurrent workers =
// different scopes), falling back to the sid outside a routed turn.
func TestCopyScope(t *testing.T) {
	if got := CopyScope(contract.ToolContext{Scope: "discord:group:1", Sid: "s_xyz"}); got != "discord:group:1" {
		t.Fatalf("scope wins, got %q", got)
	}
	if got := CopyScope(contract.ToolContext{Sid: "s_xyz"}); got != "s_xyz" {
		t.Fatalf("sid fallback, got %q", got)
	}
}
