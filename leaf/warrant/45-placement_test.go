package warrant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbob/contract"
)

// stageFile writes a fake staged download (named like files.Store does:
// "<nanos>-<hex>-<name>") under home/staging and returns its absolute path.
func stageFile(t *testing.T, home, stagedBase string, data []byte) string {
	t.Helper()
	dir := filepath.Join(home, "staging")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	p := filepath.Join(dir, stagedBase)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	return p
}

func inboxDir(t *testing.T, home, scope string) string {
	t.Helper()
	d, err := SpaceLocalDir(home, scope)
	if err != nil {
		t.Fatalf("SpaceLocalDir: %v", err)
	}
	return filepath.Join(d, contract.InboxSubdir)
}

// TestPlaceAttachmentsCleanNaming: the inbox file is named from the clean display
// name (display == on-disk), the staged prefix is dropped, and Path is rewritten
// space-relative with LocalPath cleared.
func TestPlaceAttachmentsCleanNaming(t *testing.T) {
	home := t.TempDir()
	scope := "telegram_dm_123"
	staged := stageFile(t, home, "1782779006539634923-88a6a627-photo-222.jpg", []byte("img"))

	atts := []contract.Attachment{{Kind: "image", FileName: "photo-222.jpg", LocalPath: staged}}
	out := PlaceAttachments(home, scope, atts)

	if got := out[0].Path; got != "inbox/photo-222.jpg" {
		t.Errorf("Path = %q, want inbox/photo-222.jpg", got)
	}
	if out[0].LocalPath != "" {
		t.Errorf("LocalPath = %q, want cleared", out[0].LocalPath)
	}
	if _, err := os.Stat(filepath.Join(inboxDir(t, home, scope), "photo-222.jpg")); err != nil {
		t.Errorf("clean-named file not in inbox: %v", err)
	}
}

// TestPlaceAttachmentsFailureClearsPath pins F55: an attachment that can't be placed
// (move fails on a missing staged file) or was never downloaded (empty LocalPath) must
// NOT keep a staged, space-dead Path — a dead handle would otherwise reach the prompt +
// history and fail every ocr/read. It is cleared to "" (degraded to "not downloaded").
func TestPlaceAttachmentsFailureClearsPath(t *testing.T) {
	home := t.TempDir()
	scope := "telegram_dm_123"
	// One good attachment (should place), one whose staged file is missing (move fails),
	// one never downloaded but carrying a stale staged Path.
	good := stageFile(t, home, "1-a-ok.jpg", []byte("img"))
	atts := []contract.Attachment{
		{Kind: "image", FileName: "ok.jpg", LocalPath: good},
		{Kind: "image", FileName: "gone.jpg", LocalPath: filepath.Join(home, "nope", "gone.jpg"), Path: "staging/gone.jpg"},
		{Kind: "image", FileName: "undown.jpg", Path: "staging/undown.jpg"}, // LocalPath == "" → never downloaded
	}
	out := PlaceAttachments(home, scope, atts)

	if out[0].Path != "inbox/ok.jpg" {
		t.Errorf("good attachment Path = %q, want inbox/ok.jpg", out[0].Path)
	}
	if out[1].Path != "" {
		t.Errorf("move-failed attachment must clear its staged Path, got %q", out[1].Path)
	}
	if out[2].Path != "" {
		t.Errorf("never-downloaded attachment must clear its staged Path, got %q", out[2].Path)
	}
}

// TestPlaceAttachmentsDedup: two attachments with the same display name don't
// overwrite — the second gets a "-2" stem suffix.
func TestPlaceAttachmentsDedup(t *testing.T) {
	home := t.TempDir()
	scope := "telegram_dm_123"
	a1 := stageFile(t, home, "111-aaaa-photo.jpg", []byte("one"))
	a2 := stageFile(t, home, "222-bbbb-photo.jpg", []byte("two"))

	out := PlaceAttachments(home, scope, []contract.Attachment{
		{Kind: "image", FileName: "photo.jpg", LocalPath: a1},
		{Kind: "image", FileName: "photo.jpg", LocalPath: a2},
	})

	if out[0].Path != "inbox/photo.jpg" {
		t.Errorf("first Path = %q, want inbox/photo.jpg", out[0].Path)
	}
	if out[1].Path != "inbox/photo-2.jpg" {
		t.Errorf("second Path = %q, want inbox/photo-2.jpg", out[1].Path)
	}
	// both files survive with their own bytes (no overwrite)
	inbox := inboxDir(t, home, scope)
	if b, _ := os.ReadFile(filepath.Join(inbox, "photo.jpg")); string(b) != "one" {
		t.Errorf("photo.jpg = %q, want one", b)
	}
	if b, _ := os.ReadFile(filepath.Join(inbox, "photo-2.jpg")); string(b) != "two" {
		t.Errorf("photo-2.jpg = %q, want two", b)
	}
}

