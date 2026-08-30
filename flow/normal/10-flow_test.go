package normal

import (
	"testing"

	"agentbob/contract"
)

// fakeSkillSet is a minimal contract.SkillCatalog/SkillSet for the dump test.
type fakeSkillSet struct {
	infos  []contract.SkillInfo
	skills map[string]contract.Skill
	bodies map[string]string
}

func (s fakeSkillSet) List() []contract.SkillInfo               { return s.infos }
func (s fakeSkillSet) Get(n string) (contract.Skill, bool)      { sk, ok := s.skills[n]; return sk, ok }
func (s fakeSkillSet) Read(n string) (string, bool)             { b, ok := s.bodies[n]; return b, ok }
func (fakeSkillSet) Files(string) ([]contract.SkillFile, error) { return nil, nil }

// TestFlowStrategy pins the normal flow's STRATEGY: catch-all Accepts, Priority 0,
// SessionModeNone, and the admin/user principal mint from the gate stamp.
func TestFlowStrategy(t *testing.T) {
	f := &flow{}
	if !f.Accepts(contract.TurnWork{}) {
		t.Error("normal flow must accept everything (catch-all)")
	}
	if f.Priority() != 0 {
		t.Errorf("normal flow Priority = %d, want 0 (catch-all)", f.Priority())
	}
	if f.Mode() != contract.SessionModeNone {
		t.Errorf("normal flow Mode = %v, want SessionModeNone", f.Mode())
	}
	if f.Name() != "normal" {
		t.Errorf("normal flow Name = %q, want \"normal\"", f.Name())
	}

	admin := contract.TurnWork{Events: []contract.MessageEvent{{IsAdmin: true}}}
	if got := flowIdentity(admin); got != "admin" {
		t.Errorf("flowIdentity(admin) = %q, want \"admin\"", got)
	}
	if got := flowIdentity(contract.TurnWork{Events: []contract.MessageEvent{{}}}); got != "user" {
		t.Errorf("flowIdentity(non-admin) = %q, want \"user\"", got)
	}
}

// TestFlowIdentity_BatchAnchor pins the batch-anchor escalation rule: a turn runs
// as admin iff ANY human event in the batch is admin (a co-present admin lends
// power in a shared session); a pure-internal batch (no human) inherits the
// session's admin ownership. Internal events never carry IsAdmin themselves.
func TestFlowIdentity_BatchAnchor(t *testing.T) {
	admin := contract.MessageEvent{Source: "telegram", IsAdmin: true, Text: "看下账本"}
	user := contract.MessageEvent{Source: "telegram", Text: "导出生产库"}
	internal := contract.MessageEvent{Source: contract.SourceNameInternal, Text: "worker return"}

	cases := []struct {
		name      string
		events    []contract.MessageEvent
		sessAdmin bool
		want      string
	}{
		{"① admin head, non-admin tail → escalate", []contract.MessageEvent{admin, user}, false, "admin"},
		{"② internal head, admin human → escalate (B5 fixed)", []contract.MessageEvent{internal, admin}, false, "admin"},
		{"non-admin solo → user (tight)", []contract.MessageEvent{user}, false, "user"},
		{"non-admin batch even if session admin-owned → user (human present wins)", []contract.MessageEvent{user, user}, true, "user"},
		{"③ pure-internal, admin-owned session → inherit admin", []contract.MessageEvent{internal}, true, "admin"},
		{"③' pure-internal, non-admin session → user", []contract.MessageEvent{internal}, false, "user"},
		{"empty batch → user", nil, false, "user"},
	}
	for _, c := range cases {
		work := contract.TurnWork{Events: c.events, SessionAdmin: c.sessAdmin}
		if got := flowIdentity(work); got != c.want {
			t.Errorf("%s: flowIdentity = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestCallerIdentity_Anchor pins that the DM-vs-group recall decision keys on the
// human sender, not a coalesced internal event at the head of a drain batch: an
// internal-headed batch whose human is in a DM must clamp to that human's handle,
// not fall through to room recall.
func TestCallerIdentity_Anchor(t *testing.T) {
	internal := contract.MessageEvent{Source: contract.SourceNameInternal, ChatType: contract.ChatDM}
	dm := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, UserID: "u1"}
	group := contract.MessageEvent{Source: "telegram", ChatType: contract.ChatGroup, UserID: "u1"}

	// DM human behind an internal head → per-handle (NOT room).
	got := callerIdentity([]contract.MessageEvent{internal, dm}, "telegram:dm:1")
	if got.Room != "" || len(got.Handles) != 1 || got.Handles[0] != "telegram:u1" {
		t.Errorf("internal+DM-human caller = %+v, want Handles=[telegram:u1], no Room", got)
	}
	// Group human → recall the whole room.
	got = callerIdentity([]contract.MessageEvent{group}, "telegram:group:9")
	if got.Room != "telegram:group:9" || len(got.Handles) != 0 {
		t.Errorf("group-human caller = %+v, want Room=scope, no Handles", got)
	}
	// Pure-internal batch in a DM scope (cron wake / dispatch return): internal
	// emits carry no ChatType — must NOT set Room (a Room key no DM read ever
	// queries); falls to the personal partition (D28).
	bare := contract.MessageEvent{Source: contract.SourceNameInternal}
	got = callerIdentity([]contract.MessageEvent{bare}, "telegram:dm:1")
	if got.Room != "" {
		t.Errorf("pure-internal DM caller = %+v, want no Room", got)
	}
	// Pure-internal batch in a GROUP scope: classify by the scope grammar → Room.
	got = callerIdentity([]contract.MessageEvent{bare}, "telegram:group:9")
	if got.Room != "telegram:group:9" {
		t.Errorf("pure-internal group caller = %+v, want Room=scope", got)
	}
}
