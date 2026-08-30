package feishu

import (
	"context"
	"testing"

	"agentbob/contract"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func strp(s string) *string { return &s }

// collectAttachments with a nil file store (capture disabled) must still return
// a post message's prose — an early return there swallowed the text, and
// onMessageReceive's empty-text+no-media gate then dropped the WHOLE message.
// The nil guard now lives in downloadResource (graceful degrade, matching the
// failed-download contract), so the prose passes through and image blocks still
// yield metadata-only attachments (empty LocalPath) for describeAtt.
func TestCollectAttachmentsNilStoreKeepsPostText(t *testing.T) {
	s := &Source{name: "feishu"} // files == nil, api == nil: capture disabled
	msg := &larkim.EventMessage{
		MessageId:   strp("om_1"),
		MessageType: strp("post"),
		Content:     strp(`{"title":"报告","content":[[{"tag":"text","text":"看一下这个"}],[{"tag":"img","image_key":"img_k1"}]]}`),
	}

	text, atts := s.collectAttachments(context.Background(), msg, contract.MessageEvent{})
	if want := "报告\n看一下这个"; text != want {
		t.Errorf("post text = %q, want %q (prose must survive a nil file store)", text, want)
	}
	if len(atts) != 1 {
		t.Fatalf("attachments = %d, want 1 metadata-only attachment", len(atts))
	}
	if atts[0].LocalPath != "" || atts[0].Path != "" {
		t.Errorf("attachment staged despite nil store: LocalPath=%q Path=%q", atts[0].LocalPath, atts[0].Path)
	}
	if atts[0].Kind != "image" {
		t.Errorf("attachment Kind = %q, want image", atts[0].Kind)
	}
}

// A pure media message under a nil store degrades to a metadata-only attachment
// (same shape as a failed download) without touching the API client.
func TestCollectAttachmentsNilStoreImageDegrades(t *testing.T) {
	s := &Source{name: "feishu"}
	msg := &larkim.EventMessage{
		MessageId:   strp("om_2"),
		MessageType: strp("image"),
		Content:     strp(`{"image_key":"img_k2"}`),
	}

	text, atts := s.collectAttachments(context.Background(), msg, contract.MessageEvent{})
	if text != "" {
		t.Errorf("image message text = %q, want empty", text)
	}
	if len(atts) != 1 || atts[0].LocalPath != "" {
		t.Fatalf("attachments = %+v, want one metadata-only attachment", atts)
	}
}
