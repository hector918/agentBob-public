package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"

	"agentbob/contract"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// collectAttachments downloads the media carried by a non-text inbound message
// into the staging file store and returns the resulting text (only the rich-
// text "post" type carries inline text alongside media) plus the attachment
// list. It dispatches on msg.MessageType; unknown types yield ("", nil).
//
// Staging is scope-blind: every file lands in the fixed per-source staging
// bucket (resolveSubdir → the source name) and the pipeline's PlaceAttachments
// relocates it into the resolved session scope later. The source never resolves
// the scope itself (mirrors telegram's collectAttachments).
//
// Like telegram, a download failure still appends the attachment (with an empty
// LocalPath/Path) so the prompt layer's describeAtt can note that the user
// shared something even when the bytes couldn't be fetched. The nil-file-store
// case (capture disabled) degrades the same way inside downloadResource — an
// early return here would also swallow a post message's prose, dropping the
// whole message at onMessageReceive's empty-text+no-media gate.
func (s *Source) collectAttachments(ctx context.Context, msg *larkim.EventMessage, _ contract.MessageEvent) (string, []contract.Attachment) {
	if msg == nil {
		return "", nil
	}
	subdir := s.resolveSubdir()
	msgID := str(msg.MessageId)
	content := str(msg.Content)

	switch str(msg.MessageType) {
	case "image":
		key := imageKeyOf(content)
		if key == "" {
			return "", nil
		}
		name := fmt.Sprintf("image-%s.jpg", msgID)
		att, _ := s.downloadResource(ctx, msgID, key, "image", name, subdir)
		att.Kind = "image"
		att.MIME = "image/jpeg"
		return "", []contract.Attachment{att}

	case "file":
		key, fileName := fileKeyName(content)
		if key == "" {
			return "", nil
		}
		att, _ := s.downloadResource(ctx, msgID, key, "file", fileName, subdir)
		att.Kind = "document"
		// att.FileName, not fileName: the content JSON may omit file_name, in
		// which case downloadResource backfilled the server-reported name.
		att.MIME = mimeByName(att.FileName)
		return "", []contract.Attachment{att}

	case "audio":
		key, _ := fileKeyName(content)
		if key == "" {
			return "", nil
		}
		name := fmt.Sprintf("audio-%s.opus", msgID)
		att, _ := s.downloadResource(ctx, msgID, key, "file", name, subdir)
		att.Kind = "voice" // 语音条与 telegram 对齐:转写并入 ev.Text
		att.MIME = "audio/ogg"
		return "", []contract.Attachment{att}

	case "media":
		key, _ := fileKeyName(content)
		if key == "" {
			return "", nil
		}
		name := fmt.Sprintf("video-%s.mp4", msgID)
		att, _ := s.downloadResource(ctx, msgID, key, "file", name, subdir)
		att.Kind = "video"
		att.MIME = "video/mp4"
		return "", []contract.Attachment{att}

	case "post":
		text, imageKeys := parsePostContent(content)
		var atts []contract.Attachment
		for i, key := range imageKeys {
			name := fmt.Sprintf("image-%s-%d.jpg", msgID, i)
			att, _ := s.downloadResource(ctx, msgID, key, "image", name, subdir)
			att.Kind = "image"
			att.MIME = "image/jpeg"
			atts = append(atts, att)
		}
		return text, atts
	}
	return "", nil
}

// resolveSubdir returns the scope-agnostic staging bucket — the source name. The
// pipeline relocates each file into the resolved scope sandbox (PlaceAttachments)
// once that lands, so the source never resolves the session scope itself.
func (s *Source) resolveSubdir() string { return s.name }

// downloadResource fetches one Feishu message resource (image_key or file_key)
// and streams it into the staging store. resType is "image" for image messages
// or "file" for file/audio/media messages (the SDK's resource type discriminator).
// On any error it logs at DEBUG and returns the attachment with empty
// LocalPath/Path (graceful degrade — matches telegram). The returned att already
// carries FileName/Size; the caller sets Kind/MIME.
func (s *Source) downloadResource(ctx context.Context, msgID, key, resType, fileName, subdir string) (contract.Attachment, bool) {
	att := contract.Attachment{FileName: fileName}

	// Nil-safe: without a file store nothing can be staged (capture disabled —
	// CLI / debug paths), and without an API client nothing can be fetched.
	// Degrade like a failed download: the caller still appends the metadata-only
	// attachment for describeAtt, and a post message's prose passes through.
	if s.files == nil || s.api == nil {
		return att, false
	}

	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(msgID).
		FileKey(key).
		Type(resType).
		Build()
	resp, err := s.api.Im.V1.MessageResource.Get(ctx, req)
	if err != nil {
		slog.Debug("feishu: attachment download failed", "type", resType, "key", key, "msg_id", msgID, "name", fileName, "err", s.redactErr(err))
		return att, false
	}
	if !resp.Success() {
		slog.Debug("feishu: attachment download non-OK", "type", resType, "key", key, "msg_id", msgID, "name", fileName, "code", resp.Code, "msg", resp.Msg)
		return att, false
	}
	// Prefer the server-reported filename when the caller had none (file
	// messages always carry file_name, but be defensive).
	name := fileName
	if name == "" {
		name = resp.FileName
	}
	// Backfill so the attachment metadata names the same file that lands on
	// disk (describeAtt would otherwise show an unnamed attachment).
	att.FileName = name
	abs, rel, n, sErr := s.files.Save(subdir, name, resp.File, s.maxAttBytes)
	if sErr != nil {
		slog.Debug("feishu: attachment not staged", "type", resType, "name", name, "err", sErr)
		return att, false
	}
	att.LocalPath = abs
	att.Path = rel
	att.Size = n
	return att, true
}

