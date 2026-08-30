package telegram

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// writePNG puts a w×h picture on disk. Only its header is ever read.
func writePNG(t *testing.T, w, h int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pic.png")
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Telegram rejects a photo outright past its own limits, so eligibility decides
// between "inline picture" and "arrives at all" — the fallback is the contract
// (contract.PhotoSender), not a safety net.
func TestPhotoEligibilityFollowsTelegramsLimits(t *testing.T) {
	if !photoEligible(writePNG(t, 1024, 1024)) {
		t.Error("a plain 1024² png must go as a photo — that is the whole point")
	}
	if !photoEligible(writePNG(t, 832, 1216)) {
		t.Error("a portrait generation must go as a photo")
	}
	// width + height > 10000
	if photoEligible(writePNG(t, 9000, 2000)) {
		t.Error("a picture past the dimension sum must fall back to a document")
	}
	// ratio > 20
	if photoEligible(writePNG(t, 2100, 100)) {
		t.Error("a picture past the ratio limit must fall back to a document")
	}
}

// Anything the standard decoders do not recognise is not a picture as far as this
// check is concerned — WebP and HEIC included, and an iPhone shares HEIC by
// default. Guessing would mean an API rejection instead of a delivery.
func TestNonImagesAndUnknownFormatsFallBackToDocument(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.7\n..."), 0o600); err != nil {
		t.Fatal(err)
	}
	if photoEligible(pdf) {
		t.Error("a pdf must never go as a photo")
	}
	heic := filepath.Join(dir, "IMG_0001.heic")
	if err := os.WriteFile(heic, []byte("\x00\x00\x00\x18ftypheic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if photoEligible(heic) {
		t.Error("heic is not decodable here — it must go as a document rather than be guessed at")
	}
	if photoEligible(filepath.Join(dir, "does-not-exist.png")) {
		t.Error("a missing file must not be classified as a photo")
	}
}

// Over 10 MB Telegram refuses the photo, whatever its dimensions are.
func TestOversizeFileFallsBackToDocument(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.jpg")
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	// A real header followed by enough padding to break the size limit.
	body := append(buf.Bytes(), make([]byte, tgPhotoMaxBytes)...)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if photoEligible(p) {
		t.Errorf("a %d-byte file must fall back to a document", len(body))
	}
}
