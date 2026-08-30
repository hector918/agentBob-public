package warrant

import (
	"os"
	"path/filepath"
	"testing"

	"agentbob/contract"
)

// The Transcribed mark is set BEFORE placement and read AFTER it. Placement rewrites
// Path and clears LocalPath; if it ever rebuilt the attachments instead of mutating them
// in place, the mark would vanish silently and the redundant-fetch regression would come
// straight back with no test failing.
func TestPlacementPreservesTranscribedMark(t *testing.T) {
	home := t.TempDir()
	staged := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(staged, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := PlaceAttachments(home, "telegram:dm:1", []contract.Attachment{
		{Kind: "voice", FileName: "voice.ogg", LocalPath: staged, Transcribed: true},
	})
	if len(out) != 1 {
		t.Fatalf("got %d attachments", len(out))
	}
	if out[0].Path == "" {
		t.Fatalf("placement did not place: %+v", out[0])
	}
	if !out[0].Transcribed {
		t.Error("placement dropped the Transcribed mark — the prompt list would go silent")
	}
}
