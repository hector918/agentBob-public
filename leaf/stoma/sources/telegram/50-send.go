package telegram

import (
	"bufio"
	"context"
	"fmt"
	"image"
	_ "image/jpeg" // register jpeg for image.DecodeConfig (photo eligibility)
	_ "image/png"  // register png for image.DecodeConfig
	"io"
	"os"
	"path/filepath"
	"strconv"

	"agentbob/contract"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// chatTarget parses a Target's ids into Telegram's native types. A bad chat id
// is a hard error; a bad thread id degrades to no-thread.
func chatTarget(t contract.Target) (chatID int64, threadID int, err error) {
	chatID, err = strconv.ParseInt(t.ChatID, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("telegram: invalid chat_id %q: %w", t.ChatID, err)
	}
	if t.ThreadID != "" {
		if v, perr := strconv.Atoi(t.ThreadID); perr == nil {
			threadID = v
		}
	}
	return chatID, threadID, nil
}

// Send posts a one-off text message (busy notices, redeem confirmations, slash
// replies). The error indicates REJECTION, not delivery failure.
func (s *Source) Send(ctx context.Context, t contract.Target, text string) error {
	if s.b == nil || s.closed.Load() {
		return fmt.Errorf("telegram: source not serving")
	}
	chatID, threadID, err := chatTarget(t)
	if err != nil {
		return err
	}
	p := &bot.SendMessageParams{ChatID: chatID, Text: text}
	if threadID != 0 {
		p.MessageThreadID = threadID
	}
	// F8: honor Target.ReplyToID so a one-off send (busy-queue notice / redeem bounce)
	// threads under the message it answers — mirrors the streaming path (20-stream.go) and
	// the session layer that deliberately fills ReplyToID on those notices. A bad id
	// degrades to no anchor (AllowSendingWithoutReply). Before F8 this field was silently
	// dropped (feishu honored it, telegram/discord didn't — a 3-source drift).
	if t.ReplyToID != "" {
		if v, perr := strconv.Atoi(t.ReplyToID); perr == nil {
			p.ReplyParameters = &models.ReplyParameters{MessageID: v, AllowSendingWithoutReply: true}
		}
	}
	// Through the bot's single outbound serialiser so one-off sends share the 429
	// cooldown with the streaming edits — else they fire in parallel and back off
	// independently, overrunning the per-bot limit (the "停了下来" 429 incident).
	if err := s.gate.Do(ctx, func() error {
		_, e := s.b.SendMessage(ctx, p)
		return e
	}); err != nil {
		return redactTokenErr(err, s.token)
	}
	return nil
}

// SendFile posts the file at path as a document with an optional caption.
func (s *Source) SendFile(ctx context.Context, t contract.Target, path, caption string) error {
	if s.b == nil || s.closed.Load() {
		return fmt.Errorf("telegram: source not serving")
	}
	chatID, threadID, err := chatTarget(t)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("telegram: open %q: %w", path, err)
	}
	defer f.Close()
	p := &bot.SendDocumentParams{
		ChatID:   chatID,
		Document: &models.InputFileUpload{Filename: filepath.Base(path), Data: f},
		Caption:  caption,
	}
	if threadID != 0 {
		p.MessageThreadID = threadID
	}
	if err := s.gate.Do(ctx, func() error {
		// Rewind on each attempt: a 429 relay re-invokes this closure and the
		// previous attempt already consumed the reader (an un-rewound retry
		// uploads a zero-byte document). Seek failure → fail rather than retry
		// a partial upload.
		if _, e := f.Seek(0, io.SeekStart); e != nil {
			return e
		}
		_, e := s.b.SendDocument(ctx, p)
		return e
	}); err != nil {
		return redactTokenErr(err, s.token)
	}
	return nil
}

// Telegram's own limits on sendPhoto. A file that breaks any of them is rejected
// by the API, so they are checked here and the send degrades to a document — an
// inline picture is a nicety, arriving is not.
const (
	tgPhotoMaxBytes = 10 << 20 // 10 MB
	tgPhotoMaxSum   = 10000    // width + height
	tgPhotoMaxRatio = 20.0     // longest side / shortest side
)

// SendPhoto posts the file at path as a PHOTO — inline in the conversation, the
// way a picture is meant to arrive — falling back to SendFile whenever it cannot.
//
// The fallback is not defensive coding, it is the contract (contract.PhotoSender):
// Telegram re-encodes photos server-side, and refuses outright anything over its
// limits, so "send it as a picture" has to mean "…or as a file, but send it".
//
// It deliberately does NOT retry as a document after an API error: the gate
// already relays 429s, and any other failure here could have delivered — a second
// send would risk two copies in the chat.
func (s *Source) SendPhoto(ctx context.Context, t contract.Target, path, caption string) error {
	if s.b == nil || s.closed.Load() {
		return fmt.Errorf("telegram: source not serving")
	}
	if !photoEligible(path) {
		return s.SendFile(ctx, t, path, caption)
	}
	chatID, threadID, err := chatTarget(t)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("telegram: open %q: %w", path, err)
	}
	defer f.Close()
	p := &bot.SendPhotoParams{
		ChatID:  chatID,
		Photo:   &models.InputFileUpload{Filename: filepath.Base(path), Data: f},
		Caption: caption,
	}
	if threadID != 0 {
		p.MessageThreadID = threadID
	}
	if err := s.gate.Do(ctx, func() error {
		// Rewind per attempt, same reason as SendFile: a relayed 429 re-runs this
		// closure and the reader is already spent.
		if _, e := f.Seek(0, io.SeekStart); e != nil {
			return e
		}
		_, e := s.b.SendPhoto(ctx, p)
		return e
	}); err != nil {
		return redactTokenErr(err, s.token)
	}
	return nil
}

// photoEligible reports whether Telegram would accept this file as a photo.
//
// DecodeConfig reads only as far as the header, but NOT a fixed distance into the
// file: a phone JPEG carries EXIF and an embedded thumbnail ahead of its SOF
// marker, so capping the reader at a few hundred bytes would classify every
// iPhone photo as "not an image" and quietly demote it to a document. It is
// handed the file and left to stop where it stops.
func photoEligible(path string) bool {
	st, err := os.Stat(path)
	if err != nil || st.Size() > tgPhotoMaxBytes {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(bufio.NewReader(f))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return false // not a picture the standard decoders know → send it as a file
	}
	if cfg.Width+cfg.Height > tgPhotoMaxSum {
		return false
	}
	long, short := cfg.Width, cfg.Height
	if short > long {
		long, short = short, long
	}
	return float64(long)/float64(short) <= tgPhotoMaxRatio
}

// SendButtons degrades to a plain text message: inline-button callback routing
// is deferred (no handler registry), so buttons are dropped rather than rendered
// as dead controls. Returns "" (no tracked message id).
func (s *Source) SendButtons(ctx context.Context, t contract.Target, text string, _ []contract.Button) (string, error) {
	return "", s.Send(ctx, t, text)
}
