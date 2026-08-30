package discord

import (
	"context"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"agentbob/contract"

	"github.com/bwmarrin/discordgo"
)

// attHTTPClient downloads inbound attachment bytes from Discord's CDN. The
// per-request context (carrying the gateway ctx) bounds each download; the
// client timeout is a backstop for a stuck connection.
var attHTTPClient = &http.Client{Timeout: 60 * time.Second}

// collectAttachments downloads the media carried by an inbound message into the
// staging file store and returns the attachment list. Discord delivers every
// attachment with a ready CDN URL (m.Attachments), so capture is a plain HTTP
// GET — no SDK resource call (unlike feishu).
//
// Staging is scope-blind: every file lands in one shared bucket (the source
// name) and the pipeline's PlaceAttachments relocates it into the resolved
// session scope later.
//
// Like the other sources, a download failure still appends the attachment (with
// empty LocalPath/Path) so the prompt layer's describeAtt can note the user
// shared something even when the bytes couldn't be fetched.
func (s *Source) collectAttachments(ctx context.Context, m *discordgo.Message, ev contract.MessageEvent) []contract.Attachment {
	if m == nil || len(m.Attachments) == 0 {
		return nil
	}
	// Nil-safe: without a file store we cannot stage anything — skip capture
	// entirely (CLI / debug / disabled paths).
	if s.files == nil {
		return nil
	}
	subdir := s.resolveSubdir(ev)
	var out []contract.Attachment
	for _, a := range m.Attachments {
		if a == nil || a.URL == "" {
			continue
		}
		out = append(out, s.downloadAttachment(ctx, a, subdir))
	}
	return out
}

// resolveSubdir returns the scope-AGNOSTIC staging bucket for ev's attachments:
// a fixed bucket named for the source. The source no longer resolves the session
// scope itself — it stages every file in one shared bucket and the pipeline
// relocates it into the resolved scope sandbox (PlaceAttachments) once
// ResolveSession has run.
func (s *Source) resolveSubdir(contract.MessageEvent) string { return s.name }

// downloadAttachment streams one attachment's bytes from its CDN URL into the
// staging store. On any error it logs and returns the attachment with empty
// LocalPath/Path (graceful degrade). Kind/MIME are set from the content-type
// (falling back to the filename extension).
func (s *Source) downloadAttachment(ctx context.Context, a *discordgo.MessageAttachment, subdir string) contract.Attachment {
	name := a.Filename
	if name == "" {
		name = "file-" + a.ID
	}
	att := contract.Attachment{
		FileName: name,
		Kind:     kindOf(a.ContentType, name),
		MIME:     mimeOf(a.ContentType, name),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		slog.Debug("discord: attachment request build failed", "name", name, "err", err)
		return att
	}
	resp, err := attHTTPClient.Do(req)
	if err != nil {
		slog.Info("discord: attachment download failed", "name", name, "err", s.redactErr(err))
		return att
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Info("discord: attachment download non-OK", "name", name, "status", resp.StatusCode)
		return att
	}

	abs, rel, n, sErr := s.files.Save(subdir, name, resp.Body, s.maxAttBytes)
	if sErr != nil {
		slog.Debug("discord: attachment not staged", "name", name, "err", sErr)
		return att
	}
	att.LocalPath = abs
	att.Path = rel
	att.Size = n
	return att
}

// kindOf classifies an attachment by its content-type, falling back to the
// filename extension. One of "image" / "audio" / "video" / "document" — the
// same Kind vocabulary the preprocess layer dispatches on.
func kindOf(contentType, name string) string {
	switch ct := baseType(contentType); {
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	case ct != "":
		return "document"
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp":
		return "image"
	case ".mp3", ".ogg", ".opus", ".wav", ".m4a", ".flac":
		return "audio"
	case ".mp4", ".mov", ".webm", ".mkv", ".avi":
		return "video"
	default:
		return "document"
	}
}

// mimeOf returns the attachment's MIME: the reported content-type when present,
// else a best-effort guess from the filename extension ("" when unknown —
// acceptable; preprocess sniffs images by Kind anyway).
func mimeOf(contentType, name string) string {
	if ct := baseType(contentType); ct != "" {
		return ct
	}
	ext := filepath.Ext(name)
	if ext == "" {
		return ""
	}
	return baseType(mime.TypeByExtension(ext))
}

// baseType strips any "; charset=…" parameters and surrounding space from a
// MIME / content-type string.
func baseType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(strings.ToLower(ct))
}
