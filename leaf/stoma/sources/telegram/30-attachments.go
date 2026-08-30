package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/scrub"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type mediaRef struct {
	fileID   string
	kind     string // matches contract.Attachment.Kind
	fileName string
	mime     string
	size     int64
}

// mediaOf enumerates the downloadable media on a Telegram message.
func mediaOf(m *models.Message) []mediaRef {
	var out []mediaRef
	if len(m.Photo) > 0 {
		// pick the largest size
		best := m.Photo[0]
		for _, p := range m.Photo {
			if int64(p.Width)*int64(p.Height) > int64(best.Width)*int64(best.Height) {
				best = p
			}
		}
		// Per-message-id filename so a Telegram media-group album (3+
		// photos sharing one caption) produces unique sandbox files +
		// disambiguates the images for the model in the text prompt
		// (the API only sees base64+MIME, no filename — so identical
		// "photo.jpg" labels would collide). m.ID is Telegram's int64
		// message id, unique within a chat for the lifetime we care about.
		out = append(out, mediaRef{fileID: best.FileID, kind: "image", fileName: fmt.Sprintf("photo-%d.jpg", m.ID), mime: "image/jpeg", size: int64(best.FileSize)})
	}
	if m.Document != nil {
		out = append(out, mediaRef{fileID: m.Document.FileID, kind: "document", fileName: m.Document.FileName, mime: m.Document.MimeType, size: int64(m.Document.FileSize)})
	}
	if m.Video != nil {
		out = append(out, mediaRef{fileID: m.Video.FileID, kind: "video", fileName: orDefault(m.Video.FileName, "video.mp4"), mime: m.Video.MimeType, size: int64(m.Video.FileSize)})
	}
	if m.Voice != nil {
		out = append(out, mediaRef{fileID: m.Voice.FileID, kind: "voice", fileName: "voice.ogg", mime: orDefault(m.Voice.MimeType, "audio/ogg"), size: int64(m.Voice.FileSize)})
	}
	if m.Audio != nil {
		out = append(out, mediaRef{fileID: m.Audio.FileID, kind: "audio", fileName: orDefault(m.Audio.FileName, "audio"), mime: m.Audio.MimeType, size: int64(m.Audio.FileSize)})
	}
	if m.Animation != nil {
		out = append(out, mediaRef{fileID: m.Animation.FileID, kind: "animation", fileName: orDefault(m.Animation.FileName, "animation.mp4"), mime: m.Animation.MimeType, size: int64(m.Animation.FileSize)})
	}
	if m.Sticker != nil {
		name, mime := stickerNameMIME(m.Sticker)
		out = append(out, mediaRef{fileID: m.Sticker.FileID, kind: "sticker", fileName: name, mime: mime, size: int64(m.Sticker.FileSize)})
	}
	return out
}

