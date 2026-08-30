package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

// TestStickerNameMIME pins the per-format sticker mapping: a static
// sticker stays image/webp, but animated (.tgs gzip-JSON) and video
// (.webm) stickers MUST get their real MIME so they don't punch through
// the downstream isImageAttachment gate and feed non-image bytes to a
// vision model.
func TestStickerNameMIME(t *testing.T) {
	cases := []struct {
		name     string
		st       *models.Sticker
		wantName string
		wantMIME string
	}{
		{"static", &models.Sticker{}, "sticker.webp", "image/webp"},
		{"animated", &models.Sticker{IsAnimated: true}, "sticker.tgs", "application/x-tgsticker"},
		{"video", &models.Sticker{IsVideo: true}, "sticker.webm", "video/webm"},
		// Video wins over animated if both were ever set.
		{"video_over_animated", &models.Sticker{IsVideo: true, IsAnimated: true}, "sticker.webm", "video/webm"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotName, gotMIME := stickerNameMIME(c.st)
			if gotName != c.wantName || gotMIME != c.wantMIME {
				t.Errorf("stickerNameMIME(%s) = (%q, %q); want (%q, %q)",
					c.name, gotName, gotMIME, c.wantName, c.wantMIME)
			}
		})
	}
}

// TestMediaOfSticker confirms the end-to-end mediaOf enumeration applies
// the per-format mapping (not the old hardcoded image/webp).
func TestMediaOfSticker(t *testing.T) {
	cases := []struct {
		name     string
		st       *models.Sticker
		wantMIME string
	}{
		{"static", &models.Sticker{FileID: "s1"}, "image/webp"},
		{"animated", &models.Sticker{FileID: "s2", IsAnimated: true}, "application/x-tgsticker"},
		{"video", &models.Sticker{FileID: "s3", IsVideo: true}, "video/webm"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refs := mediaOf(&models.Message{Sticker: c.st})
			if len(refs) != 1 {
				t.Fatalf("mediaOf returned %d refs, want 1", len(refs))
			}
			if refs[0].kind != "sticker" {
				t.Errorf("kind = %q, want sticker", refs[0].kind)
			}
			if refs[0].mime != c.wantMIME {
				t.Errorf("mime = %q, want %q", refs[0].mime, c.wantMIME)
			}
		})
	}
}