// TestPlaceAttachmentsSanitized: a crafted FileName (path separators, traversal,
// control chars) cannot escape the inbox — it folds to a single safe base.
func TestPlaceAttachmentsSanitized(t *testing.T) {
	home := t.TempDir()
	scope := "telegram_dm_123"
	staged := stageFile(t, home, "333-cccc-x.png", []byte("img"))

	out := PlaceAttachments(home, scope, []contract.Attachment{
		{Kind: "image", FileName: "../../../etc/evil\n.png", LocalPath: staged},
	})

	rel := out[0].Path
	if !strings.HasPrefix(rel, "inbox/") || strings.ContainsAny(rel, "\n") {
		t.Fatalf("Path = %q, want a clean inbox/ path", rel)
	}
	base := strings.TrimPrefix(rel, "inbox/")
	if strings.ContainsAny(base, `/\`) || strings.Contains(base, "..") {
		t.Errorf("sanitised base still unsafe: %q", base)
	}
	// the placed file must sit directly under the inbox, nowhere else
	if _, err := os.Stat(filepath.Join(inboxDir(t, home, scope), base)); err != nil {
		t.Errorf("sanitised file not in inbox: %v", err)
	}
}

// TestPlaceAttachmentsCJKName: an all-CJK display name (e.g. "发票.jpg") must not
// collapse to a bare extension-less token — the extension survives sanitizing so a
// later turn's kind/MIME resolution (mime-by-extension) still sees an image (F54).
func TestPlaceAttachmentsCJKName(t *testing.T) {
	home := t.TempDir()
	scope := "telegram_dm_123"
	staged := stageFile(t, home, "555-eeee-fapiao.jpg", []byte("img"))

	out := PlaceAttachments(home, scope, []contract.Attachment{
		{Kind: "image", FileName: "发票.jpg", LocalPath: staged},
	})

	if out[0].Path != "inbox/file.jpg" {
		t.Errorf("Path = %q, want inbox/file.jpg (extension preserved)", out[0].Path)
	}
	if _, err := os.Stat(filepath.Join(inboxDir(t, home, scope), "file.jpg")); err != nil {
		t.Errorf("CJK-named file not in inbox under its fallback name: %v", err)
	}
}

// TestInboxBaseName pins the sanitizer's edge cases: extension preservation for
// non-ASCII stems, the staged-base fallback when nothing is usable, and that a
// crafted extension can't ride through unsanitized.
func TestInboxBaseName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"photo-222.jpg", "photo-222.jpg"}, // clean ASCII name passes through
		{"发票.jpg", "file.jpg"},             // all-CJK stem → fallback stem, extension kept
		{"报告", ""},                         // no extension, no safe chars → staged-base fallback
		{"发票.文档", ""},                      // non-ASCII "extension" is stem too → nothing usable
		{"invoice.超", "invoice"},           // unusable extension sanitized away, stem survives
		{"photo.tar.gz", "photo.tar.gz"},   // only the final extension is split off
		{"-report.pdf", "report.pdf"},      // leading punctuation trimmed off the stem
		{strings.Repeat("a", 100) + ".jpg", strings.Repeat("a", 76) + ".jpg"}, // truncation keeps ext
	}
	for _, c := range cases {
		if got := inboxBaseName(c.in); got != c.want {
			t.Errorf("inboxBaseName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPlaceAttachmentsNoFileNameFallback: an attachment with no usable FileName keeps
// the unique staged base (still placed, still collision-proof).
func TestPlaceAttachmentsNoFileNameFallback(t *testing.T) {
	home := t.TempDir()
	scope := "telegram_dm_123"
	staged := stageFile(t, home, "444-dddd-sticker.webp", []byte("img"))

	out := PlaceAttachments(home, scope, []contract.Attachment{
		{Kind: "sticker", FileName: "", LocalPath: staged},
	})

	if out[0].Path != "inbox/444-dddd-sticker.webp" {
		t.Errorf("Path = %q, want the staged base kept", out[0].Path)
	}
	if _, err := os.Stat(filepath.Join(inboxDir(t, home, scope), "444-dddd-sticker.webp")); err != nil {
		t.Errorf("fallback file not in inbox: %v", err)
	}
}
