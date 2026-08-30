package agora

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentbob/leaf/agora/store"
)

// TestMemberFailStore: snapshots ring as files per (company,role); since reads + sweeps;
// pairs enumerates (co,role) that have failures.
func TestMemberFailStore(t *testing.T) {
	s := &memberFailStore{base: t.TempDir()}
	s.put("co_acme", "boss", "轨迹 A")
	s.put("co_acme", "boss", "轨迹 B")
	s.put("co_globex", "agent", "轨迹 C")
	s.put("", "boss", "ignored") // blank co → safeSeg makes it "_", still records; ok

	got := s.since("co_acme", "boss", time.Time{})
	if len(got) != 2 {
		t.Fatalf("want 2 snapshots for boss@acme, got %d", len(got))
	}
	for _, g := range got {
		if g.OK || g.Snapshot == "" {
			t.Fatalf("malformed member trace: %+v", g)
		}
	}
	// pairs enumerates roles with failures (boss@acme, agent@globex, _@boss from blank).
	if p := s.pairs(); len(p) < 2 {
		t.Fatalf("pairs should list the (co,role) with failures, got %v", p)
	}
	// Watermark sweep: since(>= newest) consumes + deletes.
	maxAt := got[len(got)-1].At
	if g := s.since("co_acme", "boss", maxAt); len(g) != 0 {
		t.Fatalf("since(watermark) should be empty (swept), got %d", len(g))
	}
	if g := s.since("co_acme", "boss", time.Time{}); len(g) != 0 {
		t.Fatal("swept files should be gone")
	}
	// other role untouched
	if g := s.since("co_globex", "agent", time.Time{}); len(g) != 1 {
		t.Fatalf("other role untouched, got %d", len(g))
	}
}

func TestMemberInsightStore(t *testing.T) {
	s := &memberInsightStore{base: t.TempDir()}
	if err := s.put("co_acme", "boss", "做X要小心"); err != nil {
		t.Fatal(err)
	}
	if s.read("co_acme", "boss") != "做X要小心" {
		t.Fatalf("read = %q", s.read("co_acme", "boss"))
	}
	if s.read("co_acme", "worker") != "" {
		t.Fatal("unwritten role → empty")
	}
	// empty removes
	if err := s.put("co_acme", "boss", "  "); err != nil {
		t.Fatal(err)
	}
	if s.read("co_acme", "boss") != "" {
		t.Fatal("empty guidance should remove the file")
	}
}

// TestMemberResolveAndRecord: a member with TWO active roles → resolveRoles returns both,
// RecordFailure rings the snapshot under each, guidanceForScope concatenates their text.
func TestMemberResolveAndRecord(t *testing.T) {
	org, loader := seedOrg(t)
	// seedOrg's alice = boss@co_acme; add a second active role for multi-role coverage.
	org.UpsertEmployment(store.Employment{ID: "e_alice2", MemberID: "m_alice", CompanyID: "co_globex", RoleName: "agent", Status: "active"})
	router := NewInboxRouter(org, loader)
	if err := router.Reload(context.Background()); err != nil {
		t.Fatalf("router reload: %v", err)
	}
	m := newMemberLearn(t.TempDir(), org, router)
	scope := MemberOwnedInboxScope("alice", "co_acme")

	roles := m.resolveRoles(scope)
	if len(roles) != 2 {
		t.Fatalf("alice holds 2 active roles, resolveRoles got %v", roles)
	}

	m.RecordFailure(context.Background(), scope, "失败轨迹")
	if p := m.fail.pairs(); len(p) != 2 {
		t.Fatalf("RecordFailure should ring under BOTH roles, got %v", p)
	}

	// guidance injection: put guidance on one role → guidanceForMember picks it up.
	_ = m.ins.put("co_acme", "boss", "boss 经验：先确认再汇总")
	g := m.guidanceForMember(m.memberIDForScope(scope))
	if !strings.Contains(g, "先确认再汇总") {
		t.Fatalf("guidanceForMember should inject the role's guidance, got %q", g)
	}

	// A non-member / unknown scope resolves to nothing (no panic, no record).
	if r := m.resolveRoles("telegram:dm:nobody"); len(r) != 0 {
		t.Fatalf("unknown scope → no roles, got %v", r)
	}
}