// imageKeyOf extracts the image_key from an "image" message's content JSON
// ({"image_key":"img_v2_xxx"}). Empty on malformed input.
func imageKeyOf(content string) string {
	var c struct {
		ImageKey string `json:"image_key"`
	}
	if json.Unmarshal([]byte(content), &c) != nil {
		return ""
	}
	return c.ImageKey
}

// fileKeyName extracts the file_key (and best-effort file_name) from a
// file/audio/media message's content JSON. Empty key on malformed input.
func fileKeyName(content string) (key, name string) {
	var c struct {
		FileKey  string `json:"file_key"`
		FileName string `json:"file_name"`
	}
	if json.Unmarshal([]byte(content), &c) != nil {
		return "", ""
	}
	return c.FileKey, c.FileName
}

// mimeByName returns a best-effort MIME from a filename extension ("" when
// unknown — acceptable; preprocess sniffs images by Kind anyway).
func mimeByName(name string) string {
	ext := filepath.Ext(name)
	if ext == "" {
		return ""
	}
	// mime.TypeByExtension may append "; charset=utf-8"; keep just the type.
	t := mime.TypeByExtension(ext)
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

// postBlock is one inline span of a Feishu rich-text ("post") message.
type postBlock struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	ImageKey string `json:"image_key"`
}

// parsePostContent parses a Feishu rich-text ("post") message's content JSON
// and returns the concatenated visible text (title + body lines) plus every
// embedded image_key.
//
// The post payload is a {"title","content"} object whose "content" is a
// [][]block grid (a list of lines, each a list of inline blocks). It may also
// arrive locale-wrapped as {"zh_cn":{...}} / {"en_us":{...}}; when the top
// level has no "content" key, the first locale sub-object is used (a defensive
// fallback — the inbound message.receive event delivers the un-wrapped form;
// the locale wrapper is the send schema).
//
// The title is prepended as the first line: a user whose only prose is in the
// post title (body = just an image) would otherwise have it dropped — and the
// whole message swallowed by onMessageReceive's empty-text+no-media gate.
//
// Block handling:
//   - "text" / "a"  → their .text is kept (links contribute their label).
//   - "at"          → dropped (mentions are not part of the user's prose).
//   - "img"         → its image_key is collected as an image attachment.
//
// Lines are joined with "\n"; empty lines (e.g. an image-only line) are skipped
// so the prose carries no blank-line noise. Malformed input yields ("", nil) —
// never panics.
func parsePostContent(raw string) (text string, imageKeys []string) {
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &top) != nil {
		return "", nil
	}
	// Resolve the object that actually holds "content": the top level for the
	// inbound shape, else the first locale sub-object of a send-style wrapper.
	obj := top
	if _, ok := top["content"]; !ok {
		obj = nil
		for _, sub := range top {
			var inner map[string]json.RawMessage
			if json.Unmarshal(sub, &inner) != nil {
				continue
			}
			if _, found := inner["content"]; found {
				obj = inner
				break
			}
		}
		if obj == nil {
			return "", nil
		}
	}

	var lines [][]postBlock
	if json.Unmarshal(obj["content"], &lines) != nil {
		return "", nil
	}

	var sb strings.Builder
	var title string
	if t, ok := obj["title"]; ok {
		_ = json.Unmarshal(t, &title) // ignore: non-string title just stays ""
	}
	if strings.TrimSpace(title) != "" {
		sb.WriteString(title)
	}
	for _, line := range lines {
		var lineSB strings.Builder
		for _, b := range line {
			switch b.Tag {
			case "text", "a":
				lineSB.WriteString(b.Text)
			case "img":
				if b.ImageKey != "" {
					imageKeys = append(imageKeys, b.ImageKey)
				}
			case "at":
				// mention — strip from prose
			}
		}
		if lineSB.Len() == 0 {
			continue // skip empty (e.g. image-only) lines — no blank-line noise
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(lineSB.String())
	}
	return strings.TrimSpace(sb.String()), imageKeys
}
