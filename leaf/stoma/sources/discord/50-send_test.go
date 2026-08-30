package discord

import (
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func rateLimitErr(after time.Duration) error {
	return &discordgo.RateLimitError{RateLimit: &discordgo.RateLimit{
		TooManyRequests: &discordgo.TooManyRequests{RetryAfter: after},
	}}
}

// discordRateLimited recognises the typed *RateLimitError (and only it) — the shared
// classifier the streaming wire and the one-off send retry both use (L-D3).
func TestDiscordRateLimited(t *testing.T) {
	if d, ok := discordRateLimited(rateLimitErr(3 * time.Second)); !ok || d != 3*time.Second {
		t.Fatalf("429 classify = (%v,%v), want (3s,true)", d, ok)
	}
	if _, ok := discordRateLimited(errors.New("nope")); ok {
		t.Fatal("a non-429 error must not classify as rate-limited")
	}
}
