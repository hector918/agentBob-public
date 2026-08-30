package feishu

import larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

// senderOpenID extracts the human sender's open_id from an inbound message
// event — the value bob uses as contract.MessageEvent.UserID. Returns "" when
// absent; the caller drops the event rather than fabricating an identity
// (design decision: open_id only, no fallback).
func senderOpenID(e *larkim.P2MessageReceiveV1) string {
	if e == nil || e.Event == nil || e.Event.Sender == nil {
		return ""
	}
	sid := e.Event.Sender.SenderId
	if sid == nil || sid.OpenId == nil {
		return ""
	}
	return *sid.OpenId
}

// senderUnionID extracts the sender's union_id — the tenant-wide identity that
// is STABLE ACROSS feishu apps (open_id is per-app). Used as
// contract.MessageEvent.StableUID so the accounts ledger keys one person to one
// handle across multiple feishu apps. "" when absent (the accounts layer then
// falls back to open_id). The gate/allowlist path keeps using open_id (UserID)
// regardless.
func senderUnionID(e *larkim.P2MessageReceiveV1) string {
	if e == nil || e.Event == nil || e.Event.Sender == nil {
		return ""
	}
	sid := e.Event.Sender.SenderId
	if sid == nil || sid.UnionId == nil {
		return ""
	}
	return *sid.UnionId
}
