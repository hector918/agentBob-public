package providers

import (
	"strings"
	"testing"

	"agentbob/contract"
)

// TestToWireAudioBlock verifies that a contract.Message carrying Audio is
// translated into the content-block-array wire form with an audio_url
// block whose URL is a base64 data URI — mirroring the image_url path.
func TestToWireAudioBlock(t *testing.T) {
	wm := toWire(contract.Message{
		Role:    "user",
		Content: "what did i say?",
		Audio:   []contract.AudioRef{{Data: []byte("fake-ogg-bytes"), MIME: "audio/ogg"}},
	})

	blocks, ok := wm.Content.([]wireContentBlock)
	if !ok {
		t.Fatalf("expected content to be []wireContentBlock, got %T", wm.Content)
	}

	var audio *wireContentBlock
	for i := range blocks {
		if blocks[i].Type == "audio_url" {
			audio = &blocks[i]
			break
		}
	}
	if audio == nil {
		t.Fatalf("no audio_url block in content: %+v", blocks)
	}
	if audio.AudioURL == nil {
		t.Fatalf("audio_url block has nil AudioURL")
	}
	if !strings.HasPrefix(audio.AudioURL.URL, "data:audio/ogg;base64,") {
		t.Fatalf("audio_url URL not a base64 data URI: %q", audio.AudioURL.URL)
	}
}

// TestToWireAudioDefaultMIME verifies the MIME defaults to audio/ogg
// when AudioRef.MIME is empty.
func TestToWireAudioDefaultMIME(t *testing.T) {
	wm := toWire(contract.Message{
		Role:  "user",
		Audio: []contract.AudioRef{{Data: []byte("x")}},
	})
	blocks, ok := wm.Content.([]wireContentBlock)
	if !ok {
		t.Fatalf("expected []wireContentBlock, got %T", wm.Content)
	}
	if len(blocks) != 1 || blocks[0].AudioURL == nil {
		t.Fatalf("expected one audio_url block, got %+v", blocks)
	}
	if !strings.HasPrefix(blocks[0].AudioURL.URL, "data:audio/ogg;base64,") {
		t.Fatalf("expected default audio/ogg MIME, got %q", blocks[0].AudioURL.URL)
	}
}
