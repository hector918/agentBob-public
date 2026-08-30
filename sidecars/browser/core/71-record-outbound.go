package core

import (
	"context"
	"log/slog"
)

// RecordOutbound records a bob-sent message id in the message_index so a
// later reply-to-bot routes back to the originating session, instead of
// falling back to a fresh one. Shared by the source Wire.Send leaves
// (telegram / email / wechat / feishu), which each previously re-coded
// the same nil/empty guard + warn string — the platform msgID is born in
// the leaf, but the recording side-effect is identical.
//
// No-op (silent) when store is nil, scope is empty, or msgID is empty —
// these are the "one-off notice / sink not wired / no scope" cases where
// there's nothing to index. A RecordMessage error is logged at WARN and
// swallowed: a missing index row degrades to the chat's active sid, not a
// delivery failure.
func RecordOutbound(ctx context.Context, store SessionStore, platform, chatID, msgID, scope string) {
	if store == nil || scope == "" || msgID == "" {
		return
	}
	if err := store.RecordMessage(ctx, platform, chatID, msgID, scope, ""); err != nil {
		slog.Warn("could not record bot message in message_index — a later reply will fall back to a fresh session",
			"platform", platform, "chat_id", chatID, "msg_id", msgID, "session", scope, "err", err)
	}
}
