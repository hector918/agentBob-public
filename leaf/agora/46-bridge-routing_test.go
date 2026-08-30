package agora

import (
	"context"
	"testing"

	"agentbob/contract"
	"agentbob/leaf/agora/store"
)

// newBridgeImpl builds an org with a bridge inbox that has BOTH a bound external
// chat (inbox_source) AND a BridgeDefaultTargetInboxID pointing at a member inbox,
// so BridgeRouting can report a non-zero externalTarget + defaultInboxScope.
func newBridgeImpl(t *testing.T) *Impl {
	t.Helper()
	org := NewOrgCache()
	org.UpsertCompany(store.Company{ID: "co_acme", Name: "Acme", Status: "active"})
	if err := org.SeedCompanyRolesFromYAML("co_acme", []byte("roles:\n  - boss\ngrants:\n  tool:use:read_file:\n    boss: on\n")); err != nil {
		t.Fatalf("seed roles: %v", err)
	}
	org.UpsertMember(store.Member{ID: "m_alice", Name: "alice"})
	org.UpsertEmployment(store.Employment{ID: "e_alice", MemberID: "m_alice", CompanyID: "co_acme", RoleName: "boss", Status: "active"})
	org.UpsertInbox(store.Inbox{ID: "ib_alice", Kind: "member", MemberID: "m_alice", Scope: "co:acme/alice", Name: "co_acme", Status: "active"})
	// Bridge inbox: owns the company, bound to a telegram group, default target = alice.
	org.UpsertInbox(store.Inbox{ID: "ib_bridge", Kind: "bridge", OwnerCompanyID: "co_acme", Scope: "co:acme/bridge", Status: "active", BridgeDefaultTargetInboxID: "ib_alice"})

	loader := &fakeSourceLoader{srcs: []store.InboxSource{
		{ID: "s_bridge", InboxID: "ib_bridge", SourceID: "telegram", ChatType: contract.ChatGroup, ChatID: "900"},
	}}
	router := NewInboxRouter(org, loader)
	if err := router.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return newImpl(org, router, nil)
}

func TestBridgeRoutingBridge(t *testing.T) {
	a := newBridgeImpl(t)
	ctx := context.Background()

	// The bridge resolves both via its own scope AND via the bound external chat.
	for _, scope := range []string{"co:acme/bridge", "telegram:group:900"} {
		ext, defScope, _, isBridge := a.BridgeRouting(ctx, scope)
		if !isBridge {
			t.Fatalf("%s: isBridge=false, want true", scope)
		}
		if ext.Source != "telegram" || ext.ChatID != "900" {
			t.Errorf("%s: externalTarget=%+v, want telegram/900", scope, ext)
		}
		if defScope != "co:acme/alice" {
			t.Errorf("%s: defaultInboxScope=%q, want co:acme/alice", scope, defScope)
		}
	}
}

func TestBridgeRoutingMemberIsNotBridge(t *testing.T) {
	a := newBridgeImpl(t)
	// A member inbox scope routes as a normal turn (isBridge=false).
	ext, defScope, _, isBridge := a.BridgeRouting(context.Background(), "co:acme/alice")
	if isBridge {
		t.Fatal("member inbox reported isBridge=true")
	}
	if ext != (contract.Target{}) || defScope != "" {
		t.Errorf("member inbox: ext=%+v def=%q, want zero/empty", ext, defScope)
	}
	// A non-agora scope: also not a bridge.
	if _, _, _, isBridge := a.BridgeRouting(context.Background(), "telegram:dm:nope"); isBridge {
		t.Error("non-agora scope reported isBridge=true")
	}
}

func TestBridgeRoutingNoExternalNoDefault(t *testing.T) {
	// A bridge with neither a bound source nor a default target → zero ext + "" def,
	// but still isBridge (it must NOT fall through to a turn).
	org := NewOrgCache()
	org.UpsertCompany(store.Company{ID: "co_bare", Name: "Bare", Status: "active"})
	org.UpsertInbox(store.Inbox{ID: "ib_bare", Kind: "bridge", OwnerCompanyID: "co_bare", Scope: "co:bare/bridge", Status: "active"})
	router := NewInboxRouter(org, nil)
	if err := router.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	a := newImpl(org, router, nil)

	ext, defScope, _, isBridge := a.BridgeRouting(context.Background(), "co:bare/bridge")
	if !isBridge {
		t.Fatal("bare bridge isBridge=false, want true")
	}
	if ext != (contract.Target{}) {
		t.Errorf("bare bridge externalTarget=%+v, want zero", ext)
	}
	if defScope != "" {
		t.Errorf("bare bridge defaultInboxScope=%q, want empty", defScope)
	}
}

func TestParseBridgeToChat(t *testing.T) {
	// auto with an event → uses ev coords.
	ev := &contract.MessageEvent{Source: "telegram", ChatType: contract.ChatDM, ChatID: "555"}
	if src, ct, chat, err := ParseBridgeToChat("auto", ev, nil); err != nil || src != "telegram" || ct != contract.ChatDM || chat != "555" {
		t.Fatalf("auto: (%q,%q,%q,%v)", src, ct, chat, err)
	}
	// auto with nil event → error.
	if _, _, _, err := ParseBridgeToChat("auto", nil, nil); err == nil {
		t.Error("auto with nil event should error")
	}
	// explicit triple.
	if src, ct, chat, err := ParseBridgeToChat("telegram:group:-100", nil, nil); err != nil || src != "telegram" || ct != contract.ChatGroup || chat != "-100" {
		t.Fatalf("explicit: (%q,%q,%q,%v)", src, ct, chat, err)
	}
	// reserved source → reject.
	if _, _, _, err := ParseBridgeToChat("internal:dm:1", nil, nil); err == nil {
		t.Error("reserved source should be rejected")
	}
	// bad chat-type token → reject.
	if _, _, _, err := ParseBridgeToChat("telegram:channel:1", nil, nil); err == nil {
		t.Error("bad chat-type token should be rejected")
	}
	// malformed (missing parts) → reject.
	if _, _, _, err := ParseBridgeToChat("telegram:1", nil, nil); err == nil {
		t.Error("malformed arg should be rejected")
	}
}
