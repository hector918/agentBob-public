package accounts

import (
	"context"
	"testing"

	"agentbob/contract"
)

// TestDegradedAccounts pins the fail-CLOSED stand-in the module Provides when its store
// Migrate fails (F39): every routing query reads as "unbound / unknown", which drives the
// router to funnel non-admin senders to intro/refuse instead of the full flow. (Admins
// pass the router's normal floor independently of this read.)
func TestDegradedAccounts(t *testing.T) {
	var a contract.Accounts = degradedAccounts{}
	if _, ok := a.AccountFor(context.Background(), contract.MessageEvent{Source: "tg", UserID: "u1"}); ok {
		t.Fatal("degraded AccountFor must report UNBOUND (ok=false) so the router fails closed")
	}
	if lang, ok := a.LangFor(context.Background(), "tg", "u1"); ok || lang != "" {
		t.Fatalf("degraded LangFor must be empty/unknown, got %q/%v", lang, ok)
	}
	a.RememberLang(context.Background(), "tg", "u1", "en") // no-op, must not panic
}
