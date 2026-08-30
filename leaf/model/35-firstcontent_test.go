// Tests for ErrNoModelAvailable classification — the typed sentinel callers
// branch on (replaces the old substring-matching of pick's error text).
//
// The first-content-deadline streaming tests that lived here were removed with
// the pool's channel-based ChatStream entry (the surviving streaming path,
// ChatStreamWatch, keeps its own first-content-stall coverage in
// 45-chat-stream-watch_test.go).
//
// White-box (package model); shares fakeChatter + helpers with 30-failover_test.go.
package model

import (
	"context"
	"errors"
	"testing"

	"agentbob/contract"
)

// Chat: every candidate erroring, and no entry matching the required tags,
// both classify as ErrNoModelAvailable so callers can branch on it.
func TestChatErrNoModelAvailable(t *testing.T) {
	t.Run("all candidates fail", func(t *testing.T) {
		a := &fakeChatter{chatErr: errors.New("a down")}
		b := &fakeChatter{chatErr: errors.New("b down")}
		p := newTestPool(entry("a", 3, a), entry("b", 0, b))

		_, err := p.Chat(context.Background(), smartReq, nil)
		if !errors.Is(err, ErrNoModelAvailable) {
			t.Fatalf("all-candidates-failed must classify as ErrNoModelAvailable, got %v", err)
		}
	})

	t.Run("no tag match", func(t *testing.T) {
		p := newTestPool(entry("a", 3, &fakeChatter{text: "ok"})) // tagged "smart"
		_, err := p.Chat(context.Background(), contract.ModelRequest{Requires: []string{"vision"}}, nil)
		if !errors.Is(err, ErrNoModelAvailable) {
			t.Fatalf("no-tag-match must classify as ErrNoModelAvailable, got %v", err)
		}
	})
}
