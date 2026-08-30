package accounts

import (
	"context"
	"strings"
	"testing"

	"agentbob/contract"
)

// The i18n catalog isn't loaded in unit tests, so i18n.T echoes the KEY and drops the
// arguments — assertions below look for the key, never for the id it would render.

// A person's first question is "which account am I", and until bare /accounts
// answered it there was no way to find out: `show` demanded an id, `list` is
// admin-only and prints the whole table.
func TestBareAccountsNamesThisChannelsAccount(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	m := bindingManager(st, nil)
	ev := dmEv("telegram", "u1", "/accounts")
	if _, err := m.CreateAndBindSelf(ctx, ev, "Hector"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	sink := &capSink{}
	if err := m.slashAccounts(ctx, contract.SlashContext{Sink: sink, Event: ev}); err != nil {
		t.Fatalf("slashAccounts: %v", err)
	}
	// The bound line, specifically — not self_none / self_unknown, which share the prefix.
	if !strings.HasPrefix(sink.out, "slash.accounts.self\n") {
		t.Errorf("bare /accounts must name this channel's account, got %q", sink.out)
	}
	// The usage line still follows — the identity is an addition, not a replacement.
	if !strings.Contains(sink.out, "slash.accounts.usage") {
		t.Errorf("bare /accounts dropped the usage line: %q", sink.out)
	}
}

// An unbound channel gets a distinct answer, not a blank one — and it must not be
// confused with the store-read failure, which says something else entirely.
func TestBareAccountsOnAnUnboundChannel(t *testing.T) {
	sink := &capSink{}
	m := bindingManager(newMemStore(), nil)
	if err := m.slashAccounts(context.Background(),
		contract.SlashContext{Sink: sink, Event: dmEv("telegram", "nobody", "/accounts")}); err != nil {
		t.Fatalf("slashAccounts: %v", err)
	}
	if !strings.Contains(sink.out, "slash.accounts.self_none") {
		t.Errorf("an unbound channel must be told so, got %q", sink.out)
	}
}

// `/accounts show` with no id means "mine" and needs no admin rights — the answer is
// the caller's own row. Naming someone else's id stays admin-only.
func TestShowWithoutIDIsSelfAndNeedsNoAdmin(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	m := bindingManager(st, nil)
	ev := dmEv("telegram", "u1", "/accounts show")
	if _, err := m.CreateAndBindSelf(ctx, ev, "Hector"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	sink := &capSink{}
	if err := m.cmdShow(ctx, contract.SlashContext{Sink: sink, Event: ev}, nil); err != nil {
		t.Fatalf("cmdShow(self): %v", err)
	}
	// The channel ROW is the evidence it resolved to the caller's own account: that is
	// the only account in this store with a handle on it.
	if !strings.Contains(sink.out, "slash.accounts.show_header") ||
		!strings.Contains(sink.out, "slash.accounts.show_channel_row") {
		t.Errorf("self show did not render the caller's own account, got %q", sink.out)
	}

	other, _ := st.CreateAccount(ctx, "Someone Else", "", 1)
	sink2 := &capSink{}
	if err := m.cmdShow(ctx, contract.SlashContext{Sink: sink2, Event: ev}, []string{other.ID}); err == nil {
		t.Errorf("a non-admin naming another account must be refused, got %q", sink2.out)
	} else if !strings.Contains(sink2.out, "slash.accounts.admin_required") {
		t.Errorf("refusal = %q, want the admin-required message", sink2.out)
	}

	// …but naming YOUR OWN id is the same request as omitting it. Refusing that would
	// be a distinction nobody could guess.
	self, _, _ := m.AccountForHandle(ctx, "telegram", "u1")
	sink3 := &capSink{}
	if err := m.cmdShow(ctx, contract.SlashContext{Sink: sink3, Event: ev}, []string{self}); err != nil {
		t.Errorf("a non-admin naming its own id: %v (%s)", err, sink3.out)
	}
}

// Every one of these used to mint a plain llm key wearing the policy as its LABEL,
// and the mistake only surfaced later as a 403 that pointed nowhere near its cause.
func TestAPIKeyNewRefusesMalformedPolicyFlags(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{
		"kind=image",      // singular
		"kinds",           // the '=' never made it
		"kinds=llm,",      // a space after the comma cut the list in two
		",image",          // the orphaned half of `kinds=llm ,image`
		"models=a,",       // same, on the other form
		"label=promptlib", // a label may not carry '='
	} {
		st := newMemStore()
		acc, _ := st.CreateAccount(ctx, "friend", "", 1)
		m := bindingManager(st, nil)
		sink := &capSink{}
		sc := contract.SlashContext{IsAdmin: true, Sink: sink, Event: contract.MessageEvent{ChatType: contract.ChatDM}}
		err := m.cmdAPIKeyNew(ctx, sc, []string{acc.ID, bad})
		if err == nil {
			t.Errorf("%q should be refused, got reply %q", bad, sink.out)
			continue
		}
		if !strings.Contains(sink.out, "slash.accounts.apikey_bad_flag") {
			t.Errorf("%q: reply = %q, want the malformed-flag message", bad, sink.out)
		}
		if keys, _ := m.ListAPIKeys(ctx, acc.ID); len(keys) != 0 {
			t.Errorf("%q minted a key anyway: %+v", bad, keys)
		}
	}
}

// A label is still free text — refusing near-miss flags must not cost the ordinary
// case, and a well-formed policy must still parse.
func TestAPIKeyNewKeepsOrdinaryLabels(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	acc, _ := st.CreateAccount(ctx, "friend", "", 1)
	m := bindingManager(st, nil)
	sink := &capSink{}
	sc := contract.SlashContext{IsAdmin: true, Sink: sink, Event: contract.MessageEvent{ChatType: contract.ChatDM}}
	if err := m.cmdAPIKeyNew(ctx, sc, []string{acc.ID, "kinds=llm,image", "promptlib", "生图"}); err != nil {
		t.Fatalf("mint: %v (%s)", err, sink.out)
	}
	keys, _ := m.ListAPIKeys(ctx, acc.ID)
	if len(keys) != 1 {
		t.Fatalf("want one key, got %d", len(keys))
	}
	if got := strings.Join(keys[0].Kinds, ","); got != "llm,image" {
		t.Errorf("kinds = %q, want llm,image", got)
	}
	if keys[0].Label != "promptlib 生图" {
		t.Errorf("label = %q, want the two free-text words", keys[0].Label)
	}
}