// stickerNameMIME maps a Telegram sticker to its true filename + MIME.
// Telegram stickers come in three formats and models.Sticker flags which:
//   - video sticker (IsVideo)     → .webm / video/webm
//   - animated sticker (IsAnimated, a gzipped-JSON Lottie .tgs) → .tgs /
//     application/x-tgsticker (Telegram's own content-type for .tgs)
//   - static sticker (neither)    → .webp / image/webp
//
// Mislabeling .tgs/.webm bytes as image/webp punches through the
// isImageAttachment MIME gate downstream, feeding gzip-JSON / video
// bytes to vision models that then mis-decode them. IsVideo wins over
// IsAnimated (a video sticker is never also reported animated, but the
// explicit order documents the precedence).
func stickerNameMIME(s *models.Sticker) (name, mime string) {
	switch {
	case s.IsVideo:
		return "sticker.webm", "video/webm"
	case s.IsAnimated:
		return "sticker.tgs", "application/x-tgsticker"
	default:
		return "sticker.webp", "image/webp"
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// hasMedia reports whether m carries any downloadable media.
func hasMedia(m *models.Message) bool { return m != nil && len(mediaOf(m)) > 0 }

// collectAttachments downloads m's media into the file store and returns the
// corresponding contract.Attachment list. Files that fail to download (or exceed the
// size limit) are still listed, with LocalPath == "". The subdir argument
// scopes downloads to per-session_scope sandbox directories so all
// attachments for one chat (and its multiple sids) live together —
// see Source.resolveSubdir + 30-session-key.go on the gateway side.
func (s *Source) collectAttachments(ctx context.Context, m *models.Message, fromReply bool, subdir string) []contract.Attachment {
	if s.files == nil || m == nil {
		return nil
	}
	var out []contract.Attachment
	for _, mr := range mediaOf(m) {
		a := contract.Attachment{Kind: mr.kind, MIME: mr.mime, FileName: mr.fileName, Size: mr.size, FromReply: fromReply}
		if path, relPath, n, err := s.downloadFile(ctx, mr.fileID, mr.fileName, subdir); err != nil {
			slog.Debug("telegram: attachment not downloaded", "kind", mr.kind, "name", mr.fileName, "size", mr.size, "err", err)
		} else {
			a.LocalPath = path
			a.Path = relPath
			if n > 0 {
				a.Size = n
			}
		}
		out = append(out, a)
	}
	return out
}

func (s *Source) downloadFile(ctx context.Context, fileID, name, subdir string) (absPath, relPath string, size int64, err error) {
	f, err := s.b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return "", "", 0, redactTokenErr(err, s.token)
	}
	// URL contains the bot token in the path segment. net/http wraps
	// the URL into returned errors (e.g., "Get \"https://api.telegram.org/file/botXXX/...\":
	// dial tcp ..."), which slog would then log verbatim — a TOKEN LEAK
	// any operator tailing logs can see. Build the URL but wrap every
	// returned error with redactTokenErr so the token never reaches
	// any log line.
	url := "https://api.telegram.org/file/bot" + s.token + "/" + f.FilePath
	dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", 0, redactTokenErr(err, s.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", 0, redactTokenErr(err, s.token)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return "", "", 0, &httpStatusError{resp.StatusCode}
	}
	// Short-circuit on advertised Content-Length so a server pretending
	// to send a multi-GB file doesn't force us to spool maxAttBytes of
	// data through Save before LimitReader trips. Content-Length is
	// trusted only as a "definitely too big" signal — actual size still
	// gets enforced by files.Save's LimitReader (the server may lie
	// short).
	maxBytes := s.attMaxBytes()
	if maxBytes > 0 && resp.ContentLength > 0 && resp.ContentLength > maxBytes {
		return "", "", 0, fmt.Errorf("telegram: attachment %s too large: %d bytes (cap %d)", name, resp.ContentLength, maxBytes)
	}
	abs, rel, n, sErr := s.files.Save(subdir, name, resp.Body, maxBytes)
	return abs, rel, n, redactTokenErr(sErr, s.token)
}

// resolveSubdir returns the scope-AGNOSTIC staging subdir for ev's
// attachments, or a "telegram" fallback bucket when the source wasn't
// wired with a resolver (CLI / debug paths). The source no longer
// resolves the session scope itself: it stages every file in one shared
// bucket and the pipeline (Manager, via Deps.PlaceAttachments) relocates
// it into the resolved scope once ResolveSession has run. This kills the
// old divergence where a source heuristic and the agora-aware router
// disagreed on the scope and attachments landed in the wrong sandbox.
func (s *Source) resolveSubdir(contract.MessageEvent) string {
	// Scope-agnostic staging bucket; the pipeline relocates into the resolved
	// scope sandbox (PlaceAttachments) once that lands.
	return "telegram"
}

// redactTokenErr returns an error whose Error() string has the bot
// token replaced by "<redacted>". Preserves errors.Is/As semantics by
// wrapping the original error.
func redactTokenErr(err error, token string) error {
	if err == nil {
		return err
	}
	msg := err.Error()
	red := msg
	if token != "" {
		red = strings.ReplaceAll(red, token, "<redacted>")
	}
	// Generic secret backstop (DSN passwords, URL tokens) so telegram matches the
	// scrub depth feishu/discord get via common.RedactErr (S-34) — but KEEP the
	// Unwrap-preserving redactedErr wrapper: common.RedactErr returns a plain
	// errString that drops Unwrap, and telegram's redaction tests + errors.Is
	// callers rely on the chain surviving.
	red = scrub.Scrub(red)
	if red == msg {
		return err // nothing changed — keep the original error's type + Unwrap chain
	}
	return &redactedErr{wrapped: err, msg: red}
}

type redactedErr struct {
	wrapped error
	msg     string
}

func (e *redactedErr) Error() string { return e.msg }
func (e *redactedErr) Unwrap() error { return e.wrapped }

// Compile-time check: errors.Is/As still find the original sentinel
// through the wrapper (e.g. context.DeadlineExceeded).
var _ interface{ Unwrap() error } = (*redactedErr)(nil)

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return "download HTTP status " + http.StatusText(e.code) }
