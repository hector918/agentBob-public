package feishu

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"agentbob/heartwood/prompt"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// nameGetTimeout caps the synchronous Message.Get calls the inbound worker path
// makes (senderName and fetchParentInfo), so a slow/hanging Feishu API can't
// park a recv worker indefinitely (the transport's 60s header timeout is the
// only other bound).
const nameGetTimeout = 5 * time.Second

// shouldDropAsGroupNoise decides whether a multi-party inbound message is noise
// that the source should ignore. Such a message reaches bob only when the bot
// was @-mentioned OR it replies to one of bob's own messages (reply-to-bot).
// Only the DM type ("p2p") always passes; EVERY non-p2p chat_type — "group" AND
// "topic_group" (话题群) and any future/unknown value — is gated, matching
// mapChatType (which maps everything but p2p to ChatGroup) and telegram's gate
// (`ct != ChatDM && !mentionsBot && !replyToBot`). Gating on != "p2p" (not
// == "group") closes the topic_group hole where the bot answered all chatter.
//
// Pure so the gate decision is testable without a live SDK.
func shouldDropAsGroupNoise(chatType string, mentionsBot, replyToBot bool) bool {
	return chatType != "p2p" && !mentionsBot && !replyToBot
}

// replyParentInfo is the best-effort context resolved from a replied-to
// (parent) message: the quoted text, the parent sender's display name, and
// whether the parent was sent by THIS bot (reply-to-bot).
type replyParentInfo struct {
	text    string
	user    string
	fromBot bool
}

// parentCacheMax bounds the parent-fetch cache. N images replying to the same
// prompt would otherwise refetch the same parent N times; the cache collapses
// those to one Message.Get. The cap keeps memory bounded for a long-lived
// process; on overflow the whole map is dropped (simple, no LRU dep) — a rare
// event that costs at most a re-fetch.
const parentCacheMax = 512

// parentInfo returns the cached/fetched quote text + sender name for a parent
// message. Concurrency-safe: onMessageReceive can run concurrently. On any
// fetch error it returns an empty info (and does not cache, so a transient
// failure can be retried by a later reply).
func (s *Source) parentInfo(ctx context.Context, parentID string) replyParentInfo {
	s.parentMu.Lock()
	if s.parentCache != nil {
		if info, ok := s.parentCache[parentID]; ok {
			s.parentMu.Unlock()
			return info
		}
	}
	s.parentMu.Unlock()

	info, ok := s.fetchParentInfo(ctx, parentID)
	if !ok {
		return replyParentInfo{} // leave text/user empty; reply id/bot already set by caller
	}

	s.parentMu.Lock()
	if s.parentCache == nil {
		s.parentCache = make(map[string]replyParentInfo)
	}
	if len(s.parentCache) >= parentCacheMax {
		// Simple bound: drop the whole map rather than pull an LRU dep. Rare
		// for a single-bot deploy; the cost is at most a re-fetch.
		s.parentCache = make(map[string]replyParentInfo)
	}
	s.parentCache[parentID] = info
	s.parentMu.Unlock()
	return info
}

// fetchParentInfo resolves the replied-to message via Message.Get. The response
// shape is resp.Data.Items[0] carrying MsgType, Body.Content (the per-type
// content JSON), and Sender.SenderName. Returns ok==false on any error / empty
// payload so the caller leaves the quote empty without failing the inbound.
func (s *Source) fetchParentInfo(ctx context.Context, parentID string) (replyParentInfo, bool) {
	if s.api == nil {
		return replyParentInfo{}, false
	}
	// Same per-call cap as senderName: this fetch runs on the recv worker for
	// every reply (before the group-noise gate), so a slow API must fail fast
	// (→ ok=false, quote empty / fromBot=false) instead of parking the worker
	// for the transport's full 60s.
	gctx, cancel := context.WithTimeout(ctx, nameGetTimeout)
	defer cancel()
	req := larkim.NewGetMessageReqBuilder().MessageId(parentID).Build()
	resp, err := s.api.Im.V1.Message.Get(gctx, req)
	if err != nil {
		slog.Debug("feishu: parent message fetch failed", "parent_id", parentID, "err", s.redactErr(err))
		return replyParentInfo{}, false
	}
	if !resp.Success() || resp.Data == nil || len(resp.Data.Items) == 0 || resp.Data.Items[0] == nil {
		slog.Debug("feishu: parent message empty/non-OK", "parent_id", parentID)
		return replyParentInfo{}, false
	}
	item := resp.Data.Items[0]

	content := ""
	if item.Body != nil {
		content = str(item.Body.Content)
	}
	info := replyParentInfo{text: parentQuoteText(str(item.MsgType), content)}
	if item.Sender != nil {
		info.user = str(item.Sender.SenderName)
		// reply-to-bot: the parent was sent by THIS bot. Match by id_type — a bot/app
		// send carries IdType="app_id" (Id = our app_id), a human carries "open_id"
		// (Id = their open_id). The bot-reply case is the app_id branch; the open_id
		// branch is defensive. Mismatch / empty id → false (fails safe to @-only).
		switch str(item.Sender.IdType) {
		case "app_id":
			info.fromBot = s.appID != "" && str(item.Sender.Id) == s.appID
		case "open_id":
			info.fromBot = s.botOpenID != "" && str(item.Sender.Id) == s.botOpenID
		}
	}
	return info, true
}

