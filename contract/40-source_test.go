package contract

import "testing"

// TargetForScope must be the faithful inverse of ScopeFor wherever the grammar is
// lossless — that pairing is the whole reason both live in one file. A drift here
// means a recovered image is delivered to the wrong chat, or to none.
func TestTargetForScopeRoundTripsScopeFor(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		chatType ChatType
		chatID   string
		threadID string
		want     Target
	}{
		{
			name: "dm", source: "telegram", chatType: ChatDM, chatID: "12345",
			want: Target{Source: "telegram", ChatID: "12345"},
		},
		{
			// The email forwarded-alias case: a thread splits one DM into sub-scopes,
			// and ScopeFor keeps it, so the inverse must too.
			name: "dm with thread", source: "email", chatType: ChatDM, chatID: "a@b.c", threadID: "alias1",
			want: Target{Source: "email", ChatID: "a@b.c", ThreadID: "alias1"},
		},
		{
			name: "group", source: "telegram", chatType: ChatGroup, chatID: "-100200",
			want: Target{Source: "telegram", ChatID: "-100200"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := ScopeFor(tc.source, tc.chatType, tc.chatID, tc.threadID)
			got, ok := TargetForScope(scope)
			if !ok {
				t.Fatalf("TargetForScope(%q) = not ok, want a target", scope)
			}
			if got != tc.want {
				t.Errorf("TargetForScope(%q) = %+v, want %+v", scope, got, tc.want)
			}
		})
	}
}

// The group grammar flattens threads, so a forum topic comes back as its plain
// group. That is a documented LOSS, not a bug — but it must land in the right
// chat, never in a different one.
func TestTargetForScopeFlattensGroupThread(t *testing.T) {
	scope := ScopeFor("telegram", ChatGroup, "-100200", "topic-7")
	got, ok := TargetForScope(scope)
	if !ok {
		t.Fatalf("TargetForScope(%q) = not ok", scope)
	}
	if got.ChatID != "-100200" {
		t.Errorf("ChatID = %q, want the group id -100200", got.ChatID)
	}
	if got.ThreadID != "" {
		t.Errorf("ThreadID = %q, want empty (ScopeFor drops group threads)", got.ThreadID)
	}
}

// A virtual-group member sub-scope addresses the same underlying chat, so it must
// resolve to the base group rather than being refused.
func TestTargetForScopeResolvesMemberSubScope(t *testing.T) {
	base := ScopeFor("telegram", ChatGroup, "-100200", "")
	got, ok := TargetForScope(base + "#sales")
	if !ok {
		t.Fatalf("member sub-scope = not ok, want the base group's target")
	}
	if got.ChatID != "-100200" || got.Source != "telegram" {
		t.Errorf("got %+v, want the base group chat", got)
	}
}

// Anything that is not a deliverable chat must be refused rather than guessed at:
// a caller that guesses sends a user's picture somewhere unrelated.
func TestTargetForScopeRefusesUndeliverable(t *testing.T) {
	for _, scope := range []string{
		"",                    // nothing
		"some-dispatch-scope", // internal: chat id verbatim, no source prefix
		ScopeFor(SourceNameInternal, ChatDM, "x", ""), // ditto, built through the grammar
		"telegram:",          // no kind, no id
		"telegram:dm:",       // no chat id
		"telegram:group:",    // no chat id
		"telegram:mystery:1", // unknown kind
		":dm:1",              // no source
	} {
		if got, ok := TargetForScope(scope); ok {
			t.Errorf("TargetForScope(%q) = %+v, ok — want refusal", scope, got)
		}
	}
}
