package discord

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agentbob/contract"
	"agentbob/leaf/stoma/sources/sendgate"

	"github.com/bwmarrin/discordgo"
)

// Send posts a one-off text message (async feedback / admin notice).
// Synchronous — the returned error is REJECTION/failure per contract.Source.Send.
// sendText returns the raw error, so this boundary redacts before handing it to
// a caller that may log it. A 429 is retried via the shared sendgate.Relay (L-D3) —
// the one-off path doesn't ride the streamsink core's retry, and discordgo's own
// per-route pacing makes a per-bot serialiser (sendgate.Gate) unnecessary here.
func (s *Source) Send(ctx context.Context, t contract.Target, text string) error {
	err := sendgate.Relay(ctx, discordRateLimited, func() error {
		// F8: honor Target.ReplyToID so a one-off send (busy-queue notice / redeem bounce)
		// threads under the message it answers — mirrors the streaming path and the session
		// layer that fills it. Empty → no anchor (createMessage skips a blank reference).
		_, e := s.sendText(ctx, t, text, t.ReplyToID)
		return e
	})
	return s.redactErr(err)
}

// SendFile uploads path to Discord and posts it as a file message to t.
// SYNCHRONOUS: the bytes are read + uploaded before this returns, so the caller
// may delete the source file immediately. caption, if non-empty, rides along as
// the message content.
func (s *Source) SendFile(ctx context.Context, t contract.Target, path, caption string) error {
	if s.closed.Load() {
		return fmt.Errorf("discord: source closed")
	}
	if t.ChatID == "" {
		return fmt.Errorf("discord: SendFile needs a chat_id")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("discord: SendFile open %s: %w", path, err)
	}
	defer f.Close()

	err = sendgate.Relay(ctx, discordRateLimited, func() error {
		// Rewind on each attempt: a 429 retry must re-read the file from the start (the
		// previous attempt consumed the reader). Seek failure → don't retry a partial upload.
		if _, e := f.Seek(0, io.SeekStart); e != nil {
			return e
		}
		_, e := s.dg.ChannelMessageSendComplex(t.ChatID, &discordgo.MessageSend{
			Content: caption,
			Files:   []*discordgo.File{{Name: filepath.Base(path), Reader: f}},
		}, discordgo.WithContext(ctx))
		return e
	})
	if err != nil {
		return s.redactErr(fmt.Errorf("discord: SendFile upload: %w", err))
	}
	return nil
}

// SendButtons degrades to a plain text message: inline-button callback routing
// is deferred (no handler registry), so buttons are dropped rather than rendered
// as dead controls. Returns "" (no tracked message id).
func (s *Source) SendButtons(ctx context.Context, t contract.Target, text string, _ []contract.Button) (string, error) {
	return "", s.Send(ctx, t, text)
}