// nameCacheMax bounds the per-sender name cache (distinct senders, not per-message).
const nameCacheMax = 4096

// senderName resolves a sender's display name by open_id for the per-message author +
// the group "[名字]: " attribution. The inbound event carries only ids, so the name is
// fetched once via Message.Get(msgID).Sender.SenderName and cached per open_id. Empty
// results are NOT cached (a transient failure retries next time). "" when unresolved —
// the turn then renders no prefix, same as a nameless speaker. Bounded like parentCache.
func (s *Source) senderName(ctx context.Context, openID, msgID string) string {
	if openID == "" || msgID == "" || s.api == nil {
		return ""
	}
	s.nameMu.Lock()
	if n, ok := s.nameCache[openID]; ok {
		s.nameMu.Unlock()
		return n
	}
	s.nameMu.Unlock()

	gctx, cancel := context.WithTimeout(ctx, nameGetTimeout)
	defer cancel()
	req := larkim.NewGetMessageReqBuilder().MessageId(msgID).Build()
	resp, err := s.api.Im.V1.Message.Get(gctx, req)
	if err != nil {
		slog.Debug("feishu: sender name fetch failed", "msg_id", msgID, "err", s.redactErr(err))
		return ""
	}
	if !resp.Success() || resp.Data == nil || len(resp.Data.Items) == 0 || resp.Data.Items[0] == nil || resp.Data.Items[0].Sender == nil {
		return ""
	}
	name := str(resp.Data.Items[0].Sender.SenderName)
	if name == "" {
		return ""
	}
	s.nameMu.Lock()
	if s.nameCache == nil {
		s.nameCache = make(map[string]string)
	}
	if len(s.nameCache) >= nameCacheMax {
		s.nameCache = make(map[string]string) // simple bound, like parentCache
	}
	s.nameCache[openID] = name
	s.nameMu.Unlock()
	return name
}

// parentQuoteText renders a short quote for a replied-to message of the given
// type. Text/post messages contribute their prose; media types go through
// prompt.QuotedMediaNote, the shared wording every source's quote note must use.
// Unknown / unparseable → "".
//
// inAttachments is always false here: collectAttachments (30-attachments.go) takes
// only the CURRENT message, so unlike telegram this source never ingests a parent's
// media — the note's out-of-reach clause is unconditionally true. A feishu change that
// starts fetching parent media has to revisit this argument.
func parentQuoteText(msgType, content string) string {
	switch msgType {
	case "text":
		return textContent(content)
	case "post":
		t, _ := parsePostContent(content)
		if t == "" {
			return prompt.QuotedMediaNote("一张图片", false) // a post with no prose is image-only
		}
		return t
	case "image":
		return prompt.QuotedMediaNote("一张图片", false)
	case "file":
		if _, name := fileKeyName(content); name != "" {
			return prompt.QuotedMediaNote("一个文件："+name, false)
		}
		return prompt.QuotedMediaNote("一个文件", false)
	case "audio":
		return prompt.QuotedMediaNote("一条语音", false)
	case "media":
		return prompt.QuotedMediaNote("一段视频", false)
	case "sticker":
		return prompt.QuotedMediaNote("一个表情", false)
	default:
		// share_chat / share_user / interactive / unknown: best-effort try at a
		// "text" field, else empty.
		var c struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(content), &c) == nil && c.Text != "" {
			return c.Text
		}
		return ""
	}
}
