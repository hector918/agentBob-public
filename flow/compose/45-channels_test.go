package compose

import (
	"context"
	"testing"

	"agentbob/contract"
)

// recWarrant records the space last handed to File/Exec so the test can assert
// what the gate let through (and that a refused space never reaches warrant).
type recWarrant struct{ lastSpace string }

func (r *recWarrant) Grants(context.Context, string) contract.GrantSet             { return nil }
func (r *recWarrant) Check(context.Context, string, string, string) (bool, string) { return true, "" }
func (r *recWarrant) Authorize(context.Context, contract.GrantSet, []contract.ToolSpec) contract.ToolSet {
	return nil
}
func (r *recWarrant) AuthorizeSkills(context.Context, contract.GrantSet, []contract.SkillInfo) contract.SkillSet {
	return nil
}
func (r *recWarrant) File(_ context.Context, _ string, space string) (contract.FileChannel, error) {
	r.lastSpace = space
	return nil, nil
}
func (r *recWarrant) Exec(_ context.Context, _ string, space string) (contract.ExecChannel, error) {
	r.lastSpace = space
	return nil, nil
}
func (r *recWarrant) Cut(string) {}
func (r *recWarrant) ResolveCredential(context.Context, contract.GrantSet, string) (any, error) {
	return nil, nil
}

// TestBoundChannelsSpaceGate pins the cross-tenant invariant: a turn can open its
// default space and any space in Allowed, and NOTHING else — a refused space never
// reaches warrant.
func TestBoundChannelsSpaceGate(t *testing.T) {
	ctx := context.Background()
	w := &recWarrant{}
	b := BoundChannels{
		W: w, Identity: "member:alice", DefSpace: "member:alice/inbox:1",
		Allowed: SpaceSet([]string{"company:acme", "company:globex"}),
	}

	if _, err := b.OpenFile(ctx, ""); err != nil || w.lastSpace != "member:alice/inbox:1" {
		t.Fatalf("empty space must resolve to the default (got space=%q err=%v)", w.lastSpace, err)
	}
	if _, err := b.OpenFile(ctx, "company:globex"); err != nil || w.lastSpace != "company:globex" {
		t.Fatalf("an Allowed space must pass through verbatim (got space=%q err=%v)", w.lastSpace, err)
	}

	w.lastSpace = "SENTINEL"
	if _, err := b.OpenFile(ctx, "company:evilcorp"); err == nil {
		t.Error("a space outside the Allowed set must be refused")
	}
	if w.lastSpace != "SENTINEL" {
		t.Errorf("a refused space must NOT reach warrant (warrant saw %q)", w.lastSpace)
	}
	if _, err := b.OpenExec(ctx, "telegram:dm:victim"); err == nil {
		t.Error("OpenExec must apply the same space gate")
	}

	// Normal (non-agora) turn: nil Allowed → only the default space is reachable.
	bn := BoundChannels{W: w, Identity: "user", DefSpace: "telegram:dm:bob"}
	if _, err := bn.OpenFile(ctx, "telegram:dm:other"); err == nil {
		t.Error("with nil Allowed, a non-default space must be refused")
	}
}

// TestBoundChannels_EmptyIdentityRefused pins D41: a turn with an EMPTY principal (the
// locked-down agora case — inbox unresolved) opens NO capability channel, even with a
// live warrant + default space; the refusal doesn't rely on the tool bag being empty,
// and a refused open never reaches warrant.
func TestBoundChannels_EmptyIdentityRefused(t *testing.T) {
	ctx := context.Background()
	w := &recWarrant{lastSpace: "SENTINEL"}
	b := BoundChannels{W: w, Identity: "", DefSpace: "member:ghost/inbox:1"} // empty identity, live warrant

	if _, err := b.OpenFile(ctx, ""); err != ErrUnidentified {
		t.Fatalf("empty-identity OpenFile = %v, want ErrUnidentified", err)
	}
	if _, err := b.OpenExec(ctx, ""); err != ErrUnidentified {
		t.Fatalf("empty-identity OpenExec = %v, want ErrUnidentified", err)
	}
	if _, err := b.Build(ctx, "wordpress"); err != ErrUnidentified {
		t.Fatalf("empty-identity Build = %v, want ErrUnidentified", err)
	}
	if w.lastSpace != "SENTINEL" {
		t.Errorf("an unidentified open must NOT reach warrant (warrant saw %q)", w.lastSpace)
	}
}
