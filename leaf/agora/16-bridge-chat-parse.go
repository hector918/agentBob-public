package agora

import (
	"fmt"
	"strings"

	"agentbob/contract"
)

// This file ports skeleton agora/65-bridge-chat-parse.go into trunk, adapting to
// trunk's coordinate model: trunk stores an inbox_source's chat TYPE (dm|group)
// alongside source + chat id, so ParseBridgeToChat returns the chat type too
// (skeleton stored only source+chat). The gateway (contract.Gateway.SourceByName)
// replaces skeleton's SourceMap for the "is this source registered" check.

// reservedNonChatSources are source names that cannot be bridge targets — they're
// not real chat surfaces. webui has no chat; internal is the agora-emit
// pseudo-source; bridge is the inbox kind we're trying to wire INTO (chicken/egg);
// local has no remote chat surface to bind to.
var reservedNonChatSources = map[string]bool{
	"webui":                     true,
	contract.SourceNameInternal: true,
	"bridge":                    true,
	"local":                     true,
}

// ParseBridgeToChat parses a --bridge-to-chat=<arg> value into the
// (sourceID, chatType, chatID) coordinates of an inbox_source row. Accepts:
//
//   - "auto"                  → use ev.Source + ev.ChatType + ev.ChatID (the
//     slash path, run inside a chat; ev must be non-nil, ev.Source non-reserved
//     and (when gw != nil) registered, ev.ChatID non-empty).
//   - "<source>:<dm|group>:<chat>" → explicit triple; validates the dm/group
//     token (rejecting other tokens) and that the source is non-reserved and
//     (when gw != nil) registered.
//
// ev may be nil — the CLI path passes nil since it doesn't run inside a chat;
// "auto" then errors. gw may be nil too (the CLI path is a separate process with
// no live gateway); when nil, registration is skipped but the reserved-name check
// still runs.
//
// Errors are suitable to return verbatim to the admin (slash reply / CLI stderr).
func ParseBridgeToChat(arg string, ev *contract.MessageEvent, gw contract.Gateway) (string, contract.ChatType, string, error) {
	arg = strings.TrimSpace(arg) // tolerate "auto " / shell-quoting whitespace
	if arg == "auto" {
		if ev == nil {
			return "", "", "", fmt.Errorf("--bridge-to-chat=auto 需要聊天上下文；改用 --bridge-to-chat=<source>:<dm|group>:<chat>")
		}
		if ev.Source == "" || ev.ChatID == "" {
			return "", "", "", fmt.Errorf("--bridge-to-chat=auto：当前事件没有 source/chat（source=%q chat=%q）；改用 --bridge-to-chat=<source>:<dm|group>:<chat>", ev.Source, ev.ChatID)
		}
		if reservedNonChatSources[ev.Source] {
			return "", "", "", fmt.Errorf("--bridge-to-chat=auto：source %q 不是真实聊天平台；改用 --bridge-to-chat=<source>:<dm|group>:<chat>（telegram/wechat 机器人名）", ev.Source)
		}
		if gw != nil && gw.SourceByName(ev.Source) == nil {
			return "", "", "", fmt.Errorf("--bridge-to-chat=auto：source %q 未注册；检查 bob 启动日志里的已注册 source 名", ev.Source)
		}
		ct := ev.ChatType
		if ct == "" {
			ct = contract.ChatGroup
		}
		return ev.Source, ct, ev.ChatID, nil
	}
	parts := strings.SplitN(arg, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("--bridge-to-chat=%q：格式应为 <source>:<dm|group>:<chat>（例如 telegram:group:-100、telegram-prod:dm:12345）或 'auto'", arg)
	}
	src, typeTok, chat := parts[0], parts[1], parts[2]
	if reservedNonChatSources[src] {
		return "", "", "", fmt.Errorf("--bridge-to-chat=%q：source %q 不是真实聊天平台（保留名）；请填 telegram/wechat 机器人名", arg, src)
	}
	ct, ok := parseBridgeChatType(typeTok)
	if !ok {
		return "", "", "", fmt.Errorf("--bridge-to-chat=%q：聊天类型必须是 dm 或 group，收到 %q", arg, typeTok)
	}
	if gw != nil && gw.SourceByName(src) == nil {
		return "", "", "", fmt.Errorf("--bridge-to-chat=%q：source %q 未注册；检查 bob 启动日志里的已注册 source 名", arg, src)
	}
	return src, ct, chat, nil
}

// parseBridgeChatType maps the dm/group token to a contract.ChatType. Only dm and
// group are valid bridge bindings (channel/topic are not bound directly).
func parseBridgeChatType(tok string) (contract.ChatType, bool) {
	switch strings.ToLower(tok) {
	case "dm":
		return contract.ChatDM, true
	case "group":
		return contract.ChatGroup, true
	default:
		return "", false
	}
}
